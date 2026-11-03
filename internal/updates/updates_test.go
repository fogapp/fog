package updates

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

func TestCommitOutdated(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name          string
		local, remote string
		want          bool
	}{
		{"same full sha", full, full, false},
		{"short local matches remote", full[:12], full, false},
		{"remote moved on", full, "fedcba9876543210fedcba9876543210fedcba98", true},
		{"short local, remote moved on", full[:12], "fedcba9876543210fedcba9876543210fedcba98", true},
		{"empty local never outdated", "", full, false},
		{"empty remote never outdated", full, "", false},
	}
	for _, c := range cases {
		if got := commitOutdated(c.local, c.remote); got != c.want {
			t.Errorf("%s: commitOutdated(%q, %q) = %v, want %v", c.name, c.local, c.remote, got, c.want)
		}
	}
}

// The verdict a manual "check for updates" reports. An unknown must never
// come out as "no update": that reads to the operator as "up to date".
func TestUpdateAvailable(t *testing.T) {
	const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const shaA = "0123456789abcdef0123456789abcdef01234567"
	const shaB = "fedcba9876543210fedcba9876543210fedcba98"

	cases := []struct {
		name          string
		source        string
		local, remote string
		want          bool
	}{
		{"image digests match", "image", digestA, digestA, false},
		{"image digest moved on", "image", digestA, digestB, true},
		{"image never pulled", "image", "", digestA, false},
		{"image registry gave nothing", "image", digestA, "", false},
		{"git head unchanged", "git", shaA, shaA, false},
		{"git short local matches head", "git", shaA[:12], shaA, false},
		{"git head moved on", "git", shaA, shaB, true},
		{"git never deployed", "git", "", shaA, false},
	}
	for _, c := range cases {
		if got := updateAvailable(c.source, c.local, c.remote); got != c.want {
			t.Errorf("%s: updateAvailable(%q, %q, %q) = %v, want %v", c.name, c.source, c.local, c.remote, got, c.want)
		}
	}
}

// testChecker wires a Checker to a real (temp) store, a discard logger, and
// a notification recorder. Engine is a zero-value engine: enough for the op
// bookkeeping (BeginOp/Deploying) without any Docker underneath.
func testChecker(t *testing.T) (*Checker, *[]string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var sent []string
	c := &Checker{
		Store:  st,
		Engine: &apps.Engine{},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notify: func(title, msg string, ok bool) { sent = append(sent, title) },
	}
	return c, &sent
}

// The notify policy announces a remote version once. UpdateAvailable stays
// true until the operator updates, so without dedup the enforce loop would
// re-push the same news every tick, forever.
func TestNotifyUpdateOncePerRemoteRef(t *testing.T) {
	c, sent := testChecker(t)
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}
	spec := &apps.AppSpec{}
	spec.Source.Type = "image"
	spec.Source.Image = "x/y:latest"

	res := CheckResult{App: "music", UpdateAvailable: true, To: "aaaaaaaaaaaa"}
	c.notifyUpdate(*app, spec, res)
	c.notifyUpdate(*app, spec, res) // same remote ref: the next tick
	if len(*sent) != 1 {
		t.Fatalf("same remote ref notified %d times, want 1", len(*sent))
	}

	res.To = "bbbbbbbbbbbb" // remote moved on: genuinely new news
	c.notifyUpdate(*app, spec, res)
	if len(*sent) != 2 {
		t.Fatalf("new remote ref: %d notifications total, want 2", len(*sent))
	}
}

// A failing auto-update stands down: the same remote ref is neither
// re-attempted nor re-alerted on the next tick (only after the ref changes
// or the cooldown passes). The app here has no revisions, so every Apply
// fails before touching Docker — a stand-in for a persistently failing
// update.
func TestAutoUpdateStandsDownAfterFailure(t *testing.T) {
	c, sent := testChecker(t)
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}

	res := CheckResult{App: "music", UpdateAvailable: true, To: "aaaaaaaaaaaa"}
	c.autoUpdate(context.Background(), *app, res)
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "Auto-update failed") {
		t.Fatalf("first failure: notifications %v, want one failure alert", *sent)
	}

	c.autoUpdate(context.Background(), *app, res) // next tick, same ref
	if len(*sent) != 1 {
		t.Fatalf("same ref within cooldown re-attempted/re-alerted: %v", *sent)
	}

	res.To = "bbbbbbbbbbbb" // remote moved on: worth another attempt
	c.autoUpdate(context.Background(), *app, res)
	if len(*sent) != 2 {
		t.Fatalf("new remote ref: %d notifications total, want 2", len(*sent))
	}
}

