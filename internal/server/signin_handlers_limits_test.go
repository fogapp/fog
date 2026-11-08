package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// fromPeer makes a request arrive from addr, carrying a Cf-Connecting-Ip
// when cf is set — as anyone could; fog takes no request's word for it.
func fromPeer(addr, cf string) func(*http.Request) {
	return func(r *http.Request) {
		r.RemoteAddr = addr + ":40000"
		if cf != "" {
			r.Header.Set("Cf-Connecting-Ip", cf)
		}
	}
}

// viaCaddy makes a request arrive as Caddy forwards it to fogd: from Caddy's
// container, with the proxy secret, naming client. On the tunnel listener
// client is the visitor Cloudflare names (internal/proxy, TunnelPort); on
// :443 it is the TCP peer, which for cloudflared pointed there is the host's
// bridge gateway.
func viaCaddy(t *testing.T, srv *Server, client, cf string) func(*http.Request) {
	t.Helper()
	if srv.proxy == nil {
		srv.proxy = &proxy.Proxy{Store: srv.store, Vault: srv.vault} // as in production: Caddy must prove itself
	}
	secret, err := srv.proxy.ForwardSecret()
	if err != nil {
		t.Fatal(err)
	}
	return func(r *http.Request) {
		r.RemoteAddr = "172.18.0.2:40000"
		r.Header.Set(proxy.HeaderSecret, secret)
		r.Header.Set(proxy.HeaderClientIP, client)
		if cf != "" {
			r.Header.Set("Cf-Connecting-Ip", cf)
		}
	}
}

// withCookies carries every cookie a response set.
func withCookies(w *httptest.ResponseRecorder) func(*http.Request) {
	return func(r *http.Request) {
		for _, c := range w.Result().Cookies() {
			if c.MaxAge >= 0 && c.Value != "" {
				r.AddCookie(c)
			}
		}
	}
}

