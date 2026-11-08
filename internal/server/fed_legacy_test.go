package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/vault"
)

// testFog is one fog in a test: its server, handler, admin cookie and
// address.
type testFog struct {
	s      *Server
	h      http.Handler
	cookie *http.Cookie
	srv    *httptest.Server
}

// newFog starts a fog; legacy gives it an identity from before instance
// IDs were derived from keys (a random ULID).
func newFog(t *testing.T, legacy bool) *testFog {
	t.Helper()
	f := &testFog{}
	f.s, f.h = newTestServer(t)
	if legacy {
		pub, priv, _ := ed25519.GenerateKey(nil)
		if err := (vault.Secrets{Store: f.s.store, Vault: f.s.vault}).Put(0, fed.SecretIdentityKey, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
			t.Fatal(err)
		}
		if err := f.s.store.SetFedIdentity("01JV1CT1M0000000000000000A", base64.StdEncoding.EncodeToString(pub)); err != nil {
			t.Fatal(err)
		}
		if !f.s.fed.LegacyIdentity() {
			t.Fatal("the fixture's identity is not legacy")
		}
	}
	f.cookie = setup(t, f.h)
	f.srv = httptest.NewServer(f.h)
	t.Cleanup(f.srv.Close)
	if w := do(t, f.h, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":%q}`, f.srv.URL), withCookie(f.cookie)); w.Code != http.StatusOK {
		t.Fatalf("set url: %d %s", w.Code, w.Body)
	}
	return f
}

func (f *testFog) keyBound(t *testing.T) bool {
	t.Helper()
	var info struct {
		Identity struct {
			KeyBound bool `json:"keyBound"`
		} `json:"identity"`
	}
	w := do(t, f.h, "GET", "/api/federation", "", withCookie(f.cookie))
	json.Unmarshal(w.Body.Bytes(), &info)
	return info.Identity.KeyBound
}

func (f *testFog) renew(t *testing.T, breakPairings bool) (int, string) {
	t.Helper()
	w := do(t, f.h, "POST", "/api/federation/identity/renew", fmt.Sprintf(`{"breakPairings":%v}`, breakPairings), withCookie(f.cookie))
	return w.Code, w.Body.String()
}

// A fog whose identity predates key-bound IDs is told it must renew before
// it can pair with anyone new — minting an invite, or joining one — and
// once renewed it pairs.
func TestLegacyFogRenewsToPairAnew(t *testing.T) {
	t.Parallel()

	a, b := newFog(t, true), newFog(t, false)
	if a.keyBound(t) || !b.keyBound(t) {
		t.Fatal("keyBound reads wrong")
	}
	if w := do(t, a.h, "POST", "/api/federation/invites", `{"grants":[]}`, withCookie(a.cookie)); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "renew") {
		t.Fatalf("a legacy fog minting an invite: %d %s", w.Code, w.Body)
	}
	code := mintInvite(t, b.h, b.cookie, `[]`)
	if w := do(t, a.h, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(a.cookie)); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "renew") {
		t.Fatalf("a legacy fog joining: %d %s", w.Code, w.Body)
	}
	// Nothing knows this fog by its old ID but its own chat rooms, if any;
	// whatever there is, the renewal says so first.
	status, body := a.renew(t, false)
	if status == http.StatusConflict {
		if !strings.Contains(body, "chat room") {
			t.Fatalf("a refusal to renew that doesn't say why: %s", body)
		}
		status, body = a.renew(t, true)
	}
	if status != http.StatusOK || !a.keyBound(t) {
		t.Fatalf("renewing: %d %s", status, body)
	}
	if w := do(t, a.h, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(a.cookie)); w.Code != http.StatusOK {
		t.Fatalf("joining after renewal: %d %s", w.Code, w.Body)
	}
	if status, _ := a.renew(t, true); status != http.StatusConflict {
		t.Fatalf("renewing a key-bound identity: %d", status)
	}
}

