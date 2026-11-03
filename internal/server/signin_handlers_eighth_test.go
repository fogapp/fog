package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/store"
)

// chatCredOf is the fog_chat a registration set.
func chatCredOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if (c.Name == "fog_chat" || c.Name == "__Host-fog_chat") && c.MaxAge > 0 {
			return c
		}
	}
	t.Fatalf("no chat credential: %d %s", w.Code, w.Body)
	return nil
}

// browserCookies are every cookie a sign-in set: the session and its
// browser cookie.
func browserCookies(w *httptest.ResponseRecorder) func(*http.Request) {
	return func(r *http.Request) {
		for _, c := range w.Result().Cookies() {
			if c.MaxAge > 0 {
				r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
			}
		}
	}
}

// The attack (eighth review, 1): a plain fog_chat, renamed __Host-fog_chat,
// was taken as unpinned. Which cookie fog set is recorded with the
// credential, and the pin follows the record, never the presented name.
func TestARenamedChatCredentialIsStillPinned(t *testing.T) {
	_, h, op := bareIPOperator(t)
	cred := chatCredOf(t, registerChatDevice(t, h, "web", withCookie(op), fromPeer("192.168.1.20", "")))
	if cred.Name != "fog_chat" {
		t.Fatalf("a bare address's chat credential: %s", cred.Name)
	}
	renamed := &http.Cookie{Name: "__Host-fog_chat", Value: cred.Value}
	https := func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }
	if c := do(t, h, "GET", "/api/chat/me", "", withCookie(renamed), https, fromPeer("192.168.1.66", "")).Code; c == http.StatusOK {
		t.Fatal("a plain chat credential renamed __Host- worked from another address")
	}
	if c := do(t, h, "GET", "/api/chat/me", "", withCookie(cred), fromPeer("192.168.1.20", "")).Code; c != http.StatusOK {
		t.Fatalf("the chat credential at home: %d", c)
	}
}

// The attack (eighth review, 2): a chat-only credential listed and ended
// the person's browser sessions, and pointed their alerts elsewhere. A
// chat credential or a device's token sees and ends only devices'
// sessions; where alerts go is the browser's own, freshly proven.
func TestAChatCredentialStaysInChat(t *testing.T) {
	srv, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup", goodSetup)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d", w.Code)
	}
	browser := browserCookies(w)
	op := personCookie(t, w)
	webReg := registerChatDevice(t, h, "web", browser)
	cred := chatCredOf(t, webReg)
	webDev := int64(decode(t, webReg)["device"].(map[string]any)["id"].(float64))
	phone, dev := nativeToken(t, registerChatDevice(t, h, "native", browser))
	// Each changes only its own device's push target: the other's is not
	// theirs to point elsewhere or silence.
	otherDev := map[string]int64{"a chat credential": dev, "a native token": webDev}
	sess, _, _ := srv.store.SessionPerson(auth.DigestToken(op.Value))
	for name, as := range map[string]func(*http.Request){"a chat credential": withCookie(cred), "a native token": bearerOf(phone)} {
		list := decode(t, do(t, h, "GET", "/api/chat/me/sessions", "", as))["sessions"].([]any)
		for _, s := range list {
			if int64(s.(map[string]any)["id"].(float64)) == sess.ID {
				t.Errorf("%s listed the browser's session", name)
			}
		}
		if c := do(t, h, "DELETE", fmt.Sprintf("/api/chat/me/sessions/%d", sess.ID), "", as).Code; c != http.StatusNotFound {
			t.Errorf("%s ending the browser's session: %d", name, c)
		}
		if c := do(t, h, "PUT", "/api/chat/me/prefs", `{"ntfyTopic":"attackers-topic"}`, as).Code; c != http.StatusForbidden {
			t.Errorf("%s pointing alerts elsewhere: %d", name, c)
		}
		if c := do(t, h, "POST", fmt.Sprintf("/api/chat/me/devices/%d/push", otherDev[name]), `{"endpoint":"https://push.example.com/x","keys":{"p256dh":"a","auth":"b"}}`, as).Code; c != http.StatusForbidden {
			t.Errorf("%s adding another device's push target: %d", name, c)
		}
		if c := do(t, h, "DELETE", fmt.Sprintf("/api/chat/me/devices/%d/push", otherDev[name]), "", as).Code; c != http.StatusForbidden {
			t.Errorf("%s removing another device's push target: %d", name, c)
		}
		execSQL(t, srv.store, `UPDATE people SET prefs = '{"pushEnabled":true}' WHERE handle = 'admin'`)
		if c := do(t, h, "PUT", "/api/chat/me/prefs", `{"pushEnabled":false}`, as).Code; c != http.StatusForbidden {
			t.Errorf("%s turning push off: %d", name, c)
		}
	}
	if _, _, err := srv.store.SessionPerson(auth.DigestToken(op.Value)); err != nil {
		t.Fatal("the browser's session was ended")
	}
	// The browser itself, freshly signed in, may.
	if c := do(t, h, "PUT", "/api/chat/me/prefs", `{"ntfyTopic":"my-topic"}`, browser, withCookie(cred)).Code; c != http.StatusOK {
		t.Fatalf("the browser's own alert change: %d", c)
	}
	// ... and not once its proof is stale.
	execSQL(t, srv.store, fmt.Sprintf(`UPDATE people_sessions SET auth_time = '%s' WHERE token_digest = '%s'`,
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), auth.DigestToken(op.Value)))
	if c := do(t, h, "PUT", "/api/chat/me/prefs", `{"ntfyTopic":"other-topic"}`, browser, withCookie(cred)).Code; c != http.StatusConflict {
		t.Fatalf("a stale browser's alert change: %d", c)
	}
	// Prefs that aren't about alerts stay chat's.
	if c := do(t, h, "PUT", "/api/chat/me/prefs", `{"ntfyTopic":"my-topic","receipts":true}`, withCookie(cred)).Code; c != http.StatusOK {
		t.Fatalf("a chat preference from the chat credential: %d", c)
	}
}

