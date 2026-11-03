package server

// Regression tests for the sign-in review (docs/signin-contract.md): each
// performs the attack or failure a finding described and shows it no longer
// works.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- helpers ----

// asKim makes Stream a household app and the fixture's browser Kim's.
func (f *idpFixture) asKim(t *testing.T) *http.Cookie {
	t.Helper()
	kim := addMember(t, f.srv, "kim")
	if err := f.srv.store.PutSigninPolicy(store.SigninPolicy{AppID: f.app.ID, Audience: "household"}); err != nil {
		t.Fatal(err)
	}
	f.browser.Jar, _ = cookiejar.New(nil)
	f.signInBrowser(t, kim)
	return kim
}

// idClaims verifies an ID token as the RP and returns its claims.
func (f *idpFixture) idClaims(t *testing.T, tok *oauth2.Token) map[string]any {
	t.Helper()
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	idt, err := f.provider.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(ctx, tok.Extra("id_token").(string))
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := idt.Claims(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// deliver lets any delivery already kicked finish, delivers what the
// outbox still owes, and returns the logout tokens the app received since
// the last call.
func (f *idpFixture) deliver(app *appBackchannel) []map[string]any {
	f.srv.bgWG.Wait()
	f.srv.DeliverLogouts(context.Background())
	got := app.got[app.seen:]
	app.seen = len(app.got)
	return got
}

func patchPerson(t *testing.T, f *idpFixture, handle, body string) {
	t.Helper()
	p, err := f.srv.store.PersonByHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, f.srv.Handler(), "PATCH", fmt.Sprintf("/api/people/%d", p.ID), body, withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatalf("patch %s: %d %s", handle, w.Code, w.Body)
	}
}

// ---- 1. secret rotation ----

// TestRevokingASecretNowRefusesTheLeakedOne: one act ends a leaked secret,
// where it used to take two rotations.
func TestRevokingASecretNowRefusesTheLeakedOne(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	h := f.srv.Handler()
	// A routine rotation leaves the running secret working.
	if w := do(t, h, "POST", "/api/apps/stream/signin/rotate", "", withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}
	if code := tokenStatusAt(t, f, "/oauth2/par", url.Values{"response_type": {"code"}}); code == "invalid_client" {
		t.Fatal("a routine rotation locked the running app out")
	}
	tok := f.signInRP(t)
	// The secret leaks: revoke it now.
	w := do(t, h, "POST", "/api/apps/stream/signin/rotate", `{"now":true}`, withCookie(f.operator))
	if w.Code != http.StatusOK {
		t.Fatalf("revoke now: %d %s", w.Code, w.Body)
	}
	if code := tokenStatusAt(t, f, "/oauth2/par", url.Values{"response_type": {"code"}}); code != "invalid_client" {
		t.Fatalf("the leaked secret still authenticates: %q", code)
	}
	// What was minted under it is gone too.
	if _, err := f.provider.UserInfo(gooidc.ClientContext(context.Background(), f.rp), oauth2.StaticTokenSource(tok)); err == nil {
		t.Fatal("an access token minted under the leaked secret still works")
	}
	if !auditHas(t, f.srv, "app.signin.revoke_secret") {
		t.Fatal("revoking was not audited")
	}
}

// ---- 2. a change that reduces access ends app sessions ----

// TestDemotionSignsOutOfApps is the demotion finding: a member made guest
// kept every app session; now each app that saw them is told, by subject.
func TestDemotionSignsOutOfApps(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	f.asKim(t)
	sub := f.idClaims(t, f.signInRP(t))["sub"]
	f.deliver(app)

	patchPerson(t, f, "kim", `{"role":"guest"}`)
	got := f.deliver(app)
	if len(got) != 1 || got[0]["sub"] != sub {
		t.Fatalf("after demotion the app heard %v; want a logout for %v", got, sub)
	}
	// A rename takes nothing away: nothing is sent.
	patchPerson(t, f, "kim", `{"displayName":"Kim W."}`)
	if got := f.deliver(app); len(got) != 0 {
		t.Fatalf("a rename sent %v", got)
	}
}

// TestALesserRoleSignsOut: lowering someone's role in an app's policy ends
// their session there, as removing them does.
func TestALesserRoleSignsOut(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	f.asKim(t)
	if w := do(t, f.srv.Handler(), "PUT", "/api/apps/stream/signin", `{"audience":"household","people":[{"handle":"kim","role":"admin"}]}`, withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	f.signInRP(t)
	f.deliver(app)
	if w := do(t, f.srv.Handler(), "PUT", "/api/apps/stream/signin", `{"audience":"household","people":[{"handle":"kim","role":"member"}]}`, withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if got := f.deliver(app); len(got) != 1 || got[0]["sub"] == nil {
		t.Fatalf("admin → member sent %v", got)
	}
}

// TestADeployThatNarrowsSignsOut: an app that changes its own role map on
// deploy ends the sessions it decided under the old one.
func TestADeployThatNarrowsSignsOut(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	f.signInRP(t) // the operator, as admin
	f.deliver(app)
	m := signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Logout: "/auth/oidc/backchannel",
		Roles: []string{"admin", "member"}, Default: map[string]string{"operator": "member", "member": "member"}}
	if _, err := f.srv.signin.Register(f.app, appOrigin, issuerURL, "", m); err != nil {
		t.Fatal(err)
	}
	if got := f.deliver(app); len(got) != 1 {
		t.Fatalf("a deploy that made the operator a member sent %v", got)
	}
}

// TestTheTokenEndpointAsksPolicyAgain: a code minted before a demotion is
// not redeemed after it.
func TestTheTokenEndpointAsksPolicyAgain(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	f.asKim(t)
	verifier := oauth2.GenerateVerifier()
	cb := f.authorize(t, f.push(t, verifier, nil))
	patchPerson(t, f, "kim", `{"role":"guest"}`)
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	if _, err := f.cfg.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier)); err == nil {
		t.Fatal("a code minted before a demotion was redeemed after it")
	}
}

