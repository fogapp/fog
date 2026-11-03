package fed

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func sealKeys(t *testing.T) *Identity {
	t.Helper()
	st, vlt := testDeps(t)
	id, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// records splits a sealed stream into its header and records.
func records(t *testing.T, sealed []byte) (hdr []byte, recs [][]byte) {
	t.Helper()
	hdr, rest := sealed[:1+sealEncLen], sealed[1+sealEncLen:]
	for len(rest) > 0 {
		n := binary.BigEndian.Uint32(rest[:4])
		recs = append(recs, rest[:4+n])
		rest = rest[4+n:]
	}
	return hdr, recs
}

func join(hdr []byte, recs ...[]byte) []byte {
	out := append([]byte{}, hdr...)
	for _, r := range recs {
		out = append(out, r...)
	}
	return out
}

// A sealed body opens only whole, untouched, in order, for the exchange it
// was sealed for, by the key it was sealed to.
func TestSealedBodiesOpenOnlyAsSealed(t *testing.T) {
	to, other := sealKeys(t), sealKeys(t)
	pub, _ := ParseEncKey(to.EncPublicB64())
	info := RequestInfo("POST", "/fed/v1/heartbeat", "01FROM", to.InstanceID, "2026-11-02T10:00:00.000000001Z")
	plain := bytes.Repeat([]byte("the household's people and apps "), 3*SealChunk/32) // three records and a bit
	sealed, err := Seal(pub, info, plain)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := to.openBody(info, sealed); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("own body: %v", err)
	}
	hdr, recs := records(t, sealed)
	if len(recs) < 3 {
		t.Fatalf("want several records, have %d", len(recs))
	}
	other2, _ := ParseEncKey(to.EncPublicB64())
	otherSealed, _ := Seal(other2, info, plain)
	_, otherRecs := records(t, otherSealed)

	flip := append([]byte{}, sealed...)
	flip[len(flip)/2] ^= 1
	for name, bad := range map[string][]byte{
		"a bit flipped":              flip,
		"cut before the final":       join(hdr, recs[:len(recs)-1]...),
		"cut mid-record":             sealed[:len(sealed)-7],
		"records reordered":          join(hdr, append([][]byte{recs[1], recs[0]}, recs[2:]...)...),
		"a record dropped":           join(hdr, append([][]byte{recs[0]}, recs[2:]...)...),
		"a record from another body": join(hdr, append([][]byte{otherRecs[0]}, recs[1:]...)...),
		"data after the final":       append(append([]byte{}, sealed...), recs[0]...),
		"not sealed at all":          plain,
		"empty":                      {},
	} {
		if _, err := to.openBody(info, bad); !errors.Is(err, ErrSealed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Moved to another exchange: every field the info binds.
	for name, moved := range map[string][]byte{
		"another target":    RequestInfo("POST", "/fed/v1/feedback", "01FROM", to.InstanceID, "2026-11-02T10:00:00.000000001Z"),
		"another method":    RequestInfo("PUT", "/fed/v1/heartbeat", "01FROM", to.InstanceID, "2026-11-02T10:00:00.000000001Z"),
		"another requester": RequestInfo("POST", "/fed/v1/heartbeat", "01OTHER", to.InstanceID, "2026-11-02T10:00:00.000000001Z"),
		"another recipient": RequestInfo("POST", "/fed/v1/heartbeat", "01FROM", "01ELSE", "2026-11-02T10:00:00.000000001Z"),
		"another request":   RequestInfo("POST", "/fed/v1/heartbeat", "01FROM", to.InstanceID, "2026-11-02T10:00:00.000000002Z"),
		"an answer's":       AnswerInfo("POST", "/fed/v1/heartbeat", "01FROM", "sig", to.InstanceID),
	} {
		if _, err := to.openBody(moved, sealed); !errors.Is(err, ErrSealed) {
			t.Errorf("moved to %s: %v", name, err)
		}
	}
	if _, err := other.openBody(info, sealed); !errors.Is(err, ErrSealed) {
		t.Errorf("opened by another key: %v", err)
	}
	// An answer is bound to its status and the request's signature too.
	ans := AnswerInfo("POST", SealedPath, "01FROM", "the-request-sig", "01RESP")
	sa, _ := Seal(pub, ans, []byte(`{"shares":[]}`))
	for name, moved := range map[string][]byte{
		"another request":   AnswerInfo("POST", SealedPath, "01FROM", "a-later-request", "01RESP"),
		"another requester": AnswerInfo("POST", SealedPath, "01OTHER", "the-request-sig", "01RESP"),
		"another responder": AnswerInfo("POST", SealedPath, "01FROM", "the-request-sig", "01ELSE"),
	} {
		if _, err := to.openBody(moved, sa); !errors.Is(err, ErrSealed) {
			t.Errorf("answer moved to %s: %v", name, err)
		}
	}
}

// A peer paired before bodies were sealed has no encryption key pinned. It
// is learned over the signed hello — only with the binding of the peer's
// identity key — before anything is sealed to it, and pinned: a different
// key later is refused and raised, never taken.
func TestEncryptionKeyIsLearnedSignedAndPinned(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}
	me, _ := s.Identity()
	them, impostor := sealKeys(t), sealKeys(t)
	var mu sync.Mutex
	var got [][]byte
	present := them // whose encryption key the hello presents, bound by them
	_ = me
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		sr, _, err := OpenSealedRequest(them, r, wire, them.InstanceID)
		if err != nil {
			http.Error(w, "unsealed", http.StatusForbidden)
			return
		}
		if sr.Hello {
			p := *them
			p.EncPublic, p.encPriv = present.EncPublic, present.encPriv
			p.EncBinding = them.SignTranscript(encBindingLabel, them.InstanceID, present.EncPublic)
			WriteRecorded(w, &p, sr, http.StatusOK, nil, []byte("{}"))
			return
		}
		plain, _ := io.ReadAll(sr.Inner.Body)
		mu.Lock()
		got = append(got, plain)
		mu.Unlock()
		WriteRecorded(w, them, sr, http.StatusOK, nil, []byte(`{"ok":true}`))
	}))
	defer srv.Close()
	peer, _ := st.CreatePairedPeer(them.InstanceID, "old-pairing", srv.URL, them.PublicKeyB64())
	if peer.EncPublicKey != "" {
		t.Fatal("test wants a pairing with no encryption key yet")
	}
	ctx := context.Background()
	if _, err := s.SignedPost(ctx, peer, "/fed/v1/feedback", "application/json", []byte(`{"items":["secret report"]}`)); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.FedPeerByID(peer.ID); p.EncPublicKey != them.EncPublicB64() {
		t.Fatalf("the key was not pinned from the signed hello: %q", p.EncPublicKey)
	}
	if len(got) != 1 || string(got[0]) != `{"items":["secret report"]}` {
		t.Fatalf("the peer did not receive the body sealed to its key: %q", got)
	}

	// Now the address presents another key — bound by the peer's identity
	// key, even. It is refused, the operator told, and nothing goes to it.
	present = impostor
	s.connsMu.Lock()
	for _, pc := range s.conns {
		pc.tr.CloseIdleConnections()
	}
	s.conns = nil // new connections, new proofs
	s.connsMu.Unlock()
	peer, _ = st.FedPeerByID(peer.ID)
	if _, err := s.SignedPost(ctx, peer, "/fed/v1/feedback", "application/json", []byte(`{"items":["second"]}`)); !errors.Is(err, ErrEncKeyChanged) {
		t.Fatalf("a changed encryption key: %v", err)
	}
	if p, _ := st.FedPeerByID(peer.ID); p.EncPublicKey != them.EncPublicB64() {
		t.Fatal("the pinned key was replaced")
	}
	if n, _ := st.CountActiveNotices(); n == 0 {
		t.Fatal("the operator was not told")
	}
	if len(got) != 1 {
		t.Fatalf("a body went out under the changed key: %d", len(got))
	}
}

