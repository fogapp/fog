package files

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A listing that finds the folder as it left it writes nothing: no
// transaction, no writer lock. Only a change on disk costs a write.
func TestQuietListingWritesNothing(t *testing.T) {
	m := newManager(t)
	write(t, m, "docs/readme.md", "hello")
	write(t, m, "docs/deep/notes.txt", "notes")
	write(t, m, "photo.jpg", "not really")
	ix, err := m.Index()
	if err != nil {
		t.Fatal(err)
	}
	visit := func() {
		t.Helper()
		for _, dir := range []string{"", "docs", "docs/deep"} {
			if _, err := m.List(dir, ListOpts{}); err != nil {
				t.Fatal(err)
			}
		}
		for _, rel := range []string{"photo.jpg", "docs", "docs/readme.md"} {
			if _, err := m.Stat(rel); err != nil {
				t.Fatal(err)
			}
		}
	}
	visit() // first sight: the rows are written
	if ix.writes.Load() == 0 {
		t.Fatal("the first listing wrote nothing; the seam is not counting")
	}
	before := ix.writes.Load()
	visit()
	if got := ix.writes.Load() - before; got != 0 {
		t.Fatalf("listing an unchanged tree wrote %d transactions", got)
	}
	// After a walk (which stamps walked_at) a listing is still quiet.
	if err := m.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	before = ix.writes.Load()
	visit()
	if got := ix.writes.Load() - before; got != 0 {
		t.Fatalf("listing after a walk wrote %d transactions", got)
	}
	// A change on disk is still picked up.
	write(t, m, "docs/readme.md", "hello, longer")
	l, err := m.List("docs", ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if ix.writes.Load() == before {
		t.Error("a rewritten file was not written to the index")
	}
	for _, e := range l.Entries {
		if e.Name == "readme.md" && e.Size != 13 {
			t.Errorf("readme size = %d, want 13", e.Size)
		}
	}
}

// Stat of a file reads that one file: its row follows the disk and the
// folders above it are resized by the difference, without the parent's
// other entries being re-read.
func TestStatRefreshesJustTheFile(t *testing.T) {
	m := newManager(t)
	write(t, m, "docs/a.txt", "aaaa")
	write(t, m, "docs/b.txt", "bb")
	if _, err := m.List("", ListOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.List("docs", ListOpts{}); err != nil {
		t.Fatal(err)
	}
	ix, _ := m.Index()
	// Behind fog's back: a.txt grows, and a new file appears beside it.
	write(t, m, "docs/a.txt", "aaaaaaaaaa")
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(m.Root(), "docs", "a.txt"), later, later)
	write(t, m, "docs/c.txt", "c")
	var changed []string
	m.OnChange = func(rel string) { changed = append(changed, rel) }

	d, err := m.Stat("docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if d.Size != 10 || d.ModifiedNanos() != later.UnixNano() {
		t.Errorf("stat = size %d mtime %d, want 10 %d", d.Size, d.ModifiedNanos(), later.UnixNano())
	}
	if _, err := ix.Get("docs/c.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat of one file re-read its folder: c.txt row %v", err)
	}
	for _, dir := range []string{"docs", ""} {
		if n, _ := ix.Get(dir); n.Size != 12 {
			t.Errorf("%q size = %d, want 12 after the bump", dir, n.Size)
		}
	}
	if len(changed) != 1 || changed[0] != "docs" {
		t.Errorf("OnChange = %v, want [docs]", changed)
	}
	// Unchanged since: no write.
	before := ix.writes.Load()
	if _, err := m.Stat("docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	if ix.writes.Load() != before {
		t.Error("stat of an unchanged file wrote")
	}
	// Gone from disk: the parent refresh drops it.
	os.Remove(filepath.Join(m.Root(), "docs", "b.txt"))
	if _, err := m.Stat("docs/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat of a removed file: %v", err)
	}
	// Never seen: the parent refresh adds it.
	if d, err := m.Stat("docs/c.txt"); err != nil || d.Size != 1 {
		t.Errorf("stat of a file the index had not seen: %+v %v", d.Node, err)
	}
}

// A rename or move carries the index rows across before the folders are
// re-read, so the re-read finds nothing new; OnChange still has to hear
// about both folders, or a shared folder that gained or lost an entry is
// never marked dirty.
func TestRenameAndMoveFireOnChange(t *testing.T) {
	m := newManager(t)
	write(t, m, "docs/a.txt", "aaaa")
	write(t, m, "docs/b.txt", "bb")
	write(t, m, "docs/c.txt", "c")
	write(t, m, "other/keep.txt", "k")
	for _, dir := range []string{"", "docs", "other"} {
		if _, err := m.List(dir, ListOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	var changed []string
	m.OnChange = func(rel string) { changed = append(changed, rel) }
	saw := func(verb string, want ...string) {
		t.Helper()
		slices.Sort(changed)
		changed = slices.Compact(changed)
		if !slices.Equal(changed, want) {
			t.Errorf("%s: OnChange = %v, want %v", verb, changed, want)
		}
		changed = nil
	}

	if _, err := m.Rename("docs/a.txt", "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	saw("rename", "docs")
	if _, err := m.Move([]string{"docs/b.txt"}, "other", ConflictAsk); err != nil {
		t.Fatal(err)
	}
	saw("move", "docs", "other")
	if err := m.renameAny("docs/c.txt", "other/c.txt"); err != nil {
		t.Fatal(err)
	}
	saw("WebDAV rename", "docs", "other")
}
