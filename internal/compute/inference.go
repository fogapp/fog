package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/lucid"
	"gitea.lan/matthew/fog/internal/store"
)

// Inference is the second kind of pool: apps see "fog · Inference", one
// OpenAI-compatible endpoint on fogd, never the machine behind it. fogd
// routes each request by its model to a machine that has it; a machine's
// fog-node fronts its own model server and takes only fog's key.

// InferencePool is the pool's id: a grant switches it per app like a
// video pool.
const InferencePool = "inference"

// InferencePath is where an app reaches it on fogd, with its own
// FOG_API_TOKEN (FOG_INFERENCE_URL).
const InferencePath = "/app/v1/inference/v1/"

// maxInferenceBody bounds a request fogd reads to route: a chat with a
// long context, never a file upload.
const maxInferenceBody = 32 << 20

// Inference is the pool of machines lending a model server, as Settings ›
// Compute shows it; nil when there's none.
func Inference(machines []Machine) *Pool {
	var p *Pool
	for _, m := range machines {
		if m.Inventory.Inference == nil {
			continue
		}
		if p == nil {
			p = &Pool{ID: InferencePool, Name: PoolName(InferencePool), Family: "openai", Models: []string{}}
		}
		p.Machines = append(p.Machines, m.ID)
		if !m.Online || m.Paused {
			continue
		}
		p.Online++
		for _, model := range m.Inventory.Inference.Models {
			if !slices.Contains(p.Models, model) {
				p.Models = append(p.Models, model)
			}
		}
	}
	if p != nil {
		slices.Sort(p.Models)
	}
	return p
}

// lending reports the machines that can take an inference request now.
func lending(machines []Machine) []Machine {
	var out []Machine
	for _, m := range machines {
		if m.Online && !m.Paused && m.Inventory.Inference != nil && m.Inventory.Inference.URL != "" {
			out = append(out, m)
		}
	}
	return out
}

// InferenceAllowed reports whether app may use the inference pool: it
// declares inference work, and the operator hasn't switched it off.
func InferenceAllowed(st *store.Store, app *store.App) bool {
	raw, err := st.AppCompute(app.ID)
	if err != nil {
		return false
	}
	d := ParseDeclared(raw)
	if d == nil || d.Inference == nil {
		return false
	}
	off, err := st.ComputeGrantOff(app.ID, InferencePool)
	return err == nil && !off
}

// householdOnly reports whether app's fog.yaml keeps its requests on machines
// whose model server is the machine itself.
func householdOnly(st *store.Store, app *store.App) bool {
	raw, err := st.AppCompute(app.ID)
	if err != nil {
		return true // can't read the limit, so hold to it
	}
	d := ParseDeclared(raw)
	return d != nil && d.Inference != nil && d.Inference.Locality != ""
}

// local narrows pool to the machines reporting their model server as their
// own.
func local(pool []Machine) []Machine {
	var out []Machine
	for _, m := range pool {
		if m.Inventory.Inference.Local {
			out = append(out, m)
		}
	}
	return out
}

