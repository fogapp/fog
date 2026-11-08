package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// The update token is sent only over https, only to the source's host, and
// only while the source is the one it was saved for — an admin who points
// the source at a server of their own collects nothing.
func TestUpdateTokenStaysWithItsSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := (vault.Secrets{Store: st, Vault: vlt}).Put(0, SecretUpdateBasicAuth, "s3cret"); err != nil {
		t.Fatal(err)
	}
	u := &Updater{Store: st, Vault: vlt}
	sent := func(rawURL string) bool {
		req, _ := http.NewRequest("GET", rawURL, nil)
		u.authorize(req)
		return req.Header.Get("Authorization") != ""
	}

	st.SetSetting(SettingUpdateBaseURL, "https://git.example.com/me/fog")
	st.SetSetting(SettingUpdateTokenOrigin, "https://git.example.com")
	if !sent("https://git.example.com/me/fog/raw/branch/main/release/latest.json") {
		t.Error("token not sent to the source it was saved for")
	}
	if sent("https://cdn.example.net/fogd-linux-amd64") {
		t.Error("token sent to a host the manifest pointed at")
	}

	// The source moved (by the setting, or by FOG_UPDATE_BASE_URL) without
	// the token being saved again: it stays home.
	st.SetSetting(SettingUpdateBaseURL, "https://evil.example.org/me/fog")
	if sent("https://evil.example.org/me/fog/latest.json") {
		t.Error("token followed the source to another host")
	}

	// Never over plain http, even to its own host.
	st.SetSetting(SettingUpdateBaseURL, "http://git.example.com/me/fog")
	st.SetSetting(SettingUpdateTokenOrigin, "http://git.example.com")
	if sent("http://git.example.com/me/fog/latest.json") {
		t.Error("token sent over plain http")
	}

	if err := CheckTokenSource("http://git.example.com/me/fog"); err == nil {
		t.Error("a token was accepted for an http source")
	}
	if err := CheckTokenSource("https://git.example.com/me/fog"); err != nil {
		t.Error(err)
	}
	if !TokenFollowsSource("https://git.example.com/me/fog", "https://GIT.example.com/me/fog-fork") {
		t.Error("a move within one host dropped the token")
	}
	if TokenFollowsSource("https://git.example.com/me/fog", "https://evil.example.org/me/fog") {
		t.Error("a move to another host kept the token")
	}
	if TokenFollowsSource("https://git.example.com/me/fog", "http://git.example.com/me/fog") {
		t.Error("a move to http on the same host kept the token")
	}
}

// Go keeps Authorization across a redirect to the same host name, even to
// plain http or another port. A source (or anything answering for it) that
// redirects there must not receive the token: it is decided again on every
// hop, and goes only to https on the origin it was saved for.
func TestUpdateTokenIsNotResentOnARedirect(t *testing.T) {
	t.Parallel()

	var leaked atomic.Value
	leaked.Store("")
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{}`))
	}))
	defer plain.Close()
	var src *httptest.Server
	src = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-http": // same host, plain http, another port
			http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
		case "/to-https":
			http.Redirect(w, r, src.URL+"/landed", http.StatusFound)
		case "/landed":
			w.Write([]byte(r.Header.Get("Authorization")))
		}
	}))
	defer src.Close()

	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, _ := vault.Open(dir)
	(vault.Secrets{Store: st, Vault: vlt}).Put(0, SecretUpdateBasicAuth, "s3cret")
	st.SetSetting(SettingUpdateBaseURL, src.URL+"/me/fog")
	st.SetSetting(SettingUpdateTokenOrigin, SourceOrigin(src.URL+"/me/fog"))
	u := &Updater{Store: st, Vault: vlt, transport: src.Client().Transport}

	// The plain server shares the TLS server's host name (127.0.0.1).
	if _, err := u.getBytes(context.Background(), src.URL+"/to-http", 1024); err != nil {
		t.Fatal(err)
	}
	if got := leaked.Load().(string); got != "" {
		t.Errorf("token resent over a redirect to plain http on another port: %q", got)
	}
	// A redirect within the source's https origin keeps it.
	body, err := u.getBytes(context.Background(), src.URL+"/to-https", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "token s3cret" {
		t.Errorf("token not sent after a redirect within the source: %q", body)
	}
}
