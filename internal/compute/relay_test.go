package compute

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// fakeApp is an app speaking fog-compute/1, the way Stream does.
type fakeApp struct {
	mu      sync.Mutex
	pools   []TranscoderRequest
	reports []AppReport
	toStart []AppRun
	toStop  []string
	pollMS  int
	// forget makes the next poll answer 404, as an app whose admin
	// deleted the transcoder does.
	forget bool
	// deleted counts DELETE /fog/compute/transcoders/{id}.
	deleted int
}

func (f *fakeApp) Call(_ context.Context, app *store.App, method, path string, body any) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(body)
	switch {
	case method == "POST" && path == AppTranscodersPath:
		var req TranscoderRequest
		json.Unmarshal(raw, &req)
		f.pools = append(f.pools, req)
		return json.Marshal(TranscoderAnswer{ID: "t-" + req.Pool})
	case method == "DELETE" && strings.HasPrefix(path, AppTranscodersPath+"/"):
		f.deleted++
		return nil, nil
	case method == "POST" && strings.HasSuffix(path, "/poll"):
		if f.forget {
			f.forget = false
			return nil, ErrGone
		}
		var rep AppReport
		json.Unmarshal(raw, &rep)
		f.reports = append(f.reports, rep)
		ans := AppAnswer{Start: f.toStart, Stop: f.toStop, PollMS: f.pollMS}
		f.toStart, f.toStop = nil, nil
		return json.Marshal(ans)
	}
	return nil, errors.New("unexpected call " + method + " " + path)
}

func (f *fakeApp) lastReport() AppReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) == 0 {
		return AppReport{}
	}
	return f.reports[len(f.reports)-1]
}

func (f *fakeApp) start(runs ...AppRun) {
	f.mu.Lock()
	f.toStart = append(f.toStart, runs...)
	f.mu.Unlock()
}

type relayRig struct {
	st     *store.Store
	svc    *Service
	relay  *Relay
	app    *fakeApp
	stream store.App
	cancel context.CancelFunc
}

func newRelayRig(t *testing.T) *relayRig {
	t.Helper()
	r, st := testRegistry(t)
	svc := &Service{Registry: r}
	app := &fakeApp{pollMS: 20}
	relay := NewRelay(svc, st, app, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a, err := st.CreateApp("stream")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppCompute(a.ID, `{"contract":"fog-compute/1","video":{"classes":["interactive","batch"]}}`); err != nil {
		t.Fatal(err)
	}
	other, _ := st.CreateApp("feed")
	_ = other // declares nothing: never polled
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := &relayRig{st: st, svc: svc, relay: relay, app: app, stream: *a, cancel: cancel}
	go relay.Run(ctx)
	return rig
}

// node polls as a machine would, without waiting.
func (rig *relayRig) node(t *testing.T, token string, report NodeReport) *NodeAnswer {
	t.Helper()
	n := authenticate(rig.svc.Registry, token)
	if n == nil {
		t.Fatal("unknown node")
	}
	if report.Running == nil {
		report.Running = []string{}
	}
	ans, err := rig.svc.Poll(context.Background(), n, report, "")
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func session(id string) AppRun {
	return AppRun{ID: id, Kind: "session", Argv: []string{"ffmpeg", "-i", "{server}/fog/compute/runs/x/source"},
		Upload: "{server}/fog/compute/runs/x/files/{name}"}
}

func TestRelayIsOneTranscoderPerPool(t *testing.T) {
	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})

	waitFor(t, "the pool introduced", func() bool { rig.app.mu.Lock(); defer rig.app.mu.Unlock(); return len(rig.app.pools) == 1 })
	if p := rig.app.pools[0]; p.Pool != "video/videotoolbox" || p.Name != "fog · VideoToolbox" || p.Hello.Encoder != "videotoolbox" {
		t.Fatalf("introduced as %+v", p)
	}
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 4 })
	rep := rig.app.lastReport()
	if rep.Free != (Slots{Interactive: 4, Batch: 2}) {
		t.Fatalf("free: %+v", rep.Free)
	}

	// A second machine joins: still one transcoder, with more room.
	studio := mac("Studio")
	studio.Video.Slots = Slots{Interactive: 16, Batch: 4}
	s := pair(t, rig.svc.Registry, studio)
	rig.node(t, s.Token, NodeReport{})
	waitFor(t, "the Studio's room", func() bool { return rig.app.lastReport().Capacity.Interactive == 20 })
	rig.app.mu.Lock()
	introduced := len(rig.app.pools)
	rig.app.mu.Unlock()
	if introduced != 1 {
		t.Fatalf("introduced %d times", introduced)
	}
	if id, err := rig.st.ComputeTranscoder(rig.stream.ID, "video/videotoolbox"); err != nil || id != "t-video/videotoolbox" {
		t.Fatalf("remembered %q %v", id, err)
	}
}

