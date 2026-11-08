package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	gateDomain = "home.test"
	gateHost   = "sab.home.test"
)

// gateFixture is a fog with a domain, an operator, and one app whose route
// is gated.
type gateFixture struct {
	srv      *Server
	h        http.Handler
	operator *http.Cookie
	appID    int64
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	srv, h := newTestServer(t)
	op := setup(t, h)
	if err := srv.store.SetSetting(store.SettingBaseDomain, gateDomain); err != nil {
		t.Fatal(err)
	}
	app, err := srv.store.CreateApp("sab")
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"source":{"type":"image","image":"sabnzbd"},"routes":[{"port":8080,"auth":true}]}`
	if _, err := srv.store.CreateRevision(app.ID, spec, "", "t"); err != nil {
		t.Fatal(err)
	}
	// Caddy serves TLS exactly when a DNS token is set: an https setup.
	if err := srv.secrets().Put(0, proxy.SettingDNSToken, "test-dns-token"); err != nil {
		t.Fatal(err)
	}
	return &gateFixture{srv: srv, h: h, operator: op, appID: app.ID}
}

// forwarded makes a request look like Caddy's forward_auth subrequest for
// the gated host over https.
func forwarded(uri string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("X-Forwarded-Host", gateHost)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Method", "GET")
		r.Header.Set("X-Forwarded-Uri", uri)
	}
}

func onGateHost(r *http.Request) {
	r.Host = gateHost
	r.Header.Set("X-Forwarded-Host", gateHost)
	r.Header.Set("X-Forwarded-Proto", "https")
}

func onFogHost(r *http.Request) {
	r.Host = "fog." + gateDomain
	r.Header.Set("X-Forwarded-Proto", "https")
}

// begin is the gated host turning a browser away: where it sends it on fog,
// and the binding cookie it sets for the handshake's callback.
func (f *gateFixture) begin(t *testing.T, next string) (*url.URL, *http.Cookie) {
	t.Helper()
	w := do(t, f.h, "GET", "/api/authgate", "", forwarded(next))
	if w.Code != http.StatusFound {
		t.Fatalf("authgate without a gate cookie: %d %s", w.Code, w.Body)
	}
	authorize, err := url.Parse(w.Header().Get("Location"))
	if err != nil || authorize.Host != "fog."+gateDomain || authorize.Path != "/gate/authorize" {
		t.Fatalf("authgate sent the browser to %q", w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-fog_gate_bind" {
			if !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" {
				t.Fatalf("binding cookie attributes: %+v", c)
			}
			return authorize, c
		}
	}
	t.Fatal("authgate set no binding cookie")
	return nil, nil
}

// authorize is fog deciding for session: the callback URL it sends the
// browser to on the gated host.
func (f *gateFixture) authorize(t *testing.T, authorize *url.URL, session *http.Cookie) *url.URL {
	t.Helper()
	// The browser holds __Host-fog_session on an https setup and a plain
	// host-only fog_session on an http one; fog reads only the right one.
	w := do(t, f.h, "GET", authorize.RequestURI(), "", onFogHost, withCookie(hostCookie(session)),
		withCookie(&http.Cookie{Name: auth.CookieName, Value: session.Value}))
	if w.Code != http.StatusFound {
		t.Fatalf("authorize: %d %s", w.Code, w.Body)
	}
	cb, err := url.Parse(w.Header().Get("Location"))
	if err != nil || cb.Host != gateHost || cb.Path != "/.fog/gate/callback" {
		t.Fatalf("authorize sent the browser to %q", w.Header().Get("Location"))
	}
	return cb
}

// signIn walks the whole handshake for the session cookie and returns the
// gate cookie the host ends up with.
func (f *gateFixture) signIn(t *testing.T, session *http.Cookie, next string) *http.Cookie {
	t.Helper()
	authorize, bind := f.begin(t, next)
	cb := f.authorize(t, authorize, session)
	w := do(t, f.h, "GET", cb.RequestURI(), "", onGateHost, withCookie(bind))
	if w.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-fog_gate" {
			if !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" {
				t.Fatalf("gate cookie attributes: %+v", c)
			}
			return c
		}
	}
	t.Fatalf("callback set no gate cookie")
	return nil
}

func TestGateHandshakeAndAssertion(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	gate := f.signIn(t, f.operator, "/queue?x=1")

	w := do(t, f.h, "GET", "/api/authgate", "", forwarded("/queue"), withCookie(gate))
	if w.Code != http.StatusNoContent {
		t.Fatalf("authgate with a gate cookie: %d %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Remote-User"); got != "admin" {
		t.Errorf("Remote-User = %q", got)
	}
	if got := w.Header().Get("Remote-Groups"); got != "operator" {
		t.Errorf("Remote-Groups = %q", got)
	}
	payload, err := f.srv.keys.Verify(w.Header().Get("Fog-Assertion"))
	if err != nil {
		t.Fatalf("assertion does not verify: %v", err)
	}
	var claims struct {
		Iss, Aud, Sub, Sid string
		Exp, Iat           int64
		Roles              []string
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://"+gateHost || claims.Iss != "https://fog."+gateDomain {
		t.Errorf("iss/aud = %s / %s", claims.Iss, claims.Aud)
	}
	if !strings.HasPrefix(claims.Sub, "person:") || claims.Sid == "" {
		t.Errorf("sub/sid = %q / %q", claims.Sub, claims.Sid)
	}
	if claims.Exp-claims.Iat > int64(assertionTTL/time.Second) {
		t.Errorf("assertion lives %ds", claims.Exp-claims.Iat)
	}

	// The JWKS an app verifies against carries the key.
	w = do(t, f.h, "GET", "/oauth2/jwks", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"kty":"EC"`) {
		t.Fatalf("jwks: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), `"d":`) {
		t.Fatal("the JWKS leaks a private key")
	}
}

