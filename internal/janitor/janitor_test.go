package janitor

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/store"
)

// The scanner must flag exactly the directories no app accounts for: data
// and backups of deleted apps, volume subdirs dropped from a spec, build
// checkouts without a git app — and nothing belonging to live apps.
func TestOrphans(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Live app "keep": image source, one managed volume "config".
	app, err := st.CreateApp("keep")
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"source":{"type":"image","image":"x:1"},"volumes":[{"name":"config","mount":"/config"}]}`
	if _, err := st.CreateRevision(app.ID, spec, "", "t"); err != nil {
		t.Fatal(err)
	}

	mk := func(parts ...string) string {
		p := filepath.Join(parts...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "f"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk(dataDir, "data", "keep", "config")             // live volume — not an orphan
	gone := mk(dataDir, "data", "gone")               // deleted app's data
	dropped := mk(dataDir, "data", "keep", "olddisk") // volume no longer in spec
	checkout := mk(dataDir, "builds", "keep")         // keep is not a git app
	backupsRoot := filepath.Join(dataDir, "backups")
	stale := mk(backupsRoot, "gone")
	mk(backupsRoot, "keep") // live app's backups — not an orphan
	// fog's own trees beside the per-app ones — never orphans, though no app
	// carries their names.
	kits := mk(backupsRoot, backup.KitsDirName)
	snaps := mk(backupsRoot, backup.FilesDirName)

	j := &Janitor{
		Store:       st,
		DataDir:     dataDir,
		BackupsRoot: func() string { return backupsRoot },
		Log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	orphans, err := j.Orphans()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{gone: "data", dropped: "volume", checkout: "builds", stale: "backups"}
	if len(orphans) != len(want) {
		t.Fatalf("got %d orphans, want %d: %+v", len(orphans), len(want), orphans)
	}
	for _, o := range orphans {
		if want[o.Path] != o.Kind {
			t.Errorf("unexpected orphan %q kind %q", o.Path, o.Kind)
		}
		if o.SizeBytes <= 0 {
			t.Errorf("orphan %q has no size", o.Path)
		}
	}

	// Reclaim removes a reported path but refuses anything else.
	if err := j.Reclaim(filepath.Join(dataDir, "data", "keep", "config")); err == nil {
		t.Fatal("reclaim accepted a live path")
	}
	for _, p := range []string{kits, snaps} {
		if err := j.Reclaim(p); err == nil {
			t.Fatalf("reclaim accepted fog's own backups tree %s", p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s is gone: %v", p, err)
		}
	}
	if err := j.Reclaim(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatal("reclaimed path still exists")
	}
}

// The upload sweep removes only what the upload handler could have left
// behind: plain files directly under uploads/ past the age bound. Fresh
// files (an upload the operator is about to consume, or one still being
// streamed) and anything nested stay.
func TestSweepUploads(t *testing.T) {
	dataDir := t.TempDir()
	uploads := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(filepath.Join(uploads, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(parts ...string) string {
		p := filepath.Join(parts...)
		if err := os.WriteFile(p, []byte("tar"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := write(uploads, "artifact-1.tar")
	fresh := write(uploads, "artifact-2.tar")
	nested := write(uploads, "subdir", "artifact-3.tar")
	old := time.Now().Add(-2 * uploadMaxAge)
	for _, p := range []string{stale, nested} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	j := &Janitor{
		DataDir: dataDir,
		Log:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	j.sweepUploads()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale upload survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh upload was swept")
	}
	if _, err := os.Stat(nested); err != nil {
		t.Error("nested file was swept — the sweep must stay at the top level")
	}

	// A missing uploads dir is the common case on a box that never uploaded.
	j.DataDir = filepath.Join(dataDir, "empty")
	j.sweepUploads()
}