// ServeInference answers one app's request to the inference pool: the
// models every machine has, or the request routed to a machine with the
// model it names. Every request but the model list is in the activity log.
func (s *Service) ServeInference(w http.ResponseWriter, r *http.Request, app *store.App) {
	rest := strings.TrimPrefix(r.URL.Path, InferencePath)
	listing := rest == "models" && r.Method == http.MethodGet
	// The app's own trace, when it passed one: the request's transaction
	// is dropped when the answer streams.
	parent := lucid.WithTraceparent(context.Background(), r.Header.Get("traceparent"))
	row := store.ComputeActivity{App: app.Name, Pool: InferencePool, Kind: KindInference, What: rest}
	refuse := func(status int, msg string) {
		if !listing {
			s.refused(parent, row, &status, msg)
		}
		writeOpenAIError(w, status, msg)
	}
	if !InferenceAllowed(s.Registry.Store, app) {
		refuse(http.StatusForbidden, app.Name+" doesn't declare inference work in its fog.yaml, or it's switched off in Settings › Compute")
		return
	}
	machines, err := s.Registry.Machines()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pool := lending(machines)
	if len(pool) == 0 {
		refuse(http.StatusServiceUnavailable, "no machine is lending a model server to fog right now")
		return
	}
	// An app held to the household sees only what it may use: its model list
	// and its routing both start from the machines that qualify.
	household := householdOnly(s.Registry.Store, app)
	allowed, listed := pool, machines
	if household {
		allowed, listed = local(pool), local(pool)
		if len(allowed) == 0 {
			refuse(http.StatusServiceUnavailable, app.Name+" keeps its inference in the household (locality in its fog.yaml), and no machine lending to fog runs its model server itself")
			return
		}
	}
	if listing {
		writeModels(w, Inference(listed).Models)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInferenceBody+1))
	if err != nil || len(body) > maxInferenceBody {
		refuse(http.StatusRequestEntityTooLarge, "the request is too large")
		return
	}
	var named struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &named)
	row.Model = named.Model
	targets := s.ranked(r.Context(), app, allowed, named.Model, named.Stream)
	if len(targets) == 0 && household && choose(pool, named.Model) != nil {
		refuse(http.StatusForbidden, app.Name+" keeps its inference in the household (locality in its fog.yaml), and the only machines with the model "+named.Model+" reach it elsewhere")
		return
	}
	if len(targets) == 0 {
		refuse(http.StatusNotFound, "no machine lending to fog has the model "+named.Model)
		return
	}
	// A machine that fails before the first byte of its answer has sent the
	// app nothing, so the next machine with the model gets one more try.
	for i, target := range targets[:min(len(targets), 2)] {
		last := i == min(len(targets), 2)-1
		if s.forward(w, r, parent, row, target, rest, body, named.Model, named.Stream, last) {
			return
		}
	}
}

// forward sends one request to one machine. Unless it's the last try, a
// failure before the machine answered writes nothing and reports false, so
// the caller can try another.
func (s *Service) forward(w http.ResponseWriter, r *http.Request, parent context.Context, row store.ComputeActivity, target *Machine,
	rest string, body []byte, model string, stream, last bool) bool {
	base, err := url.Parse(target.Inventory.Inference.URL)
	if err != nil {
		if !last {
			return false
		}
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return true
	}
	row.Machine = target.Name
	concurrent := s.inflight(target.ID, 1)
	defer s.inflight(target.ID, -1)
	t := s.begin(parent, row)
	tap := &answerTap{start: t.start}
	status, failure, answered := 0, "", false
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = base.Scheme, base.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = "/v1/"+rest, ""
			pr.Out.Host = base.Host
			// Proven per request, never a bearer: the key stays here.
			nonce, at := NewNonce(), time.Now().UTC().Format(time.RFC3339Nano)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set(HeaderInferenceNonce, nonce)
			pr.Out.Header.Set(HeaderInferenceTime, at)
			sum := BodySHA256(body)
			pr.Out.Header.Set(HeaderBodySHA256, sum)
			pr.Out.Header.Set(HeaderInferenceAuth, InferenceMAC(target.secret, nonce, at, pr.Out.Method, pr.Out.URL.RequestURI(),
				pr.Out.Header.Get("Content-Type"), sum))
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("traceparent")
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
		},
		ModifyResponse: func(resp *http.Response) error {
			answered = true
			status = resp.StatusCode
			tap.ReadCloser = resp.Body
			tap.sse = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
			resp.Body = tap
			return nil
		},
		// Streamed completions arrive token by token.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status, failure = http.StatusBadGateway, target.Name+" didn't answer: "+err.Error()
			if answered || last {
				writeOpenAIError(w, http.StatusBadGateway, failure)
			}
		},
	}
	proxy.ServeHTTP(w, r)
	if !answered && !last && r.Context().Err() == nil {
		s.finish(t, OutcomeFailed, &status, failure+"; tried another machine")
		return false
	}

	ans := tap.answer()
	t.row.TTFTMS, t.row.TokensIn, t.row.TokensOut, t.row.TokensEstimated = ans.ttft.Milliseconds(), ans.in, ans.out, ans.estimated
	if ans.served != "" && t.row.Model == "" {
		t.row.Model = ans.served
	}
	outcome := OutcomeOK
	switch {
	case failure != "":
		outcome = OutcomeFailed
	case status >= 400:
		outcome, failure = OutcomeFailed, ans.err
		if failure == "" {
			failure = target.Name + " answered " + http.StatusText(status)
		}
	case r.Context().Err() != nil:
		outcome, failure = OutcomeStopped, "the app stopped waiting for the answer"
	}
	call := lucid.LLM{
		Provider: "fog", Model: model, Served: ans.served, Tier: "local", Agent: row.App, Route: "machine:" + target.Name,
		In: ans.in, Out: ans.out, Estimated: ans.estimated, CostSrc: "free", TTFT: float64(ans.ttft.Microseconds()) / 1000,
		Concurrent: concurrent, Stream: stream, Stop: ans.stop,
		Attrs: map[string]any{"app": row.App, "machine": target.Name, "endpoint": rest},
	}
	if call.Model == "" {
		call.Model = ans.served
	}
	if outcome != OutcomeOK && outcome != OutcomeStopped {
		call.Status, call.Error = "error", llmErrorClass(status)
	}
	lucid.ModelCall(t.ctx, t.start, call)
	s.finish(t, outcome, &status, failure)
	return true
}

