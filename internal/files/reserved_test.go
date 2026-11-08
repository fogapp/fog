package files

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reserved directory fog can still take back is taken back, silently: the
// operator gets their delete, not a lecture about permissions.
func TestReservedDirIsRepairedInPlace(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	root := m.Root()
	res := filepath.Join(root, ReservedDir)
	if err := m.ensureReserved(root); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The shape a stranded directory has from fog's side: present, listed by
	// its parent, and impossible to enter.
	if err := os.Chmod(res, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := probeWrite(filepath.Join(res, "trash")); !errors.Is(err, fs.ErrPermission) {
		t.Skipf("cannot stage an unenterable directory here (err = %v)", err)
	}
	if err := m.ensureReserved(root); err != nil {
		t.Fatalf("fog owns this directory and should have taken it back: %v", err)
	}
	if err := probeWrite(filepath.Join(res, "trash")); err != nil {
		t.Errorf("trash still unusable after repair: %v", err)
	}
	fi, err := os.Stat(res)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("reserved dir mode = %v, want 0700 — it stays fog's own", fi.Mode().Perm())
	}
}

// Deleting a file is the operation an operator reaches this through, so the
// repair has to happen underneath it rather than being reported.
func TestTrashRecoversFromAStrandedReservedDir(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "notes.txt", "hello")
	if _, err := m.Trash([]string{"notes.txt"}); err != nil {
		t.Fatalf("baseline delete: %v", err)
	}
	res := filepath.Join(m.Root(), ReservedDir)
	write(t, m, "again.txt", "hello")
	if err := os.Chmod(res, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(res, 0o700) })
	if _, err := m.Trash([]string{"again.txt"}); err != nil {
		t.Fatalf("delete should repair the reserved directory and proceed: %v", err)
	}
}

// When fog cannot take the directory back, the error is the one an operator
// can act on: who owns it, who fog is here, and what to change.
func TestReservedRefusedNamesTheIdentities(t *testing.T) {
	t.Parallel()

	cause := fs.ErrPermission
	err := reservedRefused("/mnt/nas/files/.fog", cause, 0, 1024, true)
	if !errors.Is(err, ErrNotConfigured) {
		t.Error("a stranded reserved directory is a configuration problem, not a request problem")
	}
	for _, want := range []string{"/mnt/nas/files/.fog", "uid 0", "uid 1024", "ownership"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q: %s", want, err)
		}
	}
	// Same uid on both sides: nothing to explain, and inventing a mismatch
	// would send the operator after a setting that is not the problem.
	plain := reservedRefused("/srv/files/.fog", cause, 0, 0, true)
	if strings.Contains(plain.Error(), "belongs to uid") {
		t.Errorf("no mismatch to name, yet: %s", plain)
	}
	unknown := reservedRefused("/srv/files/.fog", cause, 0, 1024, false)
	if strings.Contains(unknown.Error(), "belongs to uid") {
		t.Errorf("identities unknown, so nothing to assert: %s", unknown)
	}
}
