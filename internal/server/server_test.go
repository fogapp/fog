// The HTTP surface had no tests at all, which is an odd gap for the layer
// that decides who is allowed to do what. These cover the parts where a
// regression is both easy to introduce and quiet in production: the auth
// boundary, the cross-origin guard, settings validation, and the not-found
// versus internal-error distinction the UI relies on.
package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServerForTest(config.Config{DataDir: dir}, st, Deps{Vault: vlt},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv, srv.Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

func withOrigin(o string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Origin", o) }
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", auth.CookieName)
	return nil
}

const goodSetup = `{"username":"admin","password":"correct horse battery"}`

// setup completes the first-run wizard and returns the resulting session.
func setup(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := do(t, h, "POST", "/api/setup", goodSetup)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	return sessionCookie(t, w)
}

func TestSetupClosesAfterFirstAdmin(t *testing.T) {
	_, h := newTestServer(t)

	if w := do(t, h, "GET", "/api/setup/status", ""); w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	} else {
		var got map[string]any
		json.Unmarshal(w.Body.Bytes(), &got)
		if got["needsSetup"] != true {
			t.Error("a fresh server should report needsSetup")
		}
	}

	setup(t, h)

	// The endpoint is permanently closed — otherwise anyone reaching the box
	// could mint a second admin.
	w := do(t, h, "POST", "/api/setup", `{"username":"intruder","password":"correct horse battery"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("second setup: got %d, want 403", w.Code)
	}
}

func TestSetupRejectsWeakPassword(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup", `{"username":"admin","password":"short"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "10 characters") {
		t.Errorf("unhelpful message: %s", w.Body)
	}
}

func TestSetupRejectsBadBaseDomain(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup",
		`{"username":"admin","password":"correct horse battery","baseDomain":"https://example.com/x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

// A rejected setup request must leave no admin row behind — otherwise the
// "already completed" guard turns one domain typo into a 403 on every retry,
// bricking first-run setup with SettingSetupDone unset and no session.
func TestSetupRetryableAfterBadBaseDomain(t *testing.T) {
	srv, h := newTestServer(t)

	w := do(t, h, "POST", "/api/setup",
		`{"username":"admin","password":"correct horse battery","baseDomain":"https://example.com/x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad domain: got %d, want 400", w.Code)
	}
	if n, _ := srv.store.CountOperators(); n != 0 {
		t.Fatalf("rejected setup created %d operator(s)", n)
	}

	retry := do(t, h, "POST", "/api/setup",
		`{"username":"admin","password":"correct horse battery","baseDomain":"home.example.com"}`)
	if retry.Code != http.StatusOK {
		t.Fatalf("corrected retry: got %d %s, want 200", retry.Code, retry.Body)
	}
	sessionCookie(t, retry)
	if v, _ := srv.store.GetSetting(store.SettingBaseDomain); v != "home.example.com" {
		t.Errorf("base_domain = %q, want home.example.com", v)
	}
	if v, _ := srv.store.GetSetting(store.SettingSetupDone); v != "1" {
		t.Errorf("setup_done = %q, want 1", v)
	}
}

