package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDocs(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"README.md", "CHANGELOG.md", "api.markdown", "notes.txt", "Dockerfile", ".hidden.md", "zebra.MD"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("# "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "docs.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "LINK.md")); err != nil {
		t.Fatal(err)
	}
	got := listDocs(dir)
	want := []string{"README.md", "api.markdown", "CHANGELOG.md", "zebra.MD"}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].File != w {
			t.Errorf("doc %d = %q, want %q (all: %+v)", i, got[i].File, w, got)
		}
		if got[i].Size == 0 {
			t.Errorf("doc %s has no size", w)
		}
	}
}

// An app with no checkout has no docs — and no error either.
func TestListDocsMissingDir(t *testing.T) {
	if got := listDocs(filepath.Join(t.TempDir(), "nope")); got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want empty non-nil", got)
	}
}

func TestReadDoc(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hello\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644)
	os.WriteFile(filepath.Join(dir, "big.md"), []byte(strings.Repeat("x", maxDocBytes+10)), 0o644)
	os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(dir, "link.md"))
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "inner.md"), []byte("inner"), 0o644)

	doc, err := readDoc(dir, "README.md")
	if err != nil || doc.Content != "# hello\n" || doc.Truncated || doc.File != "README.md" {
		t.Fatalf("readme: %+v %v", doc, err)
	}
	big, err := readDoc(dir, "big.md")
	if err != nil || !big.Truncated || len(big.Content) != maxDocBytes {
		t.Fatalf("big: truncated=%v len=%d err=%v", big.Truncated, len(big.Content), err)
	}
	for _, bad := range []string{"secret.txt", "link.md", "sub/inner.md", "../README.md", "missing.md", "", ".hidden.md", "sub"} {
		if _, err := readDoc(dir, bad); err != ErrNoDoc {
			t.Errorf("readDoc(%q) err = %v, want ErrNoDoc", bad, err)
		}
	}
}

func TestBuilderRepoDocs(t *testing.T) {
	b := &Builder{DataDir: t.TempDir()}
	repo := filepath.Join(b.appDir("feed"), "repo")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0o644)
	if docs := b.RepoDocs("feed"); len(docs) != 1 || docs[0].File != "README.md" {
		t.Fatalf("docs: %+v", docs)
	}
	if docs := b.RepoDocs("other"); len(docs) != 0 {
		t.Fatalf("other app docs: %+v", docs)
	}
	if doc, err := b.ReadRepoDoc("feed", "README.md"); err != nil || doc.Content != "hi" {
		t.Fatalf("read: %+v %v", doc, err)
	}
}
