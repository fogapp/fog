package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// connectPair is two real instances set up for Connect: A grants B the
// apps.introduce capability and runs app "feed" behind a stub that serves
// the fog-connect/1 grants endpoint; B has exposed A to its apps.
type connectPair struct {
	*fogPair
	grantCalls    atomic.Int32
	lastGrantBody []byte
	grantAnswer   string
}

func newConnectPair(t *testing.T, grants string, contracted bool) *connectPair {
	t.Helper()
	cp := &connectPair{fogPair: twoFogs(t, grants), grantAnswer: `{"credential":{"apiKey":"feed-minted-key-123"},"payload":{"hello":"from-a"}}`}
	feedA, err := cp.a.store.CreateApp("feed")
	if err != nil {
		t.Fatal(err)
	}
	if contracted {
		cp.a.store.SetAppContracts(feedA.ID, []string{ContractConnectV1})
	}
	appStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AppGrantsPath || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer feed-fog-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cp.lastGrantBody, _ = io.ReadAll(r.Body)
		cp.grantCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, cp.grantAnswer)
	}))
	t.Cleanup(appStub.Close)
	cp.a.appGrantEndpointFn = func(ctx context.Context, app *store.App) (string, string, error) {
		return appStub.URL, "feed-fog-token", nil
	}
	// The requesting side must have exposed the peer to its apps.
	if err := cp.b.store.SetFedCapability(cp.aPeerOnB.ID, store.CapAppsDirectory, store.DirUse, "{}"); err != nil {
		t.Fatal(err)
	}
	return cp
}

// fedConnect posts one handcrafted connect request from B to A over the
// signed channel and returns A's status and body.
func (cp *connectPair) fedConnect(t *testing.T, v fed.Vouch, sig string, payload string) (int, string) {
	t.Helper()
	body := `{"vouch":"` + fed.EncodeVouch(v, sig) + `"`
	if payload != "" {
		body += `,"payload":` + payload
	}
	body += "}"
	req, _ := http.NewRequest("POST", cp.aHTTP.URL+"/fed/v1/connect", nil)
	sealFor(t, mustIdentity(t, cp.b), cp.a, req, []byte(body))
	status, out := sendFed(t, req, mustIdentity(t, cp.b), cp.a)
	return status, string(out)
}

func (cp *connectPair) vouchFromB(t *testing.T, nonce string) fed.Vouch {
	t.Helper()
	now := time.Now().UTC()
	return fed.Vouch{
		From: mustIdentity(t, cp.b).InstanceID, To: mustIdentity(t, cp.a).InstanceID, App: "feed",
		FromApp: "https://feed.b.house", FromKey: "FEED-B-PUBKEY", Nonce: nonce,
		IssuedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(fed.VouchTTL).Format(time.RFC3339),
	}
}

