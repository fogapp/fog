package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// registerChatDevice registers a fresh device key as clientKind, as opts
// present the caller.
func registerChatDevice(t *testing.T, h http.Handler, clientKind string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"name":"d","kind":"ios","deviceKey":"` + people.EncodeKey(pub) + `","clientKind":"` + clientKind + `"}`
	return do(t, h, "POST", "/api/chat/me/devices", body, opts...)
}

func nativeToken(t *testing.T, w *httptest.ResponseRecorder) (string, int64) {
	t.Helper()
	var out struct {
		Token  string
		Device struct{ ID int64 }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("no token: %d %s", w.Code, w.Body)
	}
	return out.Token, out.Device.ID
}

func bearerOf(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

// The attack (seventh review, 1): a stolen native token registered a new
// device as clientKind "web", was handed a full browser session, read the
// system secrets from anywhere — and the session outlived revoking the
// stolen device. Now a device's token mints and binds nothing; a browser's
// chat device is bound to the browser's own session, never a new one; and
// revoking a device ends its token.
func TestANativeTokenCantBecomeAnOperatorSession(t *testing.T) {
	srv, h := newTestServer(t)
	op := setup(t, h)
	tok, dev := nativeToken(t, registerChatDevice(t, h, "native", withCookie(op)))
	_ = srv
	for _, kind := range []string{"web", "native"} {
		w := registerChatDevice(t, h, kind, bearerOf(tok))
		if w.Code != http.StatusForbidden {
			t.Fatalf("a native token registering a %s device: %d", kind, w.Code)
		}
		for _, c := range w.Result().Cookies() {
			if c.MaxAge > 0 {
				t.Fatalf("a native token was handed a cookie: %v", c)
			}
		}
	}
	if c := do(t, h, "GET", fmt.Sprintf("/api/chat/me/devices/%d/challenge", dev), "", bearerOf(tok)).Code; c != http.StatusOK {
		t.Fatalf("challenge: %d", c)
	}
	if c := do(t, h, "POST", fmt.Sprintf("/api/chat/me/devices/%d/session", dev), `{"nonce":"x","signature":"x","clientKind":"web"}`, bearerOf(tok)).Code; c != http.StatusForbidden {
		t.Fatalf("a native token binding a browser session: %d", c)
	}
	if c := secretsStatus(t, h, bearerOf(tok)); c == http.StatusOK {
		t.Fatal("a native token read the system secrets")
	}
	// A browser's chat device: a credential of its own, reaching chat
	// alone; the browser's own session is untouched and stays full.
	w := registerChatDevice(t, h, "web", withCookie(op))
	if w.Code != http.StatusOK {
		t.Fatalf("the browser's own chat device: %d %s", w.Code, w.Body)
	}
	var chatCred *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName && c.MaxAge > 0 {
			t.Fatal("opening chat replaced the browser's session")
		}
		if c.Name == "fog_chat" {
			chatCred = c
		}
	}
	if chatCred == nil {
		t.Fatal("no chat credential")
	}
	if c := secretsStatus(t, h, withCookie(op)); c != http.StatusOK {
		t.Fatalf("the operator's browser after opening chat: %d", c)
	}
	if c := secretsStatus(t, h, withCookie(&http.Cookie{Name: auth.CookieName, Value: chatCred.Value})); c == http.StatusOK {
		t.Fatal("a chat credential renamed as the session read the system secrets")
	}
	if c := do(t, h, "GET", "/api/chat/me", "", withCookie(chatCred)).Code; c != http.StatusOK {
		t.Fatalf("the chat credential in chat: %d", c)
	}
	if c := do(t, h, "GET", "/api/people/me", "", withCookie(chatCred)).Code; c != http.StatusUnauthorized {
		t.Fatalf("the chat credential outside chat: %d", c)
	}
	if c := registerChatDevice(t, h, "native", withCookie(chatCred)).Code; c != http.StatusForbidden {
		t.Fatalf("a chat credential minting a native token: %d", c)
	}
	// Revoking the native device ends its token.
	if c := do(t, h, "DELETE", fmt.Sprintf("/api/chat/me/devices/%d", dev), "", withCookie(op)).Code; c != http.StatusOK {
		t.Fatalf("revoke: %d", c)
	}
	if c := do(t, h, "GET", "/api/chat/me", "", bearerOf(tok)).Code; c != http.StatusUnauthorized {
		t.Fatalf("a revoked device's token: %d", c)
	}
}

// The attack (seventh review, 2, 3 and 4): holding one session, a loop of
// device mints spent the person's sign-in budget — their password and
// passkey got 429 — and evicted their own sessions; every browser sign-in
// with chat open left an orphan that pushed out the phone's token; device
// rows were written before any budget was checked. Now mints are charged to
// the minting session, a device holds one token, device tokens are never
// evicted, and the budgets and caps are checked before anything is written.
func TestMintingCantLockAPersonOut(t *testing.T) {
	srv, h := newTestServer(t)
	op := setup(t, h)
	phone, _ := nativeToken(t, registerChatDevice(t, h, "native", withCookie(op)))
	refused := false
	for range deviceMints + 2 {
		w := registerChatDevice(t, h, "native", withCookie(op))
		if w.Code == http.StatusTooManyRequests {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("one session minted device tokens without limit")
	}
	devices := func() int {
		list, _ := srv.store.PersonDevices(1)
		return len(list)
	}
	n := devices()
	if registerChatDevice(t, h, "native", withCookie(op)).Code != http.StatusTooManyRequests || devices() != n {
		t.Fatal("a refused mint wrote a device row")
	}
	// The person's own sign-ins are untouched.
	if c := loginAs(t, h, "admin", "correct horse battery", "192.168.1.30"); c != http.StatusOK {
		t.Fatalf("the person's password sign-in after the loop: %d", c)
	}
	if c := do(t, h, "POST", "/api/people/login/passkey/begin", "", onFogHost).Code; c != http.StatusOK {
		t.Fatalf("the person's passkey sign-in after the loop: %d", c)
	}
	// Browser sign-ins past the cap never push out the phone.
	srv.signinLimits().sessionMint.Reset("1")
	r := httptest.NewRequest("GET", "/", nil)
	for range maxSessionsPerPerson + 5 {
		srv.signinLimits().sessionMint.Reset("1")
		if _, _, err := srv.mintPersonSession(r, 1, 0, "web"); err != nil {
			t.Fatal(err)
		}
	}
	if c := do(t, h, "GET", "/api/chat/me", "", bearerOf(phone)).Code; c != http.StatusOK {
		t.Fatalf("the phone's token after many browser sign-ins: %d", c)
	}
	// A device cap, and a key package cap, before anything is written.
	srv.signinLimits().deviceMint.Reset(strconv.Itoa(1))
	for devices() < maxDevicesPerPerson {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		if _, err := srv.store.CreatePersonDevice(1, "x", "web", pub, nil); err != nil {
			t.Fatal(err)
		}
	}
	if c := registerChatDevice(t, h, "web", withCookie(op)).Code; c != http.StatusConflict || devices() != maxDevicesPerPerson {
		t.Fatalf("a device past the cap: %d", c)
	}
	pkgs := make([]string, maxKeyPackagesPerRegistration+1)
	for i := range pkgs {
		pkgs[i] = "AA"
	}
	b, _ := json.Marshal(map[string]any{"deviceKey": "x", "keyPackages": pkgs})
	if c := do(t, h, "POST", "/api/chat/me/devices", string(b), withCookie(op)).Code; c != http.StatusBadRequest && c != http.StatusConflict {
		t.Fatalf("too many key packages: %d", c)
	}
}

// The attack (seventh review, 5 and 6): the share lock was the link's, so
// a stranger's guesses locked out everyone holding the link (and
// dismissing it lasted one guess); and every request re-hashed the
// password, so parallel downloads got 429. Now guessing locks out the
// guesser's address alone, and a right password earns a cookie that skips
// the hash.
func TestShareGuessesLockOutTheGuesserNotTheLink(t *testing.T) {
	srv, h := newTestServer(t)
	op := setup(t, h)
	if c := do(t, h, "POST", "/api/files/shares", `{"path":"x","days":1,"password":"short"}`, withCookie(op)).Code; c != http.StatusBadRequest {
		t.Fatalf("a 5-character share password: %d", c)
	}
	sh, err := srv.store.CreateFileShare(auth.DigestToken("share-token"), "Photos/2026", "", mustHash(t, "the real share password"), 0)
	if err != nil {
		t.Fatal(err)
	}
	sh, _ = srv.store.FileShareByID(sh.ID)
	try := func(addr, pw string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/s/share-token", nil)
		r.SetPathValue("token", "share-token")
		r.RemoteAddr = addr + ":1"
		if pw != "" {
			r.SetBasicAuth("", pw)
		}
		for _, o := range opts {
			o(r)
		}
		srv.sharePasswordOK(w, r, sh)
		return w
	}
	for i := range shareFailures {
		if c := try("203.0.113.9", fmt.Sprintf("guess-%d", i)).Code; c != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i, c)
		}
	}
	if c := try("203.0.113.9", "guess").Code; c != http.StatusTooManyRequests {
		t.Fatalf("the guesser past its budget for the link: %d", c)
	}
	// Whoever holds the link and its password still gets in.
	w := try("192.168.1.20", "the real share password")
	if w.Code != http.StatusOK {
		t.Fatalf("the link's holder beside a guesser: %d", w.Code)
	}
	// A guesser at it all day is locked out — the address, not the link.
	l := srv.signinLimits()
	for i := range shareStrikes {
		l.shareStrikes.Allow("203.0.113.9")
		_ = i
	}
	if c := try("203.0.113.9", "the real share password").Code; c != http.StatusLocked {
		t.Fatalf("a day's guesser: %d", c)
	}
	if c := try("192.168.1.21", "the real share password").Code; c != http.StatusOK {
		t.Fatalf("the link for everyone else: %d", c)
	}
	// The right password's cookie skips the hash: with the share links'
	// part of the line full, the holder's next requests still go through.
	var grant *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == shareCookieName(sh) {
			grant = c
		}
	}
	if grant == nil || grant.Path != "/s/share-token" || !grant.HttpOnly {
		t.Fatalf("no share cookie: %v", w.Result().Cookies())
	}
	release, _ := auth.HoldLine(auth.Caller{Pool: auth.PoolShare}, 9)
	for range 5 {
		if c := try("192.168.1.20", "", withCookie(grant)).Code; c != http.StatusOK {
			release()
			t.Fatalf("a download with the share cookie while the line is full: %d", c)
		}
	}
	release()
	// A link guessed at all day tells the operator.
	for i := range shareAlarm + 1 {
		try(fmt.Sprintf("198.51.%d.%d", i/250, i%250+1), "wrong")
	}
	found := false
	list, _ := srv.store.ActiveNotices()
	for _, n := range list {
		found = found || n.Kind == shareAlarmKind
	}
	if !found {
		t.Fatal("the operator was not told")
	}
}

// The attack (seventh review, 7): a sibling app pointed images at
// /oauth2/authorize and spent the person's 120-a-minute budget. Only a
// top-level page load is charged; anything else is refused uncharged.
func TestSubresourcesCantSpendAuthorize(t *testing.T) {
	f := newIdP(t, signin.Manifest{})
	h := f.srv.Handler()
	op, _ := f.srv.store.PersonByHandle("admin")
	as := func(dest string) int {
		return do(t, h, "GET", "/oauth2/authorize?client_id=stream", "", withCookie(f.operator), func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "same-site")
			r.Header.Set("Sec-Fetch-Mode", map[bool]string{true: "navigate", false: "no-cors"}[dest == "document"])
			r.Header.Set("Sec-Fetch-Dest", dest)
		}).Code
	}
	for range 200 {
		if c := as("image"); c != http.StatusBadRequest {
			t.Fatalf("an image pointed at authorize: %d", c)
		}
	}
	if f.srv.appAPILimiter.Exhausted("authorize:person:" + strconv.FormatInt(op.ID, 10)) {
		t.Fatal("subresources spent the person's authorize budget")
	}
	if c := as("document"); c == http.StatusTooManyRequests {
		t.Fatalf("the person's own sign-in after the images: %d", c)
	}
	_ = store.SessionDevice
}