func TestRelayCarriesARunToAMachineAndBack(t *testing.T) {
	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 4 })

	rig.app.start(session("s1"), AppRun{ID: "o1", Kind: "optimize", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/runs/y/files/{name}"})
	var runs []NodeRun
	waitFor(t, "the runs handed to the machine", func() bool {
		ans := rig.node(t, mini.Token, NodeReport{})
		runs = append(runs, ans.Start...)
		return len(runs) == 2
	})
	byClass := map[string]NodeRun{}
	for _, r := range runs {
		byClass[r.Class] = r
	}
	live, batch := byClass[ClassInteractive], byClass[ClassBatch]
	if live.Outputs.Stream != "gop-*.m4s" || live.Outputs.Init[0] != "init.mp4" || batch.Outputs.Final[0] != "output.mp4" {
		t.Fatalf("outputs: %+v / %+v", live.Outputs, batch.Outputs)
	}
	if !strings.HasPrefix(live.Server, DataPath) || live.Server == batch.Server || live.Upload != "{server}/fog/compute/runs/x/files/{name}" {
		t.Fatalf("server %q upload %q", live.Server, live.Upload)
	}

	rig.node(t, mini.Token, NodeReport{Running: []string{live.ID, batch.ID}, Progress: map[string]float64{batch.ID: 12.5}})
	waitFor(t, "running and progress reported", func() bool {
		rep := rig.app.lastReport()
		return len(rep.Running) == 2 && rep.Progress["o1"] == 12.5 && rep.Free == (Slots{Interactive: 3, Batch: 1})
	})

	zero := 0
	rig.node(t, mini.Token, NodeReport{Running: []string{batch.ID}, Exited: []Exited{{ID: live.ID, Status: &zero, Log: "done"}}})
	waitFor(t, "the exit reported", func() bool {
		for _, rep := range rig.app.snapshot() {
			for _, e := range rep.Exited {
				if e.ID == "s1" && e.Status != nil && *e.Status == 0 && e.Log == "done" {
					return true
				}
			}
		}
		return false
	})

	// The app stops the other; the machine hears of it.
	rig.app.mu.Lock()
	rig.app.toStop = []string{"o1"}
	rig.app.mu.Unlock()
	waitFor(t, "the stop delivered", func() bool {
		ans := rig.node(t, mini.Token, NodeReport{Running: []string{batch.ID}})
		return len(ans.Stop) == 1 && ans.Stop[0] == batch.ID
	})
}

func (f *fakeApp) snapshot() []AppReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AppReport(nil), f.reports...)
}

func exitedWith(reports []AppReport, id, log string) bool {
	for _, rep := range reports {
		for _, e := range rep.Exited {
			if e.ID == id && e.Status == nil && strings.Contains(e.Log, log) {
				return true
			}
		}
	}
	return false
}

