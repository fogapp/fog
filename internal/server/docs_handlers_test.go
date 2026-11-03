package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/build"
)

func TestAppDocs(t *testing.T) {
	srv, h := newTestServer(t)
	if w := do(t, h, "GET", "/api/apps/manor/docs", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("docs without session: %d, want 401", w.Code)
	}
	cookie := setup(t, h)
	flareApp(t, srv, "manor")

	// No engine (image-only fogd in tests): an empty list, not an error.
	w := do(t, h, "GET", "/api/apps/manor/docs", "", withCookie(cookie))
	if w.Code != http.StatusOK || w.Body.String() != "[]\n" {
		t.Fatalf("docs without engine: %d %q", w.Code, w.Body.String())
	}
	if w := do(t, h, "GET", "/api/apps/nope/docs", "", withCookie(cookie)); w.Code != http.StatusNotFound {
		t.Fatalf("docs for unknown app: %d", w.Code)
	}

	b := &build.Builder{DataDir: t.TempDir()}
	srv.engine = &apps.Engine{Builder: b}
	repo := filepath.Join(b.DataDir, "builds", "manor", "repo")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Manor\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "CHANGELOG.md"), []byte("## 1.0\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "fog.yaml"), []byte("gettingStarted: hi\n"), 0o644)

	w = do(t, h, "GET", "/api/apps/manor/docs", "", withCookie(cookie))
	var docs []build.RepoDoc
	json.Unmarshal(w.Body.Bytes(), &docs)
	if w.Code != http.StatusOK || len(docs) != 2 || docs[0].File != "README.md" || docs[1].File != "CHANGELOG.md" {
		t.Fatalf("docs: %d %+v", w.Code, docs)
	}

	w = do(t, h, "GET", "/api/apps/manor/docs/README.md", "", withCookie(cookie))
	var doc build.RepoDocContent
	json.Unmarshal(w.Body.Bytes(), &doc)
	if w.Code != http.StatusOK || doc.Content != "# Manor\n" || doc.Truncated {
		t.Fatalf("readme: %d %+v", w.Code, doc)
	}
	for _, bad := range []string{"fog.yaml", "missing.md", "..%2FREADME.md"} {
		if w := do(t, h, "GET", "/api/apps/manor/docs/"+bad, "", withCookie(cookie)); w.Code != http.StatusNotFound {
			t.Errorf("doc %q: %d, want 404", bad, w.Code)
		}
	}
}