// The attack (fourth review, 2): every Cloudflare Tunnel visitor reached
// Caddy at :443 from the host's bridge gateway, which is on an app
// container's network — so fog signed them in and then refused the session
// on every request, as a container's. Now cloudflared reaches Caddy's
// tunnel listener, which names the visitor; each visitor is their own
// address for the pin and the limits; and the gateway is never anyone's
// identity: a tunnel still pointed at :443 is refused at sign-in, with the
// operator told where it belongs, and a container is a container whatever
// Cf-Connecting-Ip it sends.
func TestTunnelVisitorsAreEachTheirOwn(t *testing.T) {

	srv, h := newTestServer(t)
	setup(t, h)
	srv.signinLimitsState.containerNets = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")} }
	login := func(handle, password string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(`{"handle":%q,"password":%q}`, handle, password)
		return do(t, h, "POST", "/api/auth/login", body, append([]func(*http.Request){onFogHost}, opts...)...)
	}
	passkeyBegin := func(opts ...func(*http.Request)) int {
		t.Helper()
		return do(t, h, "POST", "/api/people/login/passkey/begin", "", append([]func(*http.Request){onFogHost}, opts...)...).Code
	}

	// Through the tunnel listener: signed in, and the session works.
	visitor := viaCaddy(t, srv, "198.51.100.7", "198.51.100.7")
	w := login("admin", "correct horse battery", visitor)
	if w.Code != http.StatusOK {
		t.Fatalf("a tunnel visitor's sign-in: %d %s", w.Code, w.Body)
	}
	if c := do(t, h, "GET", "/api/people/me", "", onFogHost, visitor, withCookies(w)).Code; c != http.StatusOK {
		t.Fatalf("a tunnel visitor's session refused: %d", c)
	}
	// Signed in as the visitor, never as the host.
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name != auth.SecureCookieName || c.Value == "" {
			continue
		}
		found = true
		sess, _, err := srv.store.SessionPerson(auth.DigestToken(c.Value))
		if err != nil || sess.RemoteAddr != "198.51.100.7" {
			t.Fatalf("the tunnel visitor's session was signed in from %q (%v)", sess.RemoteAddr, err)
		}
	}
	if !found {
		t.Fatal("no session cookie for the tunnel visitor")
	}

	// cloudflared still pointed at :443: the client is the bridge gateway.
	// Refused at sign-in, and told why — never signed in as the gateway.
	w = login("admin", "correct horse battery", viaCaddy(t, srv, "172.18.0.1", "198.51.100.7"))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "caddy/tunnel/caddy.sock") {
		t.Fatalf("a sign-in arriving as the bridge gateway: %d %s", w.Code, w.Body)
	}
	// An app container, whatever it claims, is a container.
	if c := login("admin", "correct horse battery", viaCaddy(t, srv, "172.18.0.5", "198.51.100.7")).Code; c != http.StatusForbidden {
		t.Fatalf("a container's sign-in: %d", c)
	}
	if c := login("admin", "correct horse battery", fromPeer("172.18.0.5", "198.51.100.7")).Code; c != http.StatusForbidden {
		t.Fatalf("a container's sign-in straight to fogd: %d", c)
	}

	// Each visitor spends their own budget and no one else's.
	l := srv.signinLimits()
	for range addressFailures + 5 {
		login("nobody", "wrong guess", visitor)
	}
	if !l.address.Exhausted("198.51.100.7") {
		t.Fatal("a tunnel visitor's own address budget never ran out")
	}
	other := viaCaddy(t, srv, "203.0.113.5", "203.0.113.5")
	if c := login("admin", "correct horse battery", other).Code; c != http.StatusOK {
		t.Fatalf("a different tunnel visitor's correct password: %d", c)
	}
	if c := passkeyBegin(other); c != http.StatusOK {
		t.Fatalf("a different tunnel visitor's passkey sign-in: %d", c)
	}
	// Password failures never touch the passkey bucket, even the attacker's.
	if c := passkeyBegin(visitor); c != http.StatusOK {
		t.Fatalf("password guesses held up a passkey from the same address: %d", c)
	}

	// A Cf-Connecting-Ip a client sends itself is nobody's word: from the
	// host's own address it buys no bucket of its own (one stranger bucket,
	// ten times an address's, never unlimited) ...
	for i := range addressFailures*sharedScale + 5 {
		login("nobody", "wrong guess", fromPeer("127.0.0.1", fmt.Sprintf("10.9.%d.%d", i/250, i%250)))
	}
	if !l.sharedAddress.Exhausted(sharedAddr) {
		t.Fatal("the host's own address, rotating Cf-Connecting-Ip, is unlimited")
	}
	// ... and from a real address, rotating it buys no fresh budgets.
	for i := range addressFailures + 5 {
		login("nobody", "wrong guess", fromPeer("192.0.2.50", fmt.Sprintf("10.9.%d.%d", i/250, i%250)))
	}
	if !l.address.Exhausted("192.0.2.50") {
		t.Fatal("a forged Cf-Connecting-Ip from a real address evaded its limit")
	}
	if n := l.address.Len(); n > 4 {
		t.Fatalf("%d address keys: a forged header made buckets of its own", n)
	}
}