func TestGateRefusesClientSuppliedIdentityWithoutACookie(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	w := do(t, f.h, "GET", "/api/authgate", "", forwarded("/"), func(r *http.Request) {
		r.Header.Set("Remote-User", "admin")
		r.Header.Set("Fog-Assertion", "x.y.z")
	})
	if w.Code == http.StatusNoContent {
		t.Fatal("identity headers alone passed the gate")
	}
}

func TestGateCodesAreSingleUseAndHostBound(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	authorize, bind := f.begin(t, "/")
	cb := f.authorize(t, authorize, f.operator)

	other, err := f.srv.store.CreateApp("other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.CreateRevision(other.ID, `{"source":{"type":"image","image":"x"},"routes":[{"port":80,"auth":true}]}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	f.srv.gateHosts = ttlCache[map[string]int64]{}
	stolen := do(t, f.h, "GET", cb.RequestURI(), "", withCookie(bind), func(r *http.Request) {
		r.Host = "other." + gateDomain
		r.Header.Set("X-Forwarded-Host", "other."+gateDomain)
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	if stolen.Code != http.StatusBadRequest {
		t.Fatalf("a code minted for %s was redeemed on another host: %d", gateHost, stolen.Code)
	}
	if w := do(t, f.h, "GET", cb.RequestURI(), "", onGateHost, withCookie(bind)); w.Code != http.StatusFound {
		t.Fatalf("first redemption: %d", w.Code)
	}
	if w := do(t, f.h, "GET", cb.RequestURI(), "", onGateHost, withCookie(bind)); w.Code != http.StatusBadRequest {
		t.Fatalf("a code redeemed twice: %d", w.Code)
	}
}

// TestGateHandshakeIsBoundToTheBrowser is the login-CSRF finding: someone
// allowed through used to be able to hand another person a fresh callback
// link and sign them in to the gated app as themselves. The callback now
// redeems a code only in the browser whose binding cookie began it.
func TestGateHandshakeIsBoundToTheBrowser(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	// The attacker begins a handshake in their own browser and lets fog
	// mint a code for their own session, then sends the callback link to
	// the victim, whose browser has no binding — or one from a handshake of
	// its own, or a cookie of the right name with a guessed value.
	authorize, attackerBind := f.begin(t, "/")
	_, victimBind := f.begin(t, "/")
	for name, opts := range map[string][]func(*http.Request){
		"no binding":           {onGateHost},
		"the victim's binding": {onGateHost, withCookie(victimBind)},
		"a forged binding":     {onGateHost, withCookie(&http.Cookie{Name: attackerBind.Name, Value: "not-the-value"})},
		// What a sibling host can set: an ordinary cookie for the whole
		// domain, carrying the attacker's own binding value. Only the
		// __Host- cookie, which a sibling cannot set, is read.
		"a binding planted by a sibling": {onGateHost, withCookie(&http.Cookie{Name: "fog_gate_bind", Value: attackerBind.Value}),
			withCookie(&http.Cookie{Name: "__Secure-fog_gate_bind", Value: attackerBind.Value})},
	} {
		cb := f.authorize(t, authorize, f.operator)
		w := do(t, f.h, "GET", cb.RequestURI(), "", opts...)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: the planted callback answered %d", name, w.Code)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == "__Host-fog_gate" && c.MaxAge > 0 {
				t.Fatalf("%s: the victim's browser was signed in as the attacker", name)
			}
		}
		// The planted code is spent, so not even the attacker can use it.
		if w := do(t, f.h, "GET", cb.RequestURI(), "", onGateHost, withCookie(attackerBind)); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: the refused code still redeems: %d", name, w.Code)
		}
	}
	// In the browser that began it, the same handshake works.
	cb := f.authorize(t, authorize, f.operator)
	if w := do(t, f.h, "GET", cb.RequestURI(), "", onGateHost, withCookie(attackerBind)); w.Code != http.StatusFound {
		t.Fatalf("the bound browser was refused: %d", w.Code)
	}

	// An authorize request without a binding the host began mints nothing.
	w := do(t, f.h, "GET", "/gate/authorize?host="+gateHost+"&next=/", "", onFogHost, withCookie(hostCookie(f.operator)))
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || loc != "https://"+gateHost+"/" {
		t.Fatalf("an unbound authorize: %d %s", w.Code, loc)
	}
}