// A CheckAll that loses the overlap guard answers from stored state without
// checking anything: no failure streaks counted (an overlap used to
// double-count each real outage), no notifications, and the results reflect
// what the store knows.
func TestCheckAllOverlapReturnsStoredState(t *testing.T) {
	c, sent := testChecker(t)
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.CreateRevision(app.ID, `{"source":{"type":"image","image":"x/y:latest"}}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	const local = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const remote = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := c.Store.SetAppUpdate(app.ID, local, remote, true); err != nil {
		t.Fatal(err)
	}

	c.checkMu.Lock() // a check is already running
	defer c.checkMu.Unlock()
	results := c.CheckAll(context.Background(), true)

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.App != "music" || !r.UpdateAvailable || !r.Comparable || r.Source != "image" {
		t.Errorf("stored result mangled: %+v", r)
	}
	if r.From != apps.ShortRef(local) || r.To != apps.ShortRef(remote) {
		t.Errorf("stored refs mangled: from %q to %q", r.From, r.To)
	}
	if len(*sent) != 0 {
		t.Errorf("loser sent notifications: %v", *sent)
	}
	if n := c.failNotice().Streak("music"); n != 0 {
		t.Errorf("loser counted failure streaks: %d", n)
	}
}

// The "update source unreachable" notice re-raises every failThreshold
// consecutive failures: dismissed while still down must not mean silent
// forever. One active notice per (kind, app) — re-raises replace, not stack.
func TestNoteFailureReRaisesAfterDismissal(t *testing.T) {
	c, _ := testChecker(t)

	activeNotices := func() int {
		ns, err := c.Store.ActiveNotices()
		if err != nil {
			t.Fatal(err)
		}
		return len(ns)
	}

	c.noteFailure("music", "no route to host", "git@old.lan:m.git")
	c.noteFailure("music", "no route to host", "git@old.lan:m.git")
	if activeNotices() != 0 {
		t.Fatal("notice raised before the threshold")
	}
	c.noteFailure("music", "no route to host", "git@old.lan:m.git") // streak 3
	if activeNotices() != 1 {
		t.Fatal("no notice at the threshold")
	}

	if err := c.Store.DismissNoticeByKind("update-check-failing", "music"); err != nil {
		t.Fatal(err)
	}
	c.noteFailure("music", "no route to host", "git@old.lan:m.git") // 4
	c.noteFailure("music", "no route to host", "git@old.lan:m.git") // 5
	if activeNotices() != 0 {
		t.Fatal("notice re-raised off-cadence")
	}
	c.noteFailure("music", "no route to host", "git@old.lan:m.git") // 6: re-raise
	if activeNotices() != 1 {
		t.Fatal("dismissed notice never re-raised while source still down")
	}

	// Success clears streak and notice; a fresh outage starts from zero.
	c.noteSuccess("music")
	if activeNotices() != 0 {
		t.Fatal("notice survived a successful check")
	}
}

// After a rollback the image tag still resolves to the newer image that was
// pulled before the operator stepped back from it. The check has to compare
// what fog *deployed*, not what the tag now points at, or an app running
// last month's version reads as up to date.
func TestDeployedDigestPrefersWhatWasDeployed(t *testing.T) {
	c, _ := testChecker(t)
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}
	const older = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newer = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := c.Store.RecordAppVersion(app.ID, "image", newer, "x/y:latest", 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.RecordAppVersion(app.ID, "image", older, "x/y@"+older, 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	got, err := c.deployedDigest(app)
	if err != nil {
		t.Fatal(err)
	}
	if got != older {
		t.Errorf("deployed digest = %q, want the rolled-back-to %q", got, older)
	}
	if !updateAvailable("image", got, newer) {
		t.Error("an app deliberately running an older digest reads as up to date")
	}
}

// A failed registry lookup keeps what actually went wrong. It used to be
// replaced by a bare "registry lookup failed", which left the operator
// nothing to act on.
func TestRemoteDigestKeepsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := remoteDigest(ctx, "x/y:latest")
	if err == nil {
		t.Fatal("a cancelled lookup succeeded")
	}
	// Without docker installed the cause is the missing binary; with it, the
	// cancelled context. Either way it must survive the wrapping.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("registry lookup error lost its cause: %v", err)
	}
}

// The lookup's output includes stderr, so a warning printed beside the
// digest must not be read as the digest.
func TestDigestInSkipsWarnings(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for _, out := range []string{d, "WARNING: something odd\n" + d, d + "\nWARNING: after"} {
		if got, err := digestIn(out); err != nil || got != d {
			t.Errorf("digestIn(%q) = %q, %v; want %q", out, got, err, d)
		}
	}
	if _, err := digestIn("WARNING: no manifest"); err == nil {
		t.Error("output without a digest was accepted")
	}
}

// A git app's recorded versions must not be mistaken for an image app's
// digests: the source changed and no image has deployed yet, so there is
// nothing to compare.
func TestDeployedDigestIgnoresGitVersions(t *testing.T) {
	c, _ := testChecker(t)
	app, err := c.Store.CreateApp("manor")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Store.RecordAppVersion(app.ID, "git", "0123456789abcdef", "fog/manor:0123456789ab", 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.deployedDigest(app); err != nil || got != "" {
		t.Errorf("deployed digest = %q, %v; a commit sha was handed back as this image app's digest", got, err)
	}
}

// An app with no recorded version has nothing to compare, and says so: the
// locally pulled tag is not consulted, since it need not be what runs.
func TestDeployedDigestWithoutVersion(t *testing.T) {
	c, _ := testChecker(t)
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.deployedDigest(app)
	if err != nil || got != "" {
		t.Fatalf("deployed digest = %q, %v; want nothing to compare", got, err)
	}
	if updateAvailable("image", got, "sha256:"+strings.Repeat("b", 64)) {
		t.Error("an app with no recorded version reads as having an update")
	}
}

// Rollback only ever deploys a version fog watched this app run. A digest
// from somewhere else is refused before any snapshot is taken or any
// container is touched.
func TestRollbackRefusesUnknownVersion(t *testing.T) {
	c, _ := testChecker(t)
	if _, err := c.Store.CreateApp("music"); err != nil {
		t.Fatal(err)
	}
	err := c.Rollback(context.Background(), "music", "sha256:deadbeef", "matthew")
	if err == nil {
		t.Fatal("rollback accepted a version this app never ran")
	}
	if !strings.Contains(err.Error(), "no record") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// An app rebuilt from a different kind of source can't go back to versions
// of the old one: a digest is not a commit, and pinning one as the other
// would fail deep inside a deploy instead of here.
func TestRollbackRefusesVersionFromAnotherSource(t *testing.T) {
	c, _ := testChecker(t)
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.CreateRevision(app.ID, `{"source":{"type":"git","repo":"git@host:m.git"}}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.RecordAppVersion(app.ID, "image", "sha256:aaaa", "x/y:latest", 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	err = c.Rollback(context.Background(), "music", "sha256:aaaa", "matthew")
	if err == nil || !strings.Contains(err.Error(), "no longer be deployed") {
		t.Fatalf("got %v, want a refusal naming the source change", err)
	}
}

// A rollback on an app that updates itself turns auto-updates off. Left on,
// the next enforce tick sees the version just stepped away from as an update
// and deploys it straight back.
func TestPauseAutoUpdatesAfterRollback(t *testing.T) {
	c, sent := testChecker(t)
	c.Engine = &apps.Engine{Store: c.Store}
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}
	const spec = `{"source":{"type":"image","image":"x/y:latest"},"updatePolicy":"auto"}`
	if _, err := c.Store.CreateRevision(app.ID, spec, "", "t"); err != nil {
		t.Fatal(err)
	}
	s, err := apps.LatestSpec(c.Store, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.pauseAutoUpdates(context.Background(), "music", s, "matthew")

	after, err := apps.LatestSpec(c.Store, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatePolicy != "notify" {
		t.Errorf("policy after rollback = %q, want notify", after.UpdatePolicy)
	}
	if after.Source.Image != "x/y:latest" {
		t.Errorf("pausing updates changed the image to %q", after.Source.Image)
	}
	rev, err := c.Store.LatestRevision(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Rev != 2 || !strings.Contains(rev.Comment, "rolled back") {
		t.Errorf("got rev %d %q, want a second revision saying why", rev.Rev, rev.Comment)
	}
	if len(*sent) != 0 {
		t.Errorf("a pause that worked still alerted: %v", *sent)
	}
}

// Only auto is paused: an app on notify or manual is already not going to
// update itself, and a revision saying nothing changed is noise in History.
func TestPauseAutoUpdatesLeavesOtherPoliciesAlone(t *testing.T) {
	c, _ := testChecker(t)
	c.Engine = &apps.Engine{Store: c.Store}
	app, err := c.Store.CreateApp("music")
	if err != nil {
		t.Fatal(err)
	}
	for i, policy := range []string{"notify", ""} {
		spec := `{"source":{"type":"image","image":"x/y:latest"},"updatePolicy":"` + policy + `"}`
		if _, err := c.Store.CreateRevision(app.ID, spec, "", "t"); err != nil {
			t.Fatal(err)
		}
		s, err := apps.LatestSpec(c.Store, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		c.pauseAutoUpdates(context.Background(), "music", s, "matthew")
		rev, err := c.Store.LatestRevision(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rev.Rev != int64(i+1) {
			t.Errorf("policy %q: a revision was written (rev %d)", policy, rev.Rev)
		}
	}
}
