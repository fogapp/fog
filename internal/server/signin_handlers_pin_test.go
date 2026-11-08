package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
)

// bareIPOperator sets a fog up from the operator's browser at 192.168.1.20
// on its bare address, and returns the plain session cookie it was given.
func bareIPOperator(t *testing.T) (*Server, http.Handler, *http.Cookie) {
	t.Helper()
	srv, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup", goodSetup, fromPeer("192.168.1.20", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	return srv, h, sessionCookie(t, w)
}

func secretsStatus(t *testing.T, h http.Handler, opts ...func(*http.Request)) int {
	t.Helper()
	return do(t, h, "GET", "/api/system/secrets", "", opts...).Code
}

// TestARenamedCookieIsStillPinned is the first third-review finding: the
// pin was waived for a token arriving as __Host-fog_session — a name the
// client chooses — and a request became "https" by saying so in
// X-Forwarded-Proto, or became fog's host by naming it. An app renamed the
// operator's plain cookie and read the secrets. The pin now follows how fog
// issued the session, and X-Forwarded-* is believed only from Caddy.
func TestARenamedCookieIsStillPinned(t *testing.T) {

	srv, h, plain := bareIPOperator(t)
	srv.proxy = &proxy.Proxy{Store: srv.store, Vault: srv.vault} // as in production: Caddy must prove itself
	if err := srv.store.SetSetting(store.SettingBaseDomain, gateDomain); err != nil {
		t.Fatal(err)
	}
	httpsSetup(t, srv)
	renamed := &http.Cookie{Name: auth.SecureCookieName, Value: plain.Value}
	asFog := func(r *http.Request) {
		r.Host = "fog." + gateDomain
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	for name, opts := range map[string][]func(*http.Request){
		"renamed, from the app's container":     {withCookie(renamed), asFog, fromPeer("172.18.0.5", "")},
		"renamed, from a LAN machine":           {withCookie(renamed), asFog, fromPeer("192.168.1.66", "")},
		"renamed, Host fog.<domain>, no header": {withCookie(renamed), func(r *http.Request) { r.Host = "fog." + gateDomain }, fromPeer("172.18.0.5", "")},
	} {
		if c := secretsStatus(t, h, opts...); c == http.StatusOK {
			t.Errorf("%s read the system secrets", name)
		}
		if c := do(t, h, "PUT", "/api/settings", `{"server_name":"pwned"}`, opts...).Code; c == http.StatusOK {
			t.Errorf("%s changed a setting", name)
		}
	}

	// Without Caddy's secret, X-Forwarded-Proto is dropped: a "https" sign-in
	// over plain http to a bare address is handed the plain, pinned cookie.
	w := do(t, h, "POST", "/api/auth/login", goodSetup, fromPeer("192.168.1.30", ""), func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SecureCookieName && c.MaxAge > 0 {
			t.Fatalf("an unproven X-Forwarded-Proto earned a __Host- session: %+v", c)
		}
	}
	// With Caddy's secret it stands.
	secret, err := srv.proxy.ForwardSecret()
	if err != nil {
		t.Fatal(err)
	}
	w = do(t, h, "POST", "/api/auth/login", goodSetup, fromPeer("172.30.0.2", ""), func(r *http.Request) {
		r.Host = "fog." + gateDomain
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set(proxy.HeaderSecret, secret)
		r.Header.Set(proxy.HeaderClientIP, "192.168.1.30")
	})
	host := false
	for _, c := range w.Result().Cookies() {
		host = host || (c.Name == auth.SecureCookieName && c.MaxAge > 0)
	}
	if !host {
		t.Fatalf("a sign-in through Caddy over https got no __Host- session: %d %v", w.Code, w.Result().Cookies())
	}
}

// TestAWebChatDeviceSessionIsPinned is the second: a web chat device's
// session is set as the browser's cookie — plain fog_session on a bare
// address — and device sessions were exempt from the pin. A native client's
// bearer token stays usable from anywhere, but never as a cookie; and no
// session of any kind reaches fog from an app container's network.
func TestAWebChatDeviceSessionIsPinned(t *testing.T) {
	t.Parallel()

	srv, h, _ := bareIPOperator(t)
	op, _ := srv.store.PersonByHandle("admin")
	dev, err := srv.store.CreatePersonDevice(op.ID, "browser", "web", []byte("device-key-web-0123456789abcdef01"), nil)
	if err != nil {
		t.Fatal(err)
	}
	native, err := srv.store.CreatePersonDevice(op.ID, "phone", "ios", []byte("device-key-ios-0123456789abcdef01"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(deviceID int64, kind string) string {
		r := httptest.NewRequest("POST", "/api/chat/me/devices", nil)
		r.RemoteAddr = "192.168.1.20:40000"
		tok, _, err := srv.mintPersonSession(r, op.ID, deviceID, kind, "pop")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	web, phone := mint(dev, "web"), mint(native, "native")
	webCookie := &http.Cookie{Name: auth.CookieName, Value: web}
	bearer := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}
	// A device's session reaches chat alone (signin_handlers_device.go).
	if c := do(t, h, "GET", "/api/chat/me", "", withCookie(webCookie), fromPeer("192.168.1.20", "")).Code; c != http.StatusOK {
		t.Fatalf("the web device's own browser, in chat: %d", c)
	}
	if c := do(t, h, "GET", "/api/auth/me", "", withCookie(webCookie), fromPeer("192.168.1.20", "")).Code; c != http.StatusUnauthorized {
		t.Fatalf("a device's session outside chat: %d", c)
	}
	for name, opts := range map[string][]func(*http.Request){
		"the web device's cookie from the app's container": {withCookie(webCookie), fromPeer("172.18.0.5", "")},
		"the web device's cookie renamed __Host-":          {withCookie(&http.Cookie{Name: auth.SecureCookieName, Value: web}), fromPeer("172.18.0.5", "")},
		"the web device's cookie as a bearer":              {bearer(web), fromPeer("172.18.0.5", "")},
		"a native token presented as a cookie":             {withCookie(&http.Cookie{Name: auth.CookieName, Value: phone}), fromPeer("192.168.1.20", "")},
	} {
		if c := do(t, h, "GET", "/api/chat/me", "", opts...).Code; c == http.StatusOK {
			t.Errorf("%s was accepted", name)
		}
	}
	// The phone moves networks and keeps working — in chat, the only place
	// its token reaches (signin_handlers_device.go).
	if c := do(t, h, "GET", "/api/chat/me", "", bearer(phone), fromPeer("10.20.30.40", "")).Code; c != http.StatusOK {
		t.Fatalf("a native client after moving networks: %d", c)
	}
	// But nothing at all from an app container's network.
	srv.signinLimitsState.containerNets = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")} }
	if c := secretsStatus(t, h, bearer(phone), fromPeer("172.18.0.5", "")); c == http.StatusOK {
		t.Fatal("a session reached the admin routes from an app container's network")
	}
}

// TestTheAdminPasswordRouteNeedsAFreshProof is the third: the operator's
// "set password" for someone needed no fresh proof, took device and bearer
// sessions, and worked on the operator's own account.
func TestTheAdminPasswordRouteNeedsAFreshProof(t *testing.T) {

	srv, h := newTestServer(t)
	op := setup(t, h)
	addMember(t, srv, "kim")
	kim, _ := srv.store.PersonByHandle("kim")
	me, _ := srv.store.PersonByHandle("admin")
	set := func(id int64, opts ...func(*http.Request)) int {
		return do(t, h, "POST", fmt.Sprintf("/api/people/%d/password", id), `{"password":"a new long password"}`, opts...).Code
	}
	if c := set(me.ID, withCookie(op)); c != http.StatusForbidden {
		t.Errorf("the operator's own password through the admin route: %d", c)
	}
	if c := set(kim.ID, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+op.Value) }); c != http.StatusForbidden && c != http.StatusUnauthorized {
		t.Errorf("a bearer token set a password: %d", c)
	}
	execSQL(t, srv.store, fmt.Sprintf(`UPDATE people_sessions SET auth_time = '%s' WHERE token_digest = '%s'`,
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), auth.DigestToken(op.Value)))
	if c := set(kim.ID, withCookie(op)); c != http.StatusConflict {
		t.Errorf("a stale session set a password: %d", c)
	}
	if c := do(t, h, "POST", "/api/people/reauth/password", `{"password":"correct horse battery"}`, withCookie(op)).Code; c != http.StatusOK {
		t.Fatalf("confirm: %d", c)
	}
	if c := set(kim.ID, withCookie(op)); c != http.StatusOK {
		t.Fatalf("a freshly proven operator: %d", c)
	}
}

