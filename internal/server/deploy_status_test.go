package server

import (
	"encoding/json"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

// The deploy-status poll reads only the log's tail, yet must answer exactly
// as it did from the full log: the same tail (multi-byte text included,
// since the store cuts by characters and LogTail by bytes), and a diagnosis
// matched anywhere in the log, not just in the tail it read.
func TestDeployStatusReadsTailButDiagnosesWholeLog(t *testing.T) {
	srv, h := newTestServer(t)
	srv.engine = &apps.Engine{Store: srv.store}
	cookie := setup(t, h)

	app, err := srv.store.CreateApp("web")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := srv.store.CreateRevision(app.ID, "{}", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("fatal: Could not resolve host: gitea.lan\n") // only near the start
	for b.Len() < 20000 {
		b.WriteString("building… step ✓ ünïcode line\n")
	}
	full := b.String()
	id, err := srv.store.CreateDeploy(app.ID, rev.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishDeploy(id, "failed", full); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/api/apps/web/deploy-status", "", withCookie(cookie))
	if w.Code != 200 {
		t.Fatalf("deploy-status: %d %s", w.Code, w.Body)
	}
	var got struct {
		Last struct {
			Status    string          `json:"status"`
			LogTail   string          `json:"logTail"`
			Diagnosis *apps.Diagnosis `json:"diagnosis"`
		} `json:"last"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := apps.LogTail(full, 4000); got.Last.LogTail != want {
		t.Errorf("logTail differs from the full log's tail:\n got %d bytes\nwant %d bytes", len(got.Last.LogTail), len(want))
	}
	if got.Last.Diagnosis == nil {
		t.Error("no diagnosis: the matching line sits before the tail and was never read")
	}
}

// The shell's deploy tray lists every app with an operation in flight: its
// stage, and how long its deploy has run once a deploy row exists — an
// update's snapshot comes first and reads as a stage alone.
func TestLiveDeploysListsAppsInFlight(t *testing.T) {
	srv, h := newTestServer(t)
	srv.engine = &apps.Engine{Store: srv.store}
	cookie := setup(t, h)

	app, err := srv.store.CreateApp("web")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := srv.store.CreateRevision(app.ID, "{}", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	read := func() []liveDeployView {
		t.Helper()
		w := do(t, h, "GET", "/api/deploys", "", withCookie(cookie))
		if w.Code != 200 {
			t.Fatalf("deploys: %d %s", w.Code, w.Body)
		}
		var got liveDeploysResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Deploys
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("idle: got %+v", got)
	}
	if !srv.engine.BeginOp("web", "taking a snapshot") {
		t.Fatal("BeginOp refused")
	}
	got := read()
	if len(got) != 1 || got[0].App != "web" || got[0].Stage != "taking a snapshot" || got[0].ElapsedMs != 0 {
		t.Fatalf("before the deploy row: got %+v", got)
	}
	if _, err := srv.store.CreateDeploy(app.ID, rev.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 1 || got[0].ElapsedMs <= 0 {
		t.Fatalf("with a running deploy: got %+v", got)
	}
	srv.engine.EndOp("web")
	if got := read(); len(got) != 0 {
		t.Fatalf("after: got %+v", got)
	}
}
