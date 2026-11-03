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
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// These tests drive fog's provider with an independent relying party —
// coreos/go-oidc and golang.org/x/oauth2, the libraries most Go apps use —
// rather than with fog's own idea of the protocol. Everything the RP sees
// arrives over HTTP, as it would in production.

const (
	idpDomain = "home.test"
	issuerURL = "https://fog." + idpDomain
	appOrigin = "https://stream." + idpDomain
	redirect  = appOrigin + "/auth/oidc/callback"
)

// hostRouter sends every request, whatever its host, to the test server,
// keeping the Host header and saying it arrived over https — Caddy's part.
type hostRouter struct{ addr string }

func (h hostRouter) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = r.URL.Host
	r.URL.Scheme, r.URL.Host = "http", h.addr
	r.Header.Set("X-Forwarded-Proto", "https")
	return http.DefaultTransport.RoundTrip(r)
}

type idpFixture struct {
	srv      *Server
	ts       *httptest.Server
	rp       *http.Client // the app's back channel
	browser  *http.Client // a browser: cookies, no automatic redirects
	provider *gooidc.Provider
	cfg      oauth2.Config
	secret   string
	app      *store.App
	operator *http.Cookie
}

// confirm records a fresh proof on the operator's session, as /confirm does.
func (f *idpFixture) confirm(t *testing.T, amr []string) {
	t.Helper()
	sess, _, err := f.srv.store.SessionPerson(auth.DigestToken(f.operator.Value))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.UpdateSessionAssurance(sess.ID, amr); err != nil {
		t.Fatal(err)
	}
}

func newIdP(t *testing.T, m signin.Manifest) *idpFixture {
	t.Helper()
	srv, h := newTestServer(t)
	op := setup(t, h)
	if err := srv.store.SetSetting(store.SettingBaseDomain, idpDomain); err != nil {
		t.Fatal(err)
	}
	httpsSetup(t, srv)
	app, err := srv.store.CreateApp("stream")
	if err != nil {
		t.Fatal(err)
	}
	if m.Contract == "" {
		m = signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Logout: "/auth/oidc/backchannel",
			Roles: []string{"admin", "member"}, Default: map[string]string{"operator": "admin", "member": "member"}}
	}
	reg, err := srv.signin.Register(app, appOrigin, issuerURL, "", m)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(sameTestClient(h)) // one client in-process and over the socket
	t.Cleanup(ts.Close)
	router := hostRouter{addr: ts.Listener.Addr().String()}
	jar, _ := cookiejar.New(nil)
	f := &idpFixture{
		srv: srv, ts: ts, app: app, secret: reg.Env["OIDC_CLIENT_SECRET"], operator: op,
		rp:      &http.Client{Transport: router},
		browser: &http.Client{Transport: router, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	f.signInBrowser(t, op)
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	f.provider, err = gooidc.NewProvider(ctx, issuerURL)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	f.cfg = oauth2.Config{ClientID: "stream", ClientSecret: f.secret, Endpoint: f.provider.Endpoint(), RedirectURL: redirect,
		Scopes: []string{gooidc.ScopeOpenID, "profile", "roles"}}
	return f
}

// signInBrowser gives the browser a fog session.
func (f *idpFixture) signInBrowser(t *testing.T, c *http.Cookie) {
	u, _ := url.Parse(issuerURL)
	f.browser.Jar.SetCookies(u, []*http.Cookie{{Name: auth.SecureCookieName, Value: c.Value, Path: "/", Secure: true}})
}

// push makes a pushed authorization request the way fog-kit does.
func (f *idpFixture) push(t *testing.T, verifier string, extra url.Values) string {
	t.Helper()
	form := url.Values{
		"response_type": {"code"}, "client_id": {"stream"}, "redirect_uri": {redirect},
		"scope": {"openid profile roles"}, "state": {"st"}, "nonce": {"no"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"},
	}
	for k, v := range extra {
		form[k] = v
	}
	req, _ := http.NewRequest("POST", issuerURL+"/oauth2/par", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("stream", f.secret)
	res, err := f.rp.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		RequestURI string `json:"request_uri"`
	}
	if res.StatusCode != http.StatusCreated || json.NewDecoder(res.Body).Decode(&out) != nil {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("par: %d %s", res.StatusCode, body)
	}
	return out.RequestURI
}

// authorize sends the browser to fog and returns where fog sent it.
func (f *idpFixture) authorize(t *testing.T, requestURI string) *url.URL {
	t.Helper()
	q := url.Values{"client_id": {"stream"}, "request_uri": {requestURI}}
	return f.follow(t, issuerURL+"/oauth2/authorize?"+q.Encode())
}

func (f *idpFixture) follow(t *testing.T, raw string) *url.URL {
	t.Helper()
	res, err := f.browser.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("GET %s: %d", raw, res.StatusCode)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestInteropCodeFlowWithAnIndependentRP(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	verifier := oauth2.GenerateVerifier()
	cb := f.authorize(t, f.push(t, verifier, nil))
	if got := cb.Scheme + "://" + cb.Host + cb.Path; got != redirect {
		t.Fatalf("redirected to %s", cb)
	}
	if cb.Query().Get("iss") != issuerURL || cb.Query().Get("state") != "st" {
		t.Fatalf("callback = %s", cb)
	}
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	tok, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(ctx, raw)
	if err != nil {
		t.Fatalf("the RP rejected fog's ID token: %v", err)
	}
	if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
		t.Fatalf("at_hash: %v", err)
	}
	var claims struct {
		Nonce, Sid, Acr, PreferredUsername string
		Roles                              []string
		Amr                                []string
	}
	if err := idt.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	if claims.Nonce != "no" || claims.Sid == "" || len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Fatalf("claims = %+v", claims)
	}
	if !strings.HasPrefix(idt.Subject, "person:") {
		t.Fatalf("sub = %s", idt.Subject)
	}
	info, err := f.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil || info.Subject != idt.Subject {
		t.Fatalf("userinfo: %v %+v", err, info)
	}

	// The same code again: refused, and what it minted is revoked.
	if _, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier)); err == nil {
		t.Fatal("a code was redeemed twice")
	}
	if _, err := f.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
		t.Fatal("the access token outlived its code's replay")
	}
}

