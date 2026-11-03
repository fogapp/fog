package apps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// groupOf answers a file's gid, without following a final symlink.
func groupOf(t *testing.T, p string) int {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return int(st.Sys().(*syscall.Stat_t).Gid)
}

// twoGroups answers two groups this user may chown files to, or skips: the
// attack is told by a gid that changes where it mustn't, and an unprivileged
// test can only move files between its own groups.
func twoGroups(t *testing.T) (primary, other int) {
	t.Helper()
	primary = os.Getgid()
	gs, _ := os.Getgroups()
	for _, g := range gs {
		if g != primary {
			probe := filepath.Join(t.TempDir(), "probe")
			os.WriteFile(probe, nil, 0o644)
			if os.Chown(probe, -1, g) == nil {
				return primary, g
			}
		}
	}
	t.Skip("this user has one group; can't observe a chown")
	return 0, 0
}

// The attack: an app plants, inside its own managed volume, a link to a
// file of the host's (and a directory link to a host directory). fogd's
// recursive chown, running as root, must re-own the links themselves and
// never what they name.
func TestEnsureVolumeOwnerNeverFollowsLinksOutOfTheVolume(t *testing.T) {
	primary, other := twoGroups(t)
	host := t.TempDir()
	secret := filepath.Join(host, "shadow")
	os.WriteFile(secret, []byte("root:x"), 0o600)
	hostDir := filepath.Join(host, "etc")
	os.Mkdir(hostDir, 0o755)
	inside := filepath.Join(hostDir, "passwd")
	os.WriteFile(inside, nil, 0o644)
	for _, p := range []string{secret, hostDir, inside} {
		if err := os.Chown(p, -1, other); err != nil {
			t.Fatal(err)
		}
	}

	vol := t.TempDir()
	os.MkdirAll(filepath.Join(vol, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(vol, "a", "b", "own"), nil, 0o644)
	os.Symlink(secret, filepath.Join(vol, "a", "link"))
	os.Symlink(hostDir, filepath.Join(vol, "dirlink"))
	for _, p := range []string{vol, filepath.Join(vol, "a"), filepath.Join(vol, "a", "b", "own")} {
		os.Chown(p, -1, other)
	}

	owner := fmt.Sprintf("%d:%d", os.Getuid(), primary)
	if err := ensureVolumeOwner(vol, owner, false, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{secret, hostDir, inside} {
		if g := groupOf(t, p); g != other {
			t.Errorf("%s was re-owned through a link in the volume (gid %d)", p, g)
		}
	}
	// The volume's own files were re-owned, links included.
	for _, p := range []string{vol, filepath.Join(vol, "a", "b", "own"), filepath.Join(vol, "a", "link"), filepath.Join(vol, "dirlink")} {
		if g := groupOf(t, p); g != primary {
			t.Errorf("%s not re-owned (gid %d)", p, g)
		}
	}
}

// A host path that is itself a symlink — one another app sharing its parent
// swapped in, pointing at /etc — is refused, not chowned through.
func TestEnsureVolumeOwnerRefusesASymlinkedHostPath(t *testing.T) {
	primary, other := twoGroups(t)
	target := t.TempDir()
	os.Chown(target, -1, other)
	link := filepath.Join(t.TempDir(), "share")
	os.Symlink(target, link)
	err := ensureVolumeOwner(link, fmt.Sprintf("%d:%d", os.Getuid(), primary), false, false)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if g := groupOf(t, target); g != other {
		t.Errorf("the link's target was re-owned (gid %d)", g)
	}
	// And Render's path for a host volume with an owner: ensureHostDir
	// reports the link as existing, and the chown then refuses it.
	if created, err := ensureHostDir(link); err != nil || created {
		t.Fatalf("ensureHostDir = %v, %v", created, err)
	}
}

// The attack on Drive: an app with a Drive volume replaces a folder with a
// link to a host directory, then a deploy asks for a Drive folder below it.
// fogd must not create directories outside Drive.
func TestMkdirInRootStaysInDrive(t *testing.T) {
	drive := t.TempDir()
	host := t.TempDir()
	os.MkdirAll(filepath.Join(drive, "Content"), 0o755)
	os.Symlink(host, filepath.Join(drive, "Content", "Movies"))
	if err := mkdirInRoot(drive, filepath.Join("Content", "Movies", "new")); err == nil {
		t.Error("created a folder through a link out of Drive")
	}
	if _, err := os.Stat(filepath.Join(host, "new")); err == nil {
		t.Fatal("a directory was created outside Drive")
	}
	if err := mkdirInRoot(drive, filepath.Join("Content", "Books", "x")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(drive, "Content", "Books", "x")); err != nil || !st.IsDir() {
		t.Fatal("an ordinary Drive folder wasn't created")
	}
}
