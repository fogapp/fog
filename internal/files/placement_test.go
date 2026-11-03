package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBinds stands in for mount(8): a bind is recorded, not made, so the
// folder under the root stays the plain directory it was and the tests
// read the location itself to see what moved. sameDir and the mount table
// answer from the record.
type fakeBinds struct {
	mu    sync.Mutex
	at    map[string]string // mount point -> location
	fail  error
	calls []string
}

func useFakeBinds(t *testing.T) *fakeBinds {
	t.Helper()
	fb := &fakeBinds{at: map[string]string{}}
	oldSupported, oldBind, oldUnbind, oldSame, oldMounted := bindSupported, bindMount, unbindMount, sameDir, mountedPoints
	t.Cleanup(func() {
		bindSupported, bindMount, unbindMount, sameDir, mountedPoints = oldSupported, oldBind, oldUnbind, oldSame, oldMounted
	})
	bindSupported = true
	bindMount = func(_ context.Context, src, dst string) error {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.calls = append(fb.calls, "bind "+src+" "+dst)
		if fb.fail != nil {
			return fb.fail
		}
		fb.at[dst] = src
		return nil
	}
	unbindMount = func(_ context.Context, dst string) error {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.calls = append(fb.calls, "unbind "+dst)
		delete(fb.at, dst)
		return nil
	}
	sameDir = func(a, b string) bool {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		return fb.at[b] == a || fb.at[a] == b
	}
	mountedPoints = func() map[string]bool {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		out := map[string]bool{}
		for mp := range fb.at {
			out[mp] = true
		}
		return out
	}
	return fb
}

func (fb *fakeBinds) bound(mp string) string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.at[mp]
}

// Moving Apps to another disk carries its files there, binds the new place
// over the folder, and brackets the job with stopping and starting the
// apps that use it.
func TestPlacementMove(t *testing.T) {
	fb := useFakeBinds(t)
	m := newManager(t)
	write(t, m, "Apps/Sonarr/config.xml", "<config/>")
	write(t, m, "Content/Movies/film.mkv", "film")
	m.List("", ListOpts{})

	var stoppedAt string
	var started []string
	m.StopAppsUnder = func(_ context.Context, path string) ([]string, error) {
		stoppedAt = path
		return []string{"sonarr"}, nil
	}
	m.StartApps = func(_ context.Context, names []string) error {
		started = names
		return nil
	}

	loc := filepath.Join(t.TempDir(), "fast", "fog-apps")
	job, err := m.MovePlacement(AppsDir, loc, "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	mp := filepath.Join(m.Root(), AppsDir)
	if m.Location(AppsDir) != loc {
		t.Errorf("location = %q, want %q", m.Location(AppsDir), loc)
	}
	if b, err := os.ReadFile(filepath.Join(loc, "Sonarr", "config.xml")); err != nil || string(b) != "<config/>" {
		t.Errorf("moved file: %v %q", err, b)
	}
	if fb.bound(mp) != loc {
		t.Errorf("bound at %s: %q, want %q", mp, fb.bound(mp), loc)
	}
	if hasFiles(mp) {
		t.Error("the folder under the mount still has files")
	}
	if stoppedAt != mp || strings.Join(started, ",") != "sonarr" {
		t.Errorf("stopped under %q, started %v", stoppedAt, started)
	}
	if b, err := os.ReadFile(filepath.Join(m.Root(), "Content", "Movies", "film.mkv")); err != nil || string(b) != "film" {
		t.Errorf("Content was touched: %v %q", err, b)
	}
	var apps Placement
	for _, p := range m.Status().Placements {
		if p.Folder == AppsDir {
			apps = p
		}
	}
	if apps.Location != loc || !apps.Bound || apps.Error != "" {
		t.Errorf("status = %+v", apps)
	}

	// And back: the bind comes down and the files return under the root.
	job, err = m.MovePlacement(AppsDir, "", "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job back = %+v", j)
	}
	if m.Location(AppsDir) != "" || fb.bound(mp) != "" {
		t.Errorf("after moving back: location %q, bound %q", m.Location(AppsDir), fb.bound(mp))
	}
	if b, err := os.ReadFile(filepath.Join(mp, "Sonarr", "config.xml")); err != nil || string(b) != "<config/>" {
		t.Errorf("file back in the root: %v %q", err, b)
	}
}

