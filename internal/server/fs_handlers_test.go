package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestFsListRequiresAuth(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)
	if w := do(t, h, "GET", "/api/system/fs?path=/", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated list: got %d, want 401", w.Code)
	}
}

func TestFsList(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/api/system/fs?path="+dir, "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var got fsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != dir {
		t.Errorf("path = %q, want %q", got.Path, dir)
	}
	if got.Parent != filepath.Dir(dir) {
		t.Errorf("parent = %q, want %q", got.Parent, filepath.Dir(dir))
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (%+v)", len(got.Entries), got.Entries)
	}
	// Directories sort before files regardless of name order.
	if !got.Entries[0].Dir || got.Entries[0].Name != "sub" {
		t.Errorf("first entry = %+v, want dir 'sub'", got.Entries[0])
	}
	if got.Entries[1].Dir || got.Entries[1].Name != "file.txt" || got.Entries[1].SizeBytes != 2 {
		t.Errorf("second entry = %+v, want 2-byte file 'file.txt'", got.Entries[1])
	}
	if got.Entries[0].Owner == "" {
		t.Error("entry owner missing")
	}
	if got.FreeBytes == 0 || got.TotalBytes == 0 {
		t.Error("disk usage missing")
	}
}

func TestFsListRejectsBadPaths(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	for _, p := range []string{"relative/path", filepath.Join(t.TempDir(), "missing")} {
		if w := do(t, h, "GET", "/api/system/fs?path="+p, "", withCookie(c)); w.Code != http.StatusBadRequest {
			t.Errorf("path %q: got %d, want 400", p, w.Code)
		}
	}
	// A file is not browsable.
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o644)
	if w := do(t, h, "GET", "/api/system/fs?path="+f, "", withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("file path: got %d, want 400", w.Code)
	}
}

func TestFsMkdir(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	target := filepath.Join(t.TempDir(), "a", "b")
	w := do(t, h, "POST", "/api/system/fs/mkdir", `{"path":"`+target+`"}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("mkdir: %d %s", w.Code, w.Body)
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.IsDir() {
		t.Fatalf("target not created: %v", err)
	}

	// Owner validation happens before any directory is created.
	bad := filepath.Join(t.TempDir(), "c")
	if w := do(t, h, "POST", "/api/system/fs/mkdir", `{"path":"`+bad+`","owner":"nobody"}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("bad owner: got %d, want 400", w.Code)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("dir was created despite invalid owner")
	}

	if w := do(t, h, "POST", "/api/system/fs/mkdir", `{"path":"relative"}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("relative path: got %d, want 400", w.Code)
	}
}
