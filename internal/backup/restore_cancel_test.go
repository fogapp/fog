package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// stagingLocOf is the staging directory of a managed volume, addressed the
// way the manager walks it: from the data dir, component by component.
func stagingLocOf(m *Manager, app, volume string) volumeLoc {
	return volumeLoc{anchor: m.Mat.DataDir, rel: []string{"data", app, volume, "fog", "restore"}}
}

// stage writes an armed marker and artifact into one of an app's volumes,
// the way StageArtifact does.
func stage(t *testing.T, m *Manager, app, volume string) string {
	t.Helper()
	dir := filepath.Join(m.Mat.DataPath(app), volume, "fog", "restore")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending.json"), []byte(`{"armed":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending.tar"), []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{Mat: &apps.Materializer{DataDir: t.TempDir()}}
}

// The marker is what the app's boot guard reads, so disarming must remove it
// along with the staged artifact — and must not fail on a staging directory
// that has already been partly cleared.
func TestDisarmRestore(t *testing.T) {
	m := newTestManager(t)
	dir := stage(t, m, "hub", "storage")
	loc := stagingLocOf(m, "hub", "storage")

	if err := disarmRestore(loc); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pending.json", "pending.tar"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Fatalf("%s still present after disarm", f)
		}
	}
	if err := disarmRestore(loc); err != nil {
		t.Fatalf("disarming an already-clear directory: %v", err)
	}
}

// Staged restores are found from disk across every managed volume — the state
// an operator has to be able to see after fogd restarts mid-restore.
func TestRestoreStaged(t *testing.T) {
	m := newTestManager(t)
	if m.RestoreStaged("hub") {
		t.Fatal("nothing staged should read as nothing staged")
	}

	// Not the first volume: cancel must clear whatever is actually armed,
	// including a marker in a volume the spec has since reordered.
	dir := stage(t, m, "hub", "immich-cache")
	if !m.RestoreStaged("hub") {
		t.Fatal("armed marker not found")
	}
	if got := m.stagedMarkers("hub"); len(got) != 1 || got[0].path() != dir {
		t.Fatalf("markers: %v", got)
	}
	if m.RestoreStaged("feed") {
		t.Fatal("another app's marker leaked across apps")
	}

	if err := disarmRestore(stagingLocOf(m, "hub", "immich-cache")); err != nil {
		t.Fatal(err)
	}
	if m.RestoreStaged("hub") {
		t.Fatal("still staged after disarm")
	}
}

// Interrupting must both cancel the restore's context and wait for it to
// return, so the disarm that follows isn't racing a restore still writing
// those same files.
func TestInterruptRestore(t *testing.T) {
	m := newTestManager(t)
	if m.interruptRestore(context.Background(), "hub") {
		t.Fatal("reported interrupting a restore that was never running")
	}

	ctx, op := m.beginRestore(context.Background(), "hub")
	returned := make(chan struct{})
	go func() {
		<-ctx.Done() // stands in for the wait on the app's boot guard
		m.endRestore("hub", op)
		close(returned)
	}()

	if !m.interruptRestore(context.Background(), "hub") {
		t.Fatal("in-flight restore was not interrupted")
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt returned before the restore let go")
	}
	if !op.cancelled.Load() {
		t.Fatal("cancelled flag not set — the restore would report a bare context error")
	}
	if m.interruptRestore(context.Background(), "hub") {
		t.Fatal("finished restore still registered as in flight")
	}
}