func TestInteropRefusesWithoutPKCEOrWithTheWrongVerifier(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	cb := f.authorize(t, f.push(t, oauth2.GenerateVerifier(), nil))
	if _, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(oauth2.GenerateVerifier())); err == nil {
		t.Fatal("exchanged with the wrong verifier")
	}
	cb = f.authorize(t, f.push(t, oauth2.GenerateVerifier(), nil))
	if _, err := f.cfg.Exchange(ctx, cb.Query().Get("code")); err == nil {
		t.Fatal("exchanged without a verifier")
	}
}

func TestInteropClientMustUsePAR(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	u := f.cfg.AuthCodeURL("st", oauth2.S256ChallengeOption(oauth2.GenerateVerifier()))
	loc := f.follow(t, u)
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("a front-channel request from a PAR-only client got %s", loc)
	}
}

func TestInteropPromptNoneAndSignIn(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	f.browser.Jar, _ = cookiejar.New(nil) // nobody signed in
	loc := f.authorize(t, f.push(t, oauth2.GenerateVerifier(), url.Values{"prompt": {"none"}}))
	if loc.Query().Get("error") != "login_required" || loc.Query().Get("iss") != issuerURL {
		t.Fatalf("prompt=none signed out: %s", loc)
	}
	loc = f.authorize(t, f.push(t, oauth2.GenerateVerifier(), nil))
	if loc.Path != "/login" || !strings.HasPrefix(loc.Query().Get("next"), "/oauth2/authorize?") || loc.Query().Get("app") != "stream" {
		t.Fatalf("signed out: sent to %s", loc)
	}
}

func TestInteropMaxAgeZeroAsksAgainThenContinues(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	verifier := oauth2.GenerateVerifier()
	loc := f.authorize(t, f.push(t, verifier, url.Values{"max_age": {"0"}}))
	if loc.Path != "/confirm" {
		t.Fatalf("max_age=0 went to %s", loc)
	}
	// The person confirms: the session's proof is fresh.
	f.confirm(t, []string{"pwd"})
	cb := f.follow(t, issuerURL+loc.Query().Get("next"))
	if cb.Query().Get("code") == "" {
		t.Fatalf("after confirming: %s", cb)
	}
}

func TestInteropPasskeyRequiredStepsUp(t *testing.T) {
	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", ACR: "phr"})
	loc := f.authorize(t, f.push(t, oauth2.GenerateVerifier(), nil))
	if loc.Path != "/confirm" || loc.Query().Get("with") != "passkey" {
		t.Fatalf("a password session reached a passkey app: %s", loc)
	}
}