// The checks that run before any job: where a folder may go, and when
// adopting would hide files or moving would mix two trees.
func TestPlacementRefusals(t *testing.T) {
	useFakeBinds(t)
	m := newManager(t)
	write(t, m, "Content/Movies/film.mkv", "film")

	for _, c := range []struct {
		folder, to, mode, want string
	}{
		{"Photos", "/x", "move", "only Apps and Content"},
		{ContentDir, filepath.Join(m.Root(), "inside"), "move", "outside the Drive location"},
		{ContentDir, "relative/path", "move", "absolute"},
		{ContentDir, t.TempDir(), "sideways", "move or adopt"},
		{ContentDir, t.TempDir(), "adopt", "has files in it"},
	} {
		if _, err := m.MovePlacement(c.folder, c.to, c.mode); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("MovePlacement(%s, %s, %s) = %v, want %q", c.folder, c.to, c.mode, err, c.want)
		}
	}
	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "already.txt"), []byte("x"), 0o644)
	if _, err := m.MovePlacement(ContentDir, full, "move"); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("move into a full folder: %v", err)
	}
	// Two folders may not share a location.
	m.Store.SetSetting(SettingAppsLocation, full)
	if _, err := m.MovePlacement(ContentDir, filepath.Join(full, "sub"), "adopt"); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("overlapping locations: %v", err)
	}
}