// A key the peer's identity didn't bind is never learned, and with no key
// nothing is sent: a body is never sent unsealed.
func TestUnboundEncryptionKeyIsNotLearned(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}
	them, mallory := sealKeys(t), sealKeys(t)
	var bodies int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		if len(wire) == 0 {
			// The peer's own signature on the answer, but the encryption key
			// is Mallory's, bound by Mallory.
			sr, _, _ := OpenSealedRequest(them, r, wire, them.InstanceID)
			p := *them
			p.EncPublic, p.EncBinding = mallory.EncPublic, mallory.SignTranscript(encBindingLabel, them.InstanceID, mallory.EncPublic)
			WriteRecorded(w, &p, sr, http.StatusOK, nil, []byte("{}"))
			return
		}
		bodies++
	}))
	defer srv.Close()
	peer, _ := st.CreatePairedPeer(them.InstanceID, "p", srv.URL, them.PublicKeyB64())
	if _, err := s.SignedPost(context.Background(), peer, "/fed/v1/feedback", "application/json", []byte(`{"x":1}`)); err == nil {
		t.Fatal("a body was sent with no bound key")
	}
	if p, _ := st.FedPeerByID(peer.ID); p.EncPublicKey != "" {
		t.Fatalf("an unbound key was pinned: %q", p.EncPublicKey)
	}
	if bodies != 0 {
		t.Fatal("a request reached the peer")
	}
}

// Downgrade: a peer that answers its proof unsealed, or without a key at
// all — a fog from before sealed bodies, or not a fog — gets nothing.
// There is no unsealed fallback.
func TestDowngradeIsRefused(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}
	them := sealKeys(t)
	var bodies int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		if len(wire) == 0 {
			// Signed exactly as before sealed bodies: plain "{}", no key.
			them.SignAnswer(w.Header(), r.URL.RequestURI(), http.StatusOK, BoundTo(r.Header), []byte("{}"))
			w.Write([]byte("{}"))
			return
		}
		bodies++
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, pinned := range []bool{false, true} {
		peer, _ := st.CreatePairedPeer(them.InstanceID+map[bool]string{false: "", true: "-pinned"}[pinned], map[bool]string{false: "new", true: "pinned"}[pinned], srv.URL, them.PublicKeyB64())
		if pinned {
			st.SetFedPeerEncKey(peer.ID, them.EncPublicB64())
			peer, _ = st.FedPeerByID(peer.ID)
		}
		_, err := s.SignedPost(ctx, peer, "/fed/v1/feedback", "application/json", []byte(`{"x":1}`))
		if !errors.Is(err, ErrNoEncryption) && !errors.Is(err, ErrUnproven) {
			t.Errorf("pinned=%v: an unsealing peer: %v", pinned, err)
		}
		if _, err := s.SignedGetJSON(ctx, peer, "/fed/v1/files/catalog", "", nil); err == nil {
			t.Errorf("pinned=%v: a GET to an unsealing peer succeeded", pinned)
		}
	}
	if bodies != 0 {
		t.Fatalf("%d requests reached a peer that doesn't seal", bodies)
	}
	// And an invite without the inviter's encryption key is refused before
	// anything is dialed.
	if _, err := s.Join(ctx, InviteCode(srv.URL, "secret", "old", ""), ""); !errors.Is(err, ErrNoEncryption) {
		t.Fatalf("an invite from a fog that doesn't seal: %v", err)
	}
}
