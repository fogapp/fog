package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/signin"
)

// TestAnAppCantReplayTheOperatorsSession is the bare-IP finding: on
// http://<ip>:8080 the session is a plain fog_session, which the browser
// also sends to a direct-port app at http://<ip>:<port>. The app replayed
// it from its container — no Origin, a GET — and read the system secrets.
// A plain fog_session now works only from the address it was signed in
// from, and a web session is never accepted as a bearer token.
func TestAnAppCantReplayTheOperatorsSession(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup", goodSetup, fromPeer("192.168.1.20", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	cookie := sessionCookie(t, w)
	if cookie.Name != auth.CookieName {
		t.Fatalf("a bare address's cookie is %s", cookie.Name)
	}
	secrets := func(opts ...func(*http.Request)) int {
		return do(t, h, "GET", "/api/system/secrets", "", opts...).Code
	}
	if c := secrets(withCookie(cookie), fromPeer("192.168.1.20", "")); c != http.StatusOK {
		t.Fatalf("the operator's own browser: %d", c)
	}
	for name, opts := range map[string][]func(*http.Request){
		"the app's container replaying the cookie":           {withCookie(cookie), fromPeer("172.18.0.5", "")},
		"a LAN machine the app sent it to":                   {withCookie(cookie), fromPeer("192.168.1.66", "")},
		"the cookie's value as a bearer, from the container": {func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+cookie.Value) }, fromPeer("172.18.0.5", "")},
	} {
		if c := secrets(opts...); c == http.StatusOK {
			t.Errorf("%s read the system secrets", name)
		}
	}
	if c := do(t, h, "PUT", "/api/settings", `{"server_name":"pwned"}`, withCookie(cookie), fromPeer("172.18.0.5", "")).Code; c == http.StatusOK {
		t.Fatal("the app's container changed a setting with the operator's cookie")
	}
}

// TestABrowserMutationMustComeFromFogsPage: a sibling app's page (same-site)
// or any cross-site page can make the browser send fog's cookie; a request
// that says so, or that hides where it came from, changes nothing.
func TestABrowserMutationMustComeFromFogsPage(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	op := setup(t, h)
	put := func(opts ...func(*http.Request)) int {
		return do(t, h, "PUT", "/api/settings", `{"server_name":"x"}`, append([]func(*http.Request){withCookie(op)}, opts...)...).Code
	}
	site := func(v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", v) } }
	mode := func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "cors") }
	for name, opts := range map[string][]func(*http.Request){
		"a sibling app's page":             {site("same-site"), withOrigin("http://example.com")},
		"a cross-site page":                {site("cross-site"), withOrigin("http://example.com")},
		"a browser request with no Origin": {site("same-origin"), mode},
		"no Origin, cross-site":            {site("cross-site")},
	} {
		if c := put(opts...); c != http.StatusForbidden {
			t.Errorf("%s: %d", name, c)
		}
	}
	if c := put(site("same-origin"), mode, withOrigin("http://example.com")); c != http.StatusOK {
		t.Fatalf("fog's own page: %d", c)
	}
}

// TestARebindingPageCantRunSetup: a page whose DNS name was rebound to the
// box's address posts /api/setup from its own origin, so the same-origin
// check passed it and it became the operator. Its Host names a domain that
// isn't this box.
func TestARebindingPageCantRunSetup(t *testing.T) {

	srv, h := newTestServer(t)
	rebound := func(r *http.Request) {
		r.Host = "rebind.evil.example:8080"
		r.Header.Set("Origin", "http://rebind.evil.example:8080")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Mode", "cors")
	}
	for _, path := range []string{"/api/setup", "/api/auth/login", "/api/people/login/passkey/begin"} {
		if w := do(t, h, "POST", path, goodSetup, rebound); w.Code != http.StatusMisdirectedRequest {
			t.Fatalf("%s from a rebound name: %d %s", path, w.Code, w.Body)
		}
	}
	if n, _ := srv.store.CountOperators(); n != 0 {
		t.Fatal("a rebinding page became the operator")
	}
	// The same request at an address of the box's own runs setup.
	if w := do(t, h, "POST", "/api/setup", goodSetup, func(r *http.Request) {
		r.Host = "localhost:8080"
		r.Header.Set("Origin", "http://localhost:8080")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}); w.Code != http.StatusOK {
		t.Fatalf("setup at localhost: %d %s", w.Code, w.Body)
	}
}