// ---- 4. the session cap ----

// TestIDTokenStatesWhenFogsSessionEnds: every ID token carries the fog
// session's expiry, so an app can end its own no later.
func TestIDTokenStatesWhenFogsSessionEnds(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	c := f.idClaims(t, f.signInRP(t))
	sess, _, err := f.srv.store.SessionPerson(auth.DigestToken(f.operator.Value))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c[oidc.ClaimSessionExpires].(float64); !ok || int64(got) != sess.ExpiresAt.Unix() {
		t.Fatalf("%s = %v, want %d", oidc.ClaimSessionExpires, c[oidc.ClaimSessionExpires], sess.ExpiresAt.Unix())
	}
	disc := do(t, f.srv.Handler(), "GET", "/.well-known/openid-configuration", "", onFogHost)
	if !strings.Contains(disc.Body.String(), oidc.ClaimSessionExpires) || strings.Contains(disc.Body.String(), `"picture"`) {
		t.Fatalf("discovery claims: %s", disc.Body)
	}
}

// TestAnExpiredSessionSendsALogout: a session that runs out used to be
// pruned silently; now the apps it signed in to hear by sid.
func TestAnExpiredSessionSendsALogout(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	sid := f.idClaims(t, f.signInRP(t))["sid"]
	execSQL(t, f.srv.store, `UPDATE people_sessions SET expires_at = '2000-01-01T00:00:00Z'`)
	if _, err := f.srv.store.PruneExpiredSessions(); err != nil {
		t.Fatal(err)
	}
	if got := f.deliver(app); len(got) != 1 || got[0]["sid"] != sid {
		t.Fatalf("after expiry the app heard %v; want a logout for %v", got, sid)
	}
}

// ---- 8/extra. end_session takes only an ID token as its hint ----

