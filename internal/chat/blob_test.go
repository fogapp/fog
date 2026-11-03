package chat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// Blob names come from clients and peers; only a digest may name a file.
func TestBlobPathRejectsNonDigests(t *testing.T) {
	dir := t.TempDir()
	s := &Service{DataDir: filepath.Join(dir, "data")}
	// A file a traversal would reach, to prove it stays unreachable.
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := strings.Repeat("ab", 32)
	if p, err := s.blobPath(good); err != nil || p != filepath.Join(s.DataDir, "chat", "blobs", "ab", good) {
		t.Fatalf("digest: %q %v", p, err)
	}
	for _, bad := range []string{
		"", "a", "..", "../../../secret",
		"../../../" + strings.Repeat("a", 55),
		strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65),
	} {
		if _, err := s.blobPath(bad); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("blobPath(%q) = %v, want not found", bad, err)
		}
		if f, err := s.OpenBlobRaw(bad); err == nil {
			f.Close()
			t.Errorf("OpenBlobRaw(%q) opened a file", bad)
		}
	}
}
