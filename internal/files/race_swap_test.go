package files

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// An app flips Apps/Evil/sub between a real folder and a link to
// ../../Private as fast as it can while the operator works on what they
// see in sub. Every verb acts by descriptor, so whichever way a race goes
// nothing ever crosses into or out of Private: its file is never moved,
// renamed, trashed or replaced, and nothing lands beside it.
func TestNoVerbCrossesIntoALinkFlippedMidOperation(t *testing.T) {
	m := newManager(t)
	root := m.Root()
	write(t, m, "Private/x", "secret")
	write(t, m, "Docs/c.txt", "copy me")
	evil := filepath.Join(root, "Apps", "Evil")
	os.MkdirAll(filepath.Join(evil, "sub"), 0o755)
	os.MkdirAll(filepath.Join(evil, "out"), 0o755)
	if err := os.Symlink("../../Private", filepath.Join(evil, "sub.link")); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex // held by the flipper while sub is between states
	var stop atomic.Bool
	var flips atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub, real, link := filepath.Join(evil, "sub"), filepath.Join(evil, "sub.real"), filepath.Join(evil, "sub.link")
		for !stop.Load() {
			mu.Lock()
			os.Rename(sub, real)
			os.Rename(link, sub)
			mu.Unlock()
			mu.Lock()
			os.Rename(sub, link)
			os.Rename(real, sub)
			mu.Unlock()
			flips.Add(1)
		}
	}()
	defer func() { stop.Store(true); <-done }()

	// realSub is where the real folder is right now (under mu).
	realSub := func() string {
		if fi, err := os.Lstat(filepath.Join(evil, "sub")); err == nil && fi.IsDir() {
			return filepath.Join(evil, "sub")
		}
		return filepath.Join(evil, "sub.real")
	}
	plant := func(name string) {
		mu.Lock()
		os.WriteFile(filepath.Join(realSub(), name), []byte("decoy"), 0o644)
		mu.Unlock()
	}
	check := func(verb string) {
		t.Helper()
		if b, err := os.ReadFile(filepath.Join(root, "Private", "x")); err != nil || string(b) != "secret" {
			t.Fatalf("%s: Private/x was taken or changed: %q %v", verb, b, err)
		}
		ents, _ := os.ReadDir(filepath.Join(root, "Private"))
		if len(ents) != 1 {
			var names []string
			for _, e := range ents {
				names = append(names, e.Name())
			}
			t.Fatalf("%s: something crossed into Private: %v", verb, names)
		}
		mu.Lock() // the scan itself must not wander through the link
		defer mu.Unlock()
		for _, d := range []string{filepath.Join(evil, "out"), realSub()} {
			ents, _ := os.ReadDir(d)
			for _, e := range ents {
				if b, err := os.ReadFile(filepath.Join(d, e.Name())); err == nil && string(b) == "secret" {
					t.Fatalf("%s: Private/x crossed into the app's folder as %s", verb, filepath.Join(d, e.Name()))
				}
			}
		}
	}

	const rounds = 150
	verbs := []struct {
		name string
		run  func(i int)
	}{
		{"rename", func(int) { plant("x"); m.Rename("Apps/Evil/sub/x", "y") }},
		{"move", func(int) { plant("x"); m.Move([]string{"Apps/Evil/sub/x"}, "Apps/Evil/out", ConflictRename) }},
		{"dav move", func(int) { plant("x"); m.renameAny("Apps/Evil/sub/x", "Apps/Evil/out/z") }},
		{"trash", func(int) { plant("x"); m.Trash([]string{"Apps/Evil/sub/x"}) }},
		{"mkdir", func(i int) { m.Mkdir("Apps/Evil/sub/d" + strings.Repeat("d", i%3)) }},
		{"upload", func(int) {
			u, err := m.BeginUpload("Apps/Evil/sub/up.txt", 2, "", "", ConflictRename)
			if err == nil {
				m.AppendUpload(u.ID, 0, strings.NewReader("hi"))
				m.CompleteUpload(u.ID)
			}
		}},
		{"copy", func(int) {
			if job, err := m.Copy([]string{"Docs/c.txt"}, "Apps/Evil/sub", ConflictRename); err == nil {
				waitJob(t, m, job.ID)
			}
		}},
		{"restore", func(int) {
			plant("r")
			items, err := m.Trash([]string{"Apps/Evil/sub/r"})
			if err == nil && len(items) == 1 {
				m.RestoreTrash([]string{items[0].ID})
			}
		}},
	}
	for _, v := range verbs {
		for i := range rounds {
			v.run(i)
			check(v.name)
		}
	}
	if flips.Load() == 0 {
		t.Fatal("the folder never flipped")
	}
}
