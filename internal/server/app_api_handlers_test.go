package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// appWithToken creates a managed app plus its FOG_API_TOKEN the way a
// deploy would: vault secret + digest row.
func appWithToken(t *testing.T, s *Server, name string) (int64, string) {
	t.Helper()
	app, err := s.store.CreateApp(name)
	if err != nil {
		t.Fatal(err)
	}
	token := "apptoken-" + name
	ct, err := s.vault.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSecret(app.ID, apps.SecretFogAPIToken, ct); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetAppAPITokenDigest(app.ID, auth.DigestToken(token)); err != nil {
		t.Fatal(err)
	}
	return app.ID, token
}

// The full app-plane pipe: A publishes a directory to B via heartbeat, B
// exposes the peer to its apps, and B's app reads it all through /app/v1
// with nothing but its FOG_API_TOKEN.
func TestAppFederationPeersEndToEnd(t *testing.T) {
	t.Parallel()

	// Instance A: the inviter, publishing a fixed directory to every peer.
	a, aHandler := newTestServer(t)
	a.fed.DirectoryFor = func(peer *store.FedPeer) []fed.DirectoryApp {
		return []fed.DirectoryApp{{App: "feed", URL: "https://feed.a.example"}}
	}
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	do(t, aHandler, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie))
	code := mintInvite(t, aHandler, aCookie, `[]`)

	// Instance B: joins, then heartbeats to pick up A's directory.
	b, bHandler := newTestServer(t)
	bCookie := setup(t, bHandler)
	w := do(t, bHandler, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var joined struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	json.Unmarshal(w.Body.Bytes(), &joined)
	bPeer, _ := b.store.FedPeerByID(joined.ID)
	if err := b.fed.HeartbeatPeer(t.Context(), bPeer); err != nil {
		t.Fatal(err)
	}

	// B's app can't see the peer until the operator exposes it.
	_, token := appWithToken(t, b, "feed")
	appGet := func() []appPeerView {
		t.Helper()
		w := do(t, bHandler, "GET", "/app/v1/connect/peers", "", withBearer(token))
		if w.Code != http.StatusOK {
			t.Fatalf("app peers: %d %s", w.Code, w.Body)
		}
		var resp struct {
			Peers []appPeerView `json:"peers"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Peers
	}
	if peers := appGet(); len(peers) != 0 {
		t.Fatalf("peer visible to apps before the toggle: %+v", peers)
	}

	// Expose it (the "show their apps to my apps" toggle).
	w = do(t, bHandler, "PUT", fmt.Sprintf("/api/federation/peers/%d", joined.ID),
		`{"capabilities":[{"capability":"apps.directory","direction":"use","enabled":true}]}`, withCookie(bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("expose: %d %s", w.Code, w.Body)
	}
	peers := appGet()
	if len(peers) != 1 || peers[0].Name != joined.Name || !peers[0].Online {
		t.Fatalf("app peers: %+v", peers)
	}
	if len(peers[0].Apps) != 1 || peers[0].Apps[0].App != "feed" || peers[0].Apps[0].URL != "https://feed.a.example" {
		t.Fatalf("directory: %+v", peers[0].Apps)
	}
	// Unlabelled, displayName is the name — so an app can read it always.
	if peers[0].DisplayName != joined.Name {
		t.Fatalf("displayName without a label: %q, want %q", peers[0].DisplayName, joined.Name)
	}

	// Labelling the peer changes what apps show a human, and nothing else.
	// The label is free-form and non-unique, so it must never become the key
	// introductions resolve by.
	w = do(t, bHandler, "PUT", fmt.Sprintf("/api/federation/peers/%d", joined.ID),
		`{"label":"Dave's house"}`, withCookie(bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("label: %d %s", w.Code, w.Body)
	}
	peers = appGet()
	if len(peers) != 1 || peers[0].DisplayName != "Dave's house" {
		t.Fatalf("labelled displayName: %+v", peers)
	}
	if peers[0].Name != joined.Name {
		t.Fatalf("label moved the addressing key: %q, want %q", peers[0].Name, joined.Name)
	}

	// No token, wrong token: refused.
	if w := do(t, bHandler, "GET", "/app/v1/connect/peers", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := do(t, bHandler, "GET", "/app/v1/connect/peers", "", withBearer("wrong")); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", w.Code)
	}
}

// DirectoryFor only publishes what the grant covers.
func TestDirectoryRespectsGrantAndDomain(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t)
	// A peer with no apps.directory grant gets nothing even with apps around.
	p, _ := srv.store.CreatePairedPeer("01DIR", "dave", "https://x", "k")
	// Heartbeats to a peer without the grant advertise no directory.
	payload, err := srv.fed.PayloadFor(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Directory) != 0 {
		t.Fatalf("directory published without grant: %+v", payload.Directory)
	}
	// With the grant, the heartbeat's grants list names it.
	srv.store.SetFedCapability(p.ID, store.CapAppsDirectory, store.DirGrant, "{}")
	payload, _ = srv.fed.PayloadFor(p)
	found := false
	for _, g := range payload.Grants {
		if g == store.CapAppsDirectory {
			found = true
		}
	}
	if !found {
		t.Fatalf("grants list missing apps.directory: %+v", payload.Grants)
	}
}

// An app reads the household's reachability verdict through /app/v1 with
// nothing but its FOG_API_TOKEN, and what it reads is byte-for-byte what the
// Federation page shows the operator — one computation, two doors.
func TestAppFederationReachabilityMatchesSessionAPI(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	cookie := setup(t, h)
	_, token := appWithToken(t, s, "feed")

	if w := do(t, h, "GET", "/app/v1/connect/reachability", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d %s, want 401", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/app/v1/connect/reachability", "", withBearer("not-a-token")); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer: %d %s, want 401", w.Code, w.Body)
	}

	appView := func() (fedReachability, map[string]json.RawMessage) {
		t.Helper()
		w := do(t, h, "GET", "/app/v1/connect/reachability", "", withBearer(token))
		if w.Code != http.StatusOK {
			t.Fatalf("app reachability: %d %s", w.Code, w.Body)
		}
		var r fedReachability
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		return r, raw
	}
	sessionView := func() fedReachability {
		t.Helper()
		w := do(t, h, "GET", "/api/federation", "", withCookie(cookie))
		if w.Code != http.StatusOK {
			t.Fatalf("get federation: %d %s", w.Code, w.Body)
		}
		var resp struct {
			Reachability fedReachability `json:"reachability"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Reachability
	}

	// Fresh, domainless: dial-out-only, with advice — and the fields an app
	// keys off are always present, even when false/none.
	r, raw := appView()
	for _, k := range []string{"reachable", "via", "tunnel"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("app reachability missing %q: %s", k, raw)
		}
	}
	if r.Reachable || r.Via != "none" || r.Advice == "" {
		t.Fatalf("fresh instance should be unreachable with advice, got %+v", r)
	}
	if r != sessionView() {
		t.Fatalf("app plane %+v disagrees with session API %+v", r, sessionView())
	}

	// Advertise a URL: both doors flip together.
	if w := do(t, h, "PUT", "/api/federation/identity",
		`{"url":"https://fog.example.com"}`, withCookie(cookie)); w.Code != http.StatusOK {
		t.Fatalf("set advertised url: %d %s", w.Code, w.Body)
	}
	r, _ = appView()
	if !r.Reachable || r.Via != "advertised" || r.URL != "https://fog.example.com" || r.Advice != "" {
		t.Fatalf("after advertising a URL, expected reachable/advertised/no-advice, got %+v", r)
	}
	if r != sessionView() {
		t.Fatalf("app plane %+v disagrees with session API %+v", r, sessionView())
	}
}