func TestInteropMembersOnlyWhenShared(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	kim := addMember(t, f.srv, "kim")
	jar, _ := cookiejar.New(nil)
	f.browser.Jar = jar
	f.signInBrowser(t, kim)
	res, err := f.browser.Get(issuerURL + "/oauth2/authorize?" + url.Values{"client_id": {"stream"}, "request_uri": {f.push(t, oauth2.GenerateVerifier(), nil)}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a member reached an operator-only app: %d", res.StatusCode)
	}
	if err := f.srv.store.PutSigninPolicy(store.SigninPolicy{AppID: f.app.ID, Audience: "household"}); err != nil {
		t.Fatal(err)
	}
	verifier := oauth2.GenerateVerifier()
	cb := f.authorize(t, f.push(t, verifier, nil))
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	tok, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatal(err)
	}
	idt, err := f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(ctx, tok.Extra("id_token").(string))
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ Roles []string }
	idt.Claims(&c)
	if len(c.Roles) != 1 || c.Roles[0] != "member" {
		t.Fatalf("kim's role = %v", c.Roles)
	}
}

func TestInteropEndSessionWithHint(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	verifier := oauth2.GenerateVerifier()
	cb := f.authorize(t, f.push(t, verifier, nil))
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	tok, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"id_token_hint": {tok.Extra("id_token").(string)}, "post_logout_redirect_uri": {appOrigin + "/"}, "state": {"bye"}}
	loc := f.follow(t, issuerURL+"/oauth2/end_session?"+q.Encode())
	if loc.String() != appOrigin+"/?state=bye" {
		t.Fatalf("after sign-out: %s", loc)
	}
	if loc := f.authorize(t, f.push(t, oauth2.GenerateVerifier(), nil)); loc.Path != "/login" {
		t.Fatalf("still signed in after end_session: %s", loc)
	}
	// An unregistered destination is refused outright.
	res, _ := f.browser.Get(issuerURL + "/oauth2/end_session?" + url.Values{"client_id": {"stream"}, "post_logout_redirect_uri": {"https://evil.test/"}}.Encode())
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered post-logout redirect: %d", res.StatusCode)
	}
}

func TestInteropClientAuthentication(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	for name, set := range map[string]func(*http.Request){
		"wrong secret": func(r *http.Request) { r.SetBasicAuth("stream", "nope") },
		"no auth":      func(*http.Request) {},
		"other client": func(r *http.Request) { r.SetBasicAuth("hub", f.secret) },
	} {
		req, _ := http.NewRequest("POST", issuerURL+"/oauth2/par", strings.NewReader("client_id=stream"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		set(req)
		res, err := f.rp.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
}

func TestAppSigninProofVerifiesAgainstTheIssuerKeys(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	if err := f.srv.store.SetAppAPITokenDigest(f.app.ID, auth.DigestToken("stream-token")); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", issuerURL+"/app/v1/signin", nil)
	req.Header.Set("Authorization", "Bearer stream-token")
	res, err := f.rp.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct{ Proof, Issuer, ClientID string }
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&out) != nil {
		t.Fatalf("proof: %d", res.StatusCode)
	}
	payload, err := f.srv.keys.Verify(out.Proof)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	json.Unmarshal(payload, &c)
	if c["issuer"] != issuerURL || c["client_id"] != "stream" {
		t.Fatalf("proof claims = %v", c)
	}
}

func TestReauthRecordsOnTheSameSession(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	before, _, _ := f.srv.store.SessionPerson(auth.DigestToken(f.operator.Value))
	w := do(t, f.srv.Handler(), "POST", "/api/people/reauth/password", `{"password":"wrong password here"}`, withCookie(f.operator))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	w = do(t, f.srv.Handler(), "POST", "/api/people/reauth/password", `{"password":"correct horse battery"}`, withCookie(f.operator))
	if w.Code != http.StatusOK {
		t.Fatalf("reauth: %d %s", w.Code, w.Body)
	}
	after, _, _ := f.srv.store.SessionPerson(auth.DigestToken(f.operator.Value))
	if after.ID != before.ID || after.SID != before.SID || !after.AuthTime.After(before.AuthTime) {
		t.Fatalf("reauth minted a new session or kept the old proof: %+v → %+v", before, after)
	}
}

func TestSigninTabView(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	addMember(t, f.srv, "kim")
	w := do(t, f.srv.Handler(), "GET", "/api/apps/stream/signin", "", withCookie(f.operator))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var v signinView
	json.Unmarshal(w.Body.Bytes(), &v)
	if v.Mode != "oidc" || v.Client == nil || v.Client.ClientID != "stream" || len(v.Household) != 2 || v.Audience != "operator" {
		t.Fatalf("view = %+v", v)
	}
	w = do(t, f.srv.Handler(), "PUT", "/api/apps/stream/signin", `{"audience":"people","people":[{"handle":"kim","role":"owner"}]}`, withCookie(f.operator))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a role the app doesn't have was accepted: %d", w.Code)
	}
}

// appBackchannel stands in for the app's /auth/oidc/backchannel: it records
// what fog delivers, and answers as it is told.
type appBackchannel struct {
	got    []map[string]any
	status int
	seen   int // how many of got a test has looked at
}

func (f *idpFixture) receiveLogouts(t *testing.T, app *appBackchannel) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		payload, err := f.srv.keys.Verify(r.PostForm.Get("logout_token"))
		if err != nil {
			t.Errorf("a logout token that does not verify: %v", err)
			w.WriteHeader(400)
			return
		}
		var c map[string]any
		json.Unmarshal(payload, &c)
		app.got = append(app.got, c)
		if app.status == 0 {
			app.status = 200
		}
		w.WriteHeader(app.status)
	}))
	t.Cleanup(ts.Close)
	f.srv.logouts.HTTP = &http.Client{Transport: hostRouter{addr: ts.Listener.Addr().String()}}
}