// The whole chain across two real instances: feed@B asks its fogd to
// connect to feed@A; the vouch relays over the signed channel; A's fogd
// authorizes it and asks its local feed (a stub here) to mint a credential,
// which flows back to feed@B with the handshake payload feed@A answered.
// Nobody typed a URL or an API key anywhere.
func TestConnectEndToEnd(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce"}]`, true)
	reqID, token := appWithToken(t, cp.b, "feed")
	cp.b.store.SetAppContracts(reqID, []string{ContractConnectV1})
	w := do(t, cp.bH, "POST", "/app/v1/connect/grants",
		fmt.Sprintf(`{"peer":%q,"app":"feed","publicKey":"FEED-B-PUBKEY","payload":{"hello":"from-b"}}`, cp.aPeerOnB.Name), withBearer(token))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var got struct {
		App        string          `json:"app"`
		Credential json.RawMessage `json:"credential"`
		Payload    json.RawMessage `json:"payload"`
		Peer       struct {
			Instance string `json:"instance"`
			Name     string `json:"name"`
		} `json:"peer"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.App != "feed" || !strings.Contains(string(got.Credential), "feed-minted-key-123") {
		t.Fatalf("connect response: %s", w.Body)
	}
	if string(got.Payload) != `{"hello":"from-a"}` {
		t.Fatalf("the granting app's payload did not come back: %s", w.Body)
	}
	if got.Peer.Instance != mustIdentity(t, cp.a).InstanceID || got.Peer.Name != cp.aPeerOnB.Name {
		t.Fatalf("peer in the answer = %+v", got.Peer)
	}
	// The granting app saw who is connecting, its key, the vouch itself
	// and the requester's payload.
	var grant struct {
		Peer struct {
			Instance  string `json:"instance"`
			PublicKey string `json:"publicKey"`
		} `json:"peer"`
		Vouch   string          `json:"vouch"`
		Payload json.RawMessage `json:"payload"`
	}
	json.Unmarshal(cp.lastGrantBody, &grant)
	if grant.Peer.Instance != mustIdentity(t, cp.b).InstanceID || grant.Peer.PublicKey != "FEED-B-PUBKEY" {
		t.Fatalf("grant payload names the wrong peer: %s", cp.lastGrantBody)
	}
	if grant.Vouch == "" || string(grant.Payload) != `{"hello":"from-b"}` {
		t.Fatalf("grant payload missing vouch or handshake payload: %s", cp.lastGrantBody)
	}
	if v, _, err := fed.DecodeVouch(grant.Vouch); err != nil || v.App != "feed" || v.Nonce == "" {
		t.Fatalf("the app was handed an unusable vouch: %+v %v", v, err)
	}
	if cp.grantCalls.Load() != 1 {
		t.Fatalf("grant calls: %d", cp.grantCalls.Load())
	}

	// Idempotency: replaying the exact signed request (same nonce) returns
	// the cached answer without minting again.
	v := cp.vouchFromB(t, "fixed-nonce-1")
	sig := fed.SignVouch(mustIdentity(t, cp.b), v)
	s1, b1 := cp.fedConnect(t, v, sig, "")
	s2, b2 := cp.fedConnect(t, v, sig, "")
	if s1 != 200 || s2 != 200 || b1 != b2 {
		t.Fatalf("replay: %d %d %s vs %s", s1, s2, b1, b2)
	}
	if cp.grantCalls.Load() != 2 { // one fresh mint for the fixed nonce, replay served from cache
		t.Fatalf("replayed connect minted again: %d calls", cp.grantCalls.Load())
	}
}

// Enforcement on the granting side, through the app plane: no
// apps.introduce grant → refused; a target app without the contract →
// refused. The requesting app hears the reason.
func TestConnectEnforcement(t *testing.T) {
	cp := newConnectPair(t, `[]`, false)
	reqID, token := appWithToken(t, cp.b, "feed")
	cp.b.store.SetAppContracts(reqID, []string{ContractConnectV1})
	connect := func() *httptest.ResponseRecorder {
		return do(t, cp.bH, "POST", "/app/v1/connect/grants",
			fmt.Sprintf(`{"peer":%q,"app":"feed"}`, cp.aPeerOnB.Name), withBearer(token))
	}
	if w := connect(); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "may not connect") {
		t.Fatalf("ungranted connect: %d %s", w.Code, w.Body)
	}
	cp.a.store.SetFedCapability(cp.bPeerOnA.ID, store.CapAppsIntroduce, store.DirGrant, "{}")
	if w := connect(); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), ContractConnectV1) {
		t.Fatalf("contractless connect: %d %s", w.Code, w.Body)
	}
	// Allow-listed to another app only.
	feedA, _ := cp.a.store.AppByName("feed")
	cp.a.store.SetAppContracts(feedA.ID, []string{ContractConnectV1})
	cp.a.store.SetFedCapability(cp.bPeerOnA.ID, store.CapAppsIntroduce, store.DirGrant, `{"apps":["hub"]}`)
	if w := connect(); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "may not connect to feed") {
		t.Fatalf("connect outside the allow-list: %d %s", w.Code, w.Body)
	}
	// A peer the operator never exposed to apps reads as unknown.
	cp.b.store.RemoveFedCapability(cp.aPeerOnB.ID, store.CapAppsDirectory, store.DirUse)
	if w := connect(); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "no such peer") {
		t.Fatalf("unexposed peer: %d %s", w.Code, w.Body)
	}
	if cp.grantCalls.Load() != 0 {
		t.Fatalf("a refused connect reached the app: %d calls", cp.grantCalls.Load())
	}
}

// authorizeConnection refuses a vouch that is not the signing peer's own,
// not for this instance, not signed by the pinned key, expired, or without
// a nonce — before the operator's policy is even consulted.
func TestConnectVouchChecks(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce"}]`, true)
	bID := mustIdentity(t, cp.b)
	cases := []struct {
		name   string
		vouch  func() (fed.Vouch, string)
		status int
		reason string
	}{
		{"minted by another instance", func() (fed.Vouch, string) {
			v := cp.vouchFromB(t, "n-other-from")
			v.From = "01JOTHERINSTANCE0000000000"
			return v, fed.SignVouch(bID, v)
		}, http.StatusForbidden, "minted by another instance"},
		{"for another instance", func() (fed.Vouch, string) {
			v := cp.vouchFromB(t, "n-other-to")
			v.To = "01JOTHERINSTANCE0000000000"
			return v, fed.SignVouch(bID, v)
		}, http.StatusForbidden, "for another instance"},
		{"tampered after signing", func() (fed.Vouch, string) {
			v := cp.vouchFromB(t, "n-tamper")
			sig := fed.SignVouch(bID, v)
			v.FromKey = "SWAPPED"
			return v, sig
		}, http.StatusForbidden, "does not verify"},
		{"expired", func() (fed.Vouch, string) {
			v := cp.vouchFromB(t, "n-expired")
			v.ExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
			return v, fed.SignVouch(bID, v)
		}, http.StatusForbidden, "expired"},
		{"no nonce", func() (fed.Vouch, string) {
			v := cp.vouchFromB(t, "")
			return v, fed.SignVouch(bID, v)
		}, http.StatusBadRequest, "no nonce"},
	}
	for _, c := range cases {
		v, sig := c.vouch()
		status, body := cp.fedConnect(t, v, sig, "")
		if status != c.status || !strings.Contains(body, c.reason) {
			t.Errorf("%s: %d %s (want %d containing %q)", c.name, status, body, c.status, c.reason)
		}
	}
	if cp.grantCalls.Load() != 0 {
		t.Fatalf("a refused vouch reached the app: %d calls", cp.grantCalls.Load())
	}
	// And a sound one, carrying no payload, still connects.
	v := cp.vouchFromB(t, "n-good")
	if status, body := cp.fedConnect(t, v, fed.SignVouch(bID, v), ""); status != 200 || !strings.Contains(body, "feed-minted-key-123") {
		t.Fatalf("sound vouch: %d %s", status, body)
	}
}