// ranked is every machine that can take a request for a model, the one
// with the fewest requests in flight first, or in the order the household's
// inference router says when one answers in time.
func (s *Service) ranked(ctx context.Context, app *store.App, pool []Machine, model string, stream bool) []*Machine {
	var out []*Machine
	for i := range pool {
		if model == "" || slices.Contains(pool[i].Inventory.Inference.Models, model) {
			out = append(out, &pool[i])
		}
	}
	s.mu.Lock()
	busy := map[string]int{}
	for id, n := range s.answering {
		busy[id] = n
	}
	s.mu.Unlock()
	slices.SortStableFunc(out, func(a, b *Machine) int { return busy[a.ID] - busy[b.ID] })
	if s.Router == nil || len(out) < 2 {
		return out
	}
	ask := RouteAsk{App: app.Name, Model: model, Stream: stream}
	for _, m := range out {
		ask.Machines = append(ask.Machines, RouteMachine{ID: m.ID, Name: m.Name, Models: m.Inventory.Inference.Models, InFlight: busy[m.ID]})
	}
	ctx, cancel := context.WithTimeout(ctx, RouterBudget)
	defer cancel()
	order, err := s.Router(ctx, ask)
	if err != nil || len(order) == 0 {
		return out
	}
	rank := map[string]int{}
	for i, name := range order {
		rank[name] = i + 1
	}
	at := func(m *Machine) int {
		if i, ok := rank[m.Name]; ok {
			return i
		}
		return len(order) + 1
	}
	slices.SortStableFunc(out, func(a, b *Machine) int { return at(a) - at(b) })
	return out
}

// RouterContract is what an app declares in fog.yaml to say, per request,
// which machine should answer (docs/compute-contract.md).
const RouterContract = "fog-inference-router/1"

// RouterBudget is how long fogd waits for a router before choosing alone.
const RouterBudget = 50 * time.Millisecond

// RouteAsk is what a router is asked: the request's app and model, and the
// machines that could answer it, as fogd would order them.
type RouteAsk struct {
	App      string         `json:"app"`
	Model    string         `json:"model"`
	Stream   bool           `json:"stream,omitempty"`
	Machines []RouteMachine `json:"machines"`
}

// RouteMachine is one machine a router can choose.
type RouteMachine struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Models   []string `json:"models"`
	InFlight int      `json:"inFlight"`
}

// inflight changes how many inference requests a machine is answering, and
// answers how many it was answering before.
func (s *Service) inflight(machine string, by int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answering == nil {
		s.answering = map[string]int{}
	}
	was := s.answering[machine]
	s.answering[machine] = was + by
	if s.answering[machine] <= 0 {
		delete(s.answering, machine)
	}
	return was
}

