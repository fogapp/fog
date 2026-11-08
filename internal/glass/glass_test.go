package glass

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGlass is an ingest endpoint that keeps each batch's records.
type fakeGlass struct {
	*httptest.Server
	mu      sync.Mutex
	status  int
	reply   string
	headers []map[string]any
	records []map[string]any
	auth    string
}

func newFakeGlass(t *testing.T) *fakeGlass {
	f := &fakeGlass{status: http.StatusAccepted, reply: `{"accepted":1}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest/v1/batch" {
			t.Errorf("posted to %s", r.URL.Path)
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		raw, _ := io.ReadAll(zr)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = r.Header.Get("Authorization")
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(nil, 1<<20)
		for first := true; sc.Scan(); first = false {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatalf("line is not JSON: %s", sc.Text())
			}
			if first {
				f.headers = append(f.headers, m)
			} else {
				f.records = append(f.records, m)
			}
		}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(f.status)
		io.WriteString(w, f.reply)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGlass) ofType(typ string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, r := range f.records {
		if r["t"] == typ {
			out = append(out, r)
		}
	}
	return out
}

func setup(t *testing.T) (*Client, *fakeGlass) {
	f := newFakeGlass(t)
	c := New("v1.2.3")
	c.Set(f.URL, "glass_ing_test")
	SetDefault(c)
	t.Cleanup(func() { SetDefault(nil) })
	return c, f
}

func TestInertWithoutConfiguration(t *testing.T) {
	c := New("v1")
	SetDefault(c)
	defer SetDefault(nil)
	ctx, txn := Start(context.Background(), "job", "deploy")
	if txn != nil || ctx != context.Background() {
		t.Fatal("an unconfigured client started a transaction")
	}
	txn.Attr("app", "x") // nil-safe
	txn.End(errors.New("boom"))
	c.Gauge("g", "", 1, nil)
	if c.Body(true) != nil {
		t.Fatal("an unconfigured client queued something")
	}
	var nilClient *Client
	nilClient.Gauge("g", "", 1, nil)
	nilClient.Set("http://x", "k")
	if _, txn := nilClient.Start(context.Background(), "job", "x"); txn != nil {
		t.Fatal("a nil client started a transaction")
	}
}

func TestMiddlewareNamesRequestsByPattern(t *testing.T) {
	c, f := setup(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/apps/{name}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/boom", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) { w.(http.Flusher).Flush() })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {})
	h := Middleware(mux)
	for _, req := range []*http.Request{
		httptest.NewRequest("GET", "/api/apps/manor", nil),
		httptest.NewRequest("POST", "/api/boom", nil),
		httptest.NewRequest("GET", "/api/stream", nil),
		httptest.NewRequest("GET", "/apps/manor", nil),
	} {
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	c.flush(true)

	txns := f.ofType("txn")
	if len(txns) != 3 {
		t.Fatalf("want 3 transactions (the stream is not one), got %d: %v", len(txns), txns)
	}
	got := txns[0]
	// The path is the route, never the request's own: its segments may be
	// secrets ("manor" stands in for one here), and a trace keeps them.
	if got["name"] != "GET /api/apps/{name}" || got["route"] != "/api/apps/{name}" || got["path"] != "/api/apps/{name}" ||
		got["http"] != 204.0 || got["kind"] != "request" || got["status"] != nil {
		t.Errorf("request transaction: %v", got)
	}
	if got["trace"] != "4bf92f3577b34da6a3ce929d0e0e4736" || got["parent"] != "00f067aa0ba902b7" {
		t.Errorf("did not continue the caller's trace: %v", got)
	}
	if txns[1]["status"] != "error" || txns[1]["error"] != "http 502" {
		t.Errorf("5xx not an error: %v", txns[1])
	}
	if txns[2]["name"] != "(static)" {
		t.Errorf("catch-all named %v", txns[2]["name"])
	}
	if f.auth != "Bearer glass_ing_test" {
		t.Errorf("authorization %q", f.auth)
	}
	proc := f.headers[0]["process"].(map[string]any)
	if proc["release"] != "v1.2.3" || proc["kind"] != "fogd" {
		t.Errorf("header process: %v", proc)
	}
}

func TestOutboundSpansAndLogsJoinTheirTransaction(t *testing.T) {
	c, f := setup(t)
	var gotParent string
	sibling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotParent = r.Header.Get("traceparent")
	}))
	defer sibling.Close()

	var out bytes.Buffer
	log := slog.New(LogHandler(slog.NewTextHandler(&out, nil)))
	ctx, txn := Start(context.Background(), "job", "deploy")
	txn.Attr("app", "manor")
	req, _ := http.NewRequestWithContext(ctx, "GET", sibling.URL+"/fog/backup?x=1", nil)
	resp, err := (&http.Client{Transport: Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	log.InfoContext(ctx, "pulled image", "app", "manor", "bytes", 42)
	log.Debug("not shipped at info")
	txn.End(nil)
	c.flush(true)

	tx := f.ofType("txn")
	if len(tx) != 1 || tx[0]["kept"] != true || tx[0]["ext_n"] != 1.0 || tx[0]["attrs"].(map[string]any)["app"] != "manor" {
		t.Fatalf("transaction: %v", tx)
	}
	spans := f.ofType("span")
	if len(spans) != 1 || spans[0]["kind"] != "http" || spans[0]["parent"] != tx[0]["span"] ||
		!strings.HasSuffix(spans[0]["res"].(string), "/fog/backup") {
		t.Fatalf("spans: %v", spans)
	}
	if !strings.Contains(gotParent, tx[0]["trace"].(string)) {
		t.Errorf("sibling got traceparent %q", gotParent)
	}
	logs := f.ofType("log")
	if len(logs) != 1 || logs[0]["msg"] != "pulled image" || logs[0]["trace"] != tx[0]["trace"] ||
		logs[0]["attrs"].(map[string]any)["bytes"] != 42.0 || logs[0]["lvl"] != "info" {
		t.Fatalf("logs: %v", logs)
	}
	if !strings.Contains(out.String(), "pulled image") {
		t.Error("the line did not reach the next handler")
	}
}

func TestTailSamplingKeepsFirstAndErrors(t *testing.T) {
	c, f := setup(t)
	c.sampleRate = 0
	run := func(err error) {
		ctx, txn := Start(context.Background(), "job", "scan")
		_, sp := StartSpan(ctx, "custom", "step", "")
		sp.End(nil)
		txn.End(err)
	}
	run(nil)                     // first of its name: kept
	run(nil)                     // ordinary: row only
	run(errors.New("disk full")) // errored: kept
	c.flush(true)
	tx := f.ofType("txn")
	if len(tx) != 3 || tx[0]["kept"] != true || tx[1]["kept"] != nil || tx[2]["kept"] != true || tx[2]["error"] != "error" {
		t.Fatalf("transactions: %v", tx)
	}
	if n := len(f.ofType("span")); n != 2 {
		t.Fatalf("want the 2 kept transactions' spans, got %d", n)
	}
}

func TestRepliesSteerTheClient(t *testing.T) {
	c, f := setup(t)
	f.reply = `{"accepted":1,"directives":{"trace_sample_rate":0.5,"log_level":"error","unknown":true}}`
	c.Gauge("fog.host.cpu_pct", "percent", 12, nil)
	c.flush(true)
	if c.sampleRate != 0.5 || c.logLevel != slog.LevelError {
		t.Fatalf("directives not applied: rate %v level %v", c.sampleRate, c.logLevel)
	}
	if h := LogHandler(slog.NewTextHandler(io.Discard, nil)); h.Enabled(context.Background(), slog.LevelWarn) != true {
		t.Fatal("the next handler's level must still hold")
	}

	f.status = http.StatusTooManyRequests
	c.Gauge("g", "", 1, nil)
	c.flush(true)
	c.Gauge("g", "", 1, nil)
	c.flush(true) // paused: dropped, not sent
	if len(f.headers) != 2 {
		t.Fatalf("sent during a 429 pause: %d batches", len(f.headers))
	}

	c.mu.Lock()
	c.pause = c.pause.AddDate(-1, 0, 0)
	c.mu.Unlock()
	f.status = http.StatusUnauthorized
	c.Gauge("g", "", 1, nil)
	c.flush(true)
	if !c.Halted() {
		t.Fatal("401 did not stop the client")
	}
	if dropped := f.headers[2]["dropped"].(map[string]any); dropped["metric"] != 2.0 {
		t.Errorf("header did not count what the pause shed: %v", dropped)
	}
	if _, txn := Start(context.Background(), "job", "x"); txn != nil {
		t.Fatal("a halted client started a transaction")
	}
	c.Set(f.URL, "glass_ing_new")
	if c.Halted() {
		t.Fatal("a new key did not lift the stop")
	}
}

func TestModelCallIsAnLLMRecordInItsTransactionsTrace(t *testing.T) {
	f := newFakeGlass(t)
	c := New("v1")
	c.Set(f.URL, "key")
	ctx, txn := c.Start(context.Background(), "compute", "inference chat/completions")
	start := txn.start
	c.ModelCall(ctx, start, LLM{Provider: "fog", Model: "qwen3:0.6b", Tier: "local", In: 5, Out: 3, Stream: true,
		Attrs: map[string]any{"machine": "Studio"}})
	txn.End(nil)
	c.flush(true)

	f.mu.Lock()
	defer f.mu.Unlock()
	var llm, tx map[string]any
	for _, r := range f.records {
		switch r["t"] {
		case "llm":
			llm = r
		case "txn":
			tx = r
		}
	}
	if llm == nil || tx == nil {
		t.Fatalf("records: %v", f.records)
	}
	if llm["model"] != "qwen3:0.6b" || llm["in"] != 5.0 || llm["out"] != 3.0 || llm["stream"] != true || llm["tier"] != "local" ||
		llm["attrs"].(map[string]any)["machine"] != "Studio" {
		t.Fatalf("llm: %v", llm)
	}
	if llm["trace"] != tx["trace"] || llm["span"] != tx["span"] {
		t.Fatalf("not in its transaction's trace: %v / %v", llm, tx)
	}

	// Outside Glass, nothing is queued.
	off := New("v1")
	off.ModelCall(context.Background(), start, LLM{Provider: "fog", Model: "m"})
	if off.Body(true) != nil {
		t.Fatal("an inert client queued a model call")
	}
}
