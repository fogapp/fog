package fed

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

const legacyID = "01JV1CT1M0000000000000000A"

// legacyService is a fog minted before instance IDs were derived from
// keys: a random ULID beside its keypair.
func legacyService(t *testing.T) *Service {
	t.Helper()
	st, vlt := testDeps(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	if err := (vault.Secrets{Store: st, Vault: vlt}).Put(0, SecretIdentityKey, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFedIdentity(legacyID, base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: st, Vault: vlt}
	if !s.LegacyIdentity() {
		t.Fatal("the fixture is not legacy")
	}
	return s
}

// A new pairing needs both instance IDs key-bound. A household already
// held here re-pairs under the rules it was paired under.
func TestNewPairingsNeedKeyBoundIDs(t *testing.T) {
	// This fog is key-bound; the other side is legacy.
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt}
	doc := testDoc(t, "01JNEWATT1C000000000000000", "Attic")
	if _, err := s.StorePairedPeer(doc, "http://attic.lan"); !errors.Is(err, ErrLegacyPeer) {
		t.Fatalf("a new pairing with a legacy ID: %v", err)
	}
	if _, err := st.FedPeerByInstanceID(doc.InstanceID); err == nil {
		t.Fatal("the refused pairing left a row")
	}
	// Held already, under the same keys: refreshed as ever.
	held, err := st.CreatePairedPeer(doc.InstanceID, "attic", "http://old.lan", doc.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.StorePairedPeer(doc, "http://attic.lan"); err != nil || got.ID != held.ID {
		t.Fatalf("re-pairing a household paired before key-bound IDs: %v", err)
	}

	// This fog is legacy: no new pairing, even with a key-bound household;
	// its existing pairings re-pair.
	ls := legacyService(t)
	if _, err := ls.StorePairedPeer(testDoc(t, "", "New"), ""); !errors.Is(err, ErrLegacySelf) {
		t.Fatalf("a legacy fog pairing anew: %v", err)
	}
	old := testDoc(t, "", "Old")
	if _, err := ls.Store.CreatePairedPeer(old.InstanceID, "old", "", old.PublicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.StorePairedPeer(old, "http://old.lan"); err != nil {
		t.Fatalf("a legacy fog re-pairing a household it holds: %v", err)
	}
}

// A legacy fog is told before anything is dialed that it can't join a
// household it doesn't already hold.
func TestLegacyFogJoinIsRefusedBeforeDialing(t *testing.T) {
	ls := legacyService(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	inviter := sealKeys(t)
	_, err := ls.Join(t.Context(), InviteCode(srv.URL, "s3cret", "north", inviter.EncPublicB64()), "")
	if !errors.Is(err, ErrLegacySelf) {
		t.Fatalf("joining anew from a legacy fog: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the legacy fog dialed the inviter anyway")
	}
}

// Renewing gives a legacy fog a key-bound identity: new keys, the ID they
// derive, its own contributions carried over to the new ID. A key-bound
// fog has nothing to renew.
func TestRenewIdentity(t *testing.T) {
	ls := legacyService(t)
	before, _ := ls.Identity()
	peer, err := ls.Store.CreatePairedPeer("01J0THER000000000000000000", "other", "", "pk")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := ls.Store.CreateFileFedSubscription(&store.FileFedSubscription{PeerID: peer.ID, RemoteShareID: 1, Title: "Drop", Folder: "Peers/other/Drop", Mode: "contribute", DetailOffered: "full", DetailWanted: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Store.UpsertFileFedReplica(store.FileFedReplica{SubscriptionID: sub.ID, Rel: "mine.txt", Kind: "file", Origin: legacyID}); err != nil {
		t.Fatal(err)
	}

	oldID, newID, err := ls.RenewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	after, err := ls.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if oldID != legacyID || newID != after.InstanceID || !IsDerivedInstanceID(newID) || newID != DeriveInstanceID(after.Public) {
		t.Fatalf("renewed %s -> %s, identity %s", oldID, newID, after.InstanceID)
	}
	if after.PublicKeyB64() == before.PublicKeyB64() || string(after.EncPublic) == string(before.EncPublic) {
		t.Fatal("the renewal kept a key")
	}
	if VerifyEncBinding(after.Public, newID, after.EncPublicB64(), after.EncBinding) != nil {
		t.Fatal("the new encryption key is not bound to the new ID")
	}
	// A fresh load agrees — the vault and the row moved together.
	fresh := &Service{Store: ls.Store, Vault: ls.Vault}
	if id, err := fresh.Identity(); err != nil || id.InstanceID != newID || id.PublicKeyB64() != after.PublicKeyB64() {
		t.Fatalf("reload after renewal: %v", err)
	}
	if r, err := ls.Store.FileFedReplica(sub.ID, "mine.txt"); err != nil || r.Origin != newID {
		t.Fatalf("our contribution still names the old ID: %+v %v", r, err)
	}
	if _, _, err := ls.RenewIdentity(); !errors.Is(err, ErrNotLegacy) {
		t.Fatalf("renewing a key-bound identity: %v", err)
	}
}

// A renewal that stops after the identity row names the new key, before
// the seeds are moved into place, is finished the next time the identity
// loads — never a fog whose row and keys disagree.
func TestInterruptedRenewalIsFinishedOnLoad(t *testing.T) {
	ls := legacyService(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	encSeed := make([]byte, 32)
	encSeed[0] = 7
	encPriv, _ := sealKEM.DeriveKeyPair(encSeed)
	secrets := vault.Secrets{Store: ls.Store, Vault: ls.Vault}
	secrets.Put(0, secretIdentityNext, base64.StdEncoding.EncodeToString(priv.Seed()))
	secrets.Put(0, secretEncryptionNext, base64.StdEncoding.EncodeToString(encSeed))
	newID := DeriveInstanceID(pub)
	if err := ls.Store.RenewFedIdentity(legacyID, newID, base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(encPriv.PublicKey().Bytes())); err != nil {
		t.Fatal(err)
	}
	// "Crash": a new process loads.
	id, err := (&Service{Store: ls.Store, Vault: ls.Vault}).Identity()
	if err != nil {
		t.Fatalf("loading after an interrupted renewal: %v", err)
	}
	if id.InstanceID != newID || id.PublicKeyB64() != base64.StdEncoding.EncodeToString(pub) {
		t.Fatalf("loaded %s", id.InstanceID)
	}
	if staged, _ := secrets.Get(0, secretIdentityNext); staged != "" {
		t.Fatal("the staged seed was left behind")
	}
}

// A row paired by a fog older than key-bound IDs may carry an ID in the
// derived form that is another key's: somebody else's name, claimed. It
// stands for no household and nothing is sent to it.
func TestForgedRowStandsForNobody(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt}
	victim, impostor := sealKeys(t), sealKeys(t)
	row, err := st.CreatePairedPeer(victim.InstanceID, "victim", "http://127.0.0.1:1", impostor.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	if CheckPeerRow(row) == nil {
		t.Fatal("a forged row checked out")
	}
	if _, err := PeerForHousehold(st, victim.InstanceID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a forged row stood for its household: %v", err)
	}
	if _, err := s.SignedPost(t.Context(), row, "/fed/v1/heartbeat", "application/json", []byte("{}")); !errors.Is(err, ErrInstanceNotBound) {
		t.Fatalf("a request to a forged row: %v", err)
	}
	// A genuine row does.
	real, _ := st.CreatePairedPeer(impostor.InstanceID, "real", "", impostor.PublicKeyB64())
	if got, err := PeerForHousehold(st, impostor.InstanceID); err != nil || got.ID != real.ID {
		t.Fatalf("a genuine row: %v", err)
	}
}