// The attack (eighth review, 3): a browser holding its chat credential
// couldn't mint a native token — the chat credential was taken for the
// minting session. The minter is the browser's own session, always.
func TestABrowserWithChatOpenCanAddAPhone(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup", goodSetup)
	browser := browserCookies(w)
	cred := chatCredOf(t, registerChatDevice(t, h, "web", browser))
	if w := registerChatDevice(t, h, "native", browser, withCookie(cred)); w.Code != http.StatusOK {
		t.Fatalf("a phone from a browser with chat open: %d %s", w.Code, w.Body)
	}
}

// The attack (eighth review, 4): a single planted fog_chat — another
// person's — beat the browser's own session on chat's routes. A chat
// credential counts only when bound to this browser and the browser's own
// person; a plant is expired wherever it could have been set.
func TestAPlantedChatCredentialDoesNotWin(t *testing.T) {
	srv, h := newTestServer(t)
	w := do(t, h, "POST", "/api/setup", goodSetup, fromPeer("192.168.1.20", ""))
	browser := browserCookies(w)
	mallory, _ := srv.store.CreatePerson("mallory", "Mallory", store.PersonRoleMember)
	// Mallory's own chat credential, from her own browser at the same
	// address.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.20:1"
	rec := httptest.NewRecorder()
	mtok, _, err := srv.mintPersonSession(r, mallory, 0, "web")
	if err != nil {
		t.Fatal(err)
	}
	srv.setSessionCookie(rec, r, mtok)
	mallorysBrowser := browserCookies(rec)
	planted := chatCredOf(t, registerChatDevice(t, h, "web", mallorysBrowser, fromPeer("192.168.1.20", "")))
	got := do(t, h, "GET", "/api/chat/me", "", fromPeer("192.168.1.20", ""), withCookie(&http.Cookie{Name: "fog_chat", Value: planted.Value}), browser)
	if p, _ := decode(t, got)["person"].(map[string]any); p == nil || p["handle"] != "admin" {
		t.Fatalf("a planted chat credential made the operator %v", p)
	}
	_ = got
	// Expired where it could have been set — at a path under /api/chat.
	w = do(t, h, "GET", "/api/chat/rooms", "", fromPeer("192.168.1.20", ""), withCookie(&http.Cookie{Name: "fog_chat", Value: planted.Value}), browser)
	expired := map[string]bool{}
	for _, c := range w.Result().Cookies() {
		if c.Name == "fog_chat" && c.MaxAge < 0 {
			expired[c.Path] = true
		}
	}
	if !expired["/api/chat"] || !expired["/api/chat/rooms"] {
		t.Fatalf("the plant is not expired at its paths: %v", expired)
	}
}

// The attack (eighth review, 6): grants for one link filled the table, and
// then no link got one. Each link holds at most its own share, its oldest
// replaced.
func TestShareGrantsAreBoundedPerLink(t *testing.T) {
	srv, _ := newTestServer(t)
	a, _ := srv.store.CreateFileShare(auth.DigestToken("a"), "A", "", mustHash(t, "the real share password"), 0)
	b, _ := srv.store.CreateFileShare(auth.DigestToken("b"), "B", "", mustHash(t, "the real share password"), 0)
	a, _ = srv.store.FileShareByID(a.ID)
	b, _ = srv.store.FileShareByID(b.ID)
	grant := func(sh *store.FileShare) *http.Cookie {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/s/x", nil)
		r.SetPathValue("token", "x")
		srv.grantShare(w, r, sh)
		for _, c := range w.Result().Cookies() {
			return c
		}
		return nil
	}
	held := func(sh *store.FileShare, c *http.Cookie) bool {
		r := httptest.NewRequest("GET", "/s/x", nil)
		r.AddCookie(c)
		return srv.shareGranted(r, sh)
	}
	first := grant(a)
	var last *http.Cookie
	for range maxShareGrants + 50 {
		last = grant(a)
	}
	if n := len(srv.signinLimits().shareGrants.m[a.ID]); n > maxShareGrants {
		t.Fatalf("link A holds %d grants", n)
	}
	if held(a, first) || !held(a, last) {
		t.Fatal("the oldest grant wasn't the one replaced")
	}
	if c := grant(b); c == nil || !held(b, c) {
		t.Fatal("another link got no grant")
	}
}