// TestCredentialsChangeOnlyWithAFreshProof is item 6: any session, a chat
// device's bearer token included, could add a passkey or a first password.
// Now only a browser's own session may change how its person signs in, and
// only within minutes of proving itself.
func TestCredentialsChangeOnlyWithAFreshProof(t *testing.T) {

	srv, h := newTestServer(t)
	op := setup(t, h)
	stale := func(c *http.Cookie) {
		t.Helper()
		execSQL(t, srv.store, fmt.Sprintf(`UPDATE people_sessions SET auth_time = '%s' WHERE token_digest = '%s'`,
			time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), auth.DigestToken(c.Value)))
	}
	code := func(w interface{ Result() *http.Response }) int { return w.Result().StatusCode }

	// A session proven an hour ago is sent to confirm first.
	stale(op)
	w := do(t, h, "POST", "/api/people/me/passkeys/begin", "", withCookie(op))
	if code(w) != http.StatusConflict || !strings.Contains(w.Body.String(), "reauth_required") {
		t.Fatalf("adding a passkey on a stale session: %d %s", w.Code, w.Body)
	}
	creds := decode(t, do(t, h, "GET", "/api/people/me", "", withCookie(op)))["credentials"].([]any)
	id := int(creds[0].(map[string]any)["id"].(float64))
	if w := do(t, h, "DELETE", fmt.Sprintf("/api/people/me/credentials/%d", id), "", withCookie(op)); w.Code != http.StatusConflict {
		t.Fatalf("removing a credential on a stale session: %d", w.Code)
	}
	// Confirming makes it fresh: past the proof check (to the passkey
	// ceremony, which plain http then refuses as it always has).
	if w := do(t, h, "POST", "/api/people/reauth/password", `{"password":"correct horse battery"}`, withCookie(op)); w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/people/me/passkeys/begin", "", withCookie(op)); w.Code == http.StatusConflict || w.Code == http.StatusForbidden {
		t.Fatalf("after confirming: %d %s", w.Code, w.Body)
	}

	// The same token as a bearer — a chat device's way in — manages nothing
	// (a web sign-in isn't even a session as a bearer: resolvePerson).
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+op.Value) }
	for path, body := range map[string]string{
		"/api/people/me/passkeys/begin": "",
		"/api/people/me/password":       `{"current":"correct horse battery","password":"another long phrase"}`,
	} {
		if w := do(t, h, "POST", path, body, bearer); w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
			t.Errorf("%s with a bearer token: %d %s", path, w.Code, w.Body)
		}
	}
	// Nor does a chat device's own session, even from a cookie.
	p, _ := srv.store.PersonByHandle("admin")
	dev, err := srv.store.CreatePersonDevice(p.ID, "phone", "ios", []byte("device-key-0123456789abcdef0123456"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, digest := auth.NewToken()
	if _, err := srv.store.CreatePersonSession(p.ID, dev, digest, "chat", time.Hour, "", "", []string{"pop"}); err != nil {
		t.Fatal(err)
	}
	// (Outside chat it is no one at all: signin_handlers_device.go.)
	if w := do(t, h, "POST", "/api/people/me/passkeys/begin", "", withCookie(&http.Cookie{Name: auth.CookieName, Value: tok})); w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
		t.Fatalf("a chat device's session added a passkey: %d", w.Code)
	}

	// A first password is a new way in: fresh proof, as a passkey is.
	kim := addMember(t, srv, "kim")
	stale(kim)
	if w := do(t, h, "POST", "/api/people/me/password", `{"password":"kims first password"}`, withCookie(kim)); w.Code != http.StatusConflict {
		t.Fatalf("a first password on a stale session: %d %s", w.Code, w.Body)
	}
}

// TestAPasskeyCeremonyCantBeEvicted: the in-flight challenges used to live
// in a table capped at 1024, evicting the oldest, and begin was limited only
// per full address — so an IPv6 /64, or the host's own address with no
// tunnel, could flush a victim's challenge mid-sign-in. Now a ceremony lives
// in the id the victim's browser holds, and both floods hit their limits.
func TestAPasskeyCeremonyCantBeEvicted(t *testing.T) {

	srv, h := newTestServer(t)
	setup(t, h)
	if err := srv.store.SetSetting(store.SettingBaseDomain, gateDomain); err != nil {
		t.Fatal(err)
	}
	begin := func(addr string) (int, string) {
		w := do(t, h, "POST", "/api/people/login/passkey/begin", "", onFogHost, fromPeer(addr, ""))
		if w.Code != http.StatusOK {
			return w.Code, ""
		}
		return w.Code, decode(t, w)["ceremony"].(string)
	}
	code, victim := begin("198.51.100.10")
	if code != http.StatusOK {
		t.Fatalf("the victim's begin: %d", code)
	}
	for name, addr := range map[string]func(int) string{
		"an IPv6 /64":         func(i int) string { return fmt.Sprintf("[2001:db8:1:2::%x]", i+1) },
		"the host, no tunnel": func(int) string { return "127.0.0.1" },
	} {
		limited := false
		for i := range 1100 {
			if c, _ := begin(addr(i)); c == http.StatusTooManyRequests {
				limited = true
			}
		}
		if !limited {
			t.Errorf("%s began 1100 ceremonies unlimited", name)
		}
	}
	// The victim's ceremony is still theirs: the finish gets past it to the
	// assertion (which this one isn't), not "this sign-in expired".
	w := do(t, h, "POST", "/api/people/login/passkey/finish", `{"ceremony":"`+victim+`","response":{}}`, onFogHost, fromPeer("198.51.100.10", ""))
	if strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("the victim's ceremony was evicted: %d %s", w.Code, w.Body)
	}
}

