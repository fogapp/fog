package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/selfupdate"
)

// The update source is held to the resolver's own rules when it is saved
// (selfupdate.ValidateSource), so a source the updater would refuse never
// reaches the store, and the refusal says why.
func TestUpdateSourceSettingUsesTheResolversRules(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	put := func(v string) (int, string) {
		body, _ := json.Marshal(map[string]string{selfupdate.SettingUpdateBaseURL: v})
		w := do(t, h, "PUT", "/api/settings", string(body), withCookie(c))
		return w.Code, w.Body.String()
	}

	for _, ok := range []string{
		"", "http://gitea.lan/matthew/fog", "https://github.com/you/fog", "https://gitlab.com/group/sub/fog",
		"https://files.example.com/fog", "https://files.example.com/fog/latest.json",
	} {
		if code, body := put(ok); code != http.StatusOK {
			t.Errorf("%q refused: %d %s", ok, code, body)
		}
		if got, _ := srv.store.GetSetting(selfupdate.SettingUpdateBaseURL); got != ok {
			t.Errorf("stored %q after saving %q", got, ok)
		}
	}

	for _, bad := range []string{
		"httpfoo", "gitea.lan/matthew/fog", "ftp://x/fog", "http://x/my fog", "http://x/fog;reboot",
		"http://x/$(id)", "http://x/fog?a=1", "http://u:p@x/fog",
	} {
		code, body := put(bad)
		if code != http.StatusBadRequest || !strings.Contains(body, "update source") || !strings.Contains(body, "not a valid URL") {
			t.Errorf("%q: %d %s, want a 400 giving the resolver's reason", bad, code, body)
		}
		if got, _ := srv.store.GetSetting(selfupdate.SettingUpdateBaseURL); got != "https://files.example.com/fog/latest.json" {
			t.Errorf("a refused %q replaced the stored source with %q", bad, got)
		}
	}
}

// The update token is bound to the host it was saved for: changing the
// source to another host deletes it (so an admin can't collect it by
// pointing the source at their own server), a change within the host keeps
// it, and a token for a plain-http source is refused outright.
func TestUpdateTokenDoesNotFollowTheSourceToAnotherHost(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	putSetting := func(v string) {
		body, _ := json.Marshal(map[string]string{selfupdate.SettingUpdateBaseURL: v})
		if w := do(t, h, "PUT", "/api/settings", string(body), withCookie(c)); w.Code != http.StatusOK {
			t.Fatalf("saving source %q: %d %s", v, w.Code, w.Body)
		}
	}
	putToken := func(v string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"key": selfupdate.SecretUpdateBasicAuth, "value": v})
		return do(t, h, "PUT", "/api/system/secrets", string(body), withCookie(c))
	}
	hasToken := func() bool {
		keys, _ := srv.store.SecretKeys(0)
		return slices.Contains(keys, selfupdate.SecretUpdateBasicAuth)
	}

	putSetting("http://gitea.lan/matthew/fog")
	if w := putToken("tok"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "plain http") {
		t.Fatalf("token for an http source: %d %s", w.Code, w.Body)
	}
	if hasToken() {
		t.Fatal("a token was stored for an http source")
	}

	putSetting("https://git.example.com/me/fog")
	if w := putToken("tok"); w.Code != http.StatusOK {
		t.Fatalf("token for an https source: %d %s", w.Code, w.Body)
	}
	if o, _ := srv.store.GetSetting(selfupdate.SettingUpdateTokenOrigin); o != "https://git.example.com" {
		t.Errorf("token bound to %q", o)
	}

	putSetting("https://git.example.com/me/fog-fork")
	if !hasToken() {
		t.Error("a source change within the same host dropped the token")
	}

	// The attack: point the source at a host you control.
	putSetting("https://evil.example.org/me/fog")
	if hasToken() {
		t.Error("the token survived a move of the source to another host")
	}
	if o, _ := srv.store.GetSetting(selfupdate.SettingUpdateTokenOrigin); o != "" {
		t.Errorf("binding kept as %q", o)
	}
}
