package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// The apps' own crossings land in the ledger beside fog's, newest first,
// tagged with the app and in fog's vocabulary — and an app that never
// implemented the optional endpoint costs the page nothing.
func TestFederationLedgerAggregatesAppLedgers(t *testing.T) {
	s, h := newTestServer(t)
	cookie := setup(t, h)

	dave, err := s.store.CreatePairedPeer("01DAVE", "dave", "https://fog.dave", "cGs=")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.Audit("me", "federation.pair", "dave", `{"role":"joiner"}`); err != nil {
		t.Fatal(err)
	}

	// feed declares the contract and serves a ledger; hub declares it and
	// answers 404, the shape of an app that never opted into the endpoint.
	appWithToken(t, s, "feed")
	appWithToken(t, s, "hub")
	feedApp, _ := s.store.AppByName("feed")
	hubApp, _ := s.store.AppByName("hub")
	s.store.SetAppContracts(feedApp.ID, []string{ContractConnectV1})
	s.store.SetAppContracts(hubApp.ID, []string{ContractConnectV1})

	var gotPeer, gotAuth string
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AppLedgerPath {
			http.NotFound(w, r)
			return
		}
		gotPeer = r.URL.Query().Get("peer")
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]any{
			{"at": "2999-01-01T00:00:00Z", "actor": "app:feed", "action": "federation.delivery.delivered",
				"subject": "01DAVE", "detail": `{"direction":"out"}`, "axis": "sharing",
				"summary": "delivered Create to feed.dave.house"},
			{"at": "2000-01-01T00:00:00Z", "action": "federation.follow.received", "subject": "01DAVE"},
		}})
	}))
	defer feed.Close()
	hub := httptest.NewServer(http.NotFoundHandler())
	defer hub.Close()
	s.appGrantEndpointFn = func(ctx context.Context, app *store.App) (string, string, error) {
		if app.Name == "feed" {
			return feed.URL, "feed-fog-token", nil
		}
		return hub.URL, "hub-fog-token", nil
	}

	get := func(q string) []ledgerEntry {
		t.Helper()
		w := do(t, h, "GET", "/api/federation/ledger"+q, "", withCookie(cookie))
		if w.Code != http.StatusOK {
			t.Fatalf("ledger %q: %d %s", q, w.Code, w.Body)
		}
		var resp struct {
			Entries []ledgerEntry `json:"entries"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Entries
	}

	all := get("")
	if len(all) != 3 {
		t.Fatalf("expected fog's row plus feed's two, got %d: %+v", len(all), all)
	}
	if all[0].Source != "feed" || all[0].Summary != "delivered Create to feed.dave.house" || all[0].Category != "sharing" {
		t.Fatalf("newest app row first, in fog's vocabulary: %+v", all[0])
	}
	if all[1].Action != "federation.pair" || all[1].Source != "" {
		t.Fatalf("fog's own row between the app's two by time: %+v", all[1])
	}
	last := all[2]
	if last.Category != "sharing" || last.Summary != "federation.follow.received" || last.Actor != "app:feed" {
		t.Fatalf("a row without axis/summary/actor takes the defaults: %+v", last)
	}
	if all[0].ID >= 0 || all[0].ID == all[2].ID {
		t.Fatalf("app rows carry distinct synthetic ids: %d %d", all[0].ID, all[2].ID)
	}
	if gotAuth != "Bearer feed-fog-token" {
		t.Fatalf("app read must carry the app's own token, got %q", gotAuth)
	}

	// Scoping to Dave carries his instance id to the app.
	get("?peer=dave")
	if gotPeer != dave.InstanceID {
		t.Fatalf("peer scope should reach the app as the instance id, got %q", gotPeer)
	}
	// A name that matches no peer asks the apps for nothing.
	gotPeer = "unset"
	if rows := get("?peer=nobody"); len(rows) != 0 || gotPeer != "unset" {
		t.Fatalf("unknown peer must not fan out to apps: rows=%d peer=%q", len(rows), gotPeer)
	}
}

func TestMergeLedgerKeepsLimitAndOrder(t *testing.T) {
	fog := []ledgerEntry{{AuditEntry: store.AuditEntry{ID: 7, At: "2026-01-02T00:00:00Z"}}}
	apps := []ledgerEntry{
		{AuditEntry: store.AuditEntry{At: "2026-01-03T00:00:00Z"}, Source: "feed"},
		{AuditEntry: store.AuditEntry{At: "2026-01-01T00:00:00Z"}, Source: "feed"},
	}
	out := mergeLedger(fog, apps, 2)
	if len(out) != 2 || out[0].Source != "feed" || out[1].ID != 7 {
		t.Fatalf("merge: %+v", out)
	}
	if out[0].ID != -1 {
		t.Fatalf("synthetic id: %d", out[0].ID)
	}
}
