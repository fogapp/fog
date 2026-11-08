package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"gitea.lan/matthew/fog/internal/jobs"
)

// The common case: an app's folder moves from one place in Drive to
// another. The bytes travel, the old path is gone, and the index follows —
// a listing of the destination shows what moved without waiting for a walk.
func TestMoveVolumeWithinDrive(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, AppsDir+"/Jellyfin/media/film.mkv", "reel")
	if _, err := m.List(AppsDir+"/Jellyfin", ListOpts{}); err != nil {
		t.Fatal(err)
	}

	from := filepath.Join(m.Root(), AppsDir, "Jellyfin", "media")
	if err := m.MoveVolume(context.Background(), from, ContentDir+"/Movies", jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Error("the old folder survived the move")
	}
	got, err := os.ReadFile(filepath.Join(m.Root(), ContentDir, "Movies", "film.mkv"))
	if err != nil || string(got) != "reel" {
		t.Fatalf("moved file: %q, %v", got, err)
	}
	l, err := m.List(ContentDir+"/Movies", ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 1 || l.Entries[0].Name != "film.mkv" {
		t.Fatalf("destination listing = %v", l.Entries)
	}
}

// A volume whose data still lives in fog's private directory — every app
// created before Drive held app data — moves in from outside the root.
func TestMoveVolumeFromOutsideDrive(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "db.sqlite"), []byte("rows"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.MoveVolume(context.Background(), outside, AppsDir+"/Kuma/data", jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(m.Root(), AppsDir, "Kuma", "data", "db.sqlite"))
	if err != nil || string(got) != "rows" {
		t.Fatalf("moved file: %q, %v", got, err)
	}
}

// Moving onto a folder that already holds a library would destroy it, so
// it is refused by name — the operator's other answer is to adopt what is
// there, which moves nothing.
func TestMoveVolumeRefusesNonEmptyDestination(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, AppsDir+"/Jellyfin/media/film.mkv", "reel")
	write(t, m, ContentDir+"/Movies/other.mkv", "theirs")

	from := filepath.Join(m.Root(), AppsDir, "Jellyfin", "media")
	err := m.MoveVolume(context.Background(), from, ContentDir+"/Movies", jobs.NewProgress())
	if !errors.Is(err, ErrDestNotEmpty) {
		t.Fatalf("move onto a full folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(from, "film.mkv")); err != nil {
		t.Error("the refused move touched the source")
	}
	if _, err := os.Stat(filepath.Join(m.Root(), ContentDir, "Movies", "other.mkv")); err != nil {
		t.Error("the refused move touched the destination")
	}
}

// A volume that was never deployed has nothing to carry; the folder is
// simply made, so the app finds it on its first deploy.
func TestMoveVolumeWithNothingToMove(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	missing := filepath.Join(t.TempDir(), "never-deployed")
	if err := m.MoveVolume(context.Background(), missing, ContentDir+"/Music", jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(m.Root(), ContentDir, "Music")); err != nil || !fi.IsDir() {
		t.Fatalf("destination folder: %v", err)
	}
}

// A volume move relocates one folder; the Drive root stays where it is, so
// the root handle other requests hold through it stays open.
func TestMoveVolumeKeepsTheSharedRootOpen(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, AppsDir+"/Jellyfin/media/film.mkv", "reel")
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(m.Root(), AppsDir, "Jellyfin", "media")
	if err := m.MoveVolume(context.Background(), from, ContentDir+"/Movies", jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat(ContentDir + "/Movies/film.mkv"); err != nil {
		t.Fatalf("a root held across the move: %v", err)
	}
}

// An app that can write beside the destination swaps its parent folder for
// a link to another app's folder while the operator's move runs. The move
// works by descriptor, so the folder lands where it was checked to go —
// never through the link — whether it is renamed or copied.
func TestMoveVolumeIgnoresALinkSwappedInMidMove(t *testing.T) {
	for _, crossDisk := range []bool{false, true} {
		m := newManager(t)
		write(t, m, AppsDir+"/Jellyfin/media/film.mkv", "reel")
		write(t, m, AppsDir+"/Victim/keep.db", "victim")
		write(t, m, ContentDir+"/New/.keep", "")
		root := m.Root()

		real := renameat
		t.Cleanup(func() { renameat = real })
		renameat = func(oldfd int, oldname string, newfd int, newname string) error {
			os.Rename(filepath.Join(root, ContentDir, "New"), filepath.Join(root, ContentDir, "New-moved"))
			if err := os.Symlink("../"+AppsDir+"/Victim", filepath.Join(root, ContentDir, "New")); err != nil {
				t.Fatal(err)
			}
			if crossDisk {
				return syscall.EXDEV
			}
			return real(oldfd, oldname, newfd, newname)
		}

		from := filepath.Join(root, AppsDir, "Jellyfin", "media")
		if err := m.MoveVolume(context.Background(), from, ContentDir+"/New/media", jobs.NewProgress()); err != nil {
			t.Fatalf("crossDisk=%v: %v", crossDisk, err)
		}
		if _, err := os.Lstat(filepath.Join(root, AppsDir, "Victim", "media")); err == nil {
			t.Fatalf("crossDisk=%v: the move went through the link into another app's folder", crossDisk)
		}
		if b, err := os.ReadFile(filepath.Join(root, ContentDir, "New-moved", "media", "film.mkv")); err != nil || string(b) != "reel" {
			t.Fatalf("crossDisk=%v: the folder did not land where it was checked to go: %q %v", crossDisk, b, err)
		}
		if _, err := os.Stat(from); !os.IsNotExist(err) {
			t.Errorf("crossDisk=%v: the source survived", crossDisk)
		}
	}
}

// A destination that is itself a link is refused outright.
func TestMoveVolumeRefusesALinkAsDestination(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, AppsDir+"/Jellyfin/media/film.mkv", "reel")
	write(t, m, AppsDir+"/Victim/keep.db", "victim")
	root := m.Root()
	os.MkdirAll(filepath.Join(root, ContentDir), 0o755)
	os.Symlink("../"+AppsDir+"/Victim", filepath.Join(root, ContentDir, "Movies"))
	err := m.MoveVolume(context.Background(), filepath.Join(root, AppsDir, "Jellyfin", "media"), ContentDir+"/Movies", jobs.NewProgress())
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("move onto a link: %v", err)
	}
}
