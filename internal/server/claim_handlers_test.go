// The installer-handoff sign-in: one token, one use, one deadline. These
// pin the properties the handoff's safety depends on — a wrong or replayed
// token silently lands on the login page, never in a session.
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/store"
)

func seedClaim(t *testing.T, srv *Server, expires time.Time) (token string) {
	t.Helper()
	token, digest := auth.NewToken()
	if _, err := srv.store.CreatePerson("matthew", "matthew", store.PersonRoleOperator); err != nil {
		t.Fatal(err)
	}
	srv.store.SetSetting(store.SettingClaimDigest, digest)
	srv.store.SetSetting(store.SettingClaimExpires, expires.UTC().Format(time.RFC3339))
	srv.store.SetSetting(store.SettingClaimUser, "matthew")
	return token
}

func claimReq(h http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/setup/claim?token="+token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func hasSession(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName && c.Value != "" {
			return true
		}
	}
	return false
}

func TestClaimHappyPathIsSingleUse(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	token := seedClaim(t, srv, time.Now().Add(time.Hour))

	rec := claimReq(h, token)
	if rec.Code != http.StatusSeeOther || !hasSession(rec) {
		t.Fatalf("claim: code=%d session=%v, want 303 with session", rec.Code, hasSession(rec))
	}
	// Replay: same token again must land on the login page, sessionless.
	rec = claimReq(h, token)
	if hasSession(rec) {
		t.Fatal("claim token honored twice")
	}
	if v, _ := srv.store.GetSetting(store.SettingClaimDigest); v != "" {
		t.Fatal("claim digest survived use")
	}
}

func TestClaimWrongToken(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	seedClaim(t, srv, time.Now().Add(time.Hour))
	rec := claimReq(h, "not-the-token")
	if rec.Code != http.StatusSeeOther || hasSession(rec) {
		t.Fatalf("wrong token: code=%d session=%v, want sessionless redirect", rec.Code, hasSession(rec))
	}
	// A wrong guess must not burn the real token.
	if v, _ := srv.store.GetSetting(store.SettingClaimDigest); v == "" {
		t.Fatal("wrong token burned the real digest")
	}
}

func TestClaimExpired(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	token := seedClaim(t, srv, time.Now().Add(-time.Minute))
	rec := claimReq(h, token)
	if hasSession(rec) {
		t.Fatal("expired token honored")
	}
	if v, _ := srv.store.GetSetting(store.SettingClaimDigest); v != "" {
		t.Fatal("expired token not burned")
	}
}

func TestClaimNoTokenSeeded(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	rec := claimReq(h, "anything")
	if hasSession(rec) {
		t.Fatal("session minted with no claim token seeded")
	}
}