// TestParallelGuessesMeetTheBudget is the fourth: checking the budget and
// charging it were two steps, so 60 parallel wrong passwords all got
// checked against a budget of ten. Now each reserves before it is checked.
func TestParallelGuessesMeetTheBudget(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)
	var wg sync.WaitGroup
	var mu sync.Mutex
	checked := 0
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := do(t, h, "POST", "/api/auth/login", `{"handle":"admin","password":"wrong guess"}`, fromPeer(fmt.Sprintf("10.2.%d.%d", i/200, i%200), "")).Code
			if c == http.StatusUnauthorized {
				mu.Lock()
				checked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if checked > accountFailures {
		t.Fatalf("%d parallel guesses were checked against a budget of %d", checked, accountFailures)
	}
}

// TestAHugeClientIDCostsNothing is the fifth: the per-client limiter keyed
// on the raw client_id, as long as the form, so each request could park
// megabytes in the limiter. The form is capped, and the key is fog's own.
func TestAHugeClientIDCostsNothing(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	huge := strings.Repeat("x", 1<<20)
	for range 20 {
		do(t, h, "POST", "/oauth2/par", "client_id="+huge+"&client_secret=s", func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		})
		do(t, h, "POST", "/oauth2/token", "grant_type=authorization_code", func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.SetBasicAuth(huge[:100000], "s")
		})
	}
	if n := srv.appAPILimiter.Len(); n > 2 {
		t.Fatalf("the limiter holds %d keys after strangers' client ids", n)
	}
}