func (f *idpFixture) signInRP(t *testing.T) *oauth2.Token {
	t.Helper()
	verifier := oauth2.GenerateVerifier()
	cb := f.authorize(t, f.push(t, verifier, nil))
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	tok, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestBackchannelLogoutOnSignOut(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	tok := f.signInRP(t)
	var idc struct{ Sid string }
	idt, _ := f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(gooidc.ClientContext(context.Background(), f.rp), tok.Extra("id_token").(string))
	idt.Claims(&idc)

	q := url.Values{"id_token_hint": {tok.Extra("id_token").(string)}}
	res, err := f.browser.Get(issuerURL + "/oauth2/end_session?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	f.srv.DeliverLogouts(context.Background())
	if len(app.got) != 1 {
		t.Fatalf("logouts delivered: %v", app.got)
	}
	c := app.got[0]
	ev, _ := c["events"].(map[string]any)
	if c["sid"] != idc.Sid || c["aud"] != "stream" || c["iss"] != issuerURL || c["jti"] == nil || ev["http://schemas.openid.net/event/backchannel-logout"] == nil {
		t.Fatalf("logout token = %v", c)
	}
	if _, hasNonce := c["nonce"]; hasNonce {
		t.Fatal("a logout token carried a nonce")
	}
	// Delivered once: nothing is owed now.
	f.srv.DeliverLogouts(context.Background())
	if len(app.got) != 1 {
		t.Fatalf("redelivered: %d", len(app.got))
	}
}

func TestBackchannelLogoutOnSuspensionAndNarrowing(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	kim := addMember(t, f.srv, "kim")
	f.srv.store.PutSigninPolicy(store.SigninPolicy{AppID: f.app.ID, Audience: "household"})
	f.browser.Jar, _ = cookiejar.New(nil)
	f.signInBrowser(t, kim)
	f.signInRP(t)

	// Narrowing Stream to the operator ends Kim's sessions there.
	if w := do(t, f.srv.Handler(), "PUT", "/api/apps/stream/signin", `{"audience":"operator","people":[]}`, withCookie(f.operator)); w.Code != 200 {
		t.Fatal(w.Body)
	}
	f.srv.DeliverLogouts(context.Background())
	if len(app.got) != 1 || !strings.HasPrefix(fmt.Sprint(app.got[0]["sub"]), "person:") {
		t.Fatalf("after narrowing: %v", app.got)
	}

	// Suspending her says so again, by subject, and her fog session's end
	// says so by sid.
	p, _ := f.srv.store.PersonByHandle("kim")
	if err := f.srv.store.UpdatePerson(p.ID, p.DisplayName, p.Role, store.PersonStatusSuspended); err != nil {
		t.Fatal(err)
	}
	f.srv.store.DeletePersonSessions(p.ID)
	f.srv.DeliverLogouts(context.Background())
	var bySub, bySid int
	for _, c := range app.got[1:] {
		if c["sub"] != nil {
			bySub++
		}
		if c["sid"] != nil {
			bySid++
		}
	}
	if bySub != 1 || bySid != 1 {
		t.Fatalf("after suspension: %v", app.got[1:])
	}
}

func TestBackchannelLogoutRetriesAndSaysWhenStuck(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{status: 503}
	f.receiveLogouts(t, app)
	f.signInRP(t)
	f.srv.store.DeletePersonSessionByDigest(auth.DigestToken(f.operator.Value))

	f.srv.DeliverLogouts(context.Background())
	if len(app.got) != 1 {
		t.Fatalf("first attempt: %d", len(app.got))
	}
	// Later, still failing: after a day it is the operator's business.
	f.srv.logouts.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	f.srv.DeliverLogouts(context.Background())
	notices, _ := f.srv.store.ActiveNotices()
	found := false
	for _, n := range notices {
		found = found || (n.Kind == "signin-logout" && n.App == "stream")
	}
	if !found {
		t.Fatalf("no notice after a day of failures: %+v", notices)
	}
	// It heals: the app answers, the row goes.
	app.status = 200
	f.srv.logouts.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	f.srv.DeliverLogouts(context.Background())
	owed, _ := f.srv.store.CountAppOutbox()
	if owed["stream"] != 0 {
		t.Fatalf("still owed: %v", owed)
	}
}

func TestSignEveryoneOut(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	f.signInRP(t)
	w := do(t, f.srv.Handler(), "POST", "/api/apps/stream/signin/signout", "", withCookie(f.operator))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	f.srv.DeliverLogouts(context.Background())
	if len(app.got) != 1 || app.got[0]["sub"] == nil {
		t.Fatalf("sign everyone out: %v", app.got)
	}
}

// A third-party app whose OIDC library cannot push its request (Paperless's
// allauth) is registered with par: false and signs in on the front channel —
// still with PKCE, still exact redirects.
func TestInteropFrontChannelClient(t *testing.T) {
	no := false
	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", PAR: &no})
	verifier := oauth2.GenerateVerifier()
	u := f.cfg.AuthCodeURL("st", oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", "n"))
	cb := f.follow(t, u)
	if cb.Query().Get("code") == "" {
		t.Fatalf("front-channel sign-in: %s", cb)
	}
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	tok, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(ctx, tok.Extra("id_token").(string)); err != nil {
		t.Fatal(err)
	}
	// Still no PKCE-less requests.
	loc := f.follow(t, f.cfg.AuthCodeURL("st"))
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("a request without PKCE got %s", loc)
	}
}

// Device sign-in (RFC 8628) with x/oauth2's own device client: the app asks
// for a code, the person approves it on fog's /device page, the app's poll
// turns into tokens — once.
func TestInteropDeviceFlow(t *testing.T) {
	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Device: true,
		Roles: []string{"admin", "member"}, Default: map[string]string{"operator": "admin", "member": "member"}})
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	if f.cfg.Endpoint.DeviceAuthURL == "" {
		t.Fatal("discovery has no device_authorization_endpoint")
	}
	// x/oauth2's DeviceAuth assumes a public client and sends no secret;
	// fog's clients are app servers and must authenticate (RFC 8628 §3.1),
	// so the request is made the way fog-kit makes it.
	da := deviceAuth(t, f, url.Values{"scope": {"openid profile roles"}, "device_name": {"Living Room TV"}})
	if !strings.HasSuffix(da.VerificationURI, "/device") || len(da.UserCode) != 9 {
		t.Fatalf("device auth = %+v", da)
	}

	// Polling before anyone approves says so.
	early := url.Values{"grant_type": {oidc.DeviceGrantType}, "device_code": {da.DeviceCode}}
	if code := tokenStatus(t, f, early); code != "authorization_pending" {
		t.Fatalf("early poll: %s", code)
	}
	if code := tokenStatus(t, f, early); code != "slow_down" {
		t.Fatalf("immediate second poll: %s", code)
	}

	// The person looks the code up as typed, sees who is asking, says yes.
	h := f.srv.Handler()
	typed := strings.ToLower(strings.ReplaceAll(da.UserCode, "-", " "))
	w := do(t, h, "POST", "/api/device/lookup", `{"userCode":"`+typed+`"}`, withCookie(f.operator), withOrigin("http://example.com"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Living Room TV") || !strings.Contains(w.Body.String(), `"app":"stream"`) {
		t.Fatalf("lookup: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/device/decide", `{"userCode":"`+da.UserCode+`","approve":true}`, withCookie(f.operator), withOrigin("http://example.com")); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}

	da.Interval = 0 // the test does not wait out the interval
	tok, err := f.cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		t.Fatal(err)
	}
	idt, err := f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(ctx, tok.Extra("id_token").(string))
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ Roles []string }
	idt.Claims(&c)
	if len(c.Roles) != 1 || c.Roles[0] != "admin" {
		t.Fatalf("roles = %v", c.Roles)
	}
	if code := tokenStatus(t, f, early); code != "invalid_grant" {
		t.Fatalf("a device code spent twice: %s", code)
	}
}