// A legacy fog's existing pairings keep working. Renewing would cut them
// off, so it is refused until the operator says so — and says which
// households — and done, it says it again in the Inbox. Pairing again
// refreshes the household's row in place.
func TestRenewingAPairedLegacyFogIsExplicit(t *testing.T) {
	t.Parallel()

	a, c := newFog(t, true), newFog(t, false)
	aID, cID := mustIdentity(t, a.s), mustIdentity(t, c.s)
	cOnA, err := a.s.store.CreatePairedPeer(cID.InstanceID, "cedar", c.srv.URL, cID.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	a.s.store.SetFedPeerEncKey(cOnA.ID, cID.EncPublicB64())
	aOnC, err := c.s.store.CreatePairedPeer(aID.InstanceID, "ash", a.srv.URL, aID.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	c.s.store.SetFedPeerEncKey(aOnC.ID, aID.EncPublicB64())
	if err := a.s.fed.HeartbeatPeer(t.Context(), cOnA); err != nil {
		t.Fatalf("a pairing made before key-bound IDs: %v", err)
	}

	status, body := a.renew(t, false)
	if status != http.StatusConflict || !strings.Contains(body, "cedar") || !strings.Contains(body, "paired again") {
		t.Fatalf("renewing without saying so: %d %s", status, body)
	}
	if id := mustIdentity(t, a.s); id.InstanceID != aID.InstanceID {
		t.Fatal("the refused renewal changed the identity")
	}
	if status, body := a.renew(t, true); status != http.StatusOK {
		t.Fatalf("renewing: %d %s", status, body)
	}
	found := false
	notices, _ := a.s.store.ActiveNotices()
	for _, n := range notices {
		found = found || (n.Kind == "fed-identity-renewed" && strings.Contains(n.Body, "cedar"))
	}
	if !found {
		t.Fatal("the renewal left no notice naming the household to pair again")
	}
	if err := a.s.fed.HeartbeatPeer(t.Context(), cOnA); err == nil {
		t.Fatal("cedar still took a's heartbeat under a's old name")
	}
	code := mintInvite(t, a.h, a.cookie, `[]`)
	if w := do(t, c.h, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(c.cookie)); w.Code != http.StatusOK {
		t.Fatalf("pairing again: %d %s", w.Code, w.Body)
	}
	if got, err := a.s.store.FedPeerByInstanceID(cID.InstanceID); err != nil || got.ID != cOnA.ID {
		t.Fatalf("pairing again made a new row instead of refreshing: %v", err)
	}
}

// The inviter turns a legacy joiner away before spending its invite, and
// tells it what to do.
func TestInviterRefusesALegacyJoinerBeforeSpending(t *testing.T) {
	t.Parallel()

	b := newFog(t, false)
	code := mintInvite(t, b.h, b.cookie, `[]`)
	inv, err := fed.ParseInvite(code)
	if err != nil {
		t.Fatal(err)
	}
	const legacy = "01JV1CT1M0000000000000000A"
	j := *mustIdentity(t, mustServer(t))
	j.InstanceID = legacy
	body, _ := json.Marshal(fed.PairRequest{Secret: inv.Secret, Instance: fed.InstanceDoc{
		InstanceID: legacy, Name: "old", URL: "https://old.example", PublicKey: j.PublicKeyB64(),
		EncryptionKey: j.EncPublicB64(), EncryptionKeySig: j.SignTranscript("fogfed1-enckey", legacy, j.EncPublic),
	}})
	req, _ := http.NewRequest("POST", b.srv.URL+"/fed/v1/pair", nil)
	req.Header.Set("Content-Type", "application/json")
	k, _ := fed.ParseEncKey(inv.EncKey)
	if err := fed.SealRequest(&j, req, fed.PairRecipient(inv.EncKey), k, body); err != nil {
		t.Fatal(err)
	}
	status, raw := sendFedAs(t, req, &j, mustIdentity(t, b.s).Public, fed.PairRecipient(inv.EncKey))
	if status != http.StatusForbidden || !strings.Contains(string(raw), "renew it") {
		t.Fatalf("a legacy joiner: %d %s", status, raw)
	}
	n := newFog(t, false)
	if w := do(t, n.h, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(n.cookie)); w.Code != http.StatusOK {
		t.Fatalf("the invite was spent by the refused joiner: %d %s", w.Code, w.Body)
	}
}
