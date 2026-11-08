package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ListDeploys is the whole-log listing the pruning tests check against;
// the app itself reads tails through ListDeploySummaries.
func (s *Store) ListDeploys(appID int64, limit int) ([]Deploy, error) {
	return queryAll(s.db, func(sc scanner) (Deploy, error) {
		var d Deploy
		err := sc.Scan(&d.ID, &d.AppID, &d.RevisionID, &d.Rev, &d.Status, &d.Log, &d.Actor, &d.StartedAt, &d.FinishedAt)
		return d, err
	}, `SELECT d.id, d.app_id, d.revision_id, r.rev, d.status, d.log, d.actor, d.started_at, COALESCE(d.finished_at, '')
		FROM deploys d JOIN app_revisions r ON r.id = d.revision_id
		WHERE d.app_id = ? ORDER BY d.id DESC LIMIT ?`, appID, limit)
}

func TestListDeploySummariesTrimsLogs(t *testing.T) {
	t.Parallel()

	s, err := openTest(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	app, err := s.CreateApp("noisy")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.CreateRevision(app.ID, `{"source":{"type":"image","image":"x"}}`, "initial", "test")
	if err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("a", 5000) + "TAIL-MARKER"
	id, err := s.CreateDeploy(app.ID, rev.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeploy(id, "succeeded", long); err != nil {
		t.Fatal(err)
	}
	shortID, err := s.CreateDeploy(app.ID, rev.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeploy(shortID, "succeeded", "brief"); err != nil {
		t.Fatal(err)
	}

	const tail = 100
	got, err := s.ListDeploySummaries(app.ID, 10, tail)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d deploys, want 2", len(got))
	}

	// Newest first: the short one.
	short, big := got[0], got[1]
	if short.Log != "brief" {
		t.Errorf("short log = %q, want %q", short.Log, "brief")
	}
	if short.LogTruncated {
		t.Error("a log below the tail size was marked truncated")
	}

	if len(big.Log) != tail {
		t.Errorf("trimmed log is %d bytes, want %d", len(big.Log), tail)
	}
	if !strings.HasSuffix(big.Log, "TAIL-MARKER") {
		t.Errorf("kept the head instead of the tail: %q", big.Log)
	}
	if !big.LogTruncated {
		t.Error("an oversized log was not flagged as truncated")
	}
}

// A deploy left 'running' by a fogd that died mid-deploy has nobody to finish
// it — the boot sweep must close it out, or the UI reports an eternal
// in-flight deploy. Finished deploys are not its business.
func TestFailInterruptedDeploys(t *testing.T) {
	t.Parallel()

	s, err := openTest(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	app, err := s.CreateApp("app")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.CreateRevision(app.ID, `{"source":{"type":"image","image":"x"}}`, "initial", "test")
	if err != nil {
		t.Fatal(err)
	}
	orphanID, err := s.CreateDeploy(app.ID, rev.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	doneID, err := s.CreateDeploy(app.ID, rev.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeploy(doneID, "succeeded", "done"); err != nil {
		t.Fatal(err)
	}

	n, err := s.FailInterruptedDeploys()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("closed %d deploys, want 1", n)
	}

	got, err := s.ListDeploys(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]Deploy{}
	for _, d := range got {
		byID[d.ID] = d
	}
	orphan := byID[orphanID]
	if orphan.Status != "failed" {
		t.Errorf("orphan status = %q, want failed", orphan.Status)
	}
	if orphan.FinishedAt == "" {
		t.Error("orphan has no finished_at")
	}
	if !strings.Contains(orphan.Log, "interrupted by fogd restart") {
		t.Errorf("orphan log does not say why it failed: %q", orphan.Log)
	}
	if done := byID[doneID]; done.Status != "succeeded" || done.Log != "done" {
		t.Errorf("the sweep touched a finished deploy: %+v", done)
	}

	// Idempotent: a second boot with nothing running sweeps nothing.
	if n, err := s.FailInterruptedDeploys(); err != nil || n != 0 {
		t.Errorf("second sweep: n=%d err=%v, want 0, nil", n, err)
	}
}

// The summary variant must not carry spec JSON — that is the whole reason it
// exists — while still returning the fields the history table renders.
func TestListRevisionSummariesOmitSpec(t *testing.T) {
	t.Parallel()

	s, err := openTest(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	app, err := s.CreateApp("app")
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"source":{"type":"image","image":"` + strings.Repeat("x", 2000) + `"}}`
	if _, err := s.CreateRevision(app.ID, spec, "first", "alice"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListRevisionSummaries(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d revisions, want 1", len(got))
	}
	if got[0].Spec != "" {
		t.Errorf("spec came along anyway (%d bytes)", len(got[0].Spec))
	}
	if got[0].Rev != 1 || got[0].Comment != "first" || got[0].Actor != "alice" {
		t.Errorf("summary lost fields the history table needs: %+v", got[0])
	}

	// The full revision is still retrievable where it is genuinely needed.
	full, err := s.LatestRevision(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Spec != spec {
		t.Error("LatestRevision no longer returns the full spec")
	}
}

// Deploy retention is per app: PruneDeploys keeps each app's newest rows,
// ClearOldDeployLogs empties the bulk (the log column) on kept-but-older
// rows without touching the history itself.
func TestDeployRetention(t *testing.T) {
	t.Parallel()

	s, err := openTest(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mkApp := func(name string, deploys int) int64 {
		app, err := s.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		rev, err := s.CreateRevision(app.ID, `{"source":{"type":"image","image":"x"}}`, "", "test")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < deploys; i++ {
			id, err := s.CreateDeploy(app.ID, rev.ID, "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FinishDeploy(id, "succeeded", fmt.Sprintf("log-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		return app.ID
	}
	busy := mkApp("busy", 5)
	quiet := mkApp("quiet", 2)

	n, err := s.PruneDeploys(3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2 (only busy's oldest)", n)
	}
	kept, err := s.ListDeploys(busy, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 || kept[0].Log != "log-4" || kept[2].Log != "log-2" {
		t.Fatalf("busy kept the wrong rows: %+v", kept)
	}
	if q, _ := s.ListDeploys(quiet, 10); len(q) != 2 {
		t.Fatalf("quiet lost rows it was under the cap for: %d", len(q))
	}

	// Trim logs beyond the newest 1 per app; rows stay, bulk goes.
	cleared, err := s.ClearOldDeployLogs(1)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 3 { // busy: 2 older kept rows; quiet: 1
		t.Fatalf("cleared %d logs, want 3", cleared)
	}
	kept, _ = s.ListDeploys(busy, 10)
	if kept[0].Log != "log-4" {
		t.Fatal("newest deploy lost its log")
	}
	for _, d := range kept[1:] {
		if d.Log != "" {
			t.Fatalf("older deploy %d kept its log %q", d.ID, d.Log)
		}
		if d.Status != "succeeded" || d.StartedAt == "" {
			t.Fatalf("log trim damaged history fields: %+v", d)
		}
	}
	// Idempotent: a second pass finds nothing with a log to clear.
	if n, _ := s.ClearOldDeployLogs(1); n != 0 {
		t.Fatalf("second trim cleared %d rows", n)
	}
}

// The deploy tray's clock: the running deploy's start, and the median of
// the last five successful deploys — failures and older runs left out.
func TestDeployTiming(t *testing.T) {
	t.Parallel()

	s, err := openTest(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	app, err := s.CreateApp("web")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.CreateRevision(app.ID, "{}", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if started, typical, err := s.DeployTiming(app.ID); err != nil || started != "" || typical != 0 {
		t.Fatalf("no deploys: got %q %v %v", started, typical, err)
	}
	// Oldest first: 500s (beyond the last five), then 10, 40, 20, 30, 60 —
	// median 30 — and a 900s failure that must not count.
	for _, d := range []struct {
		secs   int
		status string
	}{{500, "succeeded"}, {10, "succeeded"}, {40, "succeeded"}, {900, "failed"}, {20, "succeeded"}, {30, "succeeded"}, {60, "succeeded"}} {
		id, err := s.CreateDeploy(app.ID, rev.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE deploys SET status = ?, started_at = '2026-10-01T00:00:00Z', finished_at = ? WHERE id = ?`,
			d.status, fmt.Sprintf("2026-10-01T00:%02d:%02dZ", d.secs/60, d.secs%60), id); err != nil {
			t.Fatal(err)
		}
	}
	started, typical, err := s.DeployTiming(app.ID)
	if err != nil || started != "" || typical != 30*time.Second {
		t.Fatalf("finished history: got %q %v %v, want \"\" 30s", started, typical, err)
	}
	if _, err := s.CreateDeploy(app.ID, rev.ID, "test"); err != nil {
		t.Fatal(err)
	}
	started, typical, err = s.DeployTiming(app.ID)
	if err != nil || started == "" || typical != 30*time.Second {
		t.Fatalf("running deploy: got %q %v %v, want a start and 30s", started, typical, err)
	}
}