func TestGateNeverRedirectsOffHost(t *testing.T) {
	t.Parallel()

	for _, next := range []string{"https://evil.example/", "//evil.example/x", "http://" + gateHost + "/", "https://user@" + gateHost + "/", "javascript:alert(1)"} {
		if got := sameHostNext(next, gateHost, true); got != "https://"+gateHost+"/" {
			t.Errorf("next %q → %q", next, got)
		}
	}
	if got := sameHostNext("https://"+gateHost+"/a?b=c#frag", gateHost, true); got != "https://"+gateHost+"/a?b=c" {
		t.Errorf("same-host next rewritten to %q", got)
	}
}

func TestGatePolicyAdmitsMembersOnlyWhenShared(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	kim := addMember(t, f.srv, "kim")

	authorize, _ := f.begin(t, "/")
	w := do(t, f.h, "GET", authorize.RequestURI(), "", onFogHost, withCookie(hostCookie(kim)))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "shared with you") {
		t.Fatalf("a member reached an operator-only app: %d %s", w.Code, w.Body)
	}

	w = do(t, f.h, "PUT", "/api/apps/sab/signin", `{"audience":"household","people":[]}`, withCookie(f.operator))
	if w.Code != http.StatusOK {
		t.Fatalf("set policy: %d %s", w.Code, w.Body)
	}
	gate := f.signIn(t, kim, "/")

	// Narrowing the audience again shuts a live gate session out.
	if w := do(t, f.h, "PUT", "/api/apps/sab/signin", `{"audience":"operator","people":[]}`, withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := do(t, f.h, "GET", "/api/authgate", "", forwarded("/"), withCookie(gate)); w.Code != http.StatusForbidden {
		t.Fatalf("after narrowing: %d", w.Code)
	}
}

func TestGateSessionEndsWithFogSession(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	gate := f.signIn(t, f.operator, "/")
	if err := f.srv.store.DeletePersonSessionByDigest(auth.DigestToken(f.operator.Value)); err != nil {
		t.Fatal(err)
	}
	if w := do(t, f.h, "GET", "/api/authgate", "", forwarded("/"), withCookie(gate)); w.Code == http.StatusNoContent {
		t.Fatal("the gate outlived the fog session it came from")
	}
}

