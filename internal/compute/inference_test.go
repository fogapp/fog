package compute

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// modelServer is a machine's inference listener as fog-node fronts it:
// only fog's key, and it says which machine answered.
type modelServer struct {
	name   string
	models []string
	secret string
	// reply answers in place of the default, when set.
	reply func(w http.ResponseWriter)
	mu    sync.Mutex
	got   []string
}

func (m *modelServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "" || BodySHA256(body) != r.Header.Get(HeaderBodySHA256) || !MACEqual(r.Header.Get(HeaderInferenceAuth), InferenceMAC(m.secret,
			r.Header.Get(HeaderInferenceNonce), r.Header.Get(HeaderInferenceTime), r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Header.Get(HeaderBodySHA256))) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		m.mu.Lock()
		m.got = append(m.got, r.Method+" "+r.URL.Path+" "+string(body))
		reply := m.reply
		m.mu.Unlock()
		if reply != nil {
			reply(w)
			return
		}
		w.Write([]byte(`{"answered_by":"` + m.name + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type inferenceRig struct {
	svc   *Service
	st    *store.Store
	app   *store.App
	small *modelServer
	big   *modelServer
}

func newInferenceRig(t *testing.T, onThemselves ...string) *inferenceRig {
	t.Helper()
	r, st := testRegistry(t)
	rig := &inferenceRig{svc: &Service{Registry: r}, st: st}
	for _, ms := range []**modelServer{&rig.small, &rig.big} {
		*ms = &modelServer{}
	}
	rig.small.name, rig.small.models = "Mini", []string{"qwen3:0.6b"}
	rig.big.name, rig.big.models = "Studio", []string{"qwen3:0.6b", "qwen3:235b"}
	for _, ms := range []*modelServer{rig.small, rig.big} {
		inv := mac(ms.name)
		srv := ms.start(t)
		inv.Inference = &InferenceCaps{API: "openai", URL: srv.URL, Models: ms.models, Local: slices.Contains(onThemselves, ms.name)}
		ans := pair(t, r, inv)
		ms.secret = InferenceKey(TokenDigest(ans.Token))
	}
	app, _ := st.CreateApp("hud")
	st.SetAppCompute(app.ID, `{"contract":"fog-compute/1","inference":{}}`)
	rig.app = app
	return rig
}

func (rig *inferenceRig) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, InferencePath+path, strings.NewReader(body))
	w := httptest.NewRecorder()
	rig.svc.ServeInference(w, req, rig.app)
	return w
}

func TestInferenceRoutesByModel(t *testing.T) {
	rig := newInferenceRig(t)
	w := rig.do("POST", "chat/completions", `{"model":"qwen3:235b","messages":[]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Studio") {
		t.Fatalf("big model: %d %s", w.Code, w.Body)
	}
	if got := rig.big.got[0]; got != `POST /v1/chat/completions {"model":"qwen3:235b","messages":[]}` {
		t.Fatalf("the machine got %q", got)
	}
	if w := rig.do("POST", "chat/completions", `{"model":"nope"}`); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no machine lending to fog has the model nope") {
		t.Fatalf("unknown model: %d %s", w.Code, w.Body)
	}

	w = rig.do("GET", "models", "")
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Data) != 2 || list.Data[0].ID != "qwen3:0.6b" || list.Data[1].ID != "qwen3:235b" {
		t.Fatalf("models: %s", w.Body)
	}
}

func TestInferenceOnlyForAppsThatAskAndAreAllowed(t *testing.T) {
	rig := newInferenceRig(t)
	rig.st.SetComputeGrant(rig.app.ID, InferencePool, false)
	if w := rig.do("GET", "models", ""); w.Code != http.StatusForbidden {
		t.Fatalf("switched off: %d", w.Code)
	}
	rig.st.SetComputeGrant(rig.app.ID, InferencePool, true)
	other, _ := rig.st.CreateApp("feed")
	req := httptest.NewRequest("GET", InferencePath+"models", nil)
	w := httptest.NewRecorder()
	rig.svc.ServeInference(w, req, other)
	if w.Code != http.StatusForbidden {
		t.Fatalf("undeclared app: %d", w.Code)
	}
}

// household declares the app's requests stay on machines that report Local.
func (rig *inferenceRig) household() {
	rig.st.SetAppCompute(rig.app.ID, `{"contract":"fog-compute/1","inference":{"locality":"household"}}`)
}

func openAIError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var doc struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc.Error.Message == "" || doc.Error.Type != "fog" {
		t.Fatalf("not an OpenAI-shaped error: %s", w.Body)
	}
	return doc.Error.Message
}

// Both machines have the model. Whichever order they're considered in, only
// the one with its model server on itself is asked.
func TestHouseholdRoutesOnlyToLocalMachines(t *testing.T) {
	for _, tc := range []struct{ local, remote string }{{"Mini", "Studio"}, {"Studio", "Mini"}} {
		rig := newInferenceRig(t, tc.local)
		rig.household()
		for range 3 {
			w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.local) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		}
		// A request that names no model is no way around it.
		if w := rig.do("POST", "chat/completions", `{}`); w.Code != 200 || !strings.Contains(w.Body.String(), tc.local) {
			t.Fatalf("no model: %d %s", w.Code, w.Body)
		}
	}
}

func TestHouseholdRefusesAModelOnlyARemoteMachineHas(t *testing.T) {
	rig := newInferenceRig(t, "Mini")
	rig.household()
	w := rig.do("POST", "chat/completions", `{"model":"qwen3:235b"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if msg := openAIError(t, w); !strings.Contains(msg, "household") || !strings.Contains(msg, "qwen3:235b") {
		t.Errorf("message = %q", msg)
	}
	if len(rig.big.got) != 0 {
		t.Errorf("the remote machine was asked: %v", rig.big.got)
	}
	if w := rig.do("POST", "chat/completions", `{"model":"nope"}`); w.Code != http.StatusNotFound {
		t.Errorf("a model nobody has: %d", w.Code)
	}
}

func TestHouseholdWithNoLocalMachineIsRefused(t *testing.T) {
	rig := newInferenceRig(t)
	rig.household()
	for _, w := range []*httptest.ResponseRecorder{
		rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`),
		rig.do("GET", "models", ""),
	} {
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if msg := openAIError(t, w); !strings.Contains(msg, "household") {
			t.Errorf("message = %q", msg)
		}
	}
	if len(rig.small.got)+len(rig.big.got) != 0 {
		t.Errorf("a machine was asked: %v %v", rig.small.got, rig.big.got)
	}
}

// A paused local machine leaves a remote one lending, and that's no fallback.
func TestHouseholdDoesNotFallBackToARemoteMachine(t *testing.T) {
	rig := newInferenceRig(t, "Mini")
	rig.household()
	machines, _ := rig.svc.Registry.Machines()
	for _, m := range machines {
		if m.Name == "Mini" {
			rig.st.SetComputeNodeSettings(m.ID, m.Name, "", true)
		}
	}
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(rig.big.got) != 0 {
		t.Errorf("fell back: %v", rig.big.got)
	}
}

func TestHouseholdListsOnlyWhatItMayUse(t *testing.T) {
	rig := newInferenceRig(t, "Mini")
	rig.household()
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	w := rig.do("GET", "models", "")
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != "qwen3:0.6b" {
		t.Fatalf("models: %s", w.Body)
	}
}

// A misspelled limit is still a limit.
func TestAnUnknownLocalityHoldsAsHousehold(t *testing.T) {
	rig := newInferenceRig(t, "Mini")
	rig.st.SetAppCompute(rig.app.ID, `{"contract":"fog-compute/1","inference":{"locality":"houshold"}}`)
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:235b"}`); w.Code != http.StatusForbidden {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestNoLocalityUsesEveryMachine(t *testing.T) {
	rig := newInferenceRig(t, "Mini")
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:235b"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "Studio") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestInferenceSkipsPausedMachines(t *testing.T) {
	rig := newInferenceRig(t)
	machines, _ := rig.svc.Registry.Machines()
	for _, m := range machines {
		if m.Name == "Studio" {
			rig.st.SetComputeNodeSettings(m.ID, m.Name, "", true)
		}
	}
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:235b"}`); w.Code != http.StatusNotFound {
		t.Fatalf("model only on a paused machine: %d", w.Code)
	}
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "Mini") {
		t.Fatalf("still served by the Mini: %d %s", w.Code, w.Body)
	}
	p := Inference(mustMachines(t, &relayRig{svc: rig.svc}))
	if p == nil || p.Online != 1 || len(p.Models) != 1 {
		t.Fatalf("pool: %+v", p)
	}
}

func TestInferenceGoesToTheMachineWithFewerInFlight(t *testing.T) {
	rig := newInferenceRig(t)
	machines, _ := rig.svc.Registry.Machines()
	for _, m := range machines {
		if m.Name == "Mini" {
			rig.svc.inflight(m.ID, 3)
		}
	}
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "Studio") {
		t.Fatalf("the Mini is answering three already: %d %s", w.Code, w.Body)
	}
}

func TestInferenceTriesTheNextMachineWhenOneFailsBeforeAnswering(t *testing.T) {
	rig := newInferenceRig(t)
	// The Mini drops the connection without a word.
	rig.small.reply = func(w http.ResponseWriter) { panic(http.ErrAbortHandler) }
	machines, _ := rig.svc.Registry.Machines()
	for _, m := range machines {
		if m.Name == "Studio" {
			rig.svc.inflight(m.ID, 1) // so the Mini is tried first
		}
	}
	w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Studio") {
		t.Fatalf("the Studio answers once the Mini fails: %d %s", w.Code, w.Body)
	}
	activity, _ := rig.st.ComputeActivityPage(store.ComputeActivityFilter{App: rig.app.Name, Limit: 10})
	failed := 0
	for _, a := range activity {
		if a.Outcome == OutcomeFailed && strings.Contains(a.Message, "tried another machine") {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("the Mini's failure is in the activity: %+v", activity)
	}
}

func TestARouterOrdersTheMachines(t *testing.T) {
	rig := newInferenceRig(t)
	var asked RouteAsk
	rig.svc.Router = func(_ context.Context, ask RouteAsk) ([]string, error) {
		asked = ask
		return []string{"Mini"}, nil
	}
	machines, _ := rig.svc.Registry.Machines()
	for _, m := range machines {
		if m.Name == "Mini" {
			rig.svc.inflight(m.ID, 2)
		}
	}
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "Mini") {
		t.Fatalf("the router's choice wins over fewest in flight: %d %s", w.Code, w.Body)
	}
	if asked.Model != "qwen3:0.6b" || len(asked.Machines) != 2 || asked.App != "hud" {
		t.Fatalf("%+v", asked)
	}
	rig.svc.Router = func(ctx context.Context, _ RouteAsk) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "Studio") {
		t.Fatalf("a router that doesn't answer in time is passed over: %d %s", w.Code, w.Body)
	}
}

type fakeCaller struct {
	path string
	ask  RouteAsk
}

func (f *fakeCaller) Call(_ context.Context, _ *store.App, _, path string, body any) ([]byte, error) {
	f.path, f.ask = path, body.(RouteAsk)
	return []byte(`{"machines":["Studio","Mini"]}`), nil
}

func TestTheRouterIsTheAppThatDeclaredIt(t *testing.T) {
	_, st := testRegistry(t)
	caller := &fakeCaller{}
	route := NewRouter(st, caller)
	if _, err := route(context.Background(), RouteAsk{Model: "m"}); err == nil {
		t.Fatal("with no router installed fogd orders the machines itself")
	}
	app, _ := st.CreateApp("think-fleet")
	st.SetAppContracts(app.ID, []string{"fog-signin/1", RouterContract})
	got, err := route(context.Background(), RouteAsk{Model: "m"})
	if err != nil || len(got) != 2 || got[0] != "Studio" || caller.path != RouterPath || caller.ask.Model != "m" {
		t.Fatalf("%v %v %+v", got, err, caller)
	}
}