// The operator's restore decisions reach the app only through the marker its
// boot guard reads: forget_secrets must be there when chosen, and absent —
// not false-but-present noise an older app might trip on — when not.
func TestStageArtifactCarriesTheDecisions(t *testing.T) {
	m := newTestManager(t)
	spec := &apps.AppSpec{Volumes: []apps.Volume{{Name: "storage", Mount: "/rails/storage"}}}
	artifact := filepath.Join(t.TempDir(), "artifact.tar")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(m.Mat.DataPath("hook"), "storage", "fog", "restore", "pending.json")
	read := func() map[string]any {
		raw, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if err := m.StageArtifact("hook", spec, artifact, true, true, "fog-restore"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got["armed"] != true || got["confirm_identity"] != true || got["forget_secrets"] != true {
		t.Fatalf("marker = %v", got)
	}
	if err := m.StageArtifact("hook", spec, artifact, false, false, "fog-restore"); err != nil {
		t.Fatal(err)
	}
	if _, present := read()["forget_secrets"]; present {
		t.Fatalf("forget_secrets written when not chosen: %v", read())
	}
}

// The staging directory lives in a volume the app can write, so the app can
// plant a symlink where fog is about to write — as the fog/restore directory
// or in place of pending.tar — pointing at a host file outside its own
// volume. Staging must never follow it: root would otherwise overwrite that
// host file with app-chosen bytes. os.Root confines every write to the
// volume.
func TestStageArtifactRefusesSymlinkEscape(t *testing.T) {
	artifactBytes := []byte("artifact")
	newArtifact := func(t *testing.T) string {
		p := filepath.Join(t.TempDir(), "artifact.tar")
		if err := os.WriteFile(p, artifactBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	spec := &apps.AppSpec{Volumes: []apps.Volume{{Name: "storage", Mount: "/rails/storage"}}}

	// Case A: the whole fog/restore directory is a symlink out of the volume.
	t.Run("directory symlink", func(t *testing.T) {
		m := newTestManager(t)
		volRoot := filepath.Join(m.Mat.DataPath("hook"), "storage")
		if err := os.MkdirAll(volRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		secret := filepath.Join(outside, "secret")
		if err := os.WriteFile(secret, []byte("host state"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(volRoot, "fog")); err != nil {
			t.Fatal(err)
		}
		err := m.StageArtifact("hook", spec, newArtifact(t), true, false, "test")
		if err == nil {
			t.Fatal("staging through a directory symlink out of the volume must fail")
		}
		// The symlink target must be untouched: no pending.tar written into it.
		if _, err := os.Stat(filepath.Join(outside, "restore", "pending.tar")); !os.IsNotExist(err) {
			t.Fatalf("staging escaped the volume into %s", outside)
		}
	})

	// Case B: pending.tar itself is a symlink to a host file.
	t.Run("file symlink", func(t *testing.T) {
		m := newTestManager(t)
		dir := filepath.Join(m.Mat.DataPath("hook"), "storage", "fog", "restore")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "host-file")
		if err := os.WriteFile(outside, []byte("host state"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "pending.tar")); err != nil {
			t.Fatal(err)
		}
		if err := m.StageArtifact("hook", spec, newArtifact(t), true, false, "test"); err != nil {
			t.Fatalf("staging past a pre-planted file symlink should still succeed in-volume: %v", err)
		}
		// The host file keeps its content; the symlink was replaced by a real file.
		if b, _ := os.ReadFile(outside); string(b) != "host state" {
			t.Fatalf("root wrote through the symlink onto the host file: %q", b)
		}
		info, err := os.Lstat(filepath.Join(dir, "pending.tar"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("pending.tar must be a fresh regular file, got mode %v err %v", info.Mode(), err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "pending.tar")); string(b) != string(artifactBytes) {
			t.Fatalf("pending.tar content = %q", b)
		}
	})
}

// An app whose data lives in Drive stages its restore under its Drive
// folder. jen's hook did, and fog never saw it: no "Cancel restore", and a
// cancel that left the app booting into the same failing restore.
func TestRestoreStagedInADriveVolume(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app, err := st.CreateApp("hook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRevision(app.ID, `{"source":{"type":"image","image":"hook:latest"},"volumes":[{"name":"storage","mount":"/rails/storage","files":"Apps/Hook/storage"}]}`, "", "test"); err != nil {
		t.Fatal(err)
	}
	drive := t.TempDir()
	m := &Manager{Store: st, Mat: &apps.Materializer{DataDir: t.TempDir(), FilesRoot: func() string { return drive }}}
	spec, err := apps.LatestSpec(st, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "artifact.tar")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.StageArtifact("hook", spec, artifact, true, true, "test"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(drive, "Apps", "Hook", "storage", "fog", "restore")
	got := m.stagedMarkers("hook")
	if len(got) != 1 || got[0].path() != dir {
		t.Fatalf("markers: %v, want one in %s", got, dir)
	}
	if !m.RestoreStaged("hook") {
		t.Fatal("a restore staged in Drive reads as nothing staged")
	}

	if err := disarmRestore(got[0]); err != nil {
		t.Fatal(err)
	}
	if m.RestoreStaged("hook") {
		t.Fatal("still staged after disarm")
	}
}

// An app with nested volumes can rewrite, through its outer mount, the root
// of an inner volume — turning it into a symlink. Fog runs as root, so
// following that link would let the app have root write its bytes anywhere,
// or arm (and disarm) another app's restore. The volume root, and every
// directory above it below the trusted anchor, must be walked no-follow.
func TestStagingRefusesSymlinkedVolumeRoot(t *testing.T) {
	spec := &apps.AppSpec{Volumes: []apps.Volume{{Name: "storage", Mount: "/rails/storage"}}}
	artifact := filepath.Join(t.TempDir(), "artifact.tar")
	if err := os.WriteFile(artifact, []byte("APP-CONTROLLED"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("root links outside", func(t *testing.T) {
		m := newTestManager(t)
		outside := t.TempDir()
		if err := os.MkdirAll(m.Mat.DataPath("hook"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(m.Mat.DataPath("hook"), "storage")); err != nil {
			t.Fatal(err)
		}
		if err := m.StageArtifact("hook", spec, artifact, true, false, "test"); err == nil {
			t.Fatal("staging through a symlinked volume root must be refused")
		}
		if _, err := os.Stat(filepath.Join(outside, "fog", "restore", "pending.tar")); !os.IsNotExist(err) {
			t.Fatal("root wrote app-controlled bytes outside the volume")
		}
	})

	t.Run("root links into another app", func(t *testing.T) {
		m := newTestManager(t)
		victim := stage(t, m, "hub", "storage") // hub has a restore armed
		if err := os.MkdirAll(m.Mat.DataPath("hook"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(m.Mat.DataPath("hub"), "storage"), filepath.Join(m.Mat.DataPath("hook"), "storage")); err != nil {
			t.Fatal(err)
		}
		if err := m.StageArtifact("hook", spec, artifact, true, false, "test"); err == nil {
			t.Fatal("hook must not stage (arm) into hub's volume through a link")
		}
		if b, _ := os.ReadFile(filepath.Join(victim, "pending.tar")); string(b) != "artifact" {
			t.Fatalf("hub's staged artifact was overwritten: %q", b)
		}
		if got := m.stagedMarkers("hook"); len(got) != 0 {
			t.Fatalf("hook must not see (and so could not disarm) hub's marker through a link: %v", got)
		}
		if got := m.stagedMarkers("hub"); len(got) != 1 {
			t.Fatalf("hub's own marker must still read as staged: %v", got)
		}
	})

	t.Run("drive folder above the volume links elsewhere", func(t *testing.T) {
		drive := t.TempDir()
		m := &Manager{Mat: &apps.Materializer{DataDir: t.TempDir(), FilesRoot: func() string { return drive }}}
		other := filepath.Join(drive, "Apps", "Other", "storage")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(drive, "Apps", "Other"), filepath.Join(drive, "Apps", "Hook")); err != nil {
			t.Fatal(err)
		}
		dspec := &apps.AppSpec{Volumes: []apps.Volume{{Name: "storage", Mount: "/rails/storage", Files: "Apps/Hook/storage"}}}
		if err := m.StageArtifact("hook", dspec, artifact, true, false, "test"); err == nil {
			t.Fatal("staging through a linked Drive folder must be refused")
		}
		if _, err := os.Stat(filepath.Join(other, "fog")); !os.IsNotExist(err) {
			t.Fatal("staging landed in another app's Drive folder")
		}
	})

	t.Run("legacy snapshot refuses a linked volume", func(t *testing.T) {
		m := newTestManager(t)
		if err := os.MkdirAll(filepath.Join(m.Mat.DataPath("hub"), "storage"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(m.Mat.DataPath("hook"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.Symlink(filepath.Join(m.Mat.DataPath("hub"), "storage"), filepath.Join(m.Mat.DataPath("hook"), "storage"))
		loc, _ := m.volumeLoc("hook", spec.Volumes[0])
		if _, err := checkVolumeDir(loc); err == nil {
			t.Fatal("a volume-mode snapshot must not tar another app's volume through a link")
		}
	})
}