func TestRelayRefusesWhatItCantPlace(t *testing.T) {
	rig := newRelayRig(t)
	inv := mac("Mini")
	inv.Video.Slots = Slots{Interactive: 1}
	mini := pair(t, rig.svc.Registry, inv)
	rig.node(t, mini.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 1 })

	rig.app.start(session("s1"), session("s2"), AppRun{ID: "o1", Kind: "optimize", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/u/{name}"},
		AppRun{ID: "bad", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/u/{name}"})
	waitFor(t, "the refusals", func() bool {
		reps := rig.app.snapshot()
		return exitedWith(reps, "s2", "no machine in fog · VideoToolbox had room") && exitedWith(reps, "o1", "its share") &&
			exitedWith(reps, "bad", "names no outputs")
	})
}

func TestRelayLosesRunsOfAMachineThatGoesAway(t *testing.T) {
	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	studio := pair(t, rig.svc.Registry, mac("Studio"))
	rig.node(t, mini.Token, NodeReport{})
	rig.node(t, studio.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 8 })

	rig.app.start(session("s1"))
	var on string
	var id string
	waitFor(t, "the run placed", func() bool {
		for _, m := range []*paired{mini, studio} {
			if ans := rig.node(t, m.Token, NodeReport{}); len(ans.Start) == 1 {
				on, id = m.Node, ans.Start[0].ID
				return true
			}
		}
		return false
	})
	if err := rig.svc.Remove(on); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run reported lost", func() bool { return exitedWith(rig.app.snapshot(), "s1", "was removed") })
	_ = id

	// A run handed out but never confirmed is lost too, once its machine
	// stops mentioning it for long enough.
	rig.relay.mu.Lock()
	rig.relay.runs["rX"] = &run{ID: "rX", AppID: rig.stream.ID, Pool: "video/videotoolbox", AppRun: "s9", Node: "gone",
		Started: time.Now().Add(-time.Minute)}
	rig.relay.mu.Unlock()
	waitFor(t, "the stray reported lost", func() bool { return exitedWith(rig.app.snapshot(), "s9", "went offline") })
}

func TestRelayStopsWhatFogNoLongerHolds(t *testing.T) {
	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{Running: []string{"rOld"}})
	waitFor(t, "the orphan stopped", func() bool {
		ans := rig.node(t, mini.Token, NodeReport{Running: []string{"rOld"}})
		return len(ans.Stop) == 1 && ans.Stop[0] == "rOld"
	})
}

func TestDataRouteReachesOnlyARunsAppPaths(t *testing.T) {
	rig := newRelayRig(t)
	var got []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" auth="+r.Header.Get("Authorization")+" range="+r.Header.Get("Range"))
		w.Write([]byte("bytes"))
	}))
	defer app.Close()
	// On a machine that's online, so the relay keeps the run.
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.relay.mu.Lock()
	rig.relay.runs["r1"] = &run{ID: "r1", AppID: rig.stream.ID, Pool: "video/videotoolbox", AppRun: "s1", Node: mini.Node, Data: "tok",
		Started: time.Now(), Confirmed: true}
	rig.relay.mu.Unlock()
	h := rig.svc.DataHandler(func(context.Context, *store.App) (string, error) { return app.URL, nil })

	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("x"))
		if method == "PUT" {
			signAs(req, mini, BodySHA256([]byte("x")))
		} else {
			signAs(req, mini)
		}
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Range", "bytes=0-9")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Log(path, w.Body.String())
		}
		return w.Code
	}
	if c := do("GET", DataPath+"tok/fog/compute/runs/abc/source?sig=1"); c != 200 {
		t.Fatalf("source: %d", c)
	}
	if c := do("PUT", DataPath+"tok/fog/compute/runs/abc/files/gop-1.m4s"); c != 200 {
		t.Fatalf("upload: %d %v", c, got)
	}
	for _, bad := range []string{DataPath + "tok/api/v1/settings", DataPath + "nope/fog/compute/runs/abc/source",
		DataPath + "tok/fog/compute/../../api", DataPath + "tok"} {
		if c := do("GET", bad); c != http.StatusNotFound {
			t.Fatalf("%s: %d", bad, c)
		}
	}
	if len(got) != 2 || got[0] != "GET /fog/compute/runs/abc/source?sig=1 auth= range=bytes=0-9" || !strings.HasPrefix(got[1], "PUT /fog/compute/runs/abc/files/gop-1.m4s") {
		t.Fatalf("app saw %q", got)
	}
}

func TestRelayIntroducesAPoolAgainToAnAppThatForgotIt(t *testing.T) {
	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 4 })
	rig.app.mu.Lock()
	rig.app.forget = true
	rig.app.mu.Unlock()
	waitFor(t, "the pool introduced again", func() bool { rig.app.mu.Lock(); defer rig.app.mu.Unlock(); return len(rig.app.pools) == 2 })
	waitFor(t, "polls resumed with a hello", func() bool { return rig.app.lastReport().Hello != nil })
}