func TestSessionCookieIsHostOnlyUnderTheDomain(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	if err := srv.store.SetSetting(store.SettingBaseDomain, gateDomain); err != nil {
		t.Fatal(err)
	}
	httpsSetup(t, srv)
	w := do(t, h, "POST", "/api/setup", goodSetup, onFogHost)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	var got *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SecureCookieName {
			got = c
		}
		if c.Name == auth.CookieName && c.MaxAge >= 0 {
			t.Fatalf("issued the old cookie name: %+v", c)
		}
	}
	if got == nil || got.Domain != "" || !got.Secure {
		t.Fatalf("session cookie = %+v; want host-only __Host-fog_session", got)
	}
}

// TestAPlantedDomainCookieIsRefused: a sibling host (any app on the
// household's domain) can set a domain-wide fog_session in a browser — with
// its own operator's token, say — hoping fog adopts it and the victim then
// works inside the attacker's session. fog reads only __Host-fog_session
// there, never adopts the other, and expires it.
func TestAPlantedDomainCookieIsRefused(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	attacker := setup(t, h) // a valid session the planter holds
	if err := srv.store.SetSetting(store.SettingBaseDomain, gateDomain); err != nil {
		t.Fatal(err)
	}
	httpsSetup(t, srv)
	planted := &http.Cookie{Name: auth.CookieName, Value: attacker.Value}

	// A browser with no fog session of its own, carrying the planted cookie.
	w := do(t, h, "GET", "/api/auth/me", "", onFogHost, withCookie(planted))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("fog adopted a planted fog_session: %d", w.Code)
	}
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SecureCookieName && c.MaxAge >= 0 {
			t.Fatalf("fog moved the planted value into its own cookie: %+v", c)
		}
		if c.Name == auth.CookieName && c.Domain == gateDomain && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("the planted domain-wide cookie was not expired: %v", w.Result().Cookies())
	}

	// Planted beside a session of the victim's own: the victim's is the one
	// read, and signing in doesn't end the planter's session on their behalf.
	victim := addMember(t, srv, "kim")
	mine := &http.Cookie{Name: auth.SecureCookieName, Value: victim.Value}
	w = do(t, h, "GET", "/api/auth/me", "", onFogHost, withCookie(planted), withCookie(mine))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "kim") {
		t.Fatalf("with the victim's own cookie: %d %s", w.Code, w.Body)
	}

	// Over https on any host fog serves, too — not only under the domain.
	w = do(t, h, "GET", "/api/auth/me", "", withCookie(planted), func(r *http.Request) {
		r.Host = "myfog.example.net"
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a plain fog_session over https was read: %d", w.Code)
	}

	// On a plain-http bare address, where nothing can carry __Host-, fog's
	// own plain cookie still works.
	w = do(t, h, "GET", "/api/auth/me", "", withCookie(planted), func(r *http.Request) { r.Host = "192.168.1.10:8080" })
	if w.Code != http.StatusOK {
		t.Fatalf("a bare address's session: %d", w.Code)
	}
}

// hostCookie is a session cookie as a browser holds it on fog's https
// host: __Host-fog_session, the only name fog reads there.
func hostCookie(c *http.Cookie) *http.Cookie {
	return &http.Cookie{Name: auth.SecureCookieName, Value: c.Value}
}

// addMember invites and joins a member, returning their session cookie.
func addMember(t *testing.T, srv *Server, handle string) *http.Cookie {
	t.Helper()
	id, err := srv.store.CreatePerson(handle, handle, store.PersonRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	token, digest := auth.NewToken()
	if _, err := srv.store.CreatePersonSession(id, 0, digest, "web", time.Hour, "", "", []string{"pwd"}); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: auth.CookieName, Value: token}
}

