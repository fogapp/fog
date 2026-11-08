package server

import (
	"encoding/json"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/auth"
)

// davPassword makes an app password and returns a request option that
// presents it.
func davPassword(t *testing.T, h http.Handler, c *http.Cookie) func(*http.Request) {
	t.Helper()
	w := do(t, h, "POST", "/api/files/access-tokens", `{"name":"laptop"}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("create app password: %d %s", w.Code, w.Body)
	}
	var made struct {
		Password string `json:"password"`
	}
	json.Unmarshal(w.Body.Bytes(), &made)
	return func(r *http.Request) { r.SetBasicAuth("anyone", made.Password) }
}

// The share-link attack, end to end: a share of Public, a subfolder of
// decoys indexed, then swapped for a link to Private. The link must not
// serve Private's files, list them, or zip them.
func TestShareLinkDoesNotFollowASwappedInLink(t *testing.T) {

	s, h := newTestServer(t)
	c := setup(t, h)
	root := s.files.Root()
	os.MkdirAll(filepath.Join(root, "Public", "sub"), 0o755)
	os.MkdirAll(filepath.Join(root, "Private"), 0o755)
	os.WriteFile(filepath.Join(root, "Public", "sub", "secret.txt"), []byte("decoy"), 0o644)
	os.WriteFile(filepath.Join(root, "Private", "secret.txt"), []byte("the operator's secret"), 0o644)

	w := do(t, h, "POST", "/api/files/shares", `{"path":"Public","days":0}`, withCookie(c))
	if w.Code != http.StatusCreated {
		t.Fatalf("create share: %d %s", w.Code, w.Body)
	}
	var sh struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &sh)
	base := sh.URL[strings.Index(sh.URL, "/s/"):]
	if w := do(t, h, "GET", base+"/sub/secret.txt", ""); w.Code != http.StatusOK || w.Body.String() != "decoy" {
		t.Fatalf("decoy before the swap: %d %q", w.Code, w.Body)
	}

	os.RemoveAll(filepath.Join(root, "Public", "sub"))
	if err := os.Symlink("../Private", filepath.Join(root, "Public", "sub")); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "GET", base+"/sub/secret.txt", ""); strings.Contains(w.Body.String(), "operator's secret") {
		t.Fatalf("the share served Private/secret.txt through the link (%d)", w.Code)
	}
	if w := do(t, h, "GET", base+"/sub", ""); strings.Contains(w.Body.String(), "secret.txt") {
		t.Fatalf("the share listed Private through the link (%d): %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", base+"?zip=1", ""); strings.Contains(w.Body.String(), "operator's secret") || strings.Contains(w.Body.String(), "sub/secret.txt") {
		t.Fatal("the share's zip carried Private's file")
	}

	// WebDAV reaches nothing through the link either.
	basic := davPassword(t, h, c)
	if w := do(t, h, "GET", "/dav/Public/sub/secret.txt", "", basic); strings.Contains(w.Body.String(), "operator's secret") {
		t.Fatalf("WebDAV served Private/secret.txt through the link (%d)", w.Code)
	}
	w = do(t, h, "PROPFIND", "/dav/Public/sub/", "", basic, func(r *http.Request) { r.Header.Set("Depth", "1") })
	if strings.Contains(w.Body.String(), "secret.txt") {
		t.Fatalf("WebDAV listed Private through the link: %s", w.Body)
	}
}

// A page an app planted, opened from /dav/ in a browser, must not run as
// fog: the same sandbox and declared type as every other route serving
// Drive's bytes.
func TestDAVServesFilesSandboxed(t *testing.T) {

	s, h := newTestServer(t)
	c := setup(t, h)
	root := s.files.Root()
	os.MkdirAll(filepath.Join(root, "Content"), 0o755)
	os.WriteFile(filepath.Join(root, "Content", "page.html"), []byte("<script>fetch('/api/settings')</script>"), 0o644)
	os.WriteFile(filepath.Join(root, "Content", "README"), []byte("<html><script>alert(1)</script>"), 0o644)
	basic := davPassword(t, h, c)

	w := do(t, h, "GET", "/dav/Content/page.html", "", basic)
	if w.Code != http.StatusOK {
		t.Fatalf("dav get: %d %s", w.Code, w.Body)
	}
	hd := w.Header()
	if hd.Get("Content-Security-Policy") != "sandbox" || hd.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("dav html headers = %v", hd)
	}
	if !strings.HasPrefix(hd.Get("Content-Disposition"), "inline") {
		t.Errorf("disposition = %q", hd.Get("Content-Disposition"))
	}
	// No extension: declared, not sniffed into text/html, and downloaded.
	w = do(t, h, "GET", "/dav/Content/README", "", basic)
	if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Errorf("an extensionless file was sniffed as %q", ct)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("extensionless headers = %v", w.Header())
	}
	// PROPFIND reports the declared type too.
	w = do(t, h, "PROPFIND", "/dav/Content/", "", basic, func(r *http.Request) { r.Header.Set("Depth", "1") })
	if strings.Contains(w.Body.String(), "text/html; charset") {
		t.Errorf("propfind sniffed a type: %s", w.Body)
	}
}

// An app the operator visits may not search Drive just because it holds
// its token and the gate's assertion: it needs "drive" in its uses, and
// then sees neither other apps' folders, peers' files, hidden files nor
// digests.
func TestAppDriveSearchNeedsTheGrantAndIsScoped(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	gate := f.signIn(t, f.operator, "/")
	w := do(t, f.h, "GET", "/api/authgate", "", forwarded("/"), withCookie(gate))
	assertion := w.Header().Get("Fog-Assertion")
	token := "app-token-sab"
	if err := f.srv.store.SetAppAPITokenDigest(f.appID, auth.DigestToken(token)); err != nil {
		t.Fatal(err)
	}
	root := f.srv.files.Root()
	for _, p := range []string{
		"Content/Movies/target-film.mkv",
		"Apps/Sab/config/target-sab.ini",
		"Apps/Vault/target-vault.db",
		"Peers/dave/Holiday/target-peer.jpg",
		"Docs/.ssh/target-key",
		"Docs/target-doc.txt",
	} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte("x"), 0o644)
	}
	if err := f.srv.files.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	search := func() (int, string) {
		w := do(t, f.h, "GET", "/app/v1/files/search?q=target", "", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Fog-Assertion", assertion)
		})
		return w.Code, w.Body.String()
	}
	if code, body := search(); code != http.StatusForbidden {
		t.Fatalf("an app without the drive grant searched Drive: %d %s", code, body)
	}

	spec := `{"source":{"type":"image","image":"sabnzbd"},"routes":[{"port":8080,"auth":true}],"uses":["drive"]}`
	if _, err := f.srv.store.CreateRevision(f.appID, spec, "", "t"); err != nil {
		t.Fatal(err)
	}
	code, body := search()
	if code != http.StatusOK {
		t.Fatalf("granted search: %d %s", code, body)
	}
	for _, want := range []string{"target-film.mkv", "target-sab.ini", "target-doc.txt"} {
		if !strings.Contains(body, want) {
			t.Errorf("granted search is missing %s: %s", want, body)
		}
	}
	for _, never := range []string{"target-vault.db", "target-peer.jpg", "target-key", "sha256"} {
		if strings.Contains(body, never) {
			t.Errorf("an app's search shows %s: %s", never, body)
		}
	}
	// One letter is a listing, not a search.
	w = do(t, f.h, "GET", "/app/v1/files/search?q=t", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Fog-Assertion", assertion)
	})
	if strings.Contains(w.Body.String(), "target") {
		t.Errorf("a one-letter search answered: %s", w.Body)
	}
}

// A name an app chose must not lie on fog's pages: a bidi override can make
// "invoice‮fdp.exe" read as "invoiceexe.pdf", a newline can break a
// line. The share page shows such names with those characters made visible
// and isolated; the links and the download keep the real name.
func TestSharePageShowsHostileNamesSafely(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	c := setup(t, h)
	root := s.files.Root()
	spoof := "invoice‮fdp.exe"
	broken := "line\nbreak.txt"
	os.MkdirAll(filepath.Join(root, "Public"), 0o755)
	os.WriteFile(filepath.Join(root, "Public", spoof), []byte("MZ"), 0o644)
	if err := os.WriteFile(filepath.Join(root, "Public", broken), []byte("x"), 0o644); err != nil {
		t.Skip("this filesystem refuses a newline in a name:", err)
	}

	w := do(t, h, "POST", "/api/files/shares", `{"path":"Public","days":0}`, withCookie(c))
	var sh struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &sh)
	base := sh.URL[strings.Index(sh.URL, "/s/"):]
	w = do(t, h, "GET", base, "")
	if w.Code != http.StatusOK {
		t.Fatalf("share page: %d %s", w.Code, w.Body)
	}
	page := html.UnescapeString(w.Body.String())
	if strings.ContainsRune(page, '‮') {
		t.Error("the share page carries a raw right-to-left override")
	}
	if !strings.Contains(page, "<bdi>invoice⟨U+202E⟩fdp.exe</bdi>") {
		t.Errorf("the spoofing name is not shown visibly and isolated: %s", page)
	}
	if !strings.Contains(page, "<bdi>line␊break.txt</bdi>") {
		t.Errorf("the newline is not shown visibly: %s", page)
	}

	// The link still reaches the real file, and the download is named for it
	// (RFC 5987/2231 encoded, so the header carries no raw override).
	w = do(t, h, "GET", base+"/invoice%E2%80%AEfdp.exe", "")
	if w.Code != http.StatusOK || w.Body.String() != "MZ" {
		t.Fatalf("the real file: %d %q", w.Code, w.Body)
	}
	cd := w.Header().Get("Content-Disposition")
	if strings.ContainsRune(cd, '‮') || !strings.Contains(cd, "filename*=utf-8''invoice%E2%80%AEfdp.exe") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	w = do(t, h, "GET", "/api/files/raw/Public/line%0Abreak.txt?dl=1", "", withCookie(c))
	if cd := w.Header().Get("Content-Disposition"); w.Code != http.StatusOK || strings.ContainsAny(cd, "\n\r") || !strings.Contains(cd, "line%0Abreak.txt") {
		t.Errorf("raw download of a newline name: %d %q", w.Code, cd)
	}
}
