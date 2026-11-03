package issues

import (
	"context"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// TestFilesSyncSweep runs the real registration through a sweep: one check
// files two kinds, runs once for both, and each kind syncs on its own — a
// stray kind used to make SyncIssues reject the whole batch, and the check
// used to be registered (and run) once per kind.
func TestFilesSyncSweep(t *testing.T) {
	now := time.Now().UTC()
	sn := &snapshot{now: now, peerNames: map[int64]string{1: "dave"}, fileSubs: []store.FileFedSubscription{
		{ID: 2, PeerID: 1, Title: "Album", Status: "error", LastSyncedAt: now.Add(-30 * time.Hour).Format(time.RFC3339), LastError: "dial"},
		{ID: 4, PeerID: 1, Title: "Music", Status: "offer_changed"},
	}}
	var registry []check
	runs := 0
	for _, ck := range (&Scanner{}).checks() {
		for _, k := range ck.kinds() {
			if k == "files-sync-failing" || k == "files-offer-changed" {
				run := ck.run
				ck.run = func(ctx context.Context, _ *snapshot) report { runs++; return run(ctx, sn) }
				registry = append(registry, ck)
				break
			}
		}
	}
	if len(registry) != 1 {
		t.Fatalf("registry = %d checks, want the one that files both kinds", len(registry))
	}
	sc, st, _ := testScanner(t, registry)
	sc.Sweep(context.Background())
	if runs != 1 {
		t.Errorf("the check ran %d times in one sweep, want 1", runs)
	}
	open, err := st.ListIssues([]string{"open"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, is := range open {
		kinds[is.Kind] = true
	}
	if len(open) != 2 || !kinds["files-sync-failing"] || !kinds["files-offer-changed"] {
		t.Fatalf("both kinds should file, got %+v", open)
	}
}

func TestFilesSync(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	rfc := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	sn := &snapshot{now: now, peerNames: map[int64]string{1: "dave"}, fileSubs: []store.FileFedSubscription{
		{ID: 1, PeerID: 1, Title: "Holiday", Folder: "Peers/dave/Holiday", Status: "error", LastSyncedAt: rfc(2 * time.Hour), LastError: "dial"},
		{ID: 2, PeerID: 1, Title: "Album", Folder: "Peers/dave/Album", Status: "error", LastSyncedAt: rfc(30 * time.Hour), LastError: "dial"},
		{ID: 3, PeerID: 1, Title: "Docs", Folder: "Peers/dave/Docs", Status: "error", CreatedAt: rfc(48 * time.Hour), LastError: "no route"},
		{ID: 4, PeerID: 1, Title: "Music", Folder: "Peers/dave/Music", Status: "offer_changed", CurrentImpact: "dave sees 3 files as full copies."},
		{ID: 5, PeerID: 1, Title: "Fine", Folder: "Peers/dave/Fine", Status: "active"},
	}}
	rep := checkFilesSync(context.Background(), sn)
	if len(rep.evaluated) != 1 || rep.evaluated[0] != 0 {
		t.Fatalf("evaluated = %v", rep.evaluated)
	}
	kinds := map[string]string{}
	for _, f := range rep.findings {
		kinds[f.Subject] = f.Kind
		if f.FixURL != "/federation" {
			t.Errorf("%s: fix url %q", f.Subject, f.FixURL)
		}
	}
	if kinds["1"] != "" {
		t.Error("a two-hour failure is a blip, not an issue")
	}
	if kinds["2"] != "files-sync-failing" || kinds["3"] != "files-sync-failing" {
		t.Errorf("day-old failures should file: %v", kinds)
	}
	if kinds["4"] != "files-offer-changed" {
		t.Errorf("a parked offer should file as info: %v", kinds)
	}
	if kinds["5"] != "" {
		t.Error("an active subscription is not an issue")
	}
	for _, f := range rep.findings {
		if f.Subject == "4" && f.Severity != "info" {
			t.Errorf("offer severity = %s", f.Severity)
		}
		if f.Subject == "2" && (f.Severity != "warning" || f.Title != "Album from dave has not synced for a day") {
			t.Errorf("stale finding = %+v", f)
		}
	}
}