// TestEndSessionHintMustBeAnIDToken: anything else fog signs with the same
// keys — here a logout token naming the session — must not sign a person
// out without asking.
func TestEndSessionHintMustBeAnIDToken(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	sess, _, _ := f.srv.store.SessionPerson(auth.DigestToken(f.operator.Value))
	notAnIDToken, err := f.srv.keys.Sign(oidc.LogoutToken(issuerURL, "stream", "", sess.SID, "j", time.Now()), "logout+jwt")
	if err != nil {
		t.Fatal(err)
	}
	gateHint, _ := f.srv.keys.Sign(map[string]any{"iss": issuerURL, "aud": "https://sab." + idpDomain, "sid": sess.SID}, "JWT")
	for name, hint := range map[string]string{"a logout token": notAnIDToken, "a gate assertion": gateHint} {
		res, err := f.browser.Get(issuerURL + "/oauth2/end_session?" + url.Values{"id_token_hint": {hint}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if _, _, err := f.srv.store.SessionPerson(auth.DigestToken(f.operator.Value)); err != nil {
			t.Fatalf("%s as id_token_hint signed the person out without asking", name)
		}
	}
}

// ---- 7. the person is told ----

// recordNtfy wires a chat notifier whose ntfy sends land on a channel.
func recordNtfy(srv *Server) chan string {
	got := make(chan string, 16)
	srv.chat.Notify = &chat.Notifier{Svc: srv.chat, Ntfy: func(topic, title, body string) { got <- topic + ": " + title }}
	return got
}

func TestThePersonIsToldOfCredentialChanges(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	h := f.srv.Handler()
	got := recordNtfy(f.srv)
	kimCookie := addMember(t, f.srv, "kim")
	kim, _ := f.srv.store.PersonByHandle("kim")
	if err := f.srv.store.SetPersonPrefs(kim.ID, `{"ntfyTopic":"kim-phone"}`); err != nil {
		t.Fatal(err)
	}
	wait := func(what string) string {
		t.Helper()
		select {
		case m := <-got:
			if !strings.HasPrefix(m, "kim-phone: ") {
				t.Fatalf("%s went to %q", what, m)
			}
			return m
		case <-time.After(5 * time.Second):
			t.Fatalf("kim was not told of %s", what)
		}
		return ""
	}

	// The operator sets Kim a password: Kim hears, and is signed out.
	if w := do(t, h, "POST", fmt.Sprintf("/api/people/%d/password", kim.ID), `{"password":"a new long password"}`, withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatalf("set password: %d %s", w.Code, w.Body)
	}
	if m := wait("a password set for her"); !strings.Contains(m, "set a new password") {
		t.Fatalf("notice = %q", m)
	}
	if w := do(t, h, "GET", "/api/people/me", "", withCookie(kimCookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("kim's old session outlived a password the operator set: %d", w.Code)
	}

	// Kim signs in (told), and a session of hers is revoked (told).
	w := do(t, h, "POST", "/api/auth/login", `{"handle":"kim","password":"a new long password"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	wait("a sign-in")
	if w := do(t, h, "POST", fmt.Sprintf("/api/people/%d/signout", kim.ID), "", withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if m := wait("being signed out"); !strings.Contains(m, "signed you out everywhere") {
		t.Fatalf("notice = %q", m)
	}

	// And Settings › Your sign-in lists what happened.
	w = do(t, h, "POST", "/api/auth/login", `{"handle":"kim","password":"a new long password"}`)
	wait("a sign-in")
	me := do(t, h, "GET", "/api/people/me", "", withCookie(sessionCookie(t, w)))
	var view struct{ Activity []accountEvent }
	json.Unmarshal(me.Body.Bytes(), &view)
	actions := []string{}
	for _, e := range view.Activity {
		actions = append(actions, e.Action)
	}
	if len(actions) < 4 || actions[len(actions)-1] != "people.password.set" || !view.Activity[len(actions)-1].ByOther {
		t.Fatalf("activity = %+v", view.Activity)
	}
}

// ---- 9. emergency key rotation ----

func TestEmergencyKeyRotation(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	h := f.srv.Handler()
	app := &appBackchannel{}
	f.receiveLogouts(t, app)
	tok := f.signInRP(t)
	raw := tok.Extra("id_token").(string)
	if _, err := f.srv.keys.Verify(raw); err != nil {
		t.Fatal(err)
	}
	f.deliver(app)

	if w := do(t, h, "POST", "/api/signin/keys/retire", `{}`, withCookie(f.operator)); w.Code != http.StatusBadRequest {
		t.Fatalf("retired without the confirm step: %d", w.Code)
	}
	kim := addMember(t, f.srv, "kim")
	if w := do(t, h, "POST", "/api/signin/keys/retire", `{"confirm":"`+retireConfirm+`"}`, withCookie(kim)); w.Code == http.StatusOK {
		t.Fatal("a member retired the keys")
	}
	w := do(t, h, "POST", "/api/signin/keys/retire", `{"confirm":"`+retireConfirm+`"}`, withCookie(f.operator))
	if w.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", w.Code, w.Body)
	}
	// What the old key signed no longer verifies — at fog, or at an RP
	// reading the published set.
	if _, err := f.srv.keys.Verify(raw); err == nil {
		t.Fatal("an ID token signed by a retired key still verifies at fog")
	}
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	fresh, err := gooidc.NewProvider(ctx, issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Verifier(&gooidc.Config{ClientID: "stream"}).Verify(ctx, raw); err == nil {
		t.Fatal("an RP reading the key set still accepts a token signed by a retired key")
	}
	// A new key signs at once, and the apps are told everyone is out.
	if _, err := f.srv.keys.Sign(map[string]any{"x": 1}, "JWT"); err != nil {
		t.Fatalf("nothing signs after the rotation: %v", err)
	}
	// Every session the app might hold is reached, not only those of people
	// it has seen: Kim never signed in to Stream, but a leaked key could have
	// signed her in — or anyone, which only sessions-revoked can name.
	got := f.deliver(app)
	opSub := f.idClaims(t, f.signInRP(t))["sub"]
	kimP, _ := f.srv.store.PersonByHandle("kim")
	var sawOp, sawKim, sawAll bool
	for _, c := range got {
		sub, _ := c["sub"].(string)
		sawOp = sawOp || sub == opSub
		sawKim = sawKim || strings.HasSuffix(sub, "/"+kimP.UID)
		if ev, _ := c["events"].(map[string]any); ev[oidc.EventSessionsRevoked] != nil && c["sid"] == oidc.AllSessionsSID {
			sawAll = true
		}
	}
	if !sawOp || !sawKim || !sawAll {
		t.Fatalf("after the rotation the app heard %v (operator %v, kim %v, every session %v)", got, sawOp, sawKim, sawAll)
	}
	if !auditHas(t, f.srv, "signin.keys.retire") {
		t.Fatal("the rotation was not audited")
	}
}

// ---- 10. limits wherever a secret is checked ----

func fromIP(ip string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = ip + ":1234" }
}

// TestPasswordGuessesAreLimitedPerAccount: guesses at one handle from many
// addresses meet one limit, and a passkey still gets the person in.
func TestPasswordGuessesAreLimitedPerAccount(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)
	refused := false
	for i := range 15 {
		w := do(t, h, "POST", "/api/auth/login", `{"handle":"admin","password":"wrong guess `+fmt.Sprint(i)+`"}`, fromIP(fmt.Sprintf("10.0.0.%d", i)))
		refused = refused || w.Code == http.StatusTooManyRequests
	}
	if !refused {
		t.Fatal("15 guesses at one account from 15 addresses were all checked")
	}
	if w := do(t, h, "POST", "/api/auth/login", goodSetup, fromIP("10.0.1.1")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the right password inside the lockout: %d", w.Code)
	}
}

// TestReauthAndPasswordChangeAreLimitedPerPerson: a stolen session is no
// place to guess the password behind it.
func TestReauthAndPasswordChangeAreLimitedPerPerson(t *testing.T) {
	srv, h := newTestServer(t)
	op := setup(t, h)
	// At fog's domain, where the session is __Host- and not pinned to an
	// address, so the guesses can come from anywhere.
	if err := srv.store.SetSetting(store.SettingBaseDomain, gateDomain); err != nil {
		t.Fatal(err)
	}
	httpsSetup(t, srv)
	_ = op
	// Signed in over https at fog's domain: a __Host- session, not pinned.
	var hostSession *http.Cookie
	for _, c := range do(t, h, "POST", "/api/auth/login", goodSetup, onFogHost).Result().Cookies() {
		if c.Name == auth.SecureCookieName && c.MaxAge > 0 {
			hostSession = c
		}
	}
	if hostSession == nil {
		t.Fatal("no __Host- session from an https sign-in")
	}
	for path, body := range map[string]string{
		"/api/people/reauth/password": `{"password":"wrong guess"}`,
		"/api/people/me/password":     `{"current":"wrong guess","password":"another long phrase"}`,
	} {
		refused := false
		for i := range 15 {
			w := do(t, h, "POST", path, body, onFogHost, withCookie(hostSession), fromIP(fmt.Sprintf("10.1.0.%d", i)))
			refused = refused || w.Code == http.StatusTooManyRequests
		}
		if !refused {
			t.Errorf("%s: 15 guesses from 15 addresses were all checked", path)
		}
	}
}

// TestAuthorizeIsLimitedPerPerson: a signed-in person cannot hammer the
// authorization endpoint.
func TestAuthorizeIsLimitedPerPerson(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	limited := false
	for range 130 {
		res, err := f.browser.Get(issuerURL + "/oauth2/authorize?client_id=stream&request_uri=urn:ietf:params:oauth:request_uri:nope")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("130 authorize requests from one person were all served")
	}
}

// TestClientSecretGuessesDontLockTheAppOut: the per-client limit is per
// caller, so a stranger spraying wrong secrets can't spend the app's budget.
func TestClientSecretGuessesDontLockTheAppOut(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	h := f.srv.Handler()
	for range 130 {
		do(t, h, "POST", "/oauth2/par", "client_id=stream&client_secret=guess", onFogHost, fromIP("203.0.113.9"), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		})
	}
	f.push(t, oauth2.GenerateVerifier(), nil) // fails the test if refused
}

// TestDeviceCodeGuessingIsLimited: wrong user codes are charged to the
// person, and at the limit the lookup is refused before it is made.
func TestDeviceCodeGuessingIsLimited(t *testing.T) {
	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Device: true})
	da := deviceAuth(t, f, url.Values{"scope": {"openid"}})
	h := f.srv.Handler()
	for range 10 {
		do(t, h, "POST", "/api/device/lookup", `{"userCode":"BBBB-BBBB"}`, withCookie(f.operator), withOrigin("http://example.com"))
	}
	if w := do(t, h, "POST", "/api/device/lookup", `{"userCode":"`+da.UserCode+`"}`, withCookie(f.operator), withOrigin("http://example.com")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after ten wrong codes, the right one was looked up: %d", w.Code)
	}
}

// TestASiblingPageCantSpendTheDeviceBudget: a page on a sibling host shares
// fog's site, so the browser sends fog's cookie with what it fires. Lookups
// it starts — a plain GET, or a POST from its own origin or none — are
// refused before they cost the person a wrong code, and the person can
// still approve their television.
func TestASiblingPageCantSpendTheDeviceBudget(t *testing.T) {
	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Device: true})
	da := deviceAuth(t, f, url.Values{"scope": {"openid"}})
	h := f.srv.Handler()
	for range 20 {
		do(t, h, "GET", "/api/device/lookup?user_code=BBBB-BBBB", "", withCookie(f.operator))
		do(t, h, "POST", "/api/device/lookup", `{"userCode":"BBBB-BBBB"}`, withCookie(f.operator), withOrigin("https://evil.example.com"))
		do(t, h, "POST", "/api/device/lookup", `{"userCode":"BBBB-BBBB"}`, withCookie(f.operator))
	}
	if w := do(t, h, "POST", "/api/device/lookup", `{"userCode":"`+da.UserCode+`"}`, withCookie(f.operator), withOrigin("http://example.com")); w.Code != http.StatusOK {
		t.Fatalf("after a sibling's 60 tries, the person's own lookup: %d %s", w.Code, w.Body)
	}
}

// TestADeviceCodeReplayRevokesItsTokens: the device grant's code, like the
// authorization code, revokes what it minted when presented again.
func TestADeviceCodeReplayRevokesItsTokens(t *testing.T) {
	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Device: true})
	ctx := gooidc.ClientContext(context.Background(), f.rp)
	da := deviceAuth(t, f, url.Values{"scope": {"openid"}})
	if w := do(t, f.srv.Handler(), "POST", "/api/device/decide", `{"userCode":"`+da.UserCode+`","approve":true}`, withCookie(f.operator), withOrigin("http://example.com")); w.Code != 200 {
		t.Fatal(w.Body)
	}
	da.Interval = 0
	tok, err := f.cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		t.Fatal(err)
	}
	if code := tokenStatus(t, f, url.Values{"grant_type": {oidc.DeviceGrantType}, "device_code": {da.DeviceCode}}); code != "invalid_grant" {
		t.Fatalf("replay: %q", code)
	}
	if _, err := f.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
		t.Fatal("the access token outlived its device code's replay")
	}
}

// TestTheDeviceGrantRefusesRepeatedParameters: the profile refuses a
// repeated parameter at the token endpoint, the device grant included.
func TestTheDeviceGrantRefusesRepeatedParameters(t *testing.T) {
	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Device: true})
	da := deviceAuth(t, f, url.Values{"scope": {"openid"}})
	form := url.Values{"grant_type": {oidc.DeviceGrantType}, "device_code": {da.DeviceCode, "other"}}
	if code := tokenStatus(t, f, form); code != "invalid_request" {
		t.Fatalf("a repeated device_code: %q", code)
	}
}

// ---- extra: session fixation ----

// TestSigningInEndsTheBrowsersPreviousSession: on a shared computer, the
// last person's session — and the apps it signed in to — ends when the next
// person signs in, rather than lingering behind the new cookie.
func TestSigningInEndsTheBrowsersPreviousSession(t *testing.T) {
	srv, h := newTestServer(t)
	op := setup(t, h)
	kim := addMember(t, srv, "kim")
	kimP, _ := srv.store.PersonByHandle("kim")
	if err := srv.store.SetPersonPassword(kimP.ID, mustHash(t, "kims long password")); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "POST", "/api/auth/login", `{"handle":"kim","password":"kims long password"}`, withCookie(op))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if fresh := sessionCookie(t, w); fresh.Value == op.Value || fresh.Value == kim.Value {
		t.Fatal("signing in reused a token the browser brought")
	}
	if _, _, err := srv.store.SessionPerson(auth.DigestToken(op.Value)); err == nil {
		t.Fatal("the browser's previous session outlived the next sign-in")
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func auditHas(t *testing.T, srv *Server, action string) bool {
	t.Helper()
	entries, err := srv.store.RecentAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == action {
			return true
		}
	}
	return false
}