// A placed folder whose disk is not there refuses reads and writes rather
// than showing (and filling) the empty mount point; the walk keeps what
// the index knew, and the nightly snapshot skips the night.
func TestPlacementOffline(t *testing.T) {
	fb := useFakeBinds(t)
	m := newManager(t)
	write(t, m, "Content/Movies/film.mkv", "film")
	write(t, m, "notes.txt", "hi")
	if err := m.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := m.Stat(ContentDir)
	if err != nil {
		t.Fatal(err)
	}

	m.Store.SetSetting(SettingContentLocation, filepath.Join(t.TempDir(), "big"))
	fb.fail = errors.New("no such device")
	if fresh := m.EnsurePlacements(context.Background()); len(fresh) != 0 {
		t.Errorf("fresh binds = %v", fresh)
	}
	if m.placementOffline(ContentDir) == "" {
		t.Fatal("Content should be offline")
	}
	if _, err := m.List(ContentDir, ListOpts{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("listing an offline Content: %v", err)
	}
	if _, err := m.Mkdir("Content/New"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("mkdir in an offline Content: %v", err)
	}
	if _, err := m.BeginUpload("Content/Movies/b.mkv", 1, "", "", ConflictAsk); !errors.Is(err, ErrReadOnly) {
		t.Errorf("upload into an offline Content: %v", err)
	}
	if _, err := m.List("", ListOpts{}); err != nil {
		t.Errorf("the root still lists: %v", err)
	}
	if err := m.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after, err := m.Stat(ContentDir); err != nil || after.Size != before.Size {
		t.Errorf("Content size after an offline walk = %d (%v), want %d", after.Size, err, before.Size)
	}
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	if _, err := m.RunSnapshot(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("snapshot with Content offline: %v", err)
	}
	if err := m.EnsurePlacementsFor(context.Background(), []string{filepath.Join(m.Root(), "Content", "Movies")}); err == nil {
		t.Error("an app in an offline Content should not start")
	}
	if err := m.EnsurePlacementsFor(context.Background(), []string{"/srv/elsewhere"}); err != nil {
		t.Errorf("an app outside it: %v", err)
	}

	// The disk comes back: the next ensure binds it and says so.
	fb.fail = nil
	fresh := m.EnsurePlacements(context.Background())
	if len(fresh) != 1 || fresh[0] != filepath.Join(m.Root(), ContentDir) {
		t.Errorf("fresh binds = %v", fresh)
	}
	if m.placementOffline(ContentDir) != "" {
		t.Error("Content still offline")
	}
	// Already bound: nothing new.
	if fresh := m.EnsurePlacements(context.Background()); len(fresh) != 0 {
		t.Errorf("second ensure bound %v", fresh)
	}
	// The placed folder itself can't be trashed or renamed from Drive.
	if _, err := m.Trash([]string{ContentDir}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("trashing placed Content: %v", err)
	}
}

// With Content on another disk, what Drive stages before a rename — a
// trashed entry, an upload's part — stays in Content's own .fog, which no
// listing shows and no name can reach.
func TestPlacementStagesOnItsOwnDisk(t *testing.T) {
	useFakeBinds(t)
	m := newManager(t)
	write(t, m, "Content/Movies/film.mkv", "film")
	write(t, m, "notes.txt", "hi")
	m.Store.SetSetting(SettingContentLocation, filepath.Join(t.TempDir(), "big"))
	m.EnsurePlacements(context.Background())

	items, err := m.Trash([]string{"Content/Movies/film.mkv", "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Bin != ContentDir || items[1].Bin != "" {
		t.Errorf("bins = %q, %q", items[0].Bin, items[1].Bin)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "Content", ".fog", "trash", items[0].ID, "film.mkv")); err != nil {
		t.Errorf("film not in Content's own trash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), ".fog", "trash", items[1].ID, "notes.txt")); err != nil {
		t.Errorf("notes not in the root's trash: %v", err)
	}
	listed, _ := m.ListTrash()
	if len(listed) != 2 {
		t.Errorf("trash lists %d entries, want 2", len(listed))
	}
	l, err := m.List(ContentDir, ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names(l) {
		if n == ReservedDir {
			t.Error("Content's .fog is listed")
		}
	}
	if _, err := m.Mkdir("Content/.fog"); !errors.Is(err, ErrReserved) {
		t.Errorf("mkdir Content/.fog: %v", err)
	}
	if out, err := m.RestoreTrash([]string{items[0].ID}); err != nil || out[0] != "Content/Movies/film.mkv" {
		t.Errorf("restore = %v, %v", out, err)
	}

	up, err := m.BeginUpload("Content/Movies/new.mkv", 3, "", "", ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "Content", ".fog", "uploads", up.ID+".part")); err != nil {
		t.Errorf("part not staged in Content: %v", err)
	}
	if _, err := m.AppendUpload(up.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if n, err := m.CompleteUpload(up.ID); err != nil || n.Size != 3 {
		t.Errorf("complete = %+v, %v", n, err)
	}
}

// A root move takes the binds down first — a copy would otherwise carry
// the other disk's bytes into the new root — and puts them back over it.
func TestRootMoveRebindsPlacements(t *testing.T) {
	fb := useFakeBinds(t)
	m := newManager(t)
	write(t, m, "notes.txt", "hi")
	loc := filepath.Join(t.TempDir(), "fast")
	m.Store.SetSetting(SettingAppsLocation, loc)
	m.EnsurePlacements(context.Background())
	old := filepath.Join(m.Root(), AppsDir)
	if fb.bound(old) != loc {
		t.Fatal("not bound before the move")
	}
	to := filepath.Join(t.TempDir(), "moved")
	job, err := m.MoveRoot(to, "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	if fb.bound(old) != "" {
		t.Error("still bound at the old root")
	}
	if fb.bound(filepath.Join(to, AppsDir)) != loc {
		t.Errorf("not bound at the new root: %v", fb.calls)
	}
}
