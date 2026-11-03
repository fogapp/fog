package server

import (
	"crypto/ed25519"
	"crypto/rand"
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
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
)

func loginAs(t *testing.T, h http.Handler, handle, password, addr string) int {
	t.Helper()
	body := fmt.Sprintf(`{"handle":%q,"password":%q}`, handle, password)
	return do(t, h, "POST", "/api/auth/login", body, onFogHost, fromPeer(addr, "")).Code
}

// The attack (sixth review, 1): a right password cost nothing, so 36
// workers looping one guest's password held the hashing line and the
// operator's sign-in was refused; each success also added a 30-day session
// row. Now one account holds one place in the line, right passwords are
// charged to the account's and the address's success budgets, and a
// person's sessions are minted at a bounded rate and kept to a bounded
// number.
func TestAValidPasswordLoopedCantHoldSignIn(t *testing.T) {
	srv, h := newTestServer(t)
	setup(t, h)
	guest, err := srv.store.CreatePerson("guest", "Guest", store.PersonRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetPersonPassword(guest, mustHash(t, "guests long password")); err != nil {
		t.Fatal(err)
	}
	// 36 workers loop the guest's right password while the operator signs
	// in: the operator is never refused.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 36 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				loginAs(t, h, "guest", "guests long password", fmt.Sprintf("203.0.113.%d", i+1))
			}
		}()
	}
	for i := range 5 {
		if c := loginAs(t, h, "admin", "correct horse battery", fmt.Sprintf("192.168.1.%d", 20+i)); c != http.StatusOK {
			close(stop)
			wg.Wait()
			t.Fatalf("the operator's sign-in %d beside a looping guest: %d", i, c)
		}
	}
	close(stop)
	wg.Wait()
	// Right passwords are charged: the guest's success budget runs out, and
	// then a right password is refused before any hash.
	refused := false
	for i := range accountSuccesses + 1 {
		refused = loginAs(t, h, "guest", "guests long password", fmt.Sprintf("198.51.100.%d", i+1)) == http.StatusTooManyRequests || refused
	}
	if !refused {
		t.Fatal("right passwords for one account were never limited")
	}
	// And the guest holds a bounded number of sessions.
	var n int
	srv.store.DB().QueryRow(`SELECT COUNT(*) FROM people_sessions WHERE person_id = ?`, guest).Scan(&n)
	if n > accountSuccesses || n > maxSessionsPerPerson {
		t.Fatalf("the loop left %d sessions", n)
	}
	// Minting past the per-person rate is refused; past the cap, the
	// oldest go.
	r := httptest.NewRequest("GET", "/", nil)
	refused = false
	srv.signinLimits().sessionMint.Reset(fmt.Sprint(guest))
	for range sessionMints + 1 {
		if _, _, err := srv.mintPersonSession(r, guest, 0, "web"); err == errSessionRate {
			refused = true
		}
	}
	if !refused {
		t.Fatal("sessions minted for one person without limit")
	}
	for range 3 {
		srv.signinLimits().sessionMint.Reset(fmt.Sprint(guest))
		for range sessionMints {
			srv.mintPersonSession(r, guest, 0, "web")
		}
	}
	// Sessions in use are never ended to make room; once they go idle, the
	// least recently used go first.
	execSQL(t, srv.store, `UPDATE people_sessions SET last_seen_at = '2000-01-01T00:00:00Z'`)
	srv.signinLimits().sessionMint.Reset(fmt.Sprint(guest))
	srv.mintPersonSession(r, guest, 0, "web")
	srv.store.DB().QueryRow(`SELECT COUNT(*) FROM people_sessions WHERE person_id = ?`, guest).Scan(&n)
	if n != maxSessionsPerPerson {
		t.Fatalf("%d sessions held, cap %d", n, maxSessionsPerPerson)
	}
}