func TestDeviceFlowNeedsTheGrantAndHonoursNo(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	if _, err := f.cfg.DeviceAuth(ctx); err == nil {
		t.Fatal("an unauthenticated device request got a code")
	}
	if code := tokenStatusAt(t, f, "/oauth2/device_authorization", url.Values{"scope": {"openid"}}); code != "unauthorized_client" {
		t.Fatalf("a client not registered for device sign-in: %s", code)
	}

	g := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Device: true})
	da := deviceAuth(t, g, url.Values{"scope": {"openid"}})
	if w := do(t, g.srv.Handler(), "POST", "/api/device/decide", `{"userCode":"`+da.UserCode+`","approve":false}`, withCookie(g.operator), withOrigin("http://example.com")); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if code := tokenStatus(t, g, url.Values{"grant_type": {oidc.DeviceGrantType}, "device_code": {da.DeviceCode}}); code != "access_denied" {
		t.Fatalf("after no: %s", code)
	}
}

func deviceAuth(t *testing.T, f *idpFixture, form url.Values) *oauth2.DeviceAuthResponse {
	t.Helper()
	req, _ := http.NewRequest("POST", issuerURL+"/oauth2/device_authorization", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("stream", f.secret)
	res, err := f.rp.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var da oauth2.DeviceAuthResponse
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&da) != nil {
		t.Fatalf("device authorization: %d", res.StatusCode)
	}
	return &da
}

