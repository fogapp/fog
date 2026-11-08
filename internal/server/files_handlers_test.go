package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
)

func TestFilesRequireAuth(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	setup(t, h)
	for _, p := range []string{"/api/files/ls", "/api/files/status", "/api/files/raw/x", "/api/files/zip?path=x"} {
		if w := do(t, h, "GET", p, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated: got %d, want 401", p, w.Code)
		}
	}
}

func TestFilesListAndRaw(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	c := setup(t, h)
	root := s.files.Root()
	os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("hello world"), 0o644)
	os.WriteFile(filepath.Join(root, "page.html"), []byte("<script>alert(1)</script>"), 0o644)

	w := do(t, h, "GET", "/api/files/ls?path=/", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("ls: %d %s", w.Code, w.Body)
	}
	var l files.Listing
	json.Unmarshal(w.Body.Bytes(), &l)
	if !l.Root || len(l.Entries) != 2 || l.Entries[0].Name != "docs" {
		t.Fatalf("listing = %+v", l)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on listing")
	}
	w = do(t, h, "GET", "/api/files/ls?path=/", "", withCookie(c), func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
	if w.Code != http.StatusNotModified {
		t.Errorf("unchanged listing: got %d, want 304", w.Code)
	}

	// Range requests work, and the response is sandboxed with a declared type.
	w = do(t, h, "GET", "/api/files/raw/docs/a.txt", "", withCookie(c), func(r *http.Request) { r.Header.Set("Range", "bytes=6-10") })
	if w.Code != http.StatusPartialContent || w.Body.String() != "world" {
		t.Errorf("range: %d %q", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/api/files/raw/page.html", "", withCookie(c))
	if w.Header().Get("Content-Security-Policy") != "sandbox" || w.Header().Get("Content-Type") != "text/html" {
		t.Errorf("html headers = %v", w.Header())
	}
	w = do(t, h, "GET", "/api/files/raw/docs/a.txt?dl=1", "", withCookie(c))
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename=a.txt` {
		t.Errorf("disposition = %q", cd)
	}

	if w := do(t, h, "GET", "/api/files/raw/nope.txt", "", withCookie(c)); w.Code != http.StatusNotFound {
		t.Errorf("missing file: %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/files/ls?path=../", "", withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("escape: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/files/ls?path=.fog", "", withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("reserved: %d", w.Code)
	}
	w = do(t, h, "GET", "/api/files/zip?path=docs", "", withCookie(c))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/zip" || w.Body.Len() == 0 {
		t.Errorf("zip: %d %v", w.Code, w.Header())
	}
}

// The root setting is validated for real, and refuses to silently
// abandon a populated tree.
func TestFilesRootSetting(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	c := setup(t, h)
	// Relative and inside-fog paths are rejected.
	for _, bad := range []string{"relative/path", filepath.Join(s.cfg.DataDir, "data", "x")} {
		body := `{"files_root":` + jsonString(bad) + `}`
		if w := do(t, h, "PUT", "/api/settings", body, withCookie(c)); w.Code != http.StatusBadRequest {
			t.Errorf("root %q: got %d %s", bad, w.Code, w.Body)
		}
	}
	// An empty current root moves freely.
	newRoot := filepath.Join(t.TempDir(), "files")
	w := do(t, h, "PUT", "/api/settings", `{"files_root":`+jsonString(newRoot)+`}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("set root: %d %s", w.Code, w.Body)
	}
	if got := s.files.Root(); got != newRoot {
		t.Errorf("root = %s", got)
	}
	// With content it is a conflict pointing at the move job.
	os.WriteFile(filepath.Join(newRoot, "keep.txt"), []byte("x"), 0o644)
	w = do(t, h, "PUT", "/api/settings", `{"files_root":`+jsonString(filepath.Join(t.TempDir(), "other"))+`}`, withCookie(c))
	if w.Code != http.StatusConflict {
		t.Errorf("populated root change: got %d %s", w.Code, w.Body)
	}
	var st files.Status
	w = do(t, h, "GET", "/api/files/status", "", withCookie(c))
	json.Unmarshal(w.Body.Bytes(), &st)
	if !st.OK || !st.Configured || st.Root != newRoot || st.Owner != files.DefaultOwner {
		t.Errorf("status = %+v", st)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestFilesVerbs(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	c := setup(t, h)
	root := s.files.Root()
	os.MkdirAll(filepath.Join(root, "a"), 0o755)
	os.WriteFile(filepath.Join(root, "a", "x.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "y.txt"), []byte("yy"), 0o644)

	if w := do(t, h, "POST", "/api/files/mkdir", `{"path":"b"}`, withCookie(c)); w.Code != http.StatusCreated {
		t.Fatalf("mkdir: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/files/mkdir", `{"path":".fog/evil"}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("mkdir reserved: %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/files/rename", `{"path":"y.txt","name":"z.txt"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	// A move onto a taken name is a 409 that names the entry in the way.
	os.WriteFile(filepath.Join(root, "b", "x.txt"), []byte("other"), 0o644)
	w := do(t, h, "POST", "/api/files/move", `{"paths":["a/x.txt"],"to":"b"}`, withCookie(c))
	if w.Code != http.StatusConflict {
		t.Fatalf("move conflict: %d %s", w.Code, w.Body)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if conflict, _ := body["conflict"].(map[string]any); conflict["path"] != "b/x.txt" {
		t.Errorf("conflict body = %v", body)
	}
	w = do(t, h, "POST", "/api/files/move", `{"paths":["a/x.txt"],"to":"b","onConflict":"rename"}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("move rename: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(root, "b", "x 2.txt")); err != nil {
		t.Error("renamed move did not land")
	}
	if w := do(t, h, "POST", "/api/files/trash", `{"paths":["/"]}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("trash root: %d", w.Code)
	}
	w = do(t, h, "POST", "/api/files/trash", `{"paths":["b"]}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("trash: %d %s", w.Code, w.Body)
	}
	var tr struct {
		Items []files.TrashItem `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &tr)
	if len(tr.Items) != 1 || tr.Items[0].Path != "b" {
		t.Fatalf("trash items = %+v", tr.Items)
	}
	w = do(t, h, "POST", "/api/files/trash/restore", `{"ids":["`+tr.Items[0].ID+`"]}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(root, "b", "x 2.txt")); err != nil {
		t.Error("restore did not bring the folder back")
	}
	w = do(t, h, "POST", "/api/files/copy", `{"paths":["b"],"to":"a"}`, withCookie(c))
	if w.Code != http.StatusAccepted {
		t.Fatalf("copy: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "DELETE", "/api/files/trash", "", withCookie(c)); w.Code != http.StatusOK {
		t.Errorf("empty trash: %d %s", w.Code, w.Body)
	}
}

func TestFilesUploadOverHTTP(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	c := setup(t, h)
	w := do(t, h, "POST", "/api/files/uploads", `{"path":"drop/hello.txt","size":11}`, withCookie(c))
	if w.Code != http.StatusCreated {
		t.Fatalf("begin: %d %s", w.Code, w.Body)
	}
	var u files.Upload
	json.Unmarshal(w.Body.Bytes(), &u)
	chunk := func(off int64, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/files/uploads/"+u.ID, strings.NewReader(body))
		r.Header.Set("Upload-Offset", strconv.FormatInt(off, 10))
		r.Header.Set("Content-Type", "application/octet-stream")
		r.AddCookie(c)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if w := chunk(0, "hello "); w.Code != http.StatusOK {
		t.Fatalf("chunk: %d %s", w.Code, w.Body)
	}
	// A retried chunk at a stale offset: 409 with the real one.
	w = chunk(0, "hello ")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"offset":6`) {
		t.Fatalf("stale chunk: %d %s", w.Code, w.Body)
	}
	if w := chunk(6, "world"); w.Code != http.StatusOK {
		t.Fatalf("chunk 2: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "POST", "/api/files/uploads/"+u.ID+"/complete", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body)
	}
	b, err := os.ReadFile(filepath.Join(s.files.Root(), "drop", "hello.txt"))
	if err != nil || string(b) != "hello world" {
		t.Errorf("landed = %q %v", b, err)
	}
	if w := do(t, h, "GET", "/api/files/uploads/"+u.ID, "", withCookie(c)); w.Code != http.StatusNotFound {
		t.Errorf("session after complete: %d", w.Code)
	}
}

func TestFilesAccessAndShares(t *testing.T) {

	s, h := newTestServer(t)
	c := setup(t, h)
	root := s.files.Root()
	os.MkdirAll(filepath.Join(root, "pub", "inner"), 0o755)
	os.WriteFile(filepath.Join(root, "pub", "hello.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(root, "pub", "inner", "deep.txt"), []byte("deep"), 0o644)

	// WebDAV: no password, no entry; an app password opens it; .fog is invisible.
	if w := do(t, h, "PROPFIND", "/dav/", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("dav without auth: %d", w.Code)
	}
	w := do(t, h, "POST", "/api/files/access-tokens", `{"name":"laptop"}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("create app password: %d %s", w.Code, w.Body)
	}
	var made struct {
		Password string `json:"password"`
		URL      string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &made)
	if made.Password == "" || !strings.HasSuffix(made.URL, "/dav/") {
		t.Fatalf("made = %+v", made)
	}
	basic := func(r *http.Request) { r.SetBasicAuth("anyone", made.Password) }
	w = do(t, h, "PROPFIND", "/dav/pub/", "", basic, func(r *http.Request) { r.Header.Set("Depth", "1") })
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("propfind: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hello.txt") || strings.Contains(w.Body.String(), ".fog") {
		t.Errorf("propfind body: %s", w.Body)
	}
	r := httptest.NewRequest("PUT", "/dav/pub/new.txt", strings.NewReader("via dav"))
	basic(r)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dav put: %d %s", rec.Code, rec.Body)
	}
	w = do(t, h, "GET", "/api/files/stat?path=pub/new.txt", "", withCookie(c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"size":7`) {
		t.Errorf("indexed after dav put: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/dav/.fog/", "", basic); w.Code != http.StatusNotFound {
		t.Errorf("dav reserved: %d", w.Code)
	}

	// Shares: a folder link renders a page and a zip; a file link streams;
	// revoking kills it.
	w = do(t, h, "POST", "/api/files/shares", `{"path":"pub","days":7}`, withCookie(c))
	if w.Code != http.StatusCreated {
		t.Fatalf("create share: %d %s", w.Code, w.Body)
	}
	var sh struct {
		Share store.FileShare `json:"share"`
		URL   string          `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &sh)
	tokenPath := sh.URL[strings.Index(sh.URL, "/s/"):]
	w = do(t, h, "GET", tokenPath, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello.txt") || !strings.Contains(w.Body.String(), "inner/") {
		t.Fatalf("share page: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", tokenPath+"/inner/deep.txt", ""); w.Code != http.StatusOK || w.Body.String() != "deep" {
		t.Errorf("share nested file: %d %q", w.Code, w.Body.String())
	}
	if w := do(t, h, "GET", tokenPath+"/../hello.txt", ""); w.Code == http.StatusOK && w.Body.String() == "hello" {
		t.Error("share escaped its folder")
	}
	if w := do(t, h, "GET", tokenPath+"?zip=1", ""); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/zip" {
		t.Errorf("share zip: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	// Password-protected link asks.
	w = do(t, h, "POST", "/api/files/shares", `{"path":"pub/hello.txt","days":0,"password":"open sesame"}`, withCookie(c))
	json.Unmarshal(w.Body.Bytes(), &sh)
	pwPath := sh.URL[strings.Index(sh.URL, "/s/"):]
	if w := do(t, h, "GET", pwPath, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("password share without password: %d", w.Code)
	}
	if w := do(t, h, "GET", pwPath, "", func(r *http.Request) { r.SetBasicAuth("", "open sesame") }); w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Errorf("password share: %d %q", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/api/files/shares?path=pub", "", withCookie(c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"path":"pub"`) {
		t.Errorf("list shares: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "DELETE", "/api/files/shares/"+strconv.FormatInt(sh.Share.ID, 10), "", withCookie(c)); w.Code != http.StatusOK {
		t.Errorf("revoke: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", pwPath, "", func(r *http.Request) { r.SetBasicAuth("", "open sesame") }); w.Code != http.StatusNotFound {
		t.Errorf("revoked share: %d", w.Code)
	}
	if w := do(t, h, "GET", "/s/garbage", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown share: %d", w.Code)
	}
}