func TestAppActsForThePresentPersonOnly(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	gate := f.signIn(t, f.operator, "/")
	w := do(t, f.h, "GET", "/api/authgate", "", forwarded("/"), withCookie(gate))
	assertion := w.Header().Get("Fog-Assertion")

	token := "app-token-sab"
	if err := f.srv.store.SetAppAPITokenDigest(f.appID, auth.DigestToken(token)); err != nil {
		t.Fatal(err)
	}
	// The other app may search Drive, so what refuses it below is the
	// assertion — sab's, not its own — and not a missing grant.
	other, _ := f.srv.store.CreateApp("other")
	if _, err := f.srv.store.CreateRevision(other.ID, `{"source":{"type":"image","image":"other"},"uses":["drive"]}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetAppAPITokenDigest(other.ID, auth.DigestToken("app-token-other")); err != nil {
		t.Fatal(err)
	}
	search := func(tok, a string) int {
		return do(t, f.h, "GET", "/app/v1/files/search?q=x", "", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+tok)
			if a != "" {
				r.Header.Set("Fog-Assertion", a)
			}
		}).Code
	}
	if c := search(token, ""); c != http.StatusForbidden {
		t.Errorf("no assertion: %d", c)
	}
	if c := search("app-token-other", assertion); c != http.StatusForbidden {
		t.Errorf("another app spent sab's assertion: %d", c)
	}
	if c := search(token, assertion[:len(assertion)-4]+"AAAA"); c != http.StatusForbidden {
		t.Errorf("tampered assertion: %d", c)
	}
	if c := search(token, assertion); c != http.StatusForbidden {
		t.Errorf("an app not granted drive searched Drive: %d", c)
	}
	spec := `{"source":{"type":"image","image":"sabnzbd"},"routes":[{"port":8080,"auth":true}],"uses":["drive"]}`
	if _, err := f.srv.store.CreateRevision(f.appID, spec, "", "t"); err != nil {
		t.Fatal(err)
	}
	if c := search(token, ""); c != http.StatusForbidden {
		t.Errorf("granted, but no assertion: %d", c)
	}
	if c := search(token, assertion); c == http.StatusForbidden || c == http.StatusUnauthorized {
		t.Errorf("the app's own fresh assertion was refused: %d", c)
	}
}

// TestAppGateNamesOnlyTheAppsOwnHosts: what fog-client verifies assertions
// against — the issuer and the app's own gated hosts, never another app's.
func TestAppGateNamesOnlyTheAppsOwnHosts(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t)
	if err := f.srv.store.SetAppAPITokenDigest(f.appID, auth.DigestToken("sab-token")); err != nil {
		t.Fatal(err)
	}
	other, _ := f.srv.store.CreateApp("other")
	if err := f.srv.store.SetAppAPITokenDigest(other.ID, auth.DigestToken("other-token")); err != nil {
		t.Fatal(err)
	}
	get := func(tok string) gateInfo {
		w := do(t, f.h, "GET", "/app/v1/gate", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
		if w.Code != http.StatusOK {
			t.Fatalf("gate info: %d %s", w.Code, w.Body)
		}
		var g gateInfo
		json.Unmarshal(w.Body.Bytes(), &g)
		return g
	}
	if g := get("sab-token"); g.Issuer != "https://fog."+gateDomain || len(g.Audiences) != 1 || g.Audiences[0] != "https://"+gateHost {
		t.Fatalf("sab's gate info = %+v", g)
	}
	if g := get("other-token"); len(g.Audiences) != 0 {
		t.Fatalf("an app with no gated host was given audiences %v", g.Audiences)
	}
	if w := do(t, f.h, "GET", "/app/v1/gate", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("without FOG_API_TOKEN: %d", w.Code)
	}
}

// TestGateAssertionsNameOneScheme: what fog signs, what /app/v1/gate
// advertises and what fog accepts are one set of strings, scheme included,
// on an https setup and on a plain-http one alike — so no supported setup
// needs the forgeable trust-the-headers opt-out — and an assertion naming
// the other scheme is refused.
func TestGateAssertionsNameOneScheme(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, scheme, other string
		https               bool
	}{
		{"https setup", "https", "http", true},
		{"plain-http setup", "http", "https", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			if !tc.https {
				if err := f.srv.store.DeleteSecret(0, proxy.SettingDNSToken); err != nil {
					t.Fatal(err)
				}
				f.srv.gateSchemeCache = ttlCache[string]{}
			}
			if err := f.srv.store.SetAppAPITokenDigest(f.appID, auth.DigestToken("sab-token")); err != nil {
				t.Fatal(err)
			}
			w := do(t, f.h, "GET", "/app/v1/gate", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer sab-token") })
			var info gateInfo
			json.Unmarshal(w.Body.Bytes(), &info)
			wantIss, wantAud := tc.scheme+"://fog."+gateDomain, tc.scheme+"://"+gateHost
			if info.Issuer != wantIss || len(info.Audiences) != 1 || info.Audiences[0] != wantAud {
				t.Fatalf("advertised %+v, want %s / %s", info, wantIss, wantAud)
			}

			// What the gate mints matches what it advertised, whatever the
			// request's own transport (here https, as through a tunnel).
			gate := f.signIn(t, f.operator, "/")
			w = do(t, f.h, "GET", "/api/authgate", "", forwarded("/"), withCookie(gate))
			minted := w.Header().Get("Fog-Assertion")
			payload, err := f.srv.keys.Verify(minted)
			if err != nil {
				t.Fatal(err)
			}
			var c struct{ Iss, Aud string }
			json.Unmarshal(payload, &c)
			if c.Iss != wantIss || c.Aud != wantAud {
				t.Fatalf("minted iss/aud %s / %s, advertised %s / %s", c.Iss, c.Aud, wantIss, wantAud)
			}
			app, _ := f.srv.store.AppByID(f.appID)
			present := func(a string) error {
				r := httptest.NewRequest("GET", "/app/v1/files/search", nil)
				r.Header.Set("Fog-Assertion", a)
				_, err := f.srv.presentPerson(app, r)
				return err
			}
			if err := present(minted); err != nil {
				t.Fatalf("fog refused its own assertion: %v", err)
			}

			// The same assertion, signed by fog's own key, naming the other
			// scheme: refused.
			var claims map[string]any
			json.Unmarshal(payload, &claims)
			for name, mut := range map[string]func(map[string]any){
				"cross-scheme audience": func(m map[string]any) { m["aud"] = tc.other + "://" + gateHost },
				"cross-scheme issuer":   func(m map[string]any) { m["iss"] = tc.other + "://fog." + gateDomain },
			} {
				c2 := map[string]any{}
				for k, v := range claims {
					c2[k] = v
				}
				mut(c2)
				forged, err := f.srv.keys.Sign(c2, "JWT")
				if err != nil {
					t.Fatal(err)
				}
				if err := present(forged); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}
		})
	}
}

// TestAnHTTPSetupSignsInAndRefusesAPlantedCookie: on a domain install that
// serves plain http (no DNS token, so no TLS), fog set a Secure __Host-
// cookie, which no browser stores over http — nobody could sign in. The
// cookie now follows the setup's scheme: a host-only fog_session without
// Secure. A sibling host can set a domain-wide fog_session beside it; that
// one is never used — it was signed in from the planter's address — and is
// expired. And an https setup never hands out the plain cookie, whatever a
// request says of its transport.
func TestAnHTTPSetupSignsInAndRefusesAPlantedCookie(t *testing.T) {

	srv, h := newTestServer(t)
	if err := srv.store.SetSetting(store.SettingBaseDomain, gateDomain); err != nil {
		t.Fatal(err)
	}
	overHTTP := func(r *http.Request) { r.Host = "fog." + gateDomain } // no X-Forwarded-Proto: plain http
	victimAt, plantedFrom := fromPeer("192.168.1.20", ""), fromPeer("192.168.1.66", "")

	// Sign-in works, end to end.
	w := do(t, h, "POST", "/api/setup", goodSetup, overHTTP, victimAt)
	if w.Code != http.StatusOK {
		t.Fatalf("setup over http: %d %s", w.Code, w.Body)
	}
	var mine *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SecureCookieName && c.MaxAge > 0 {
			t.Fatalf("an http setup set a cookie no browser stores over http: %+v", c)
		}
		if c.Name == auth.CookieName && c.MaxAge > 0 {
			mine = c
		}
	}
	if mine == nil || mine.Secure || mine.Domain != "" {
		t.Fatalf("session cookie over http = %+v; want a host-only fog_session without Secure", mine)
	}
	me := func(opts ...func(*http.Request)) *httptest.ResponseRecorder {
		return do(t, h, "GET", "/api/auth/me", "", append([]func(*http.Request){overHTTP}, opts...)...)
	}
	if w := me(withCookie(mine), victimAt); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "admin") {
		t.Fatalf("the session over http: %d %s", w.Code, w.Body)
	}

	// The planter: a member of the household, signed in from their own
	// address, who sets their cookie domain-wide from a sibling host.
	addMember(t, srv, "kim")
	kimP, _ := srv.store.PersonByHandle("kim")
	if err := srv.store.SetPersonPassword(kimP.ID, mustHash(t, "kims long password")); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, "POST", "/api/auth/login", `{"handle":"kim","password":"kims long password"}`, overHTTP, plantedFrom)
	planted := sessionCookie(t, w)

	// Alone in the victim's browser, it is nobody.
	if w := me(withCookie(planted), victimAt); w.Code != http.StatusUnauthorized {
		t.Fatalf("a planted cookie was adopted over http: %d %s", w.Code, w.Body)
	}
	// Beside the victim's own (sent first, as a browser may), the victim is
	// who they are, and the planted copy is expired domain-wide.
	w = me(withCookie(planted), withCookie(mine), victimAt)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "admin") {
		t.Fatalf("with a planted cookie beside the victim's: %d %s", w.Code, w.Body)
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		cleared = cleared || (c.Name == auth.CookieName && c.Domain == gateDomain && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("the planted domain-wide cookie was not expired")
	}

	// An https setup: __Host- always, even for a request that says nothing
	// of TLS — it can't talk the cookie down to the plain one.
	httpsSetup(t, srv)
	w = do(t, h, "POST", "/api/auth/login", goodSetup, overHTTP, victimAt)
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName && c.MaxAge > 0 {
			t.Fatalf("an https setup handed out the plain cookie: %+v", c)
		}
	}
	var hostOnly *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SecureCookieName && c.MaxAge > 0 {
			hostOnly = c
		}
	}
	if hostOnly == nil || !hostOnly.Secure || hostOnly.Domain != "" {
		t.Fatalf("an https setup's cookie = %+v", hostOnly)
	}
	if w := me(withCookie(mine), victimAt); w.Code != http.StatusUnauthorized {
		t.Fatalf("an https setup read the plain cookie: %d", w.Code)
	}
}

// TestFogClientVerifiesTheGateOnEverySetup runs fog-client's own gate
// verifier (Ruby) against this fog's real /app/v1/gate and /oauth2/jwks, on
// an https setup and a plain-http one: an assertion fog signed verifies,
// with no opt-out. fog-client used to read the keys through OIDC discovery,
// whose issuer is always https, so on plain http nothing verified.
func TestFogClientVerifiesTheGateOnEverySetup(t *testing.T) {
	t.Parallel()

	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	gem := filepath.Join(filepath.Dir(here), "..", "..", "clients", "fog-client")
	const script = `require "fog_client"; require "fog_client/signin/gate"
v = FogClient::Signin::Gate.verifier
begin
  puts v.verify(ENV.fetch("ASSERTION")).handle
rescue FogClient::Signin::Error => e
  puts "refused: #{e.message}"
end`
	for _, https := range []bool{true, false} {
		f := newGateFixture(t)
		if !https {
			if err := f.srv.store.DeleteSecret(0, proxy.SettingDNSToken); err != nil {
				t.Fatal(err)
			}
			f.srv.gateSchemeCache = ttlCache[string]{}
		}
		if err := f.srv.store.SetAppAPITokenDigest(f.appID, auth.DigestToken("sab-token")); err != nil {
			t.Fatal(err)
		}
		gate := f.signIn(t, f.operator, "/")
		assertion := do(t, f.h, "GET", "/api/authgate", "", forwarded("/"), withCookie(gate)).Header().Get("Fog-Assertion")
		ts := httptest.NewServer(f.h)
		cmd := exec.Command(ruby, "-I", filepath.Join(gem, "lib"), "-e", script)
		cmd.Env = append(os.Environ(), "FOG_API_URL="+ts.URL, "FOG_API_TOKEN=sab-token", "ASSERTION="+assertion)
		out, err := cmd.CombinedOutput()
		ts.Close()
		if err != nil || strings.TrimSpace(string(out)) != "admin" {
			t.Fatalf("https=%v: fog-client said %q (%v)", https, out, err)
		}
	}
}
