package computenode

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

// fromFogAs signs a request to an inference listener as fog does: a MAC
// with the inference key over the request, never a bearer.
func fromFogAs(req *http.Request, key string, body []byte) {
	nonce, at := compute.NewNonce(), time.Now().UTC().Format(time.RFC3339Nano)
	sum := compute.BodySHA256(body)
	req.Header.Set(compute.HeaderInferenceNonce, nonce)
	req.Header.Set(compute.HeaderInferenceTime, at)
	req.Header.Set(compute.HeaderBodySHA256, sum)
	req.Header.Set(compute.HeaderInferenceAuth, compute.InferenceMAC(key, nonce, at, req.Method, req.URL.RequestURI(), req.Header.Get("Content-Type"), sum))
}

// tripwire is a body that fails the test if anything reads it.
type tripwire struct{ t *testing.T }

func (w tripwire) Read([]byte) (int, error) {
	w.t.Error("a body was read before the request was authenticated")
	return 0, io.EOF
}

// The listener reads no body until the request's headers prove it fog's;
// and the body must then be the one fog declared.
func TestInferenceHandlerChecksBeforeReading(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer upstream.Close()
	key := InferenceSecret("fognode_x")
	h := InferenceHandler(upstream.URL, key)
	serve := func(req *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	for name, sign := range map[string]func(*http.Request){
		"no MAC":      func(*http.Request) {},
		"another key": func(r *http.Request) { fromFogAs(r, InferenceSecret("fognode_y"), []byte("{}")) },
		"stale": func(r *http.Request) {
			fromFogAs(r, key, []byte("{}"))
			r.Header.Set(compute.HeaderInferenceTime, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
		},
		"from before it started": func(r *http.Request) {
			nonce, at := compute.NewNonce(), listenerStart.Add(-time.Second).UTC().Format(time.RFC3339Nano)
			sum := compute.BodySHA256([]byte("{}"))
			r.Header.Set(compute.HeaderInferenceNonce, nonce)
			r.Header.Set(compute.HeaderInferenceTime, at)
			r.Header.Set(compute.HeaderBodySHA256, sum)
			r.Header.Set(compute.HeaderInferenceAuth, compute.InferenceMAC(key, nonce, at, r.Method, r.URL.RequestURI(), "", sum))
		},
	} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", tripwire{t})
		sign(req)
		if code := serve(req); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	// Authenticated, but not the body fog declared.
	swapped := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"evil":1}`))
	fromFogAs(swapped, key, []byte("{}"))
	if code := serve(swapped); code != http.StatusUnauthorized {
		t.Fatalf("a swapped body: %d", code)
	}
	// The content type is covered too.
	typed := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	typed.Header.Set("Content-Type", "application/json")
	fromFogAs(typed, key, []byte("{}"))
	typed.Header.Set("Content-Type", "text/plain")
	if code := serve(typed); code != http.StatusUnauthorized {
		t.Fatalf("a changed type: %d", code)
	}
}

// The inference listener takes only requests fog MACed, each once: not a
// bearer (there is none to steal), not a replay, not a changed body.
func TestInferenceHandlerTakesOnlyFog(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get(compute.HeaderInferenceAuth) != "" {
			t.Errorf("fog's proof reached the model server")
		}
		b, _ := io.ReadAll(r.Body)
		got = append(got, string(b))
		w.Write([]byte(r.Method + " " + r.URL.Path))
	}))
	defer upstream.Close()
	key := InferenceSecret("fognode_x")
	h := InferenceHandler(upstream.URL, key)
	serve := func(req *http.Request) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"m":1}`))
	fromFogAs(req, key, []byte(`{"m":1}`))
	replay := req.Clone(context.Background())
	if code, body := serve(req); code != 200 || body != "POST /v1/chat/completions" || got[0] != `{"m":1}` {
		t.Fatalf("fog: %d %q %v", code, body, got)
	}
	replay.Body = io.NopCloser(strings.NewReader(`{"m":1}`))
	if code, _ := serve(replay); code != http.StatusUnauthorized {
		t.Fatalf("a replay: %d", code)
	}
	changed := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"m":2}`))
	fromFogAs(changed, key, []byte(`{"m":1}`))
	if code, _ := serve(changed); code != http.StatusUnauthorized {
		t.Fatalf("a changed body: %d", code)
	}
	other := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	fromFogAs(other, InferenceSecret("fognode_y"), []byte("{}"))
	if code, _ := serve(other); code != http.StatusUnauthorized {
		t.Fatalf("another key: %d", code)
	}
	for _, bearer := range []string{key, "fognode_x", compute.TokenDigest("fognode_x")} {
		b := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
		b.Header.Set("Authorization", "Bearer "+bearer)
		if code, _ := serve(b); code != http.StatusUnauthorized {
			t.Fatalf("a bearer: %d", code)
		}
	}
	outside := httptest.NewRequest("POST", "/api/pull", strings.NewReader("{}"))
	fromFogAs(outside, key, []byte("{}"))
	if code, _ := serve(outside); code != http.StatusNotFound {
		t.Fatalf("outside /v1: %d", code)
	}
}

func TestOnThisMachine(t *testing.T) {
	for upstream, want := range map[string]bool{
		"http://127.0.0.1:11434":     true,
		"http://127.1.2.3:8000":      true,
		"http://localhost:8485":      true,
		"http://LOCALHOST:8485":      true,
		"http://[::1]:11434":         true,
		"http://192.168.1.20:11434":  false,
		"https://api.openai.com/v1":  false,
		"http://0.0.0.0:11434":       false,
		"http://localhost.evil.net":  false,
		"http://127.0.0.1.nip.io":    false,
		"http://127.0.0.1@evil.net":  false,
		"http://localhost@evil.net/": false,
		"":                           false,
		"://nonsense":                false,
	} {
		if got := onThisMachine(upstream); got != want {
			t.Errorf("onThisMachine(%q) = %v", upstream, got)
		}
	}
}

// With FOG_E2E_OLLAMA set and Ollama running here, a real completion goes
// through fog-node's listener to it.
func TestInferenceThroughOllama(t *testing.T) {
	model := os.Getenv("FOG_E2E_OLLAMA")
	if model == "" {
		t.Skip("set FOG_E2E_OLLAMA to a model Ollama has to run this")
	}
	cfg := &Config{Fog: "http://127.0.0.1:8080", Token: "fognode_x", InferenceUpstream: OllamaUpstream, InferenceListen: "127.0.0.1:7798"}
	l := &Lender{Config: cfg}
	l.Refresh(context.Background())
	defer l.stop()
	req, _ := http.NewRequest("POST", "http://127.0.0.1:7798/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"Say OK."}],"max_tokens":20}`))
	fromFogAs(req, InferenceSecret("fognode_x"), []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"Say OK."}],"max_tokens":20}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"choices"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func modelsServer(t *testing.T, id string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[{"id":"`+id+`"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withKnown(t *testing.T, urls ...string) {
	t.Helper()
	was := knownUpstreams
	knownUpstreams = urls
	t.Cleanup(func() { knownUpstreams = was })
}

func TestThinkIsFoundWithNothingConfigured(t *testing.T) {
	think, ollama := modelsServer(t, "qwen"), modelsServer(t, "llama3")
	withKnown(t, think.URL, ollama.URL)
	if up := (&Config{}).upstream(context.Background()); up != think.URL {
		t.Fatalf("think comes first, got %q", up)
	}
	withKnown(t, "http://127.0.0.1:1", ollama.URL)
	if up := (&Config{}).upstream(context.Background()); up != ollama.URL {
		t.Fatalf("with no think, Ollama, got %q", up)
	}
}

func TestConfiguredServerAndOffStillWin(t *testing.T) {
	think, mine := modelsServer(t, "qwen"), modelsServer(t, "mine")
	withKnown(t, think.URL)
	if up := (&Config{InferenceUpstream: mine.URL}).upstream(context.Background()); up != mine.URL {
		t.Fatalf("got %q", up)
	}
	if up := (&Config{InferenceOff: true}).upstream(context.Background()); up != "" {
		t.Fatalf("lending is off, got %q", up)
	}
	if up := (&Config{InferenceUpstream: "http://127.0.0.1:1"}).upstream(context.Background()); up != "" {
		t.Fatalf("a configured server that's down isn't replaced by a guess, got %q", up)
	}
}

// switchable is a model server that can be up or down, and says which it
// is when asked anything.
func switchable(t *testing.T, name, model string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"`+model+`"}]}`)
			return
		}
		io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv, &up
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// The listener follows what the inventory says, at every change: Ollama
// lent first, then think once it starts, then Ollama again when think
// stops. fog is never told one server's models while the listener
// forwards to another, and hears of each change at once.
func TestLenderListenerFollowsTheInventory(t *testing.T) {
	think, thinkUp := switchable(t, "think", "qwen")
	ollama, ollamaUp := switchable(t, "ollama", "llama3")
	withKnown(t, think.URL, ollama.URL)
	ollamaUp.Store(true)
	port := freePort(t)
	l := &Lender{Config: &Config{Fog: "http://127.0.0.1:8080", Token: "fognode_x", InferenceListen: "127.0.0.1:" + port}}
	defer l.stop()
	changed := l.Changed()
	ask := func() (int, string) {
		req, _ := http.NewRequest("POST", "http://127.0.0.1:"+port+"/v1/chat/completions", strings.NewReader("{}"))
		fromFogAs(req, InferenceSecret("fognode_x"), []byte("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	expect := func(step, model, server string) {
		t.Helper()
		l.Refresh(context.Background())
		inv := l.Inventory()
		if inv == nil || len(inv.Models) != 1 || inv.Models[0] != model || inv.URL != "http://127.0.0.1:"+port {
			t.Fatalf("%s: inventory %+v", step, inv)
		}
		if code, body := ask(); code != 200 || body != server {
			t.Fatalf("%s: inventory says %s but the listener reached %d %q", step, model, code, body)
		}
		select {
		case <-changed:
		default:
			t.Fatalf("%s: fog wasn't told at once", step)
		}
	}
	expect("Ollama first", "llama3", "ollama")
	thinkUp.Store(true)
	expect("think started", "qwen", "think")
	thinkUp.Store(false)
	expect("think stopped", "llama3", "ollama")

	// Nothing to lend: nothing reported, and the listener is gone.
	ollamaUp.Store(false)
	l.Refresh(context.Background())
	if inv := l.Inventory(); inv != nil {
		t.Fatalf("lent nothing, reported %+v", inv)
	}
	if code, _ := ask(); code != 0 {
		t.Fatalf("the listener still answers: %d", code)
	}
	// An unchanged look says nothing.
	<-changed
	l.Refresh(context.Background())
	select {
	case <-changed:
		t.Fatal("told of a change that wasn't one")
	default:
	}
}

// The listener binds the address this machine reaches fog from, not every
// interface it has.
func TestLenderListensOnlyWhereFogReachesIt(t *testing.T) {
	addr, url := inferenceAddr(&Config{Fog: "http://127.0.0.1:8080"})
	if addr != "127.0.0.1:7718" || url != "http://127.0.0.1:7718" {
		t.Fatalf("default: %q %q", addr, url)
	}
	addr, url = inferenceAddr(&Config{Fog: "http://127.0.0.1:8080", InferenceListen: ":7799"})
	if addr != "127.0.0.1:7799" || url != "http://127.0.0.1:7799" {
		t.Fatalf("a port: %q %q", addr, url)
	}
	addr, url = inferenceAddr(&Config{Fog: "http://127.0.0.1:8080", InferenceListen: "0.0.0.0:7799"})
	if addr != "0.0.0.0:7799" || url != "http://127.0.0.1:7799" {
		t.Fatalf("the owner's choice of every interface: %q %q", addr, url)
	}
}

func TestLenderOffOrDownLendsNothing(t *testing.T) {
	srv, up := switchable(t, "mine", "m")
	up.Store(true)
	for name, cfg := range map[string]*Config{
		"off":             {Fog: "http://127.0.0.1:8080", InferenceOff: true, InferenceUpstream: srv.URL, InferenceListen: "127.0.0.1:" + freePort(t)},
		"configured down": {Fog: "http://127.0.0.1:8080", InferenceUpstream: "http://127.0.0.1:1", InferenceListen: "127.0.0.1:" + freePort(t)},
	} {
		l := &Lender{Config: cfg}
		l.Refresh(context.Background())
		if inv := l.Inventory(); inv != nil {
			t.Errorf("%s: lent %+v", name, inv)
		}
		l.stop()
	}
	// While nothing is lent, the handler takes fog's key and says so.
	l := &Lender{Config: &Config{Token: "fognode_x"}}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	fromFogAs(req, InferenceSecret("fognode_x"), nil)
	l.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nothing lent: %d", w.Code)
	}
}

// A change in what's lent cuts a held poll short, so fog learns of it at
// once rather than at the end of the hold.
func TestAgentTellsFogWhatItLendsAtOnce(t *testing.T) {
	f := &fakeFog{answer: compute.NodeAnswer{PollMS: 250}, hold: 5 * time.Second}
	srv := f.server(t)
	defer srv.Close()
	model, up := switchable(t, "think", "qwen")
	withKnown(t, model.URL)
	l := &Lender{Config: &Config{Fog: "http://127.0.0.1:8080", Token: "fognode_x", InferenceListen: "127.0.0.1:" + freePort(t)}}
	defer l.stop()
	a := &Agent{Dir: t.TempDir(), Config: &Config{Fog: srv.URL, Node: "n1", Token: "fognode_x", FogKey: fogKey, Name: "Mini", FFmpeg: "/nonexistent/ffmpeg"},
		Version: "v1", Log: quiet(), Lending: l, Pressed: func() bool { return false }}
	if err := a.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inv := f.lastReport().Inventory; inv == nil || inv.Inference != nil {
		t.Fatalf("first report: %+v", inv)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		up.Store(true)
		l.Refresh(context.Background())
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		a.Once(ctx) // held, then cut short
		a.Once(ctx) // says what changed
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		var got *compute.InferenceCaps
		for _, rep := range f.reports {
			if rep.Inventory != nil && rep.Inventory.Inference != nil {
				got = rep.Inventory.Inference
			}
		}
		f.mu.Unlock()
		if got != nil {
			if got.Models[0] != "qwen" {
				t.Fatalf("reported %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fog wasn't told of the new model server inside its held poll")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