// A grant answer without a credential is a refusal, not a connection: the
// requesting app never receives an empty grant to store.
func TestConnectRequiresACredential(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce"}]`, true)
	cp.grantAnswer = `{"payload":{"only":"handshake"}}`
	reqID, token := appWithToken(t, cp.b, "feed")
	cp.b.store.SetAppContracts(reqID, []string{ContractConnectV1})
	w := do(t, cp.bH, "POST", "/app/v1/connect/grants",
		fmt.Sprintf(`{"peer":%q}`, cp.aPeerOnB.Name), withBearer(token))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "no credential") {
		t.Fatalf("credential-less grant: %d %s", w.Code, w.Body)
	}
}

// Connect joins an app to its sibling. A Hub that names the peer's feed as
// its target — say, because it picked the far app by contract alone — is
// refused by its own fogd before anything crosses to the peer, so the far
// Feed never mints a credential the Hub would throw away.
func TestConnectRefusesAnotherKindOfApp(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce"}]`, true)
	reqID, token := appWithToken(t, cp.b, "hub")
	cp.b.store.SetAppContracts(reqID, []string{ContractConnectV1})
	w := do(t, cp.bH, "POST", "/app/v1/connect/grants",
		fmt.Sprintf(`{"peer":%q,"app":"feed"}`, cp.aPeerOnB.Name), withBearer(token))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "hub can only connect to hub") {
		t.Fatalf("hub → feed: %d %s", w.Code, w.Body)
	}
	if cp.grantCalls.Load() != 0 {
		t.Fatalf("the far feed was asked to mint: %d calls", cp.grantCalls.Load())
	}
	entries, _ := cp.b.store.RecentAudit(5)
	if len(entries) == 0 || entries[0].Action != "connect.request" || !strings.Contains(entries[0].Detail, `"ok":false`) {
		t.Fatalf("the refusal was not audited: %+v", entries)
	}
}