// tokenStatus posts a token request as the client and returns the OAuth
// error code ("" on success).
func tokenStatus(t *testing.T, f *idpFixture, form url.Values) string {
	return tokenStatusAt(t, f, "/oauth2/token", form)
}

func tokenStatusAt(t *testing.T, f *idpFixture, path string, form url.Values) string {
	t.Helper()
	req, _ := http.NewRequest("POST", issuerURL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("stream", f.secret)
	res, err := f.rp.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct{ Error string }
	json.NewDecoder(res.Body).Decode(&out)
	return out.Error
}

// "Sign out" posts to fog, which redirects to the app's registered
// post-logout address, and Chromium applies form-action to that redirect
// (checked against headless Chromium: 'self' alone strands the button). The
// confirm page's policy names fog and exactly that origin — no other.
func TestEndSessionConfirmFormActionNamesOnlyTheRedirect(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	f.authorize(t, f.push(t, oauth2.GenerateVerifier(), nil)) // signs the browser in
	q := url.Values{"client_id": {"stream"}, "post_logout_redirect_uri": {appOrigin + "/"}}
	res, err := f.browser.Get(issuerURL + "/oauth2/end_session?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	csp := res.Header.Get("Content-Security-Policy")
	if res.StatusCode != http.StatusOK || !strings.Contains(csp, "form-action 'self' "+appOrigin+";") && !strings.HasSuffix(csp, "form-action 'self' "+appOrigin) {
		t.Fatalf("confirm page %d, CSP %q", res.StatusCode, csp)
	}
}

func TestSigninFormAction(t *testing.T) {
	for _, c := range []struct {
		form bool
		to   string
		want string
	}{
		{false, "https://app.example.com/cb", "'none'"},
		{true, "", "'self'"},
		{true, "https://app.example.com/cb?x=1", "'self' https://app.example.com"},
		{true, "http://peer.lan:8443/visit/cb", "'self' http://peer.lan:8443"},
		// Nothing that could be policy syntax of its own gets in.
		{true, "https://a.example.com;script-src *", "'self'"},
		{true, "https://a.example.com 'unsafe-inline'", "'self'"},
		{true, "javascript:alert(1)", "'self'"},
		{true, "https://user@evil.example.com", "'self' https://evil.example.com"},
	} {
		if got := signinFormAction(c.form, c.to); got != c.want {
			t.Errorf("signinFormAction(%v, %q) = %q, want %q", c.form, c.to, got, c.want)
		}
	}
}
