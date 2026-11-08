package files

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A file moved aside to be checked before a replace is never lost to a
// crash: at the next start it goes back to its name, or beside it under a
// free name when its own is taken, and the uploads sweep never removes it
// however old it is.
func TestMovedAsideFileSurvivesACrash(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(m.Root(), filepath.FromSlash(uploadsDir))
	os.MkdirAll(stage, 0o700)
	crash := func(id, name, content string) {
		aside := filepath.Join(stage, id+".part.was")
		os.WriteFile(aside, []byte(content), 0o644)
		os.WriteFile(aside+".json", []byte(`{"path":"`+name+`"}`), 0o600)
		old := time.Now().Add(-48 * time.Hour)
		os.Chtimes(aside, old, old)
		os.Chtimes(aside+".json", old, old)
	}
	write(t, m, "Docs/.keep", "")
	crash("a", "Docs/free.txt", "mine")
	crash("b", "Docs/taken.txt", "the one that was there")
	write(t, m, "Docs/taken.txt", "landed meanwhile")

	// The sweep runs first, as if a day had passed: nothing moved aside goes.
	m.sweepUploads(root)
	m.restoreAside(root)

	if got, err := os.ReadFile(filepath.Join(m.Root(), "Docs", "free.txt")); err != nil || string(got) != "mine" {
		t.Fatalf("a moved-aside file was not put back: %q %v", got, err)
	}
	if got, _ := os.ReadFile(filepath.Join(m.Root(), "Docs", "taken.txt")); string(got) != "landed meanwhile" {
		t.Fatalf("putting back overwrote what took the name: %q", got)
	}
	found := false
	filepath.WalkDir(filepath.Join(m.Root(), "Docs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "taken") && d.Name() != "taken.txt" {
			b, _ := os.ReadFile(p)
			found = found || string(b) == "the one that was there"
		}
		return nil
	})
	if !found {
		t.Fatal("the file moved aside from a name now taken was not put beside it")
	}
	if ents, _ := os.ReadDir(stage); len(ents) != 0 {
		t.Fatalf("left in the staging folder: %v", ents)
	}
}

// An app that can write into a staging folder (Content/.fog, when it mounts
// Content) plants the receipt's name as a link to a file elsewhere in
// Drive. Moving a file aside used to write the receipt through the link,
// overwriting that file; now the link is removed as itself and the receipt
// written fresh where it is named.
func TestAsideReceiptIsNotWrittenThroughALink(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	write(t, m, "Private/secret.txt", "the operator's")
	write(t, m, "Docs/x.txt", "old")
	stage := filepath.Join(m.Root(), filepath.FromSlash(uploadsDir))
	os.MkdirAll(stage, 0o700)
	if err := os.WriteFile(filepath.Join(stage, "id.part"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../Private/secret.txt", filepath.Join(stage, "id.part.was.json")); err != nil {
		t.Fatal(err)
	}
	if err := placeOwned(root, uploadsDir+"/id.part", "Docs/x.txt", func(fs.FileInfo) bool { return true }); err != nil {
		t.Fatalf("place: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(m.Root(), "Private", "secret.txt")); string(got) != "the operator's" {
		t.Fatalf("the receipt was written through a planted link: secret.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(m.Root(), "Docs", "x.txt")); string(got) != "new" {
		t.Fatalf("x.txt = %q", got)
	}
}

// A receipt is read from where an app may have written it, so the path it
// names is held to the tree its staging folder serves: a planted file with
// a receipt naming fog's own reserved folder, a path out of the root, or
// (for Content's staging) a folder outside Content is left where it is.
func TestAsideReceiptCannotNameAnotherTree(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(m.Root(), filepath.FromSlash(uploadsDir))
	os.MkdirAll(stage, 0o700)
	for id, target := range map[string]string{"a": ".fog/uploads/evil", "b": "../outside", "c": "Docs/../.fog/x", "d": "/abs"} {
		os.WriteFile(filepath.Join(stage, id+".part.was"), []byte("planted"), 0o644)
		os.WriteFile(filepath.Join(stage, id+".part.was.json"), []byte(`{"path":"`+target+`"}`), 0o600)
	}
	m.restoreAside(root)
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := os.Lstat(filepath.Join(stage, id+".part.was")); err != nil {
			t.Errorf("receipt %s was acted on: %v", id, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(m.Root()), "outside")); err == nil {
		t.Fatal("a moved-aside file went outside the root")
	}
	// Content's staging folder serves Content alone.
	for rel, want := range map[string]bool{"Content/a.txt": true, "Content/sub/b": true, "Content": false, "Private/secret.txt": false, "Contents/x": false} {
		if got := m.asideTargetOK(rel, "Content"); got != want {
			t.Errorf("asideTargetOK(%q, Content) = %v, want %v", rel, got, want)
		}
	}
}

// An upload abandoned for longer than uploadMaxAge is swept. The sweep
// stat'ed entries through a path built from a descriptor's name, which on
// Linux is not a path from anywhere: every stat failed and nothing was
// ever swept. A moved-aside file beside it is never touched.
func TestAbandonedUploadIsSwept(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(m.Root(), filepath.FromSlash(uploadsDir))
	os.MkdirAll(stage, 0o700)
	old := time.Now().Add(-uploadMaxAge - time.Hour)
	for _, n := range []string{"0123456789abcdef.part", "0123456789abcdef.part.was"} {
		p := filepath.Join(stage, n)
		os.WriteFile(p, []byte("x"), 0o600)
		os.Chtimes(p, old, old)
	}
	m.sweepUploads(root)
	if _, err := os.Lstat(filepath.Join(stage, "0123456789abcdef.part")); !os.IsNotExist(err) {
		t.Fatalf("abandoned upload not swept: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stage, "0123456789abcdef.part.was")); err != nil {
		t.Fatalf("a moved-aside file was swept: %v", err)
	}
}