// Both sides record which fields the credential carried — the names that
// tell a Hub's api_key from a Feed's token — and never the values.
func TestConnectAuditsTheCredentialShape(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce"}]`, true)
	reqID, token := appWithToken(t, cp.b, "feed")
	cp.b.store.SetAppContracts(reqID, []string{ContractConnectV1})
	w := do(t, cp.bH, "POST", "/app/v1/connect/grants",
		fmt.Sprintf(`{"peer":%q}`, cp.aPeerOnB.Name), withBearer(token))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	for side, st := range map[string]*store.Store{"connect.request": cp.b.store, "connect.serve": cp.a.store} {
		entries, _ := st.RecentAudit(5)
		var found bool
		for _, e := range entries {
			if e.Action != side {
				continue
			}
			found = true
			if !strings.Contains(e.Detail, `"credentialKeys":"apiKey"`) || strings.Contains(e.Detail, "feed-minted-key-123") {
				t.Fatalf("%s audit detail: %s", side, e.Detail)
			}
		}
		if !found {
			t.Fatalf("no %s audit entry: %+v", side, entries)
		}
	}
}

// The grant sweep after an unpair runs under the context it is handed —
// the server's background context — so Shutdown stops it between apps
// instead of waiting out ten seconds for each one in turn.
func TestRevokeAppGrantsStopsWithItsContext(t *testing.T) {
	s, _ := newTestServer(t)
	for _, name := range []string{"feed", "hub"} {
		a, err := s.store.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		s.store.SetAppContracts(a.ID, []string{ContractConnectV1})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var asked []string
	s.appGrantEndpointFn = func(c context.Context, app *store.App) (string, string, error) {
		asked = append(asked, app.Name)
		cancel() // fogd shutting down mid-sweep
		if c.Err() == nil {
			t.Errorf("%s was asked under a context Shutdown can't reach", app.Name)
		}
		return "", "", c.Err()
	}
	s.revokeAppGrants(ctx, &store.FedPeer{Name: "b", InstanceID: "inst-b"})
	if len(asked) != 1 {
		t.Errorf("asked %v after the context ended, want just the first app", asked)
	}
}

// The requesting side holds the same line as the granting one: an app that
// never declared fog-connect/1 — any app with a token, named like a peer's
// connectable app — can't ask for a credential from it.
func TestConnectRequesterMustDeclareTheContract(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce"}]`, true)
	_, token := appWithToken(t, cp.b, "feed")
	w := do(t, cp.bH, "POST", "/app/v1/connect/grants", fmt.Sprintf(`{"peer":%q}`, cp.aPeerOnB.Name), withBearer(token))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), ContractConnectV1) {
		t.Fatalf("contractless requester: %d %s", w.Code, w.Body)
	}
	if cp.grantCalls.Load() != 0 {
		t.Fatal("a contractless requester reached the peer's app")
	}
}
