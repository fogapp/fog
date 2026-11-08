package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/flare"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

func withBearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// newFlareServer builds a server with a real Forwarder wired in — the shape
// main constructs, which newTestServer skips. newServerForTest hands the
// forwarder the server's fed service, so signed delivery works out of the
// box.
func newFlareServer(t *testing.T) (*Server, http.Handler, *flare.Forwarder) {
	t.Helper()
	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fwd := &flare.Forwarder{Store: st, Log: log}
	srv := newServerForTest(config.Config{DataDir: dir}, st, Deps{Vault: vlt, Flare: fwd}, log)
	return srv, srv.Handler(), fwd
}

// forwardingModeFor flips one peer's use:flare.ingest capability through
// the same API the Federation UI drives.
func forwardingModeFor(t *testing.T, h http.Handler, cookie *http.Cookie, peerID int64, mode string) {
	t.Helper()
	body := fmt.Sprintf(`{"capabilities":[{"capability":"flare.ingest","direction":"use","enabled":true,"config":{"mode":%q}}]}`, mode)
	w := do(t, h, "PUT", fmt.Sprintf("/api/federation/peers/%d", peerID), body, withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("set forwarding mode: %d %s", w.Code, w.Body)
	}
}

// pairFlareInstances wires up the canonical two-instance arrangement:
// upstream invites with a flare.ingest grant, downstream joins. Returns the
// downstream's peer row ID for the upstream.
func pairFlareInstances(t *testing.T, upstreamHandler http.Handler, upCookie *http.Cookie,
	upstreamURL string, downHandler http.Handler, downCookie *http.Cookie) int64 {
	t.Helper()
	do(t, upstreamHandler, "PUT", "/api/federation/identity",
		fmt.Sprintf(`{"url":"%s"}`, upstreamURL), withCookie(upCookie))
	code := mintInvite(t, upstreamHandler, upCookie, `[{"capability":"flare.ingest"}]`)
	w := do(t, downHandler, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(downCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var joined struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &joined)
	return joined.ID
}

// The two-instances test, on federation rails: dave's fog pairs with mine;
// I grant dave errors-ingest; dave ingests a local app error carrying
// private request data and forwards it — redacted — over the signed
// channel. Both roles run the real handler stack over real HTTP.
func TestFogToFogForwarding(t *testing.T) {
	t.Parallel()

	upstream, upstreamHandler, _ := newFlareServer(t)
	upCookie := setup(t, upstreamHandler)
	upstreamHTTP := httptest.NewServer(upstreamHandler)
	defer upstreamHTTP.Close()

	down, downHandler, fwd := newFlareServer(t)
	st := down.store
	downCookie := setup(t, downHandler)
	peerID := pairFlareInstances(t, upstreamHandler, upCookie, upstreamHTTP.URL, downHandler, downCookie)
	forwardingModeFor(t, downHandler, downCookie, peerID, "redacted")

	// A local app error arrives downstream, carrying request data that must
	// not cross the wire.
	appID, key := flareApp(t, down, "manor")
	private := strings.Replace(rubyEvent, `"environment":"production"`,
		`"environment":"production","request":{"url":"https://manor.dave.example/x","data":{"q":"daves secret search"}}`, 1)
	if w := do(t, downHandler, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(private), withSentryAuth(key)); w.Code != http.StatusOK {
		t.Fatalf("downstream ingest: %d %s", w.Code, w.Body)
	}

	// Drain the outbox — one real signed POST to the upstream's fed endpoint.
	fwd.Flush(context.Background())

	groups, err := upstream.store.ListOpenErrorGroupsAll(10)
	if err != nil || len(groups) != 1 {
		t.Fatalf("upstream groups: %v %d", err, len(groups))
	}
	g := groups[0]
	if g.PeerID == 0 || g.AppName != "manor" || g.ErrorClass != "PG::UndefinedColumn" {
		t.Fatalf("upstream group: %+v", g)
	}
	// Redaction happened on the sender: the stored payload upstream must
	// not contain the private request data.
	events, _ := upstream.store.ListErrorEvents(g.ID, 10)
	if len(events) != 1 {
		t.Fatalf("upstream events: %d", len(events))
	}
	ev, _ := upstream.store.ErrorEventByID(events[0].ID)
	if strings.Contains(ev.Payload, "daves secret search") || strings.Contains(ev.Payload, "manor.dave.example") {
		t.Fatal("private request data crossed the wire")
	}
	if !strings.Contains(ev.Payload, "app/models/post.rb") {
		t.Fatal("the stacktrace should have crossed the wire")
	}
	// The outbox drained.
	if due, _ := st.DueFedOutbox(peerID, "flare", 10); len(due) != 0 {
		t.Fatalf("outbox should be empty, has %d", len(due))
	}

	// Redelivery is idempotent: replaying the same event bumps nothing
	// (event_id dedupe on the receiver).
	if w := do(t, downHandler, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(private), withSentryAuth(key)); w.Code != http.StatusOK {
		t.Fatalf("re-ingest: %d", w.Code)
	}
	fwd.Flush(context.Background())
	g2, _ := upstream.store.ErrorGroupByID(g.ID)
	if g2.EventsCount != g.EventsCount {
		t.Fatalf("redelivered event double-counted: %d -> %d", g.EventsCount, g2.EventsCount)
	}

	// Loop prevention: the upstream also forwards (toward a dead peer), but
	// peer-ingested events must never re-enqueue.
	upPeer, err := upstream.store.FedPeerByInstanceID(mustIdentity(t, down).InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	forwardingModeFor(t, upstreamHandler, upCookie, upPeer.ID, "full")
	tagged := strings.Replace(rubyEvent, `"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"`, `"event_id":"aaaa9c33ec9942ab8353589fcb2e04dc"`, 1)
	tagged = strings.Replace(tagged, `"platform":"ruby"`, `"platform":"ruby","tags":{"fog.app":"manor"}`, 1)
	if code := postSignedEnvelope(t, upstreamHTTP.URL, upstream, down, tagged); code != http.StatusOK {
		t.Fatalf("signed peer ingest: %d", code)
	}
	if due, _ := upstream.store.DueFedOutbox(upPeer.ID, "flare", 10); len(due) != 0 {
		t.Fatal("peer events must never enter the receiving instance's outbox (A→B→A loop)")
	}
}

// postSignedEnvelope sends one event envelope from `sender` to a target
// instance's signed flare endpoint, returning the status code.
func postSignedEnvelope(t *testing.T, targetURL string, target, sender *Server, eventJSON string) int {
	t.Helper()
	envBytes := []byte(envelope(eventJSON))
	req, _ := http.NewRequest("POST", targetURL+"/fed/v1/flare/envelope", nil)
	req.Header.Set("Content-Type", "application/x-sentry-envelope")
	sealFor(t, mustIdentity(t, sender), target, req, envBytes)
	status, _ := sendFed(t, req, mustIdentity(t, sender), target)
	return status
}

// Peer-relayed groups behave in the receiving UI's terms: origin badge in
// the fleet inbox, reachable and actionable through the global endpoints,
// and gone — with token access — the moment the peer is unpaired.
func TestFedPeerErrorLifecycle(t *testing.T) {
	t.Parallel()

	upstream, upstreamHandler, _ := newFlareServer(t)
	upCookie := setup(t, upstreamHandler)
	upstreamHTTP := httptest.NewServer(upstreamHandler)
	defer upstreamHTTP.Close()

	down, downHandler, _ := newFlareServer(t)
	downCookie := setup(t, downHandler)
	pairFlareInstances(t, upstreamHandler, upCookie, upstreamHTTP.URL, downHandler, downCookie)

	tagged := strings.Replace(rubyEvent, `"platform":"ruby"`, `"platform":"ruby","tags":{"fog.app":"manor"}`, 1)
	if code := postSignedEnvelope(t, upstreamHTTP.URL, upstream, down, tagged); code != http.StatusOK {
		t.Fatalf("peer ingest: %d", code)
	}

	// The group is peer-owned, carries the app name, and shows in the fleet
	// inbox with its origin (the peer's local alias).
	upPeer, _ := upstream.store.FedPeerByInstanceID(mustIdentity(t, down).InstanceID)
	w := do(t, upstreamHandler, "GET", "/api/errors", "", withCookie(upCookie))
	var fleet []map[string]any
	json.Unmarshal(w.Body.Bytes(), &fleet)
	if len(fleet) != 1 || fleet[0]["app"] != "manor" || fleet[0]["origin"] != upPeer.Name {
		t.Fatalf("fleet: %+v", fleet)
	}
	gid := int64(fleet[0]["id"].(float64))

	// A label renames the peer for humans — the fleet inbox's origin follows
	// it — while the name that addresses the peer stays exactly where it was.
	if w := do(t, upstreamHandler, "PUT", fmt.Sprintf("/api/federation/peers/%d", upPeer.ID),
		`{"label":"Dave's basement"}`, withCookie(upCookie)); w.Code != http.StatusOK {
		t.Fatalf("set label: %d %s", w.Code, w.Body)
	}
	w = do(t, upstreamHandler, "GET", "/api/errors", "", withCookie(upCookie))
	json.Unmarshal(w.Body.Bytes(), &fleet)
	if len(fleet) != 1 || fleet[0]["origin"] != "Dave's basement" {
		t.Fatalf("origin should follow the label: %+v", fleet)
	}
	if relabelled, _ := upstream.store.FedPeerByID(upPeer.ID); relabelled.Name != upPeer.Name {
		t.Fatalf("label must not touch the name: %q", relabelled.Name)
	}
	// A request that says nothing about the label leaves it alone.
	if w := do(t, upstreamHandler, "PUT", fmt.Sprintf("/api/federation/peers/%d", upPeer.ID),
		`{"capabilities":[{"capability":"apps.directory","direction":"use","enabled":true}]}`,
		withCookie(upCookie)); w.Code != http.StatusOK {
		t.Fatalf("capability update: %d %s", w.Code, w.Body)
	}
	if kept, _ := upstream.store.FedPeerByID(upPeer.ID); kept.Label != "Dave's basement" {
		t.Fatalf("capability update cleared the label: %q", kept.Label)
	}
	// An explicit empty label goes back to the advertised name.
	if w := do(t, upstreamHandler, "PUT", fmt.Sprintf("/api/federation/peers/%d", upPeer.ID),
		`{"label":""}`, withCookie(upCookie)); w.Code != http.StatusOK {
		t.Fatalf("clear label: %d %s", w.Code, w.Body)
	}
	w = do(t, upstreamHandler, "GET", "/api/errors", "", withCookie(upCookie))
	json.Unmarshal(w.Body.Bytes(), &fleet)
	if fleet[0]["origin"] != upPeer.Name {
		t.Fatalf("cleared label should fall back to the name: %+v", fleet)
	}

	// Global endpoints reach it (there is no local app namespace for it).
	if w := do(t, upstreamHandler, "GET", fmt.Sprintf("/api/errors/%d", gid), "", withCookie(upCookie)); w.Code != http.StatusOK {
		t.Fatalf("global detail: %d", w.Code)
	}
	if w := do(t, upstreamHandler, "POST", fmt.Sprintf("/api/errors/%d/resolve", gid), `{"nextDeploy":true}`, withCookie(upCookie)); w.Code != http.StatusOK {
		t.Fatalf("global resolve: %d %s", w.Code, w.Body)
	}
	g, _ := upstream.store.ErrorGroupByID(gid)
	if g.Status != "resolved" || g.ResolvedRel != "abc123" {
		// nextDeploy on a peer group pins to the release the peer last
		// reported — the only deploy knowledge we have of their fleet.
		t.Fatalf("peer resolve: %+v", g)
	}

	// Unpairing revokes signature access and cascades the groups.
	if w := do(t, upstreamHandler, "DELETE", fmt.Sprintf("/api/federation/peers/%d", upPeer.ID), "", withCookie(upCookie)); w.Code != http.StatusOK {
		t.Fatalf("unpair: %d", w.Code)
	}
	if code := postSignedEnvelope(t, upstreamHTTP.URL, upstream, down, tagged); code != http.StatusForbidden {
		t.Fatalf("unpaired peer can still ingest: %d", code)
	}
	if _, err := upstream.store.ErrorGroupByID(gid); err != store.ErrNotFound {
		t.Fatalf("peer groups should cascade on unpair: %v", err)
	}
}

// The signed flare endpoint refuses peers without a grant — capability
// enforcement lives on the resource owner, whatever the sender claims.
func TestFedFlareRequiresGrant(t *testing.T) {
	t.Parallel()

	upstream, upstreamHandler, _ := newFlareServer(t)
	upCookie := setup(t, upstreamHandler)
	upstreamHTTP := httptest.NewServer(upstreamHandler)
	defer upstreamHTTP.Close()
	do(t, upstreamHandler, "PUT", "/api/federation/identity",
		fmt.Sprintf(`{"url":"%s"}`, upstreamHTTP.URL), withCookie(upCookie))
	// Invite with NO grants.
	code := mintInvite(t, upstreamHandler, upCookie, `[]`)

	down, downHandler, _ := newFlareServer(t)
	downCookie := setup(t, downHandler)
	if w := do(t, downHandler, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(downCookie)); w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}

	tagged := strings.Replace(rubyEvent, `"platform":"ruby"`, `"platform":"ruby","tags":{"fog.app":"manor"}`, 1)
	if code := postSignedEnvelope(t, upstreamHTTP.URL, upstream, down, tagged); code != http.StatusForbidden {
		t.Fatalf("ungranted peer ingest: %d, want 403", code)
	}
	if groups, _ := upstream.store.ListOpenErrorGroupsAll(10); len(groups) != 0 {
		t.Fatalf("ungranted ingest stored groups: %d", len(groups))
	}
}
