package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// Two fogs, one visit (docs/signin-contract.md): A hosts Stream;
// B is the home of the person who visits. Every hop a browser or an app
// makes goes over HTTP, routed by host as Caddy would.

const (
	hostDomain   = "host.test"
	hostIssuer   = "https://fog." + hostDomain
	hostApp      = "https://stream." + hostDomain
	hostRedirect = hostApp + "/auth/oidc/callback"
	homeDomain   = "away.test"
	homeIssuer   = "https://fog." + homeDomain
)

// byHost routes each request to the test server for its host.
type byHost map[string]string

func (b byHost) RoundTrip(r *http.Request) (*http.Response, error) {
	addr, ok := b[r.URL.Hostname()]
	if !ok {
		return nil, fmt.Errorf("no route to %s", r.URL.Host)
	}
	r = r.Clone(r.Context())
	r.Host = r.URL.Host
	r.URL.Scheme, r.URL.Host = "http", addr
	r.Header.Set("X-Forwarded-Proto", "https")
	return http.DefaultTransport.RoundTrip(r)
}

type visitFixture struct {
	*fogPair
	bHTTP    *httptest.Server
	rp       *http.Client
	browser  *http.Client
	cfg      oauth2.Config
	provider *gooidc.Provider
	secret   string
	ctx      context.Context
	verifier string
	// homeCode is the last code the home sent the browser back with.
	homeCode string
}

// routed sends the hosts it names to their test servers and everything
// else as addressed: how B reaches A at A's fog origin while A reaches B at
// its loopback address.
type routed byHost