// TestThePinReadsAnyAddressForm is the seventh: an address with a zone
// (fe80::1%lo0) or IPv4-mapped never matched itself, locking its user out.
func TestThePinReadsAnyAddressForm(t *testing.T) {
	t.Parallel()

	for _, c := range [][2]string{
		{"fe80::1%lo0", "fe80::1%lo0"},
		{"[fe80::1%lo0]", "fe80::1%lo0"},
		{"::ffff:192.168.1.20", "192.168.1.20"},
		{"[2001:db8::1]", "2001:db8::2"}, // the same /64
	} {
		if !sameClient(c[0], c[1]) {
			t.Errorf("%s and %s are one client", c[0], c[1])
		}
	}
	if sameClient("2001:db8::1", "2001:db9::1") || sameClient("192.168.1.20", "192.168.1.21") {
		t.Error("two clients matched")
	}
}

// sameTestClient serves h as the in-process test client sees it: a request
// over a real connection arrives from 192.0.2.1, the address
// httptest.NewRequest gives. A test that signs in in-process and then talks
// over a socket is one client doing both, as a browser is, and a session
// pinned to its address (resolveSession) must see one address.
func sameTestClient(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "192.0.2.1:1234"
		h.ServeHTTP(w, r)
	})
}

// The attack (fourth review, 3): on a bare address the session cookie is a
// plain fog_session, and anything sharing the host — an app on another port
// of the address — can plant one beside fog's. A cookie with a longer Path
// is sent first, and the first one that resolved won; a host-issued session
// was never pinned, so mallory's own session, carried off her https sign-in
// and planted at Path=/api, made the operator act as mallory. Now a host
// session outside its __Host- cookie is pinned; of several that would count
// only the one bound to the browser cookie fog set wins, else none; and the
// plants are expired at every path the request could have carried them
// from.
func TestAPlantedSessionDoesNotWin(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup", goodSetup, fromPeer("192.168.1.20", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	var victim []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.MaxAge > 0 {
			victim = append(victim, c)
		}
	}
	if len(victim) != 2 {
		t.Fatalf("setup set %d cookies, want the session and its browser cookie: %v", len(victim), victim)
	}
	mallory, err := srv.store.CreatePerson("mallory", "Mallory", store.PersonRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(addr, issued string) string {
		t.Helper()
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr + ":1"
		token, id, err := srv.mintPersonSession(r, mallory, 0, "web")
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.SetSessionIssuedAs(id, issued); err != nil {
			t.Fatal(err)
		}
		return token
	}
	me := func(planted ...*http.Cookie) (*httptest.ResponseRecorder, string) {
		t.Helper()
		w := do(t, h, "GET", "/api/people/me", "", fromPeer("192.168.1.20", ""), func(r *http.Request) {
			for _, c := range append(planted, victim...) { // the longer Path first, as a browser sends them
				r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
			}
		})
		var body struct {
			Person struct{ Handle string } `json:"person"`
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		return w, body.Person.Handle
	}

	// Mallory's own https sign-in, carried to the bare address and planted.
	hostSession := &http.Cookie{Name: auth.CookieName, Value: mint("192.168.1.66", store.SessionHost), Path: "/api"}
	if w, who := me(hostSession); who != "admin" {
		t.Fatalf("a planted host session made the operator %q (%d)", who, w.Code)
	}
	// Mallory signed in from the operator's own address (one NAT, one /64):
	// her session passes the pin, but not the browser binding.
	sameAddr := &http.Cookie{Name: auth.CookieName, Value: mint("192.168.1.20", store.SessionPlain), Path: "/api"}
	w, who := me(sameAddr)
	if who != "admin" {
		t.Fatalf("a planted session from the same address made the operator %q (%d)", who, w.Code)
	}
	// And it is expired where it could have been set.
	expired := map[string]bool{}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName && c.MaxAge < 0 {
			expired[c.Path] = true
		}
	}
	for _, p := range []string{"/api", "/api/", "/api/people", "/api/people/", "/api/people/me"} {
		if !expired[p] {
			t.Errorf("a plant at Path=%s is not expired: %v", p, expired)
		}
	}
	if expired["/"] {
		t.Error("fog's own cookie at / was expired")
	}
	// With a browser cookie planted too, fog can't tell which it set: it
	// trusts neither — and still never mallory.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.20:1"
	rec := httptest.NewRecorder()
	token := mint("192.168.1.20", store.SessionPlain)
	srv.bindBrowser(rec, r, token)
	var plantedBrowser *http.Cookie
	for _, c := range rec.Result().Cookies() {
		plantedBrowser = &http.Cookie{Name: c.Name, Value: c.Value, Path: "/api"}
	}
	if w, who := me(&http.Cookie{Name: auth.CookieName, Value: token, Path: "/api"}, plantedBrowser); who != "" || w.Code != http.StatusUnauthorized {
		t.Fatalf("a fully planted pair: %d as %q", w.Code, who)
	}
	// Alone, the operator's own still works.
	if _, who := me(); who != "admin" {
		t.Fatalf("the operator's own session: %q", who)
	}
}

// The attack (fifth review, 5): the admin route reset the operator's own
// chat identity — revoking their devices and keys — with only the checks a
// change to someone else's gets. As with the password route, the
// operator's own account is refused there; a member's still resets.
func TestTheOperatorCantResetTheirOwnIdentityAsAdmin(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	op := setup(t, h)
	addMember(t, srv, "kim")
	kim, _ := srv.store.PersonByHandle("kim")
	me, _ := srv.store.PersonByHandle("admin")
	reset := func(id int64) int {
		return do(t, h, "POST", fmt.Sprintf("/api/people/%d/reset-identity", id), "", withCookie(op)).Code
	}
	if c := reset(me.ID); c != http.StatusForbidden {
		t.Fatalf("the operator's own identity through the admin route: %d", c)
	}
	if c := reset(kim.ID); c != http.StatusOK {
		t.Fatalf("a member's identity: %d", c)
	}
}