// llmErrorClass is how Lucid groups a failed model call.
func llmErrorClass(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "rate_limit"
	case http.StatusServiceUnavailable:
		return "overloaded"
	case http.StatusGatewayTimeout:
		return "timeout"
	case http.StatusBadGateway:
		return "unreachable"
	case http.StatusNotFound:
		return "not_found"
	case 0:
		return "error"
	}
	if status < 500 {
		return "invalid_request"
	}
	return "server_error"
}

// maxTapped bounds what answerTap holds of an answer to read it: a whole
// JSON answer, or one line of a stream. Past it the answer still goes
// through; fog just doesn't learn its usage.
const maxTapped = 4 << 20

// answerTap reads what fog may know of a model's answer as it passes on
// to the app — its usage, its stop reason, the model that served it, when
// its first byte came — and nothing of its text.
type answerTap struct {
	io.ReadCloser
	sse   bool
	start time.Time

	first  time.Time
	buf    []byte
	over   bool
	chunks int64
	got    tappedAnswer
}

// tappedAnswer is what answerTap learned.
type tappedAnswer struct {
	ttft          time.Duration
	in, out       int64
	estimated     bool
	stop, served  string
	err           string
	usageReported bool
}

func (a *answerTap) Read(p []byte) (int, error) {
	n, err := a.ReadCloser.Read(p)
	if n > 0 {
		if a.first.IsZero() {
			a.first = time.Now()
		}
		a.take(p[:n])
	}
	return n, err
}

func (a *answerTap) take(b []byte) {
	if !a.sse {
		if !a.over && len(a.buf)+len(b) <= maxTapped {
			a.buf = append(a.buf, b...)
		} else {
			a.over, a.buf = true, nil
		}
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if !a.over && len(a.buf)+len(b) <= maxTapped {
				a.buf = append(a.buf, b...)
			} else {
				a.over, a.buf = true, nil
			}
			return
		}
		if !a.over {
			a.line(append(a.buf, b[:i]...))
		}
		a.buf, a.over, b = a.buf[:0], false, b[i+1:]
	}
}

// openAIAnswer is the part of an answer, or of one streamed chunk, fog
// reads.
type openAIAnswer struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (a *answerTap) read(raw []byte) {
	var ans openAIAnswer
	if json.Unmarshal(raw, &ans) != nil {
		return
	}
	if ans.Model != "" {
		a.got.served = ans.Model
	}
	for _, c := range ans.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			a.got.stop = *c.FinishReason
		}
	}
	if ans.Usage != nil {
		a.got.in, a.got.out, a.got.usageReported = ans.Usage.PromptTokens, ans.Usage.CompletionTokens, true
	}
	if ans.Error != nil {
		a.got.err = ans.Error.Message
	}
}

// line reads one line of a streamed answer.
func (a *answerTap) line(l []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(l), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[DONE]" {
		return
	}
	a.chunks++
	a.read(data)
}

// answer is what the tap learned once the answer has gone through. A
// stream that reported no usage counts a token per chunk, as Ollama sends
// them, and says the count is estimated.
func (a *answerTap) answer() tappedAnswer {
	if a.ReadCloser == nil {
		return a.got
	}
	if !a.first.IsZero() {
		a.got.ttft = a.first.Sub(a.start)
	}
	if a.sse {
		if len(a.buf) > 0 && !a.over {
			a.line(a.buf)
		}
		if !a.got.usageReported && a.chunks > 0 {
			a.got.out, a.got.estimated = a.chunks, true
		}
	} else if !a.over {
		a.read(a.buf)
	}
	a.buf = nil
	return a.got
}

// choose picks the machine for a model: one that has it, else — for a
// request that names none — any; nil when none has it.
func choose(pool []Machine, model string) *Machine {
	for i := range pool {
		if model == "" || slices.Contains(pool[i].Inventory.Inference.Models, model) {
			return &pool[i]
		}
	}
	return nil
}

func writeModels(w http.ResponseWriter, models []string) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, m := range models {
		list.Data = append(list.Data, model{ID: m, Object: "model", OwnedBy: "fog"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// writeOpenAIError answers in the error shape OpenAI clients read.
func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": "fog"}})
}