func (rt routed) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, ok := rt[r.URL.Hostname()]; ok {
		return byHost(rt).RoundTrip(r)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// visitPair pairs two fogs for a visit. Every address and transport is set
// before the fog it belongs to dials anyone: once two paired fogs are live,
// each one's background work (heartbeats, notices) runs on the other's
// request goroutines too, which no wait on one side covers — so nothing is
// rewired afterwards. A is known everywhere by its fog origin, as a fog with
// a domain advertises itself: the address a visit's redirect must be on.
func visitPair(t *testing.T) (*fogPair, *httptest.Server) {
	t.Helper()
	p := &fogPair{}
	p.a, p.aH = newTestServer(t)
	p.aCookie = setup(t, p.aH)
	p.aHTTP = httptest.NewServer(sameTestClient(p.aH))
	t.Cleanup(p.aHTTP.Close)
	p.a.store.SetSetting(store.SettingBaseDomain, hostDomain)
	httpsSetup(t, p.a)
	if w := do(t, p.aH, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":%q}`, hostIssuer), withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("host url: %d %s", w.Code, w.Body)
	}
	code := mintInvite(t, p.aH, p.aCookie, `[]`)

	p.b, p.bH = newTestServer(t)
	p.bCookie = setup(t, p.bH)
	p.b.fed.HTTP = &http.Client{Transport: routed{"fog." + hostDomain: strings.TrimPrefix(p.aHTTP.URL, "http://")}}
	bHTTP := httptest.NewServer(sameTestClient(p.bH))
	t.Cleanup(bHTTP.Close)
	p.b.store.SetSetting(store.SettingBaseDomain, homeDomain)
	httpsSetup(t, p.b)
	// Both directions dial: the host asks the home; the home tells the host
	// when a session ends.
	if w := do(t, p.bH, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":%q}`, bHTTP.URL), withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("home url: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.bH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	aID, _ := p.a.fed.Identity()
	bID, _ := p.b.fed.Identity()
	p.aPeerOnB, _ = p.b.store.FedPeerByInstanceID(aID.InstanceID)
	p.bPeerOnA, _ = p.a.store.FedPeerByInstanceID(bID.InstanceID)
	if p.aPeerOnB == nil || p.bPeerOnA == nil || p.aPeerOnB.URL != hostIssuer || p.bPeerOnA.URL != bHTTP.URL {
		t.Fatalf("pairing: A on B %+v, B on A %+v", p.aPeerOnB, p.bPeerOnA)
	}
	return p, bHTTP
}

func newVisitFixture(t *testing.T) *visitFixture {
	t.Helper()
	p, bHTTP := visitPair(t)
	f := &visitFixture{fogPair: p, bHTTP: bHTTP}

	app, err := p.a.store.CreateApp("stream")
	if err != nil {
		t.Fatal(err)
	}
	m := signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Logout: "/auth/oidc/backchannel",
		Roles: []string{"admin", "member", "guest"}, Default: map[string]string{"operator": "admin", "member": "member"}}
	m.Visitors = &struct {
		Role string `yaml:"role" json:"role"`
	}{Role: "guest"}
	noPAR := false
	m.PAR = &noPAR // go-oidc's RP sends its request on the front channel
	reg, err := p.a.signin.Register(app, hostApp, hostIssuer, "", m)
	if err != nil {
		t.Fatal(err)
	}
	f.secret = reg.Env["OIDC_CLIENT_SECRET"]

	routes := byHost{
		"fog." + hostDomain: strings.TrimPrefix(p.aHTTP.URL, "http://"),
		"fog." + homeDomain: strings.TrimPrefix(f.bHTTP.URL, "http://"),
	}
	jar, _ := cookiejar.New(nil)
	f.rp = &http.Client{Transport: routes}
	f.browser = &http.Client{Transport: routes, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// The visitor is B's operator, signed in at home.
	u, _ := url.Parse(homeIssuer)
	jar.SetCookies(u, []*http.Cookie{{Name: auth.SecureCookieName, Value: p.bCookie.Value, Path: "/", Secure: true}})

	f.ctx = gooidc.ClientContext(context.Background(), f.rp)
	f.provider, err = gooidc.NewProvider(f.ctx, hostIssuer)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = oauth2.Config{ClientID: "stream", ClientSecret: f.secret, Endpoint: f.provider.Endpoint(), RedirectURL: hostRedirect,
		Scopes: []string{gooidc.ScopeOpenID, "profile", "roles"}}
	return f
}

// allow is both operators' consent: B lets its people visit A; A opens
// Stream to B's people as role.
func (f *visitFixture) allow(t *testing.T, role string) {
	t.Helper()
	body := `{"capabilities":[{"capability":"people.visit","direction":"grant","enabled":true,"config":{"people":"members"}}]}`
	if w := do(t, f.bH, "PUT", fmt.Sprintf("/api/federation/peers/%d", f.aPeerOnB.ID), body, withCookie(f.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("home grant: %d %s", w.Code, w.Body)
	}
	f.grant(t, role, http.StatusOK)
}

func (f *visitFixture) grant(t *testing.T, role string, want int) {
	t.Helper()
	body := fmt.Sprintf(`{"grants":[{"peerId":%d,"role":%q}]}`, f.bPeerOnA.ID, role)
	if w := do(t, f.aH, "PUT", "/api/apps/stream/signin/visitors", body, withCookie(f.aCookie)); w.Code != want {
		t.Fatalf("host grant %q: %d %s", role, w.Code, w.Body)
	}
}

// get is one browser hop.
func (f *visitFixture) get(t *testing.T, target string) *http.Response {
	t.Helper()
	res, err := f.browser.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

// visit walks the browser from the app's sign-in to the app's callback,
// choosing "From another household?" on the host's login page. It returns
// the callback URL, or the response that stopped the walk.
func (f *visitFixture) visit(t *testing.T) (*url.URL, *http.Response) {
	t.Helper()
	f.verifier = oauth2.GenerateVerifier()
	start := f.cfg.AuthCodeURL("st", oauth2.S256ChallengeOption(f.verifier), gooidc.Nonce("no"))
	res := f.get(t, start)
	loc, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusFound || loc.Path != "/login" {
		t.Fatalf("authorize without a session: %d %s", res.StatusCode, loc)
	}
	// The login page offers the households whose people may visit.
	var peers []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	pr := do(t, f.aH, "GET", "/api/visit/peers?app=stream", "")
	json.Unmarshal(pr.Body.Bytes(), &peers)
	if len(peers) != 1 || peers[0].ID != f.bPeerOnA.ID {
		t.Fatalf("visit peers = %s", pr.Body)
	}
	next := hostIssuer + "/visit/start?" + url.Values{"peer": {fmt.Sprint(peers[0].ID)}, "next": {loc.Query().Get("next")}}.Encode()
	for range 10 {
		res = f.get(t, next)
		if res.StatusCode == http.StatusOK && res.Request.URL.Path == "/visit/authorize" {
			// The home asks the person; they continue.
			res = f.confirmVisit(t, next)
		}
		if res.StatusCode != http.StatusFound {
			return nil, res
		}
		u, _ := url.Parse(res.Header.Get("Location"))
		if u.Host == "" {
			base, _ := url.Parse(next)
			u = base.ResolveReference(u)
		}
		if strings.HasPrefix(u.String(), hostRedirect) {
			return u, nil
		}
		if u.Path == "/visit/callback" {
			f.homeCode = u.Query().Get("code")
		}
		next = u.String()
	}
	t.Fatal("too many redirects")
	return nil, nil
}

func TestVisitorSignsInWithTheirOwnFog(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	cb, stopped := f.visit(t)
	if stopped != nil {
		t.Fatalf("visit stopped: %d %s", stopped.StatusCode, stopped.Header.Get("Location"))
	}
	if cb.Query().Get("iss") != hostIssuer || cb.Query().Get("state") != "st" {
		t.Fatalf("callback = %s", cb)
	}
	tok, err := f.exchange(cb)
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Sub      string   `json:"sub"`
		Home     string   `json:"fog_home"`
		Handle   string   `json:"preferred_username"`
		Roles    []string `json:"roles"`
		SID      string   `json:"sid"`
		Email    string   `json:"email"`
		Audience string   `json:"aud"`
		Visitor  bool     `json:"fog_visitor"`
	}
	if err := tok.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	bID, _ := f.b.fed.Identity()
	if !strings.HasPrefix(claims.Sub, "person:"+bID.InstanceID+"/") || claims.Home != bID.InstanceID {
		t.Fatalf("visitor's subject is not their home's: %+v", claims)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "guest" || claims.Email != "" || !claims.Visitor {
		t.Fatalf("visitor role/email = %+v", claims)
	}
	// A visitor is not a person here.
	if people, _ := f.a.store.ListPeople(); len(people) != 1 {
		t.Fatalf("the host has %d people; a visitor must not become one", len(people))
	}
	if aID, _ := f.a.fed.Identity(); strings.Contains(claims.Sub, aID.InstanceID) {
		t.Fatalf("visitor's subject names the host: %s", claims.Sub)
	}

	// Signing out at home ends the visit, and the host tells Stream.
	if w := do(t, f.bH, "POST", "/api/people/logout", "", withCookie(f.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("home logout: %d %s", w.Code, w.Body)
	}
	f.b.logouts.Run(context.Background())
	if n := countVisitorSessions(t, f.a); n != 0 {
		t.Fatalf("visitor sessions after home logout = %d", n)
	}
	assertLogoutOwed(t, f.a, claims.SID)
}

// exchange redeems a callback's code at the host and verifies the ID token,
// as the app would.
func (f *visitFixture) exchange(cb *url.URL) (*gooidc.IDToken, error) {
	ot, err := f.cfg.Exchange(f.ctx, cb.Query().Get("code"), oauth2.VerifierOption(f.verifier))
	if err != nil {
		return nil, err
	}
	raw, _ := ot.Extra("id_token").(string)
	return f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(f.ctx, raw)
}

func countVisitorSessions(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.store.DB().QueryRow(`SELECT COUNT(*) FROM visitor_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertLogoutOwed: the host's outbox holds a logout for Stream naming sid.
func assertLogoutOwed(t *testing.T, s *Server, sid string) {
	t.Helper()
	rows, err := s.store.DueAppOutbox(time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Target == "stream" && r.Kind == store.AppOutboxLogout && strings.Contains(r.Payload, sid) {
			return
		}
	}
	t.Fatalf("no logout owed to stream for %s: %+v", sid, rows)
}

// signedIn walks a whole visit and returns the ID token Stream got.
func (f *visitFixture) signedIn(t *testing.T) *gooidc.IDToken {
	t.Helper()
	cb, stopped := f.visit(t)
	if stopped != nil {
		t.Fatalf("visit stopped: %d %s", stopped.StatusCode, stopped.Header.Get("Location"))
	}
	tok, err := f.exchange(cb)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func sidOf(t *testing.T, tok *gooidc.IDToken) string {
	var c struct {
		SID string `json:"sid"`
	}
	if err := tok.Claims(&c); err != nil || c.SID == "" {
		t.Fatalf("sid: %v", err)
	}
	return c.SID
}

func TestVisitorNeedsBothOperators(t *testing.T) {
	f := newVisitFixture(t)
	// The host opens Stream, but the home never lets its people visit.
	f.grant(t, "guest", http.StatusOK)
	if _, stopped := f.visit(t); stopped == nil || stopped.StatusCode != http.StatusBadGateway {
		t.Fatalf("visit without the home's consent: %+v", stopped)
	}
	// The home consents, the host withdraws: nobody is offered.
	f.allow(t, "guest")
	f.grant(t, "", http.StatusOK)
	var peers []any
	json.Unmarshal(do(t, f.aH, "GET", "/api/visit/peers?app=stream", "").Body.Bytes(), &peers)
	if len(peers) != 0 {
		t.Fatalf("peers offered without a grant: %v", peers)
	}
	res := f.get(t, hostIssuer+"/visit/start?"+url.Values{"peer": {fmt.Sprint(f.bPeerOnA.ID)}, "next": {"/oauth2/authorize?client_id=stream"}}.Encode())
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("start without a grant: %d", res.StatusCode)
	}
}

func TestVisitorRoleIsCapped(t *testing.T) {
	f := newVisitFixture(t)
	f.grant(t, "member", http.StatusBadRequest) // above the app's visitors.role
	f.grant(t, "admin", http.StatusBadRequest)
	f.grant(t, "owner", http.StatusBadRequest)
	f.grant(t, "guest", http.StatusOK)
}

func TestVisitorCallbackIsBoundToTheBrowser(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	// A callback carrying a state the browser never began is refused.
	res := f.get(t, hostIssuer+"/visit/callback?code=x&state=planted&iss=fog:whoever")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("planted callback: %d", res.StatusCode)
	}
	if n := countVisitorSessions(t, f.a); n != 0 {
		t.Fatalf("visitor sessions = %d", n)
	}
}

func TestVisitorUserInfoAndGrantWithdrawal(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	cb, stopped := f.visit(t)
	if stopped != nil {
		t.Fatalf("visit stopped: %d", stopped.StatusCode)
	}
	ot, err := f.cfg.Exchange(f.ctx, cb.Query().Get("code"), oauth2.VerifierOption(f.verifier))
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.provider.UserInfo(f.ctx, oauth2.StaticTokenSource(ot))
	if err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	bID, _ := f.b.fed.Identity()
	if !strings.HasPrefix(info.Subject, "person:"+bID.InstanceID+"/") || info.Email != "" {
		t.Fatalf("userinfo = %+v", info)
	}
	raw, _ := ot.Extra("id_token").(string)
	tok, _ := f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(f.ctx, raw)

	// The host withdraws the grant: Stream hears the session ended, and the
	// token it holds stops working.
	f.grant(t, "", http.StatusOK)
	assertLogoutOwed(t, f.a, sidOf(t, tok))
	if _, err := f.provider.UserInfo(f.ctx, oauth2.StaticTokenSource(ot)); err == nil {
		t.Fatal("userinfo still answers after the grant was withdrawn")
	}
}

func TestVisitEndsWhenTheHostUnpairs(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	sid := sidOf(t, f.signedIn(t))
	if w := do(t, f.aH, "DELETE", fmt.Sprintf("/api/federation/peers/%d", f.bPeerOnA.ID), "", withCookie(f.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("unpair: %d %s", w.Code, w.Body)
	}
	if n := countVisitorSessions(t, f.a); n != 0 {
		t.Fatalf("visitor sessions after unpair = %d", n)
	}
	assertLogoutOwed(t, f.a, sid)
}

func TestVisitEndsWhenTheHomeWithdraws(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	sid := sidOf(t, f.signedIn(t))
	body := `{"capabilities":[{"capability":"people.visit","direction":"grant","enabled":false}]}`
	if w := do(t, f.bH, "PUT", fmt.Sprintf("/api/federation/peers/%d", f.aPeerOnB.ID), body, withCookie(f.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("home withdraw: %d %s", w.Code, w.Body)
	}
	f.b.logouts.Run(context.Background())
	if n := countVisitorSessions(t, f.a); n != 0 {
		t.Fatalf("visitor sessions after the home withdrew = %d", n)
	}
	assertLogoutOwed(t, f.a, sid)
}

func TestVisitorPasskeyRequirementTravelsHome(t *testing.T) {
	f := newVisitFixture(t)
	// Stream now requires a passkey; the visitor's home session was a
	// password.
	c, _ := f.a.store.OIDCClient("stream")
	c.RequiredACR = "urn:fog:acr:phr"
	if err := f.a.store.PutOIDCClient(*c); err != nil {
		t.Fatal(err)
	}
	f.allow(t, "guest")
	_, stopped := f.visit(t)
	if stopped == nil {
		t.Fatal("a password visit reached a passkey-only app")
	}
	// The walk ends on the home's /confirm page, asking for the passkey
	// there, where the visitor has one.
	at := stopped.Request
	if at.Host != "fog."+homeDomain || at.URL.Path != "/confirm" || at.URL.Query().Get("with") != "passkey" {
		t.Fatalf("stopped at %d %s%s", stopped.StatusCode, at.Host, at.URL)
	}
}

// confirmVisit is the person pressing Continue on their home's page.
func (f *visitFixture) confirmVisit(t *testing.T, authorize string) *http.Response {
	t.Helper()
	u, _ := url.Parse(authorize)
	form := url.Values{"request_uri": {u.Query().Get("request_uri")}, "confirm": {"1"}}
	req, _ := http.NewRequest("POST", homeIssuer+"/visit/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", homeIssuer)
	res, err := f.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func TestVisitAsksThePersonFirst(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	// A host steering the browser to the home learns nothing without the
	// person: the GET only asks, and a cross-site POST is refused.
	f.verifier = oauth2.GenerateVerifier()
	res := f.get(t, f.cfg.AuthCodeURL("st", oauth2.S256ChallengeOption(f.verifier), gooidc.Nonce("no")))
	loc, _ := url.Parse(res.Header.Get("Location"))
	res = f.get(t, hostIssuer+"/visit/start?"+url.Values{"peer": {fmt.Sprint(f.bPeerOnA.ID)}, "next": {loc.Query().Get("next")}}.Encode())
	authorize := res.Header.Get("Location")
	if res = f.get(t, authorize); res.StatusCode != http.StatusOK {
		t.Fatalf("the home's ask: %d", res.StatusCode)
	}
	u, _ := url.Parse(authorize)
	form := url.Values{"request_uri": {u.Query().Get("request_uri")}, "confirm": {"1"}}
	req, _ := http.NewRequest("POST", homeIssuer+"/visit/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", hostIssuer)
	cross, err := f.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cross.Body.Close()
	if cross.StatusCode == http.StatusFound {
		t.Fatalf("a cross-site POST signed the person in: %s", cross.Header.Get("Location"))
	}
	// The person's own Continue does.
	if res = f.confirmVisit(t, authorize); res.StatusCode != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), hostIssuer+"/visit/callback?") {
		t.Fatalf("continue: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// TestLoweringVisitorsEndsVisits is the first visitor finding: an app that
// stops hosting visitors (or caps them lower) on its next deploy used to
// refuse only new sign-ins. Now the grants it no longer allows are
// withdrawn and the visits holding them end.
func TestLoweringVisitorsEndsVisits(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	tok := f.signedIn(t)
	sid := sidOf(t, tok)
	app, _ := f.a.store.AppByName("stream")
	m := signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Logout: "/auth/oidc/backchannel",
		Roles: []string{"admin", "member", "guest"}, Default: map[string]string{"operator": "admin", "member": "member"}}
	noPAR := false
	m.PAR = &noPAR
	if _, err := f.a.signin.Register(app, hostApp, hostIssuer, "", m); err != nil {
		t.Fatal(err)
	}
	if grants, _ := f.a.store.VisitorGrants("stream"); len(grants) != 0 {
		t.Fatalf("grants outlived the app hosting visitors: %+v", grants)
	}
	assertLogoutOwed(t, f.a, sid)
}

// TestHomeRefusesARedirectOffThePeer is the second: the home checked only
// the redirect's shape, so a peer's request could send the home's person
// anywhere. Now it must be the peer's own address.
func TestHomeRefusesARedirectOffThePeer(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	push := func(redirect string) error {
		p := visitParams{RedirectURI: redirect, State: "s", Nonce: "n", CodeChallenge: oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier()), App: "stream"}
		_, err := f.a.fed.SignedPostVerified(f.ctx, f.bPeerOnA, "/fed/v1/signin/par", "application/json", []byte(mustMarshal(p)))
		return err
	}
	for _, evil := range []string{"https://evil.test/visit/callback", "https://fog.host.test.evil.test/visit/callback", "https://user@evil.test/visit/callback"} {
		if err := push(evil); err == nil {
			t.Errorf("the home took a visit returning to %s", evil)
		}
	}
	if err := push(hostIssuer + "/visit/callback"); err != nil {
		t.Fatalf("the peer's own callback was refused: %v", err)
	}
}

// TestAReplayedVisitCodeEndsTheVisit is the third: a code redeemed twice
// revoked nothing. Now the replay is refused and the visit the first
// redemption opened ends at the host.
func TestAReplayedVisitCodeEndsTheVisit(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	f.signedIn(t)
	if f.homeCode == "" || countVisitorSessions(t, f.a) != 1 {
		t.Fatal("no visit to replay")
	}
	body := mustMarshal(map[string]string{"code": f.homeCode, "codeVerifier": "x"})
	if _, err := f.a.fed.SignedPostVerified(f.ctx, f.bPeerOnA, "/fed/v1/signin/token", "application/json", []byte(body)); err == nil {
		t.Fatal("a visit code was redeemed twice")
	}
	// The recall is owed by the time the refusal arrives (the token handler
	// writes it before answering); Run delivers it, serialised with any
	// delivery the handler kicked. No wait on B's background work: A's
	// heartbeats can start more of it at any moment.
	f.b.logouts.Run(context.Background())
	if n := countVisitorSessions(t, f.a); n != 0 {
		t.Fatalf("the replayed code's visit is still open at the host (%d)", n)
	}
}

// TestAVisitNeverOutlastsTheHomeSession: the home's token says when its
// session ends; the host's visit — and the ID token the app gets — end
// no later.
func TestAVisitNeverOutlastsTheHomeSession(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	homeEnds := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	execSQL(t, f.b.store, fmt.Sprintf(`UPDATE people_sessions SET expires_at = '%s'`, homeEnds.Format(time.RFC3339)))
	tok := f.signedIn(t)
	var c struct {
		Exp int64 `json:"fog_session_exp"`
	}
	tok.Claims(&c)
	if c.Exp != homeEnds.Unix() {
		t.Fatalf("the app was told the visit ends at %d, want the home session's %d", c.Exp, homeEnds.Unix())
	}
	var ends string
	f.a.store.DB().QueryRow(`SELECT expires_at FROM visitor_sessions`).Scan(&ends)
	if at, _ := time.Parse(time.RFC3339, ends); at.After(homeEnds) {
		t.Fatalf("the visit ends %s, after the home session's %s", at, homeEnds)
	}
}

// TestAnUnreadablePeopleConfigLetsNobodyVisit: the home's people.visit
// config used to be read as "everyone" when it was missing or unreadable.
// It fails closed now: only a config that names its people lets them visit.
func TestAnUnreadablePeopleConfigLetsNobodyVisit(t *testing.T) {
	for cfg, want := range map[string]bool{
		``:                          false,
		`not json`:                  false,
		`{}`:                        false,
		`{"people":null}`:           false,
		`{"people":42}`:             false,
		`{"people":"everyone"}`:     false,
		`{"people":["dave"]}`:       false,
		`{"people":"members"}`:      true,
		`{"people":["kim","dave"]}`: true,
	} {
		if got := peopleConfigAllows(cfg, "kim"); got != want {
			t.Errorf("config %q: allows kim = %v, want %v", cfg, got, want)
		}
	}

	// End to end: the home holds a grant whose config it can't read, and
	// its person is refused at the host's request.
	f := newVisitFixture(t)
	f.allow(t, "guest")
	if err := f.b.store.SetFedCapability(f.aPeerOnB.ID, store.CapPeopleVisit, store.DirGrant, `{"people":`); err != nil {
		t.Fatal(err)
	}
	op, _ := f.b.store.PersonByHandle("admin")
	if f.b.visitAllows(f.aPeerOnB.ID, op) {
		t.Fatal("an unreadable people.visit config let the home's operator visit")
	}
}

// TestAReplayedCodeRevokesAVisitorsToken: the host's authorization code for
// a visitor, redeemed twice, revokes the access token the first redemption
// minted — as it does a person's. Visitor tokens used to keep no record of
// their code, so nothing was revoked.
func TestAReplayedCodeRevokesAVisitorsToken(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	cb, stopped := f.visit(t)
	if stopped != nil {
		t.Fatalf("visit stopped: %d", stopped.StatusCode)
	}
	tok, err := f.cfg.Exchange(f.ctx, cb.Query().Get("code"), oauth2.VerifierOption(f.verifier))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.UserInfo(f.ctx, oauth2.StaticTokenSource(tok)); err != nil {
		t.Fatalf("the visitor's fresh token: %v", err)
	}
	if _, err := f.cfg.Exchange(f.ctx, cb.Query().Get("code"), oauth2.VerifierOption(f.verifier)); err == nil {
		t.Fatal("a visitor's code was redeemed twice")
	}
	if _, err := f.provider.UserInfo(f.ctx, oauth2.StaticTokenSource(tok)); err == nil {
		t.Fatal("the visitor's access token outlived its code's replay")
	}
}

// TestEmergencyRotationEndsVisits: a visitor's session is one an app holds
// that no person's subject names; the emergency key rotation reaches it by
// sid, and with the sessions-revoked logout besides.
func TestEmergencyRotationEndsVisits(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	sid := sidOf(t, f.signedIn(t))
	if w := do(t, f.aH, "POST", "/api/signin/keys/retire", `{"confirm":"`+retireConfirm+`"}`, withCookie(f.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", w.Code, w.Body)
	}
	assertLogoutOwed(t, f.a, sid)
}

// TestTheConsentPageSaysWhatItDisclosesAndCanContinue: the home's "Continue
// as …?" page had its body and redirect swapped, so it showed the redirect
// URL instead of what the host learns, and its CSP named no callback origin
// — Chromium then blocked the redirect after Continue. The page now carries
// the disclosure, and form-action names fog and the peer's callback origin.
func TestTheConsentPageSaysWhatItDisclosesAndCanContinue(t *testing.T) {
	f := newVisitFixture(t)
	f.allow(t, "guest")
	f.verifier = oauth2.GenerateVerifier()
	res := f.get(t, f.cfg.AuthCodeURL("st", oauth2.S256ChallengeOption(f.verifier), gooidc.Nonce("no")))
	loc, _ := url.Parse(res.Header.Get("Location"))
	res = f.get(t, hostIssuer+"/visit/start?"+url.Values{"peer": {fmt.Sprint(f.bPeerOnA.ID)}, "next": {loc.Query().Get("next")}}.Encode())
	page, err := f.browser.Get(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), "will learn your name and handle") {
		t.Fatalf("the consent page doesn't say what it discloses: %d %s", page.StatusCode, body)
	}
	if strings.Contains(string(body), "/visit/callback") {
		t.Fatalf("the consent page shows the redirect URL as its text: %s", body)
	}
	csp := page.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' "+hostIssuer) {
		t.Fatalf("form-action doesn't name the peer's callback origin %s: %q", hostIssuer, csp)
	}
}
