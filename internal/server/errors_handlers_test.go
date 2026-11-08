package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// seedError ingests one occurrence through the real pipe and returns the
// group id.
func seedError(t *testing.T, srv *Server, h http.Handler, appID int64, key string) int64 {
	t.Helper()
	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(rubyEvent), withSentryAuth(key))
	if w.Code != http.StatusOK {
		t.Fatalf("seed ingest: %d %s", w.Code, w.Body)
	}
	groups, err := srv.store.ListErrorGroups(appID, "", 10)
	if err != nil || len(groups) == 0 {
		t.Fatalf("seed: %v %d groups", err, len(groups))
	}
	return groups[0].ID
}

func TestErrorsAPIRequiresAuth(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	if w := do(t, h, "GET", "/api/errors", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("fleet without session: %d, want 401", w.Code)
	}
	if w := do(t, h, "GET", "/api/apps/manor/errors", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("app errors without session: %d, want 401", w.Code)
	}
}

func TestErrorsFleetAndAppList(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	cookie := setup(t, h)
	appID, key := flareApp(t, srv, "manor")
	seedError(t, srv, h, appID, key)

	w := do(t, h, "GET", "/api/errors", "", withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("fleet: %d %s", w.Code, w.Body)
	}
	var fleet []map[string]any
	json.Unmarshal(w.Body.Bytes(), &fleet)
	if len(fleet) != 1 || fleet[0]["app"] != "manor" {
		t.Fatalf("fleet rows: %+v", fleet)
	}

	w = do(t, h, "GET", "/api/apps/manor/errors?status=open", "", withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("app list: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/apps/manor/errors?status=bogus", "", withCookie(cookie))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bogus filter: %d, want 400", w.Code)
	}
}

func TestErrorGroupDetailAndEvent(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	cookie := setup(t, h)
	appID, key := flareApp(t, srv, "manor")
	gid := seedError(t, srv, h, appID, key)

	w := do(t, h, "GET", fmt.Sprintf("/api/apps/manor/errors/%d", gid), "", withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", w.Code, w.Body)
	}
	var detail struct {
		Events []struct{ ID int64 }
	}
	json.Unmarshal(w.Body.Bytes(), &detail)
	if len(detail.Events) != 1 {
		t.Fatalf("events: %+v", detail.Events)
	}

	w = do(t, h, "GET", fmt.Sprintf("/api/apps/manor/errors/%d/events/%d", gid, detail.Events[0].ID), "", withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("event: %d %s", w.Code, w.Body)
	}
	var ev struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Payload["platform"] != "ruby" {
		t.Fatalf("payload should come back as raw JSON: %+v", ev.Payload)
	}
}

func TestErrorGroupOwnershipEnforced(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	cookie := setup(t, h)
	appID, key := flareApp(t, srv, "manor")
	gid := seedError(t, srv, h, appID, key)
	flareApp(t, srv, "hub")

	// hub's namespace must not expose manor's group.
	w := do(t, h, "GET", fmt.Sprintf("/api/apps/hub/errors/%d", gid), "", withCookie(cookie))
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-app group access: %d, want 404", w.Code)
	}
}

func TestErrorLifecycleActions(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	cookie := setup(t, h)
	appID, key := flareApp(t, srv, "manor")
	gid := seedError(t, srv, h, appID, key)

	w := do(t, h, "POST", fmt.Sprintf("/api/apps/manor/errors/%d/resolve", gid), `{"nextDeploy":false}`, withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", w.Code, w.Body)
	}
	g, _ := srv.store.ErrorGroupByID(gid)
	if g.Status != "resolved" {
		t.Fatalf("status = %s", g.Status)
	}

	w = do(t, h, "POST", fmt.Sprintf("/api/apps/manor/errors/%d/unresolve", gid), "{}", withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("unresolve: %d %s", w.Code, w.Body)
	}
	g, _ = srv.store.ErrorGroupByID(gid)
	if g.Status != "open" {
		t.Fatalf("status = %s", g.Status)
	}

	w = do(t, h, "POST", fmt.Sprintf("/api/apps/manor/errors/%d/ignore", gid), `{"forHours":24}`, withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("ignore: %d %s", w.Code, w.Body)
	}
	g, _ = srv.store.ErrorGroupByID(gid)
	if g.Status != "ignored" || g.IgnoredUntil == "" {
		t.Fatalf("after ignore: %+v", g)
	}

	// Mutations land in the audit trail.
	entries, err := srv.store.RecentAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	for _, want := range []string{"app.errors.resolve", "app.errors.unresolve", "app.errors.ignore"} {
		if !seen[want] {
			t.Errorf("audit missing %s (have %v)", want, seen)
		}
	}
}