func TestLoginAndSession(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)

	// Unknown user and wrong password must be indistinguishable.
	wrongUser := do(t, h, "POST", "/api/auth/login", `{"username":"nobody","password":"correct horse battery"}`)
	wrongPass := do(t, h, "POST", "/api/auth/login", `{"username":"admin","password":"wrong wrong wrong"}`)
	if wrongUser.Code != http.StatusUnauthorized || wrongPass.Code != http.StatusUnauthorized {
		t.Fatalf("got %d and %d, want 401 for both", wrongUser.Code, wrongPass.Code)
	}
	if wrongUser.Body.String() != wrongPass.Body.String() {
		t.Errorf("responses differ and so leak which usernames exist:\n%s\n%s", wrongUser.Body, wrongPass.Body)
	}

	w := do(t, h, "POST", "/api/auth/login", `{"username":"admin","password":"correct horse battery"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	c := sessionCookie(t, w)
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}

	me := do(t, h, "GET", "/api/auth/me", "", withCookie(c))
	var who map[string]any
	if me.Code != http.StatusOK || json.Unmarshal(me.Body.Bytes(), &who) != nil || who["handle"] != "admin" {
		t.Errorf("me: %d %s", me.Code, me.Body)
	}
	// The handle travels under one name; the old `username` alias is gone.
	if _, ok := who["username"]; ok {
		t.Errorf("me still carries username: %s", me.Body)
	}

	// Logging out must actually invalidate the session server-side, not just
	// clear the cookie in the browser.
	if out := do(t, h, "POST", "/api/auth/logout", "", withCookie(c)); out.Code != http.StatusOK {
		t.Fatalf("logout: %d", out.Code)
	}
	if after := do(t, h, "GET", "/api/auth/me", "", withCookie(c)); after.Code != http.StatusUnauthorized {
		t.Errorf("session still valid after logout: %d", after.Code)
	}
}

// Logout expires what login set: under the base domain that is a
// domain-wide cookie, which a host-only clear would leave in place, and the
// legacy member cookie goes too.
func TestLogoutClearsTheCookiesLoginSets(t *testing.T) {
	srv, h := newTestServer(t)
	setup(t, h)
	srv.store.SetSetting(store.SettingBaseDomain, "home.example.com")
	httpsSetup(t, srv)
	onDomain := func(r *http.Request) { r.Host = "fog.home.example.com" }

	w := do(t, h, "POST", "/api/auth/login", goodSetup, onDomain)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var c *http.Cookie
	for _, k := range w.Result().Cookies() {
		if k.Name == auth.SecureCookieName {
			c = k
		}
	}
	// Host-only: the cookie goes back to fog.<domain> and never to an app.
	if c == nil || c.Domain != "" || !c.Secure {
		t.Fatalf("login cookie = %+v, want a host-only %s", c, auth.SecureCookieName)
	}
	out := do(t, h, "POST", "/api/auth/logout", "", withCookie(c), onDomain)
	if out.Code != http.StatusOK {
		t.Fatalf("logout: %d", out.Code)
	}
	cleared := map[string]bool{}
	for _, k := range out.Result().Cookies() {
		if k.MaxAge < 0 {
			cleared[k.Name+"@"+k.Domain] = true
		}
	}
	// The current cookie, and every name and scope fog ever issued, so a
	// browser carrying one from before the upgrade is signed out too.
	for _, want := range []string{
		auth.SecureCookieName + "@",
		auth.CookieName + "@home.example.com", auth.CookieName + "@",
	} {
		if !cleared[want] {
			t.Errorf("logout did not clear %s; cleared %v", want, cleared)
		}
	}
}

func TestRequireAuthRejectsBadCredentials(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)

	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"empty value", &http.Cookie{Name: auth.CookieName, Value: ""}},
		{"forged token", &http.Cookie{Name: auth.CookieName, Value: "not-a-real-token"}},
	} {
		opts := []func(*http.Request){}
		if tc.cookie != nil {
			opts = append(opts, withCookie(tc.cookie))
		}
		if w := do(t, h, "GET", "/api/settings", "", opts...); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", tc.name, w.Code)
		}
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	srv, h := newTestServer(t)
	setup(t, h)

	// Mint a session that expired an hour ago.
	token, digest := auth.NewToken()
	p, err := srv.store.PersonByHandle("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreatePersonSession(p.ID, 0, digest, "web", -time.Hour, "test", "127.0.0.1", nil); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "GET", "/api/auth/me", "", withCookie(&http.Cookie{Name: auth.CookieName, Value: token}))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired session accepted: %d", w.Code)
	}
}

// Session cookies are SameSite=Lax, which still permits some cross-site
// requests to carry them; the Origin check is what closes that gap.
func TestCrossOriginMutationRejected(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	bad := do(t, h, "PUT", "/api/settings", `{"server_name":"pwned"}`,
		withCookie(c), withOrigin("http://evil.example"))
	if bad.Code != http.StatusForbidden {
		t.Errorf("cross-origin PUT: got %d, want 403", bad.Code)
	}

	// Same origin is fine. httptest defaults Host to example.com.
	ok := do(t, h, "PUT", "/api/settings", `{"server_name":"home"}`,
		withCookie(c), withOrigin("http://example.com"))
	if ok.Code != http.StatusOK {
		t.Errorf("same-origin PUT: got %d %s", ok.Code, ok.Body)
	}

	// Reads are not state-changing and must not be blocked.
	if w := do(t, h, "GET", "/api/settings", "", withCookie(c), withOrigin("http://evil.example")); w.Code != http.StatusOK {
		t.Errorf("cross-origin GET: got %d, want 200", w.Code)
	}
}

func TestRecoveryKitPassphraseRetractsNotice(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)

	// Too short is refused outright: the kit guards the vault master key.
	if w := do(t, h, "PUT", "/api/settings", `{"recovery_kit_passphrase":"short"}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Fatalf("short passphrase: got %d, want 400 (%s)", w.Code, w.Body)
	}

	// The nightly kit's nudge is up; saving a passphrase is what it asked for.
	if err := srv.store.CreateNotice(backup.NoticeRecoveryKitUnscheduled, "", "Recovery kit is not scheduled", ""); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "PUT", "/api/settings", `{"recovery_kit_passphrase":"correct horse battery"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("passphrase rejected: %d %s", w.Code, w.Body)
	}
	notices, _ := srv.store.ActiveNotices()
	for _, n := range notices {
		if n.Kind == backup.NoticeRecoveryKitUnscheduled {
			t.Errorf("notice still active after the passphrase was set: %+v", n)
		}
	}

	// Clearing it again is allowed (kit off) and must not be mistaken for
	// having satisfied the to-do — the nightly run re-raises it on its own.
	if w := do(t, h, "PUT", "/api/settings", `{"recovery_kit_passphrase":""}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("clearing passphrase rejected: %d %s", w.Code, w.Body)
	}
}

