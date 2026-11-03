package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mt := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

	for _, tc := range []struct {
		name string
		fs   writeFS
		dst  string
		path string
	}{
		{"root", root, "a.txt", filepath.Join(dir, "a.txt")},
		{"os", osFS{}, filepath.Join(dir, "b.txt"), filepath.Join(dir, "b.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sum, err := atomicWrite(tc.fs, tc.dst, strings.NewReader("hello"), writeOpts{perm: 0o600, mtime: mt, hash: true})
			if err != nil {
				t.Fatal(err)
			}
			if sum != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
				t.Fatalf("sum %s", sum)
			}
			fi, err := os.Stat(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if !fi.ModTime().Equal(mt) || fi.Mode().Perm() != 0o600 {
				t.Fatalf("mtime %v perm %v", fi.ModTime(), fi.Mode().Perm())
			}
		})
	}

	// A digest mismatch or a refused final check leaves nothing behind,
	// not even the staging file.
	if _, err := atomicWrite(root, "c.txt", strings.NewReader("x"), writeOpts{perm: 0o644, wantSHA: "00"}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	refused := errors.New("no")
	if _, err := atomicWrite(root, "d.txt", strings.NewReader("x"), writeOpts{perm: 0o644, beforeRename: func(string) error { return refused }}); !errors.Is(err, refused) {
		t.Fatalf("refused: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if n := e.Name(); n != "a.txt" && n != "b.txt" {
			t.Errorf("left behind: %s", n)
		}
	}
}
