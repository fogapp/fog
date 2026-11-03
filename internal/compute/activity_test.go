package compute

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func activity(t *testing.T, st *store.Store) []store.ComputeActivity {
	t.Helper()
	rows, err := st.ComputeActivityPage(store.ComputeActivityFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestInferenceActivityRecordsTheCallAndItsUsage(t *testing.T) {
	rig := newInferenceRig(t)
	rig.big.reply = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"qwen3:235b","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":34}}`))
	}
	if w := rig.do("POST", "chat/completions", `{"model":"qwen3:235b","messages":[]}`); w.Code != 200 {
		t.Fatalf("routed: %d %s", w.Code, w.Body)
	}
	rig.do("POST", "chat/completions", `{"model":"nope"}`)
	rig.do("GET", "models", "")

	rows := activity(t, rig.st)
	if len(rows) != 2 {
		t.Fatalf("the model list isn't activity; want 2 rows, got %+v", rows)
	}
	refused, ok := rows[0], rows[1]
	if refused.Outcome != OutcomeRefused || refused.Machine != "" || refused.Model != "nope" || refused.Status == nil || *refused.Status != 404 ||
		!strings.Contains(refused.Message, "has the model nope") {
		t.Fatalf("refused: %+v", refused)
	}
	if ok.Outcome != OutcomeOK || ok.Machine != "Studio" || ok.App != "hud" || ok.Kind != KindInference || ok.What != "chat/completions" ||
		ok.Model != "qwen3:235b" || ok.TokensIn != 12 || ok.TokensOut != 34 || ok.TokensEstimated || *ok.Status != 200 || ok.EndedAt == "" {
		t.Fatalf("routed: %+v", ok)
	}
}

func TestInferenceActivityReadsAStream(t *testing.T) {
	rig := newInferenceRig(t)
	stream := func(usage bool) func(w http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, tok := range []string{"Hel", "lo", "!"} {
				io.WriteString(w, `data: {"model":"qwen3:0.6b","choices":[{"delta":{"content":"`+tok+`"},"finish_reason":null}]}`+"\n\n")
				w.(http.Flusher).Flush()
			}
			io.WriteString(w, `data: {"model":"qwen3:0.6b","choices":[{"delta":{},"finish_reason":"length"}]}`+"\n\n")
			if usage {
				io.WriteString(w, `data: {"model":"qwen3:0.6b","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}`+"\n\n")
			}
			io.WriteString(w, "data: [DONE]\n\n")
		}
	}
	rig.small.reply, rig.big.reply = stream(false), stream(false)
	w := rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b","stream":true}`)
	if !strings.Contains(w.Body.String(), `"Hel"`) || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("the stream didn't pass through whole: %q", w.Body)
	}
	rig.small.reply, rig.big.reply = stream(true), stream(true)
	rig.do("POST", "chat/completions", `{"model":"qwen3:0.6b","stream":true}`)

	rows := activity(t, rig.st)
	reported, estimated := rows[0], rows[1]
	if estimated.Outcome != OutcomeOK || !estimated.TokensEstimated || estimated.TokensOut != 4 || estimated.TokensIn != 0 {
		t.Fatalf("no usage in the stream, so a token a chunk: %+v", estimated)
	}
	if reported.TokensEstimated || reported.TokensIn != 5 || reported.TokensOut != 3 {
		t.Fatalf("usage the stream reported: %+v", reported)
	}
}

func TestInferenceActivityRecordsAMachineThatFails(t *testing.T) {
	rig := newInferenceRig(t)
	rig.big.reply = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"model requires more system memory"}}`))
	}
	rig.do("POST", "chat/completions", `{"model":"qwen3:235b"}`)
	row := activity(t, rig.st)[0]
	if row.Outcome != OutcomeFailed || *row.Status != 500 || row.Message != "model requires more system memory" {
		t.Fatalf("failed: %+v", row)
	}
}

func TestRelayActivityFollowsEachRun(t *testing.T) {
	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 4 })

	rig.app.start(session("ok"), session("bad"), session("gone"))
	var runs []NodeRun
	waitFor(t, "the runs handed to the machine", func() bool {
		runs = append(runs, rig.node(t, mini.Token, NodeReport{}).Start...)
		return len(runs) == 3
	})
	rows := activity(t, rig.st)
	if len(rows) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Outcome != OutcomeRunning || r.Machine != "Mini" || r.App != "stream" || r.Kind != KindVideo || r.What != ClassInteractive ||
			r.Pool != "video/videotoolbox" {
			t.Fatalf("running: %+v", r)
		}
	}

	zero, one := 0, 1
	rig.node(t, mini.Token, NodeReport{Running: []string{runs[2].ID}, Exited: []Exited{
		{ID: runs[0].ID, Status: &zero}, {ID: runs[1].ID, Status: &one, Log: "Conversion failed!"},
	}})
	if err := rig.svc.Remove(authenticate(rig.svc.Registry, mini.Token).ID); err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]store.ComputeActivity{}
	for _, r := range activity(t, rig.st) {
		outcomes[r.Outcome] = r
	}
	if r := outcomes[OutcomeOK]; *r.Status != 0 || r.EndedAt == "" {
		t.Fatalf("ok: %+v", r)
	}
	if r := outcomes[OutcomeFailed]; *r.Status != 1 || r.Message != "Conversion failed!" {
		t.Fatalf("failed: %+v", r)
	}
	if r := outcomes[OutcomeLost]; r.Message != "the machine running it was removed" {
		t.Fatalf("lost: %+v", outcomes)
	}
}

func TestANewRelayLosesWhatTheLastFogdLeftRunning(t *testing.T) {
	r, st := testRegistry(t)
	st.AddComputeActivity(&store.ComputeActivity{App: "stream", Pool: "video/videotoolbox", Kind: KindVideo, Outcome: OutcomeRunning})
	st.AddComputeActivity(&store.ComputeActivity{App: "stream", Pool: "video/videotoolbox", Kind: KindVideo, Outcome: OutcomeOK})
	NewRelay(&Service{Registry: r}, st, &fakeApp{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rows := activity(t, st)
	if rows[0].Outcome != OutcomeOK || rows[1].Outcome != OutcomeLost || rows[1].Message != ActivityRestarted {
		t.Fatalf("rows: %+v", rows)
	}
}