// The passphrase opens the recovery kit; GET /api/settings, which every page
// fetches, says whether one is stored but never what it is.
func TestSettingsMaskRecoveryKitPassphrase(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)

	getSettings := func() map[string]string {
		t.Helper()
		w := do(t, h, "GET", "/api/settings", "", withCookie(c))
		if w.Code != http.StatusOK {
			t.Fatalf("GET settings: %d %s", w.Code, w.Body)
		}
		var got map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := getSettings(); got["recovery_kit_passphrase_set"] != "" {
		t.Errorf("passphrase reported set before one was saved: %v", got)
	}

	const pass = "correct horse battery"
	w := do(t, h, "PUT", "/api/settings", `{"recovery_kit_passphrase":"`+pass+`"}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("set passphrase: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), pass) {
		t.Errorf("PUT response echoes the passphrase: %s", w.Body)
	}
	got := getSettings()
	if got["recovery_kit_passphrase"] != "" {
		t.Errorf("GET returned the passphrase: %q", got["recovery_kit_passphrase"])
	}
	if got["recovery_kit_passphrase_set"] != "on" {
		t.Errorf("recovery_kit_passphrase_set = %q, want on", got["recovery_kit_passphrase_set"])
	}

	// Saving another setting leaves the stored passphrase alone.
	if w := do(t, h, "PUT", "/api/settings", `{"server_name":"home"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("save server_name: %d %s", w.Code, w.Body)
	}
	if v := backup.KitPassphrase(srv.store, srv.vault); v != pass {
		t.Errorf("stored passphrase = %q after an unrelated save, want it kept", v)
	}
	// And it rests sealed: the settings row never holds it in the clear.
	if raw, _ := srv.store.GetSetting(backup.SettingRecoveryKitPassphrase); strings.Contains(raw, pass) {
		t.Errorf("the settings row holds the passphrase in the clear: %q", raw)
	}
}

// files_backup is the Backups page's "back up Files" toggle; it has to be a
// setting GET reports and PUT accepts, or that switch can never save.
func TestFilesBackupSettingRoundTrips(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	if w := do(t, h, "PUT", "/api/settings", `{"files_backup":"sometimes"}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("invalid files_backup: got %d, want 400 (%s)", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/settings", `{"files_backup":"off"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("files_backup off rejected: %d %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/settings", "", withCookie(c))
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["files_backup"] != "off" {
		t.Errorf("files_backup = %q after saving off, want off (%v)", got["files_backup"], got)
	}
}

func TestSettingsValidation(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)

	for _, tc := range []struct {
		name, body string
	}{
		{"unknown key", `{"not_a_setting":"x"}`},
		{"malformed domain", `{"base_domain":"http://example.com"}`},
		{"non-numeric retention", `{"backup_retention":"lots"}`},
		{"out-of-range hour", `{"backup_hour_utc":"25"}`},
		{"backup dir (moved through Drive, not written)", `{"backup_dir":"/mnt/nas/backups"}`},
		{"unknown ui mode", `{"ui_mode":"expert"}`},
	} {
		if w := do(t, h, "PUT", "/api/settings", tc.body, withCookie(c)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", tc.name, w.Code, w.Body)
		}
	}

	// A rejected batch must not have been partly applied.
	if v, _ := srv.store.GetSetting(store.SettingBaseDomain); v != "" {
		t.Errorf("base_domain was written despite the request failing: %q", v)
	}

	w := do(t, h, "PUT", "/api/settings", `{"base_domain":"home.example.com","backup_retention":"14","ui_mode":"basic"}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("valid settings rejected: %d %s", w.Code, w.Body)
	}
	if v, _ := srv.store.GetSetting(store.SettingBaseDomain); v != "home.example.com" {
		t.Errorf("base_domain = %q, want home.example.com", v)
	}
	// The response echoes current settings, so the UI never renders a value
	// the server didn't confirm.
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["backup_retention"] != "14" {
		t.Errorf("response did not echo the saved value: %v", got)
	}
}

// A missing app is a 404 with a JSON error, not a 500 — the UI distinguishes
// "no such app" from "we couldn't tell", and says different things for each.
func TestUnknownAppIsNotFound(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	w := do(t, h, "GET", "/api/apps/ghost", "", withCookie(c))
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %s", w.Body)
	}
	if body["error"] == "" {
		t.Errorf("404 carried no error message: %v", body)
	}
}

func TestLoginRateLimited(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)

	limited := false
	for i := 0; i < 15; i++ {
		w := do(t, h, "POST", "/api/auth/login", `{"username":"admin","password":"definitely wrong"}`)
		if w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("brute-force attempts were never rate limited")
	}
}

// A panicking handler must produce a JSON 500 rather than a dropped
// connection, and must not take the process with it.
func TestPanicRecovery(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	w := do(t, h, "GET", "/api/anything", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("panic response is not JSON: %s", w.Body)
	}
	if body["error"] == "" {
		t.Error("panic response carried no error message")
	}
}

// Recovery must not staple a second set of headers onto a response that has
// already started — the client would see a corrupt body.
func TestPanicAfterPartialWrite(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		panic("boom")
	}))
	w := do(t, h, "GET", "/api/anything", "")
	if w.Code != http.StatusOK {
		t.Errorf("status changed after the response had begun: %d", w.Code)
	}
	if w.Body.String() != "partial" {
		t.Errorf("body = %q, want the partial write untouched", w.Body)
	}
}

// The streaming endpoints type-assert on the ResponseWriter, so the wrapper
// has to keep those assertions working.
func TestResponseWrapperPreservesFlusher(t *testing.T) {
	srv, _ := newTestServer(t)
	var flushable bool
	h := srv.protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flushable = w.(http.Flusher)
	}))
	do(t, h, "GET", "/api/anything", "")
	if !flushable {
		t.Error("handlers can no longer flush — log streaming would buffer")
	}
}

// Legacy volume snapshots live as a directory on disk; download must bundle
// them into a tar rather than turning the operator away — the whole point of
// the endpoint is getting a copy off-box.
func TestDownloadLegacySnapshotBundlesDirectory(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)

	app, err := srv.store.CreateApp("blog")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"volume-data.tar.gz": "fake volume tarball",
		"db-blog.dump":       "fake pg dump",
		"meta.json":          `{"app":"blog"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id, err := srv.store.CreateBackup(app.ID, "manual", dir, 42)
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", fmt.Sprintf("/api/backups/%d/download", id), "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-tar" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, ".snapshot.tar") {
		t.Errorf("Content-Disposition = %q, want a .snapshot.tar filename", cd)
	}

	got := map[string]string{}
	tr := tar.NewReader(w.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = string(b)
	}
	for name, content := range files {
		if got[name] != content {
			t.Errorf("entry %q = %q, want %q", name, got[name], content)
		}
	}
}

// The downloaded recovery kit must be the artifact the DR endpoint accepts:
// sealed with the stored passphrase, never a plaintext tar.gz of the vault
// master key.
func TestRecoveryKitDownloadRoundTripsThroughDecryptKit(t *testing.T) {
	srv, h := newTestServer(t)
	srv.backup = &backup.Manager{DataDir: srv.cfg.DataDir, Store: srv.store, Log: srv.log, Vault: srv.vault}
	c := setup(t, h)

	// No passphrase configured: refuse rather than hand out plaintext.
	if w := do(t, h, "GET", "/api/system/recovery-kit", "", withCookie(c)); w.Code != http.StatusBadRequest {
		t.Fatalf("kit without passphrase: got %d, want 400", w.Code)
	}

	const pass = "correct horse battery staple"
	if err := srv.store.SetSetting(backup.SettingRecoveryKitPassphrase, pass); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "GET", "/api/system/recovery-kit", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, ".enc") {
		t.Errorf("Content-Disposition = %q, want a fog-recovery-kit-*.enc filename", cd)
	}

	plain, err := backup.DecryptKit(w.Body.Bytes(), pass)
	if err != nil {
		t.Fatalf("downloaded kit does not decrypt with the stored passphrase: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("decrypted kit is not a gzip stream: %v", err)
	}
	seen := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading kit tar: %v", err)
		}
		seen[hdr.Name] = true
	}
	if !seen["master.key"] || !seen["fog.db"] {
		t.Errorf("kit entries = %v, want master.key and fog.db", seen)
	}
}

// Changing the passphrase writes a fresh kit under it straight away, so the
// newest kit — the one a new machine reaches for — never needs the old one.
func TestPassphraseChangeWritesAFreshKit(t *testing.T) {
	srv, h := newTestServer(t)
	srv.backup = &backup.Manager{DataDir: srv.cfg.DataDir, Store: srv.store, Log: srv.log, Vault: srv.vault}
	c := setup(t, h)

	const pass = "correct horse battery staple"
	if w := do(t, h, "PUT", "/api/settings", `{"recovery_kit_passphrase":"`+pass+`"}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("set passphrase: %d %s", w.Code, w.Body)
	}

	dir := filepath.Join(srv.cfg.DataDir, "backups", "fog-recovery")
	deadline := time.Now().Add(20 * time.Second)
	for {
		kits, _ := filepath.Glob(filepath.Join(dir, "fog-recovery-kit-*.enc"))
		if len(kits) > 0 {
			sealed, err := os.ReadFile(kits[len(kits)-1])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backup.DecryptKit(sealed, pass); err != nil {
				t.Fatalf("fresh kit does not open with the new passphrase: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no kit written after the passphrase change")
		}
		time.Sleep(50 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// A wedged Docker daemon must not take /api/system down with it: while one
// goroutine's probe hangs inside the daemon, concurrent callers get the
// previous cached view immediately, and the warnings cache (its own lock)
// stays reachable too.
func TestDockerStatusDoesNotBlockConcurrentCallers(t *testing.T) {
	// A fake daemon on a unix socket — dockerx.Client speaks to whatever
	// DOCKER_SOCK names. os.MkdirTemp, not t.TempDir: unix socket paths cap
	// at ~104 bytes on macOS and test names push t.TempDir past it.
	sockDir, err := os.MkdirTemp("", "fogsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	var hang atomic.Bool
	arrived := make(chan struct{}, 16)
	release := make(chan struct{})
	fake := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			arrived <- struct{}{}
			<-release
		}
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Write([]byte("OK"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	})}
	go fake.Serve(ln)
	t.Cleanup(func() { fake.Close() })

	t.Setenv("DOCKER_SOCK", sock)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServerForTest(config.Config{DataDir: dir}, st, Deps{Vault: vlt, Docker: dockerx.New()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	if d := srv.dockerStatus(ctx); !d.Available {
		t.Fatalf("priming probe against the healthy fake: %v", d)
	}

	// Age the cache past its TTL and wedge the daemon.
	srv.dockerCache.mu.Lock()
	srv.dockerCache.at = time.Now().Add(-time.Hour)
	srv.dockerCache.mu.Unlock()
	hang.Store(true)

	leaderDone := make(chan dockerStatusView, 1)
	go func() { leaderDone <- srv.dockerStatus(ctx) }()
	<-arrived // the leader's probe is now parked inside the daemon

	follower := make(chan dockerStatusView, 1)
	go func() { follower <- srv.dockerStatus(ctx) }()
	select {
	case d := <-follower:
		if !d.Available {
			t.Errorf("follower got %v, want the previous cached view", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a concurrent dockerStatus call queued behind the in-flight probe")
	}

	warnings := make(chan []string, 1)
	go func() { warnings <- srv.cachedWarnings() }()
	select {
	case <-warnings:
	case <-time.After(2 * time.Second):
		t.Fatal("cachedWarnings blocked behind the Docker probe")
	}

	hang.Store(false)
	close(release)
	select {
	case d := <-leaderDone:
		if !d.Available {
			t.Errorf("leader got %v after the daemon recovered", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader never finished after the daemon was released")
	}
}

// A backup row whose directory has vanished (NAS unmounted, manual cleanup)
// must answer 404, not stream an empty bundle that looks like a backup.
func TestDownloadMissingSnapshotIs404(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	app, err := srv.store.CreateApp("blog")
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.store.CreateBackup(app.ID, "manual", "/nonexistent/path", 0)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "GET", fmt.Sprintf("/api/backups/%d/download", id), "", withCookie(c)); w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}
