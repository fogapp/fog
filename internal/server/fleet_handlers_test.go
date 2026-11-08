package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/updates"
)

func TestFleetStatusEndpoints(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	c := setup(t, h)
	app, _ := srv.store.CreateApp("hub")
	rev, _ := srv.store.CreateRevision(app.ID, `{"source":{"type":"image","image":"x"},"backup":{"enabled":true,"hourUTC":4},"updatePolicy":"notify"}`, "", "test")
	srv.store.SetAppUpdate(app.ID, "sha256:aaaaaaaaaaaaaaaa", "sha256:bbbbbbbbbbbbbbbb", true)
	// Installed a week ago: a just-installed app is inside its
	// first-backup grace and reports stale:false by design.
	week := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	srv.store.DB().Exec(`UPDATE apps SET created_at = ? WHERE id = ?`, week, app.ID)
	srv.store.DB().Exec(`UPDATE app_revisions SET created_at = ? WHERE id = ?`, week, rev.ID)

	if w := do(t, h, "GET", "/api/backups/status", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}

	w := do(t, h, "GET", "/api/backups/status", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("backups: %d %s", w.Code, w.Body)
	}
	var bs struct {
		Apps []backup.AppBackupStatus `json:"apps"`
	}
	json.Unmarshal(w.Body.Bytes(), &bs)
	if len(bs.Apps) != 1 || bs.Apps[0].App != "hub" || !bs.Apps[0].Enabled || !bs.Apps[0].Stale || bs.Apps[0].HourUTC == nil {
		t.Fatalf("backups status: %+v", bs.Apps)
	}

	w = do(t, h, "GET", "/api/updates/status", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("updates: %d %s", w.Code, w.Body)
	}
	var us struct {
		Apps []updates.AppUpdateStatus `json:"apps"`
	}
	json.Unmarshal(w.Body.Bytes(), &us)
	if len(us.Apps) != 1 || us.Apps[0].Policy != "notify" || !us.Apps[0].UpdateAvailable {
		t.Fatalf("updates status: %+v", us.Apps)
	}
}

func TestNotifyTest(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	c := setup(t, h)

	// Nothing configured: a 400 that says so, not a silent "ok".
	if w := do(t, h, "POST", "/api/notify/test", "", withCookie(c)); w.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body)
	}

	var hits atomic.Int32
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/fog-test" || r.Header.Get("Title") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfy.Close()
	srv.store.SetSetting("ntfy_server", ntfy.URL)
	srv.store.SetSetting("ntfy_topic", "fog-test")

	if w := do(t, h, "POST", "/api/notify/test", "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("configured: %d %s", w.Code, w.Body)
	}
	if hits.Load() != 1 {
		t.Fatalf("want one delivery, got %d", hits.Load())
	}

	// A rejecting server is reported, not swallowed.
	srv.store.SetSetting("ntfy_topic", "wrong")
	if w := do(t, h, "POST", "/api/notify/test", "", withCookie(c)); w.Code != http.StatusBadGateway {
		t.Fatalf("rejected: %d %s", w.Code, w.Body)
	}
}
