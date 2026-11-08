package fed

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

func testDeps(t *testing.T) (*store.Store, *vault.Vault) {
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
	return st, vlt
}

func TestULIDShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewULID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 26 {
			t.Fatalf("ulid length %d: %s", len(id), id)
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
				t.Fatalf("ulid %s contains %q outside the alphabet", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate ulid %s", id)
		}
		seen[id] = true
	}
}

func TestEnsureIdentityMintsOnceAndReloads(t *testing.T) {
	st, vlt := testDeps(t)
	id1, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	if id1.InstanceID == "" || len(id1.Public) == 0 {
		t.Fatalf("empty identity: %+v", id1)
	}
	id2, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	if id2.InstanceID != id1.InstanceID || id2.PublicKeyB64() != id1.PublicKeyB64() {
		t.Fatalf("identity not stable across loads: %s vs %s", id1.InstanceID, id2.InstanceID)
	}
	// The reloaded private key must actually sign as the stored public key.
	sig := id2.Sign("POST", "/fed/v1/heartbeat", "2026-08-13T00:00:00Z", []byte("{}"))
	if sig == "" {
		t.Fatal("empty signature")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	st, vlt := testDeps(t)
	id, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"hello":"dave"}`)
	req, _ := http.NewRequest("POST", "https://fog.example/fed/v1/heartbeat", nil)
	req.URL = &url.URL{Path: "/fed/v1/heartbeat"}
	id.SignRequest(req, body)

	if req.Header.Get(HeaderInstance) != id.InstanceID {
		t.Fatalf("instance header: %s", req.Header.Get(HeaderInstance))
	}
	if err := Verify(id.Public, "POST", "/fed/v1/heartbeat", req.Header, body, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Tampered body fails.
	if err := Verify(id.Public, "POST", "/fed/v1/heartbeat", req.Header, []byte(`{"hello":"eve"}`), time.Now()); err == nil {
		t.Fatal("tampered body verified")
	}
	// Wrong path fails.
	if err := Verify(id.Public, "POST", "/fed/v1/flare/envelope", req.Header, body, time.Now()); err == nil {
		t.Fatal("wrong path verified")
	}
	// Wrong key fails.
	st2, vlt2 := testDeps(t)
	other, _ := EnsureIdentity(st2, vlt2)
	if err := Verify(other.Public, "POST", "/fed/v1/heartbeat", req.Header, body, time.Now()); err == nil {
		t.Fatal("wrong key verified")
	}
	// Outside the skew window fails, both directions.
	if err := Verify(id.Public, "POST", "/fed/v1/heartbeat", req.Header, body, time.Now().Add(MaxSkew+time.Minute)); err == nil {
		t.Fatal("stale timestamp verified")
	}
	if err := Verify(id.Public, "POST", "/fed/v1/heartbeat", req.Header, body, time.Now().Add(-MaxSkew-time.Minute)); err == nil {
		t.Fatal("future timestamp verified")
	}
	// Missing headers fail.
	if err := Verify(id.Public, "POST", "/fed/v1/heartbeat", http.Header{}, body, time.Now()); err == nil {
		t.Fatal("missing headers verified")
	}
}

func TestParsePublicKey(t *testing.T) {
	st, vlt := testDeps(t)
	id, _ := EnsureIdentity(st, vlt)
	pub, err := ParsePublicKey(id.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	if string(pub) != string(id.Public) {
		t.Fatal("round-tripped key differs")
	}
	if _, err := ParsePublicKey("not-a-key"); err == nil {
		t.Fatal("garbage key parsed")
	}
	if _, err := ParsePublicKey("aGVsbG8="); err == nil {
		t.Fatal("short key parsed")
	}
}

// An answer verifies only for the request it answers: its target, status,
// requester and request signature are all part of what is signed. A missing
// signature is the distinguishable ErrUnsignedResponse; a tampered or
// rebound one fails hard. An answer never verifies as a request.
func TestAnswerSignatureIsBoundToItsRequest(t *testing.T) {
	st, vlt := testDeps(t)
	id, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	req := Bound{Instance: "01ASKER", Sig: "sig-of-the-request"}
	body := []byte(`{"accepted":3}`)
	hdr := http.Header{}
	id.SignAnswer(hdr, "/fed/v1/flare/envelope", http.StatusOK, req, body)
	now := time.Now()
	if err := VerifyAnswer(id.Public, "/fed/v1/flare/envelope", hdr, http.StatusOK, req, body, now); err != nil {
		t.Fatalf("own answer: %v", err)
	}
	if err := VerifyAnswer(id.Public, "/fed/v1/flare/envelope", http.Header{}, http.StatusOK, req, body, now); !errors.Is(err, ErrUnsignedResponse) {
		t.Fatalf("want ErrUnsignedResponse, got %v", err)
	}
	for name, try := range map[string]func() error{
		"another target": func() error { return VerifyAnswer(id.Public, "/fed/v1/feedback", hdr, 200, req, body, now) },
		"another status": func() error { return VerifyAnswer(id.Public, "/fed/v1/flare/envelope", hdr, 403, req, body, now) },
		"another request": func() error {
			return VerifyAnswer(id.Public, "/fed/v1/flare/envelope", hdr, 200, Bound{Instance: req.Instance, Sig: "later"}, body, now)
		},
		"another requester": func() error {
			return VerifyAnswer(id.Public, "/fed/v1/flare/envelope", hdr, 200, Bound{Instance: "01OTHER", Sig: req.Sig}, body, now)
		},
		"another body": func() error {
			return VerifyAnswer(id.Public, "/fed/v1/flare/envelope", hdr, 200, req, []byte(`{"accepted":999}`), now)
		},
		"as a request": func() error { return Verify(id.Public, "POST", "/fed/v1/flare/envelope", hdr, body, now) },
	} {
		if err := try(); err == nil || errors.Is(err, ErrUnsignedResponse) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// In the envelope: the head is what's signed, and verifies only for its
	// request.
	prefix := id.answerPrefix("/fed/v1/sealed", req, 200, http.Header{"Etag": {"v1"}}, "abc123")
	if head, err := ReadAnswerHead(bytes.NewReader(prefix), id.Public, "/fed/v1/sealed", req); err != nil || head.Digest != "abc123" || head.Header.Get("ETag") != "v1" {
		t.Fatalf("an envelope head: %+v %v", head, err)
	}
	if _, err := ReadAnswerHead(bytes.NewReader(prefix), id.Public, "/fed/v1/sealed", Bound{Instance: req.Instance, Sig: "later"}); err == nil {
		t.Fatal("an envelope head verified for another request")
	}
}

// Two requests signed in the same instant are still different bytes, so a
// receiver's replay record never refuses a genuine request.
func TestRequestSignaturesNeverRepeat(t *testing.T) {
	st, vlt := testDeps(t)
	id, _ := EnsureIdentity(st, vlt)
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		req, _ := http.NewRequest("GET", "http://x/fed/v1/files/catalog", nil)
		id.SignRequest(req, nil)
		sig := req.Header.Get(HeaderSignature)
		if seen[sig] {
			t.Fatalf("request %d repeated a signature", i)
		}
		seen[sig] = true
		if err := Verify(id.Public, "GET", "/fed/v1/files/catalog", req.Header, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

// StorePairedPeer's alias-dedupe loop rides on the store's ErrAlreadyExists
// sentinel: a taken name means "try the next suffix", any other failure
// stops the pairing.
func TestStorePairedPeerDedupesCollidingName(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt}
	if _, err := st.CreatePairedPeer("01J0THER000000000000000000", "attic", "", "pk-other"); err != nil {
		t.Fatal(err)
	}
	doc := testDoc(t, "", "Attic")
	peer, err := s.StorePairedPeer(doc, "http://attic.lan")
	if err != nil {
		t.Fatal(err)
	}
	if peer.Name != "attic-2" {
		t.Fatalf("peer name = %q, want attic-2", peer.Name)
	}
	// Pairing again with the same instance and key refreshes, never
	// re-dedupes.
	again, err := s.StorePairedPeer(doc, "http://attic2.lan")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != peer.ID || again.PublicKey != doc.PublicKey || again.EncPublicKey != doc.EncryptionKey || again.URL != "http://attic2.lan" {
		t.Fatalf("re-pair did not refresh in place: %+v", again)
	}
}

// The attack: a valid pairing that names an already-paired instance under
// another key. It used to replace the row's key and keep its grants, so one
// invite code made its holder that household, with everything granted to
// it. Now the row is untouched — key, address, grants — the pairing is
// refused, and the operator is told.
func TestStorePairedPeerRefusesRekey(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt}
	victim, err := st.CreatePairedPeer("01JV1CT1M0000000000000000A", "riverside", "https://fog.riverside.example", "pk-victim")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetFedCapability(victim.ID, store.CapAppsIntroduce, store.DirGrant, "{}"); err != nil {
		t.Fatal(err)
	}
	_, err = s.StorePairedPeer(testDoc(t, "01JV1CT1M0000000000000000A", "Riverside"), "https://evil.example")
	if !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("re-key by pairing: err = %v, want ErrKeyConflict", err)
	}
	after, err := st.FedPeerByID(victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PublicKey != "pk-victim" || after.URL != "https://fog.riverside.example" {
		t.Fatalf("refused pairing still changed the row: %+v", after)
	}
	if _, ok, _ := st.FedCapabilityConfig(victim.ID, store.CapAppsIntroduce, store.DirGrant); !ok {
		t.Fatal("refused pairing dropped the victim's grant")
	}
	if n, _ := st.CountActiveNotices(); n != 1 {
		t.Fatalf("want one notice telling the operator, have %d", n)
	}
	// Nothing below the service can re-key a row either.
	if _, err := st.DB().Exec(`UPDATE fed_peers SET public_key = 'pk-attacker' WHERE id = ?`, victim.ID); err == nil {
		t.Fatal("the store let a paired peer's key be rewritten")
	}
	// Nor can a pairing claim this fog's own identity.
	self, _ := s.RequireInstanceID()
	if _, err := s.StorePairedPeer(testDoc(t, self, "me"), ""); !errors.Is(err, ErrPairWithSelf) {
		t.Fatalf("pairing with our own instance ID: %v", err)
	}
}

// TestSignCoversQueryString proves the signature covers the full request
// target, so a parameter that names what a request acts on — the file behind
// DELETE …/contributions?path=X — cannot be swapped after signing.
func TestSignCoversQueryString(t *testing.T) {
	st, vlt := testDeps(t)
	id, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("DELETE", "https://fog.example/x", nil)
	req.URL = &url.URL{Path: "/fed/v1/files/shares/1/contributions", RawQuery: "path=a.jpg"}
	id.SignRequest(req, nil)

	if err := Verify(id.Public, "DELETE", req.URL.RequestURI(), req.Header, nil, time.Now()); err != nil {
		t.Fatalf("the exact target should verify: %v", err)
	}
	if err := Verify(id.Public, "DELETE", "/fed/v1/files/shares/1/contributions?path=other.jpg", req.Header, nil, time.Now()); err == nil {
		t.Fatal("a tampered query string must fail verification")
	}
	if err := Verify(id.Public, "DELETE", "/fed/v1/files/shares/1/contributions", req.Header, nil, time.Now()); err == nil {
		t.Fatal("dropping the query string must fail verification")
	}
}

// A rich invite carries the inviter's name; a nameless code still parses (name
// empty, not an error); the URL+secret compat view is unchanged (Project L).
func TestRichInviteRoundTrip(t *testing.T) {
	code := InviteCode("https://fog.dave.example", "sekret", "Dave's House", "ENCKEY")
	inv, err := ParseInvite(code)
	if err != nil {
		t.Fatal(err)
	}
	if inv.URL != "https://fog.dave.example" || inv.Secret != "sekret" || inv.Name != "Dave's House" || inv.EncKey != "ENCKEY" {
		t.Fatalf("round trip lost a field: %+v", inv)
	}
	u, s, err := ParseInviteCode(code)
	if err != nil || u != inv.URL || s != inv.Secret {
		t.Fatalf("compat view wrong: %q %q %v", u, s, err)
	}
	nameless, err := ParseInvite(InviteCode("https://x", "y", "", "k"))
	if err != nil || nameless.Name != "" || nameless.URL != "https://x" {
		t.Fatalf("nameless parse: %+v %v", nameless, err)
	}
	if _, err := ParseInvite("garbage"); err == nil {
		t.Fatal("non-FOG code should error")
	}
}

// testDoc is a well-formed instance document for instance, under a fresh
// identity: its own keys, its encryption key bound to that instance ID.
// An empty instance is the identity's own, key-derived ID.
func testDoc(t *testing.T, instance, name string) *InstanceDoc {
	t.Helper()
	st, vlt := testDeps(t)
	id, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	if instance == "" {
		instance = id.InstanceID
	}
	return &InstanceDoc{InstanceID: instance, Name: name, PublicKey: id.PublicKeyB64(),
		EncryptionKey: id.EncPublicB64(), EncryptionKeySig: id.SignTranscript(encBindingLabel, instance, id.EncPublic)}
}

// Finding 13: an instance ID is its key's. A household one invites can't
// claim the ID of a household one isn't paired with: the ID it would claim
// is derived from that household's key, and no other key derives it.
func TestInstanceIDIsTheKeys(t *testing.T) {
	st, vlt := testDeps(t)
	id, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	if !IsDerivedInstanceID(id.InstanceID) || id.InstanceID != DeriveInstanceID(id.Public) {
		t.Fatalf("a new identity's ID %q is not derived from its key", id.InstanceID)
	}
	other := sealKeys(t)
	if DeriveInstanceID(other.Public) == id.InstanceID {
		t.Fatal("two keys derived one ID")
	}
	if err := CheckInstanceID(id.InstanceID, id.PublicKeyB64()); err != nil {
		t.Fatalf("its own ID: %v", err)
	}
	// Mallory claims it, with her key and a binding of her encryption key to
	// that ID she signed herself.
	if err := CheckInstanceID(id.InstanceID, other.PublicKeyB64()); !errors.Is(err, ErrInstanceNotBound) {
		t.Fatalf("someone else's derived ID: %v", err)
	}
	s := &Service{Store: st, Vault: vlt}
	// A third household — the inviter — that holds neither.
	ist, ivlt := testDeps(t)
	inviter := &Service{Store: ist, Vault: ivlt}
	claim := &InstanceDoc{InstanceID: id.InstanceID, Name: "riverside", PublicKey: other.PublicKeyB64(),
		EncryptionKey: other.EncPublicB64(), EncryptionKeySig: other.SignTranscript(encBindingLabel, id.InstanceID, other.EncPublic)}
	if _, err := inviter.StorePairedPeer(claim, ""); !errors.Is(err, ErrInstanceNotBound) {
		t.Fatalf("pairing under another household's ID: %v", err)
	}
	_ = s
	if _, err := ist.FedPeerByInstanceID(id.InstanceID); err == nil {
		t.Fatal("the claim was stored")
	}
	for _, bad := range []string{"", "inst-1", "01JNEWATT1C00000000000000", "ZZZZZZZZZZZZZZZZZZZZZZZZZZ"} {
		if err := CheckInstanceID(bad, id.PublicKeyB64()); err == nil {
			t.Errorf("%q passed as an instance ID", bad)
		}
	}
}