// TestOAuthAndVisitStateChangesNeedFogsPage: /oauth2 and /visit sit outside
// /api's Origin check. Each of their state changes driven by a browser —
// asking for access, confirming a sign-out, continuing a visit — needs
// fog's own page; the client-authenticated endpoints need the client's
// secret and are judged by it.
func TestOAuthAndVisitStateChangesNeedFogsPage(t *testing.T) {
	t.Parallel()

	f := newIdP(t, signin.Manifest{})
	h := f.srv.Handler()
	cookie := withCookie(f.operator)

	// Asking for access: no Origin, or a sibling's page, files nothing.
	for name, opts := range map[string][]func(*http.Request){
		"no Origin":      {cookie},
		"a sibling page": {cookie, withOrigin("http://stream." + idpDomain), func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }},
	} {
		w := do(t, h, "POST", "/oauth2/ask", "app=stream", append(opts, func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		})...)
		if w.Code != http.StatusForbidden {
			t.Errorf("ask from %s: %d", name, w.Code)
		}
	}

	// Confirming a sign-out from another page signs nobody out.
	form := url.Values{"confirm": {"1"}}.Encode()
	for name, origin := range map[string]string{"a cross-site page": "https://evil.example", "no Origin": ""} {
		w := do(t, h, "POST", "/oauth2/end_session", form, cookie, func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if origin != "" {
				r.Header.Set("Origin", origin)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			}
		})
		if w.Code == http.StatusFound {
			t.Errorf("%s confirmed a sign-out: %d", name, w.Code)
		}
		if _, _, err := f.srv.store.SessionPerson(auth.DigestToken(f.operator.Value)); err != nil {
			t.Fatalf("%s signed the operator out", name)
		}
	}

	// A visit's Continue from a sibling's page is refused before it is read.
	w := do(t, h, "POST", "/visit/authorize", "request_uri=x&confirm=1", cookie, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://stream."+idpDomain)
		r.Header.Set("Sec-Fetch-Site", "same-site")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("a visit continued from a sibling page: %d", w.Code)
	}

	// The token endpoint is the client's: a browser can't use it without the
	// secret, whatever it sends.
	if code := tokenStatusAt(t, f, "/oauth2/par", url.Values{"response_type": {"code"}}); code == "invalid_client" {
		t.Fatal("the client's own back channel was refused")
	}
}

// signInOverTCP signs handle in through base over a real connection, as a
// client running beside it (the chat SDK's Node scripts) does: the session
// is pinned to the address that client connects from.
func signInOverTCP(t *testing.T, base, handle, password string) *http.Cookie {
	t.Helper()
	res, err := http.Post(base+"/api/auth/login", "application/json",
		strings.NewReader(`{"handle":"`+handle+`","password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == auth.CookieName || c.Name == auth.SecureCookieName {
			return c
		}
	}
	t.Fatalf("sign in %s over TCP: %d, no session cookie", handle, res.StatusCode)
	return nil
}

// httpsSetup makes a test server's domain install an https one, as a DNS
// token makes Caddy serve TLS in production (gateScheme): its session is
// __Host-fog_session, and its gate assertions say https.
func httpsSetup(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.secrets().Put(0, proxy.SettingDNSToken, "test-dns-token"); err != nil {
		t.Fatal(err)
	}
	srv.gateSchemeCache = ttlCache[string]{}
}