// The attack (sixth review, 3): a pinned session — replayed as a bearer, or
// long since proven — minted a native device's token, which is unpinned
// and lasts 30 days. Now only a browser's own freshly proven cookie session
// may mint one, and the token reaches chat alone.
func TestAPinnedSessionCantMintAnUnpinnedToken(t *testing.T) {
	srv, h := newTestServer(t)
	op := setup(t, h)
	register := func(opts ...func(*http.Request)) *httptest.ResponseRecorder {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		body := `{"name":"phone","kind":"ios","deviceKey":"` + people.EncodeKey(pub) + `","clientKind":"native"}`
		return do(t, h, "POST", "/api/chat/me/devices", body, opts...)
	}
	if c := register(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+op.Value) }).Code; c != http.StatusForbidden {
		t.Fatalf("a session replayed as a bearer minted a native token: %d", c)
	}
	execSQL(t, srv.store, fmt.Sprintf(`UPDATE people_sessions SET auth_time = '%s' WHERE token_digest = '%s'`,
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), auth.DigestToken(op.Value)))
	if c := register(withCookie(op)).Code; c != http.StatusConflict {
		t.Fatalf("a stale session minted a native token: %d", c)
	}
	if c := do(t, h, "POST", "/api/people/reauth/password", `{"password":"correct horse battery"}`, withCookie(op)).Code; c != http.StatusOK {
		t.Fatalf("confirm: %d", c)
	}
	w := register(withCookie(op))
	if w.Code != http.StatusOK {
		t.Fatalf("a freshly proven browser: %d %s", w.Code, w.Body)
	}
	var out struct{ Token string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("no token: %s", w.Body)
	}
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+out.Token) }
	if c := do(t, h, "GET", "/api/chat/me", "", bearer).Code; c != http.StatusOK {
		t.Fatalf("the native token in chat: %d", c)
	}
	for _, path := range []string{"/api/system/secrets", "/api/people/me", "/api/settings"} {
		if c := do(t, h, "GET", path, "", bearer).Code; c == http.StatusOK {
			t.Errorf("the native token reached %s", path)
		}
	}
}

// The attack (sixth review, 4): a token that resolves to no one — replayed
// from another address — still signed its owner out. Logout resolves the
// session as every request does, and refuses otherwise.
func TestAnUnusableTokenCantSignAnyoneOut(t *testing.T) {
	srv, h, plain := bareIPOperator(t)
	if c := do(t, h, "POST", "/api/auth/logout", "", withCookie(plain), fromPeer("192.168.1.66", "")).Code; c != http.StatusUnauthorized {
		t.Fatalf("a replayed token's logout: %d", c)
	}
	if _, _, err := srv.store.SessionPerson(auth.DigestToken(plain.Value)); err != nil {
		t.Fatal("the replay ended the operator's session")
	}
	if c := do(t, h, "POST", "/api/auth/logout", "", withCookie(plain), fromPeer("192.168.1.20", "")).Code; c != http.StatusOK {
		t.Fatalf("the operator's own logout: %d", c)
	}
}

// The attack (sixth review, 5): an app container sent Cf-Connecting-Ip to
// :443 and fog recorded a tunnel on the old service — a false critical
// alert. Only one of the host's own addresses is a tunnel's.
func TestAnAppCantRaiseATunnelAlert(t *testing.T) {
	srv, h := newTestServer(t)
	setup(t, h)
	srv.signinLimitsState.containerNets = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")} }
	l := srv.signinLimits()
	l.addrMu.Lock()
	l.addrs, l.addrsAt = map[string]bool{"172.18.0.1": true}, time.Now()
	l.addrMu.Unlock()
	edge := func(r *http.Request) { r.Header.Set(proxy.HeaderVia, proxy.ViaEdge) }
	for _, client := range []string{"172.18.0.5", "172.18.0.2"} { // an app; Caddy itself
		do(t, h, "GET", "/api/visit/peers", "", viaCaddy(t, srv, client, "198.51.100.7"), edge)
	}
	if v, _ := srv.store.GetSetting(proxy.SettingTunnelSeenEdge); v != "" {
		t.Fatal("a container's Cf-Connecting-Ip recorded a tunnel on :443")
	}
	do(t, h, "GET", "/api/visit/peers", "", viaCaddy(t, srv, "172.18.0.1", "198.51.100.7"), edge)
	if v, _ := srv.store.GetSetting(proxy.SettingTunnelSeenEdge); v == "" {
		t.Fatal("the host's bridge gateway was not recorded")
	}
}

// Sixth review, minor: passkey finish refuses an app's network as begin
// does; and a link-local address is its own pin, never all of fe80::/64.
func TestPasskeyFinishAndLinkLocalPins(t *testing.T) {
	srv, h := newTestServer(t)
	setup(t, h)
	srv.signinLimitsState.containerNets = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")} }
	if c := do(t, h, "POST", "/api/people/login/passkey/finish", `{"ceremony":"x","response":{}}`, onFogHost, fromPeer("172.18.0.5", "")).Code; c != http.StatusForbidden {
		t.Fatalf("passkey finish from an app's network: %d", c)
	}
	if sameClient("fe80::1%en0", "fe80::2%en0") || sameClient("fe80::1%en0", "fe80::1%en1") || !sameClient("fe80::1%en0", "[fe80::1%en0]") {
		t.Fatal("a link-local pin covers more than its own address and link")
	}
	if !sameClient("2001:db8::1", "2001:db8::2") || !strings.Contains(auth.AddrKey("fe80::1%en0"), "%en0") {
		t.Fatal("pin keys")
	}
}
