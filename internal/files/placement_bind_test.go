package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real thing: bind mounts through mount(8), with Apps and Content on
// filesystems of their own so every rename that could cross one does.
// Needs root and a Linux mount namespace, so it runs only when asked:
//
//	docker run --rm --privileged -v "$PWD":/src -w /src golang:1.26 sh -c '
//	  mkdir -p /mnt/fast /mnt/big && mount -t tmpfs tmpfs /mnt/fast && mount -t tmpfs tmpfs /mnt/big &&
//	  FOG_TEST_BIND=1 go test ./internal/files -run Bind -v'
func TestPlacementRealBind(t *testing.T) {
	t.Parallel()

	if os.Getenv("FOG_TEST_BIND") == "" {
		t.Skip("set FOG_TEST_BIND=1 (root, Linux, /mnt/fast and /mnt/big on their own filesystems)")
	}
	m := newManager(t)
	ctx := context.Background()
	write(t, m, "Apps/Sonarr/config.xml", "<config/>")
	write(t, m, "notes.txt", "hi")
	m.List("", ListOpts{})

	fast := filepath.Join("/mnt/fast", filepath.Base(t.TempDir()))
	big := filepath.Join("/mnt/big", filepath.Base(t.TempDir()))
	os.MkdirAll(filepath.Join(big, "Movies"), 0o755)
	os.WriteFile(filepath.Join(big, "Movies", "film.mkv"), []byte("film"), 0o644)
	appsMP, contentMP := filepath.Join(m.Root(), AppsDir), filepath.Join(m.Root(), ContentDir)
	t.Cleanup(func() {
		unbindMount(ctx, appsMP)
		unbindMount(ctx, contentMP)
	})

	job, err := m.MovePlacement(AppsDir, fast, "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("move Apps: %+v", j)
	}
	// Content is adopted: the library is on the big disk already.
	os.RemoveAll(contentMP)
	job, err = m.MovePlacement(ContentDir, big, "adopt")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("adopt Content: %+v", j)
	}
	if !sameDir(fast, appsMP) || !sameDir(big, contentMP) {
		t.Fatal("not bound")
	}
	if b, _ := os.ReadFile(filepath.Join(fast, "Sonarr", "config.xml")); string(b) != "<config/>" {
		t.Errorf("Apps on the fast disk: %q", b)
	}
	if n, err := m.Stat("Content/Movies/film.mkv"); err != nil || n.Size != 4 {
		t.Errorf("Drive sees the adopted film: %+v %v", n, err)
	}

	// Every verb that renames, on and between the three filesystems.
	items, err := m.Trash([]string{"Content/Movies/film.mkv", "Apps/Sonarr/config.xml"})
	if err != nil {
		t.Fatalf("trash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(big, ReservedDir, "trash", items[0].ID, "film.mkv")); err != nil {
		t.Errorf("film is not in the big disk's trash: %v", err)
	}
	if _, err := m.RestoreTrash([]string{items[0].ID, items[1].ID}); err != nil {
		t.Errorf("restore: %v", err)
	}
	up, err := m.BeginUpload("Content/Movies/new.mkv", 3, "", "", ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	m.AppendUpload(up.ID, 0, strings.NewReader("abc"))
	if _, err := m.CompleteUpload(up.ID); err != nil {
		t.Errorf("upload into Content: %v", err)
	}
	if _, err := m.Materialize("Apps/Sonarr/peer.txt", strings.NewReader("p"), time.Now(), ""); err != nil {
		t.Errorf("materialize into Apps: %v", err)
	}
	res, err := m.Move([]string{"notes.txt"}, "Content", ConflictAsk)
	if err != nil {
		t.Fatalf("move across disks: %v", err)
	}
	if res.Job != nil {
		if j := waitJob(t, m, res.Job.ID); j.State != "done" {
			t.Errorf("cross-disk move job: %+v", j)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(big, "notes.txt")); string(b) != "hi" {
		t.Errorf("notes on the big disk: %q", b)
	}
	if _, err := m.Trash([]string{ContentDir}); err == nil {
		t.Error("trashed the placed Content folder itself")
	}

	// And Apps back into the root: the bind comes down, the bytes return.
	job, err = m.MovePlacement(AppsDir, "", "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("move Apps back: %+v", j)
	}
	if mountedPoints()[appsMP] {
		t.Error("Apps still mounted")
	}
	if b, _ := os.ReadFile(filepath.Join(appsMP, "Sonarr", "peer.txt")); string(b) != "p" {
		t.Errorf("Apps back in the root: %q", b)
	}
}