func TestAViewerDisplacesBatchWork(t *testing.T) {
	rig := newRelayRig(t)
	inv := mac("Mini")
	inv.Video.Slots = Slots{Interactive: 2, Batch: 2}
	mini := pair(t, rig.svc.Registry, inv)
	rig.node(t, mini.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 2 })

	optimize := func(id string) AppRun {
		return AppRun{ID: id, Kind: "optimize", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/runs/y/files/{name}"}
	}
	rig.app.start(optimize("o1"))
	var started []NodeRun
	waitFor(t, "o1 placed", func() bool {
		started = append(started, rig.node(t, mini.Token, NodeReport{}).Start...)
		return len(started) == 1
	})
	time.Sleep(10 * time.Millisecond)
	rig.app.start(optimize("o2"))
	waitFor(t, "o2 placed", func() bool {
		started = append(started, rig.node(t, mini.Token, NodeReport{}).Start...)
		return len(started) == 2
	})
	ids := []string{started[0].ID, started[1].ID}
	rig.node(t, mini.Token, NodeReport{Running: ids})
	// Full of batch work, the machine still offers a viewer room.
	waitFor(t, "interactive room over batch work", func() bool {
		rep := rig.app.lastReport()
		return len(rep.Running) == 2 && rep.Free == (Slots{Interactive: 2, Batch: 0})
	})

	rig.app.start(session("s1"))
	var stop []string
	var live []NodeRun
	waitFor(t, "the viewer's run placed, the newest batch run stopped", func() bool {
		ans := rig.node(t, mini.Token, NodeReport{Running: ids})
		stop, live = append(stop, ans.Stop...), append(live, ans.Start...)
		// The displaced run may be told to stop twice: once displaced, once
		// as an orphan while this fake machine still reports it running.
		return slices.Contains(stop, started[1].ID) && len(live) == 1
	})
	if slices.Contains(stop, started[0].ID) || live[0].Class != ClassInteractive {
		t.Fatalf("stopped %v (want the newest, %s); started %+v", stop, started[1].ID, live)
	}
	waitFor(t, "o2 reported displaced", func() bool { return exitedWith(rig.app.snapshot(), "o2", "make room for a viewer") })
}

func TestBatchRoomIsSharedAmongApps(t *testing.T) {
	rig := newRelayRig(t)
	once, _ := rig.st.CreateApp("once")
	rig.st.SetAppCompute(once.ID, `{"contract":"fog-compute/1","video":{"classes":["batch"]}}`)
	inv := mac("Mini")
	inv.Video.Slots = Slots{Interactive: 4, Batch: 4}
	mini := pair(t, rig.svc.Registry, inv)
	rig.node(t, mini.Token, NodeReport{})
	// Two apps hand over batch work: each is offered half.
	waitFor(t, "a fair share", func() bool { return rig.app.lastReport().Free.Batch == 2 })
	rig.app.start(
		AppRun{ID: "o1", Kind: "optimize", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/u/{name}"},
		AppRun{ID: "o2", Kind: "optimize", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/u/{name}"},
		AppRun{ID: "o3", Kind: "optimize", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/u/{name}"})
	waitFor(t, "the third refused", func() bool { return exitedWith(rig.app.snapshot(), "o3", "its share") })
}

func TestSwitchingAPoolOffTakesItAway(t *testing.T) {
	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 4 })

	if err := rig.st.SetComputeGrant(rig.stream.ID, "video/videotoolbox", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the transcoder withdrawn", func() bool {
		_, err := rig.st.ComputeTranscoder(rig.stream.ID, "video/videotoolbox")
		return errors.Is(err, store.ErrNotFound)
	})
	rig.app.mu.Lock()
	deleted := rig.app.deleted
	rig.app.mu.Unlock()
	if deleted != 1 {
		t.Fatalf("app told %d times", deleted)
	}
	usage, _ := rig.relay.Usage(Pools(mustMachines(t, rig)))
	if len(usage) != 1 || usage[0].On || usage[0].Polling {
		t.Fatalf("usage: %+v", usage)
	}
	rig.st.SetComputeGrant(rig.stream.ID, "video/videotoolbox", true)
	waitFor(t, "introduced again", func() bool { rig.app.mu.Lock(); defer rig.app.mu.Unlock(); return len(rig.app.pools) == 2 })
}

func mustMachines(t *testing.T, rig *relayRig) []Machine {
	m, err := rig.svc.Registry.Machines()
	if err != nil {
		t.Fatal(err)
	}
	return m
}