// The attack (fourth review, 1): spend the operator's account budget, then
// flood made-up handles from many addresses until the limiter is full and
// forgets the operator's bucket — a fresh ten guesses. Now a made-up handle
// never makes a key (they share one bucket), an address out of budget never
// reaches the account limiter, and a live bucket is never forgotten.
func TestMadeUpHandlesCantResetAnAccountsBudget(t *testing.T) {

	srv, h := newTestServer(t)
	setup(t, h)
	login := func(handle, password, addr string) int {
		t.Helper()
		body := fmt.Sprintf(`{"handle":%q,"password":%q}`, handle, password)
		return do(t, h, "POST", "/api/auth/login", body, onFogHost, fromPeer(addr, "")).Code
	}
	for i := range accountFailures {
		login("admin", "wrong guess", fmt.Sprintf("198.51.100.%d", i+1))
	}
	if c := login("admin", "correct horse battery", "198.51.100.200"); c != http.StatusTooManyRequests {
		t.Fatalf("the spent account still let a guess through: %d", c)
	}
	refused := 0
	for i := range 2000 {
		if login(fmt.Sprintf("nobody%d", i), "wrong guess", fmt.Sprintf("203.0.%d.%d", i/250, i%250+1)) == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused < 2000-accountFailures*sharedScale {
		t.Fatalf("only %d of 2000 made-up handles refused: they have budgets of their own", refused)
	}
	l := srv.signinLimits()
	if n := l.account.Len(); n != 1 {
		t.Fatalf("the account limiter holds %d keys; made-up handles made keys", n)
	}
	if c := login("admin", "correct horse battery", "198.51.100.201"); c != http.StatusTooManyRequests {
		t.Fatalf("after the flood the operator's spent budget started over: %d", c)
	}
	// An address out of budget is refused before any account is charged.
	for range addressFailures {
		login("nobody", "wrong guess", "192.0.2.77")
	}
	before := l.account.Len()
	login("someone-new", "wrong guess", "192.0.2.77")
	if l.account.Len() != before {
		t.Fatal("a refused attempt reached the account limiter")
	}
}

// The attack (fourth review, 4): hold the hashing line full, and every
// refusal for "busy" gave the attempt back — so holding it cost the holder
// nothing. The line itself is fair now (auth.TestTheHashingLineIsFair);
// here, a busy refusal keeps the address's charge and gives back only the
// account's, whose owner's password was never checked.
func TestABusyRefusalStillCostsTheAddress(t *testing.T) {

	srv, _ := newTestServer(t)
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "203.0.113.9:1"
	bs := srv.credentialBuckets(r, loginAccountKey("admin"))
	if len(bs) != 2 || !bs[0].addr || bs[1].addr {
		t.Fatalf("buckets %+v: want the address first, then the account", bs)
	}
	l := srv.signinLimits()
	for range accountFailures {
		w := httptest.NewRecorder()
		if refuseLimited(w, "x", bs...) {
			t.Fatal("refused with budget left")
		}
		if !busyHashing(w, auth.ErrBusy, bs...) || w.Code != http.StatusTooManyRequests {
			t.Fatal("busy was not a 429")
		}
	}
	if l.account.Exhausted(loginAccountKey("admin")) {
		t.Fatal("busy refusals spent the account's budget")
	}
	for range addressFailures - accountFailures {
		w := httptest.NewRecorder()
		refuseLimited(w, "x", bs...)
		busyHashing(w, auth.ErrBusy, bs...)
	}
	if !l.address.Exhausted("203.0.113.9") {
		t.Fatal("busy refusals were free for the address holding the line")
	}
	if c := srv.hashCaller(r, false); c.Key != "203.0.113.9" || c.Known {
		t.Fatalf("hash caller %+v", c)
	}
}

// fog can't change a tunnel's service (the tunnel is made in Cloudflare's
// dashboard; fog holds its connector token and a DNS-only API token), so it
// notes which way tunnel visitors come in, for the issue that tells the
// operator (internal/issues, checkTunnelService). Only Caddy's word counts:
// a marker without the proxy secret, or a LAN client's own
// Cf-Connecting-Ip, records nothing.
func TestFogNotesWhichWayTunnelVisitorsCome(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	setup(t, h)
	srv.signinLimitsState.containerNets = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")} }
	srv.proxy = &proxy.Proxy{Store: srv.store, Vault: srv.vault} // as in production: Caddy must prove itself
	l := srv.signinLimits()
	l.addrMu.Lock()
	l.addrs, l.addrsAt = map[string]bool{"172.18.0.1": true}, time.Now() // this host's bridge gateway
	l.addrMu.Unlock()
	via := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set(proxy.HeaderVia, v) }
	}
	seen := func(key string) string {
		v, _ := srv.store.GetSetting(key)
		return v
	}
	// Forged, from the LAN: nothing.
	do(t, h, "GET", "/api/visit/peers", "", fromPeer("192.168.1.30", "198.51.100.7"), via(proxy.ViaTunnel))
	do(t, h, "GET", "/api/visit/peers", "", viaCaddy(t, srv, "192.168.1.30", "192.168.1.30"), via(proxy.ViaEdge))
	if seen(proxy.SettingTunnelSeenListener) != "" || seen(proxy.SettingTunnelSeenEdge) != "" {
		t.Fatal("a LAN client's own headers recorded a tunnel")
	}
	// A tunnel still on :443: Caddy names the bridge gateway.
	do(t, h, "GET", "/api/visit/peers", "", viaCaddy(t, srv, "172.18.0.1", "198.51.100.7"), via(proxy.ViaEdge))
	if seen(proxy.SettingTunnelSeenEdge) == "" || seen(proxy.SettingTunnelSeenListener) != "" {
		t.Fatal("a tunnel visitor on :443 was not noted")
	}
	// Through the tunnel listener.
	do(t, h, "GET", "/api/visit/peers", "", viaCaddy(t, srv, "198.51.100.7", "198.51.100.7"), via(proxy.ViaTunnel))
	if seen(proxy.SettingTunnelSeenListener) == "" {
		t.Fatal("a tunnel visitor through the listener was not noted")
	}
}

