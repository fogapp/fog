package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/store"
)

func TestAppHome(t *testing.T) {
	s, h := newTestServer(t)
	// No Docker behind the socket: every app reads as new or missing, as
	// the dashboard shows it with Docker down.
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "docker.sock"))
	s.engine = &apps.Engine{Store: s.store, Vault: s.vault, Docker: dockerx.New()}
	s.store.SetSetting(store.SettingBaseDomain, "home.example")
	s.store.SetSetting(store.SettingServerName, "Amberwood")

	newtabID, newtab := appWithToken(t, s, "newtab")
	withSpec(t, s, newtabID, apps.AppSpec{Uses: []string{"home"}})
	immichID, immich := appWithToken(t, s, "immich")
	withSpec(t, s, immichID, apps.AppSpec{Routes: []apps.Route{{Port: 2283}}})
	if err := s.store.SetAppContracts(immichID, []string{"fog-files/1"}); err != nil {
		t.Fatal(err)
	}

	if w := do(t, h, "GET", "/app/v1/home", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	if w := do(t, h, "GET", "/app/v1/home", "", withBearer(immich)); w.Code != http.StatusForbidden {
		t.Errorf("not granted: %d %s", w.Code, w.Body)
	}

	w := do(t, h, "GET", "/app/v1/home", "", withBearer(newtab))
	if w.Code != http.StatusOK {
		t.Fatalf("granted: %d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var v homeView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Self != "newtab" || v.Server.Name != "Amberwood" || v.Server.URL != "https://fog.home.example" {
		t.Errorf("self/server = %q %+v", v.Self, v.Server)
	}
	var im *homeApp
	for i := range v.Apps {
		if v.Apps[i].Name == "immich" {
			im = &v.Apps[i]
		}
	}
	if im == nil || len(v.Apps) != 2 {
		t.Fatalf("apps = %+v", v.Apps)
	}
	if im.Title != "Immich" || im.Icon != "/store/immich/logo.svg" || im.URL != "https://immich.home.example" ||
		im.State != "new" || len(im.Contracts) != 1 || im.Contracts[0] != "fog-files/1" || im.OpenErrors == nil {
		t.Errorf("immich = %+v", *im)
	}
	if v.Inbox.FogUpdate.Available || v.Metrics != nil {
		t.Errorf("inbox/metrics = %+v %+v", v.Inbox, v.Metrics)
	}
	// The home view's inbox is the documented shape, without the Inbox
	// summary's feedback counts.
	if strings.Contains(w.Body.String(), `"feedback"`) {
		t.Errorf("home inbox grew feedback: %s", w.Body)
	}

	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	again := do(t, h, "GET", "/app/v1/home", "", withBearer(newtab), func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
	if again.Code != http.StatusNotModified {
		t.Errorf("unchanged view: %d", again.Code)
	}
	s.store.SetSetting(store.SettingServerName, "Birchwood")
	changed := do(t, h, "GET", "/app/v1/home", "", withBearer(newtab), func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
	if changed.Code != http.StatusOK || changed.Header().Get("ETag") == etag {
		t.Errorf("changed view: %d etag %s", changed.Code, changed.Header().Get("ETag"))
	}
}

func TestUsesHomeIsValid(t *testing.T) {
	spec := apps.AppSpec{Source: apps.Source{Type: "image", Image: "x"}, Uses: []string{"home"}}
	if err := spec.Validate(); err != nil {
		t.Errorf("uses: home rejected: %v", err)
	}
}