// The attack (fifth review, 2): strangers' addresses filled the limiter
// apps and the household shared — userinfo charged every caller's address
// before looking at the token — and then an app's userinfo, a member's
// authorize, any new key at all, was refused. Now a caller fog knows only
// by address is limited among strangers, apart; a live token or a
// signed-in person never touches that limiter; and a household address
// keeps a budget of its own when strangers have filled theirs.
func TestStrangersCantLockOutAppsOrTheHousehold(t *testing.T) {
	t.Parallel()

	f := newIdP(t, signin.Manifest{})
	tok := f.signInRP(t)
	h := f.srv.Handler() // each request from the address it names, not the fixture's one client
	for _, purpose := range []string{"userinfo", "authorize"} {
		f.srv.stranger(purpose).SetRoom(20)
	}
	// A member signed in from home.
	op, _ := f.srv.store.PersonByHandle("admin")
	home := httptest.NewRequest("GET", "/", nil)
	home.RemoteAddr = "192.168.1.20:1"
	if _, _, err := f.srv.mintPersonSession(home, op.ID, 0, "web"); err != nil {
		t.Fatal(err)
	}
	userinfo := func(bearer string, opts ...func(*http.Request)) int {
		t.Helper()
		return do(t, h, "GET", "/oauth2/userinfo", "", append(opts, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+bearer)
		})...).Code
	}
	// Strangers, each from an address of their own, with made-up tokens,
	// until their room and their shared bucket are spent.
	locked := false
	for i := 0; i < 2000 && !locked; i++ {
		locked = userinfo("made-up", fromPeer(fmt.Sprintf("2001:db8:%x::1", i), "")) == http.StatusTooManyRequests
	}
	if !locked {
		t.Fatal("strangers were never limited")
	}
	if c := userinfo("made-up", fromPeer("2001:db8:ffff::1", "")); c != http.StatusTooManyRequests {
		t.Fatalf("a new stranger once strangers are full: %d", c)
	}
	// The app, from an address strangers never saw: its live token is
	// charged to its client, never to strangers.
	if c := userinfo(tok.AccessToken, fromPeer("203.0.113.200", "")); c != http.StatusOK {
		t.Fatalf("the app's userinfo after the flood: %d", c)
	}
	// A household address — one a live session was signed in from — keeps
	// a budget of its own among strangers.
	if c := userinfo("made-up", fromPeer("192.168.1.20", "")); c != http.StatusUnauthorized {
		t.Fatalf("the household's address once strangers are full: %d", c)
	}
}
