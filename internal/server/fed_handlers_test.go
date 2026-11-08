package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// mintInvite drives the inviter's API: returns the one-shot pairing code.
func mintInvite(t *testing.T, h http.Handler, cookie *http.Cookie, grants string) string {
	t.Helper()
	w := do(t, h, "POST", "/api/federation/invites", `{"grants":`+grants+`}`, withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("mint invite: %d %s", w.Code, w.Body)
	}
	var resp struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp.Code, "FOG-") {
		t.Fatalf("invite code shape: %q", resp.Code)
	}
	return resp.Code
}

// The two-instance pairing test: instance A mints an invite, instance B
// joins with the code over a real HTTP hop, and both sides end up mutually
// peered with keys exchanged — no human-visible token anywhere.
func TestFederationPairingEndToEnd(t *testing.T) {
	t.Parallel()

	// Instance A (the inviter), reachable over real HTTP.
	a, aHandler := newTestServer(t)
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	// The invite needs an advertised URL; point it at the test server.
	if w := do(t, aHandler, "PUT", "/api/federation/identity",
		fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie)); w.Code != http.StatusOK {
		t.Fatalf("set advertised url: %d %s", w.Code, w.Body)
	}
	code := mintInvite(t, aHandler, aCookie, `[{"capability":"flare.ingest"}]`)

	// Instance B (the joiner).
	b, bHandler := newTestServer(t)
	bCookie := setup(t, bHandler)
	w := do(t, bHandler, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var joined struct {
		Name       string `json:"name"`
		InstanceID string `json:"instanceId"`
		URL        string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &joined)

	aID, err := a.fed.Identity()
	if err != nil {
		t.Fatal(err)
	}
	bID, _ := b.fed.Identity()
	if joined.InstanceID != aID.InstanceID || joined.URL != aHTTP.URL {
		t.Fatalf("joined peer: %+v", joined)
	}

	// A now holds B as a peer, with the pre-selected grant applied.
	aPeer, err := a.store.FedPeerByInstanceID(bID.InstanceID)
	if err != nil {
		t.Fatalf("inviter has no peer for the joiner: %v", err)
	}
	if aPeer.PublicKey != bID.PublicKeyB64() {
		t.Fatalf("inviter's peer row: %+v", aPeer)
	}
	// B has no advertised URL, so the join fell back to the address the
	// operator reached B's UI at — A must not end up dial-in-only.
	// (httptest requests carry Host "example.com".)
	if aPeer.URL != "http://example.com" {
		t.Fatalf("inviter should hold the joiner's fallback URL, got %q", aPeer.URL)
	}
	if _, ok, _ := a.store.FedCapabilityConfig(aPeer.ID, store.CapFlareIngest, store.DirGrant); !ok {
		t.Fatal("invite grants were not applied on pairing")
	}
	// B recorded what A granted it.
	bPeer, _ := b.store.FedPeerByInstanceID(aID.InstanceID)
	if !strings.Contains(string(bPeer.RemoteGrants), "flare.ingest") {
		t.Fatalf("joiner's remote grants: %s", bPeer.RemoteGrants)
	}

	// The invite is single-use: replaying the code fails.
	w = do(t, bHandler, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(bCookie))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("replayed invite: %d %s", w.Code, w.Body)
	}

	// Heartbeat: B dials A; both sides refresh from the one exchange.
	if err := b.fed.HeartbeatPeer(context.Background(), bPeer); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	bPeer, _ = b.store.FedPeerByID(bPeer.ID)
	aPeer, _ = a.store.FedPeerByID(aPeer.ID)
	if bPeer.LastSeenAt == "" || aPeer.LastSeenAt == "" {
		t.Fatalf("heartbeat did not refresh both sides: %q %q", bPeer.LastSeenAt, aPeer.LastSeenAt)
	}
	// B's payload carried no URL (nothing configured), which must not clear
	// the fallback address A learned at pairing.
	if aPeer.URL != "http://example.com" {
		t.Fatalf("empty heartbeat URL cleared the peer address: %q", aPeer.URL)
	}

	// A URL change on A propagates to B on the next exchange — no re-pair.
	do(t, aHandler, "PUT", "/api/federation/identity", `{"url":"https://fog.moved.example"}`, withCookie(aCookie))
	if err := b.fed.HeartbeatPeer(context.Background(), bPeer); err != nil {
		t.Fatalf("heartbeat after url change: %v", err)
	}
	bPeer, _ = b.store.FedPeerByID(bPeer.ID)
	if bPeer.URL != "https://fog.moved.example" {
		t.Fatalf("advertised URL change did not propagate: %q", bPeer.URL)
	}

	// The federation view reflects it all, and reading it never mints tokens.
	w = do(t, aHandler, "GET", "/api/federation", "", withCookie(aCookie))
	var view struct {
		Identity fed.InstanceDoc `json:"identity"`
		Peers    []struct {
			Name         string `json:"name"`
			Online       bool   `json:"online"`
			Capabilities []struct {
				Capability string `json:"capability"`
				Direction  string `json:"direction"`
			} `json:"capabilities"`
		} `json:"peers"`
	}
	json.Unmarshal(w.Body.Bytes(), &view)
	if view.Identity.InstanceID != aID.InstanceID {
		t.Fatalf("identity view: %+v", view.Identity)
	}
	// The invite's grant, and the household's people — shared with every
	// new pairing by default (docs/signin-contract.md).
	if len(view.Peers) != 1 || !view.Peers[0].Online || len(view.Peers[0].Capabilities) != 2 {
		t.Fatalf("peers view: %+v", view.Peers)
	}
}

func TestFedPairRejectsBadSecrets(t *testing.T) {
	t.Parallel()

	a, aHandler := newTestServer(t)
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	do(t, aHandler, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie))
	mintInvite(t, aHandler, aCookie, `[]`)

	b, _ := newTestServer(t)
	// A joiner with a wrong secret is refused.
	badCode := fed.InviteCode(aHTTP.URL, "not-the-secret", "A", mustIdentity(t, a).EncPublicB64())
	if _, err := b.fed.Join(context.Background(), badCode, ""); err == nil {
		t.Fatal("wrong secret joined")
	}
	if _, err := a.store.FedPeerByInstanceID(mustIdentity(t, b).InstanceID); err != store.ErrNotFound {
		t.Fatalf("failed pair left a peer row: %v", err)
	}
	// Garbage codes are refused client-side.
	if _, err := b.fed.Join(context.Background(), "FOG-garbage", ""); err == nil {
		t.Fatal("garbage code joined")
	}
}

func TestFedHeartbeatRejectsUnknownAndTampered(t *testing.T) {
	t.Parallel()

	_, aHandler := newTestServer(t)
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	do(t, aHandler, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie))
	code := mintInvite(t, aHandler, aCookie, `[]`)

	b, _ := newTestServer(t)
	if _, err := b.fed.Join(context.Background(), code, ""); err != nil {
		t.Fatal(err)
	}

	// A stranger with its own valid keypair but no peer row is refused.
	c, _ := newTestServer(t)
	cID := mustIdentity(t, c)
	body := []byte(`{"fogVersion":"x","grants":[],"directory":[]}`)
	req, _ := http.NewRequest("POST", aHTTP.URL+"/fed/v1/heartbeat", strings.NewReader(string(body)))
	cID.SignRequest(req, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stranger heartbeat: %d", resp.StatusCode)
	}

	// A known peer with a tampered body is refused.
	bID := mustIdentity(t, b)
	req, _ = http.NewRequest("POST", aHTTP.URL+"/fed/v1/heartbeat", strings.NewReader(`{"fogVersion":"evil"}`))
	bID.SignRequest(req, body) // signature over different bytes
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered heartbeat: %d", resp.StatusCode)
	}
}

func mustIdentity(t *testing.T, s *Server) *fed.Identity {
	t.Helper()
	id, err := s.fed.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The disconnect story end to end: A unpairs B, nothing is sent to B, and B
// nonetheless learns — from the refusals its own heartbeats come back with.
func TestUnpairedPeerLearnsFromRefusedHeartbeats(t *testing.T) {
	t.Parallel()

	a, aHandler := newTestServer(t)
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	do(t, aHandler, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie))
	code := mintInvite(t, aHandler, aCookie, `[]`)

	b, bHandler := newTestServer(t)
	bCookie := setup(t, bHandler)
	if _, err := b.fed.Join(context.Background(), code, ""); err != nil {
		t.Fatal(err)
	}
	aInB, err := b.store.FedPeerByInstanceID(mustIdentity(t, a).InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	bInA, err := a.store.FedPeerByInstanceID(mustIdentity(t, b).InstanceID)
	if err != nil {
		t.Fatal(err)
	}

	// A ends it. B is told nothing.
	do(t, aHandler, "DELETE", fmt.Sprintf("/api/federation/peers/%d", bInA.ID), "", withCookie(aCookie))
	if got, _ := b.store.FedPeerByID(aInB.ID); got.UnpairedAt != "" {
		t.Fatal("B knew before it had dialled anyone")
	}

	// B keeps dialling. Every refusal counts, and only the threshold stamps.
	for i := 1; i <= fed.UnpairThreshold; i++ {
		err := b.fed.HeartbeatPeer(context.Background(), aInB)
		if err == nil {
			t.Fatal("heartbeat to an instance that unpaired us succeeded")
		}
		if !fed.Unpaired(err) {
			t.Fatalf("refusal %d not read as an unpairing: %v", i, err)
		}
		got, err := b.store.FedPeerByID(aInB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RejectStreak != i {
			t.Fatalf("after %d refusals streak is %d", i, got.RejectStreak)
		}
		if stamped := got.UnpairedAt != ""; stamped != (i >= fed.UnpairThreshold) {
			t.Fatalf("after %d of %d refusals stamped=%v", i, fed.UnpairThreshold, stamped)
		}
	}

	// The row survives, with everything hanging off it. A peer cannot make
	// this fog forget it by answering 403.
	if _, err := b.store.FedPeerByID(aInB.ID); err != nil {
		t.Fatalf("refusals deleted the peer row: %v", err)
	}

	// Recheck reports what actually happened rather than clearing the state.
	w := do(t, bHandler, "POST", fmt.Sprintf("/api/federation/peers/%d/recheck", aInB.ID), "", withCookie(bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("recheck: %d", w.Code)
	}
	var res struct {
		OK       bool `json:"ok"`
		Unpaired bool `json:"unpaired"`
		Peer     struct {
			UnpairedAt string `json:"unpairedAt"`
		} `json:"peer"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || !res.Unpaired || res.Peer.UnpairedAt == "" {
		t.Fatalf("recheck softened a real unpairing: %+v", res)
	}

	// Reconnecting is one step: a fresh code pasted straight in. Pairing
	// refreshes the row already held and clears the stamp as part of the
	// handshake, so the operator never waits out another heartbeat.
	if _, err := b.fed.Join(context.Background(), mintInvite(t, aHandler, aCookie, `[]`), ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := b.store.FedPeerByID(aInB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.UnpairedAt != "" || fresh.RejectStreak != 0 {
		t.Fatalf("re-pairing left the stamp: unpairedAt=%q streak=%d", fresh.UnpairedAt, fresh.RejectStreak)
	}
}

// A pairing handshake completes on both sides or on neither.
//
// The inviter finishes its entire half — burning the invite, storing our
// key, applying grants — before the joiner ever sees the response. A joiner
// that refused because it already held a row for that instance therefore
// left the two instances disagreeing about whether they were paired, with
// the code spent and nothing to show for it. Since neither identity had
// rotated, the stale row kept working and a heartbeat silently reconnected
// them minutes after the join had reported failure.
func TestRepairLeavesBothSidesPaired(t *testing.T) {
	t.Parallel()

	a, aHandler := newTestServer(t)
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	do(t, aHandler, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie))

	b, _ := newTestServer(t)
	if _, err := b.fed.Join(context.Background(), mintInvite(t, aHandler, aCookie, `[]`), ""); err != nil {
		t.Fatal(err)
	}
	aInB, err := b.store.FedPeerByInstanceID(mustIdentity(t, a).InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	bInA, err := a.store.FedPeerByInstanceID(mustIdentity(t, b).InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	// A refresh keeps the row, so it keeps everything local hanging off it.
	if err := b.store.SetFedPeerLabel(aInB.ID, "the other box"); err != nil {
		t.Fatal(err)
	}

	do(t, aHandler, "DELETE", fmt.Sprintf("/api/federation/peers/%d", bInA.ID), "", withCookie(aCookie))

	// The stale row is still there, and the operator simply pastes a new code.
	peer, err := b.fed.Join(context.Background(), mintInvite(t, aHandler, aCookie, `[]`), "")
	if err != nil {
		t.Fatalf("re-pair refused while a row for that instance was held: %v", err)
	}
	if peer.ID != aInB.ID {
		t.Fatalf("re-pair replaced row %d with %d instead of refreshing it", aInB.ID, peer.ID)
	}
	if peer.Label != "the other box" {
		t.Fatalf("re-pair dropped the local label: %q", peer.Label)
	}
	if _, err := a.store.FedPeerByInstanceID(mustIdentity(t, b).InstanceID); err != nil {
		t.Fatalf("join succeeded but the inviter holds no row: %v", err)
	}
	// Live immediately, rather than resolving itself on some later tick.
	if err := b.fed.HeartbeatPeer(context.Background(), peer); err != nil {
		t.Fatalf("heartbeat straight after a re-pair: %v", err)
	}
}

// Only "we have no row for you" means unpaired. Every other refusal is about
// the request, and must never be read as the end of a relationship.
func TestFedRejectionCodesDistinguishUnpairingFromBadRequests(t *testing.T) {
	t.Parallel()

	a, aHandler := newTestServer(t)
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	do(t, aHandler, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie))
	code := mintInvite(t, aHandler, aCookie, `[]`)

	b, _ := newTestServer(t)
	if _, err := b.fed.Join(context.Background(), code, ""); err != nil {
		t.Fatal(err)
	}
	_ = a

	var asker *fed.Identity
	codeOf := func(t *testing.T, sign func(*http.Request, []byte), body []byte) string {
		t.Helper()
		req, _ := http.NewRequest("POST", aHTTP.URL+"/fed/v1/heartbeat", nil)
		if sign != nil {
			sign(req, body)
		} else {
			req.URL.Path = fed.SealedPath
			req.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		// A refusal is sealed to the key the request presented; one with no
		// key at all (not a fog) is plain.
		status, raw := sendFed(t, req, asker, a)
		if status != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", status)
		}
		var out struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out.Code
	}
	sealAs := func(id *fed.Identity) func(*http.Request, []byte) {
		return func(r *http.Request, body []byte) { sealFor(t, id, a, r, body) }
	}

	payload := []byte(`{"fogVersion":"x","grants":[],"directory":[]}`)
	stranger := mustIdentity(t, mustServer(t))
	asker = stranger
	if got := codeOf(t, sealAs(stranger), payload); got != fed.CodeUnknownPeer {
		t.Fatalf("stranger got %q, want %q", got, fed.CodeUnknownPeer)
	}
	if got := codeOf(t, nil, payload); got != fed.CodeMissingInstance {
		t.Fatalf("unsigned got %q, want %q", got, fed.CodeMissingInstance)
	}
	// A known peer signing over different bytes: the row is fine, the
	// request is not. Reading this as an unpairing would let one corrupted
	// request tear down a healthy pairing.
	bID := mustIdentity(t, b)
	asker = bID
	// Sealed and signed, then the sealed bytes swapped for others.
	got := codeOf(t, func(r *http.Request, _ []byte) {
		sealFor(t, bID, a, r, payload)
		r.Body = io.NopCloser(strings.NewReader("tampered"))
		r.ContentLength = int64(len("tampered"))
	}, nil)
	if got != fed.CodeBadSignature {
		t.Fatalf("tampered got %q, want %q", got, fed.CodeBadSignature)
	}
	if fed.Unpaired(&fed.RemoteError{Status: 403, Code: fed.CodeBadSignature}) {
		t.Fatal("a bad signature read as an unpairing")
	}
	// A bare proxy 403 carries no code and must stay ambiguous.
	if fed.Unpaired(&fed.RemoteError{Status: 403, Message: "Forbidden"}) {
		t.Fatal("an uncoded 403 read as an unpairing")
	}
}

func mustServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newTestServer(t)
	return s
}

// A fresh, domainless instance is honestly dial-out-only, with advice; setting
// an advertised URL flips it reachable with no advice. This is the Federation
// page's reachability story (Project L).
func TestFederationReachabilityStory(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	cookie := setup(t, h)

	get := func() fedReachability {
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

	r := get()
	if r.Reachable || r.Via != "none" || r.Advice == "" {
		t.Fatalf("fresh instance should be unreachable with advice, got %+v", r)
	}

	if w := do(t, h, "PUT", "/api/federation/identity",
		`{"url":"https://fog.example.com"}`, withCookie(cookie)); w.Code != http.StatusOK {
		t.Fatalf("set advertised url: %d %s", w.Code, w.Body)
	}
	r = get()
	if !r.Reachable || r.Via != "advertised" || r.URL != "https://fog.example.com" || r.Advice != "" {
		t.Fatalf("after advertising a URL, expected reachable/advertised/no-advice, got %+v", r)
	}
	_ = s
}

// The join preview decodes a pasted invite — showing who and where — without
// pairing or spending the secret (Project L rich invites).
func TestFedJoinPreview(t *testing.T) {
	t.Parallel()

	inviter, ih := newTestServer(t)
	iCookie := setup(t, ih)
	if w := do(t, ih, "PUT", "/api/federation/identity",
		`{"url":"https://fog.inviter.example"}`, withCookie(iCookie)); w.Code != http.StatusOK {
		t.Fatalf("advertise: %d %s", w.Code, w.Body)
	}
	code := mintInvite(t, ih, iCookie, `[]`)
	_ = inviter

	_, jh := newTestServer(t)
	jCookie := setup(t, jh)
	w := do(t, jh, "POST", "/api/federation/join/preview", `{"code":`+jsonString(code)+`}`, withCookie(jCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	var prev struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &prev)
	if prev.URL != "https://fog.inviter.example" {
		t.Fatalf("preview url %q", prev.URL)
	}
	// Garbage is a clean 400, no pairing attempted.
	if w := do(t, jh, "POST", "/api/federation/join/preview", `{"code":"nope"}`, withCookie(jCookie)); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage preview should 400, got %d", w.Code)
	}
}

// The impersonation mirror shows what one peer receives from us: granted
// capabilities as "sees", the rest as withheld, in the peer's voice — and it
// mints nothing (Project L "view as a peer").
func TestFedPeerMirror(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	cookie := setup(t, h)
	// Name ourselves so the sentence reads naturally.
	if err := s.store.SetSetting(store.SettingServerName, "My House"); err != nil {
		t.Fatal(err)
	}
	peer, err := s.store.CreatePairedPeer("01ABCPEER", "dave", "https://fog.dave.example", "cGs=")
	if err != nil {
		t.Fatal(err)
	}
	// Grant exactly one capability; the mirror must split the registry cleanly.
	if err := s.store.SetFedCapability(peer.ID, store.CapFlareIngest, store.DirGrant, ""); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", fmt.Sprintf("/api/federation/peers/%d/mirror", peer.ID), "", withCookie(cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("mirror: %d %s", w.Code, w.Body)
	}
	var m struct {
		As           string `json:"as"`
		From         string `json:"from"`
		Sentence     string `json:"sentence"`
		Capabilities []struct {
			Capability string `json:"capability"`
			Granted    bool   `json:"granted"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.As != "dave" || m.From != "My House" {
		t.Fatalf("mirror voice wrong: as=%q from=%q", m.As, m.From)
	}
	if len(m.Capabilities) != len(fedCapabilities) {
		t.Fatalf("expected the whole registry (%d), got %d", len(fedCapabilities), len(m.Capabilities))
	}
	granted := 0
	for _, c := range m.Capabilities {
		if c.Granted {
			granted++
			if c.Capability != store.CapFlareIngest {
				t.Fatalf("unexpected granted cap %q", c.Capability)
			}
		}
	}
	if granted != 1 {
		t.Fatalf("exactly one cap should be granted, got %d", granted)
	}
	if !strings.Contains(m.Sentence, "dave") || !strings.Contains(m.Sentence, "do not see") {
		t.Fatalf("sentence should be in dave's voice with a withheld clause: %q", m.Sentence)
	}

	// Unknown peer is a clean 404.
	if w := do(t, h, "GET", "/api/federation/peers/9999/mirror", "", withCookie(cookie)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown peer mirror should 404, got %d", w.Code)
	}
}

// The federation ledger unifies boundary crossings from the audit log, where
// the peer identifier lands in a different column per verb — so the per-peer
// filter must look past subject alone (Project K, review fix).
func TestFederationLedger(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	cookie := setup(t, h)

	dave, err := s.store.CreatePairedPeer("01DAVE", "dave", "https://fog.dave", "cGs=")
	if err != nil {
		t.Fatal(err)
	}
	daveID := strconv.FormatInt(dave.ID, 10)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Dave crossings, each with the peer recorded where its real writer puts it:
	must(s.store.Audit("me", "federation.pair", "dave", `{"role":"joiner"}`))        // name in subject
	must(s.store.Audit("me", "files.subscribe", "Photos", "dave"))                   // name in detail
	must(s.store.Audit("federation:dave", "files.contribution", "Photos/x.jpg", "")) // name in actor
	must(s.store.Audit("me", "files.share.peer", "Photos", daveID))                  // peer id in detail
	// A different peer, and a local action that never crossed anything.
	must(s.store.Audit("me", "federation.pair", "erin", ""))
	must(s.store.Audit("me", "app.deploy.webhook", "feed", ""))

	get := func(q string) []map[string]any {
		t.Helper()
		w := do(t, h, "GET", "/api/federation/ledger"+q, "", withCookie(cookie))
		if w.Code != http.StatusOK {
			t.Fatalf("ledger %q: %d %s", q, w.Code, w.Body)
		}
		var resp struct {
			Entries []map[string]any `json:"entries"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Entries
	}

	all := get("")
	if len(all) != 5 {
		t.Fatalf("expected 5 boundary crossings (app.deploy excluded), got %d: %+v", len(all), all)
	}
	if all[0]["action"] != "federation.pair" || all[0]["subject"] != "erin" {
		t.Fatalf("newest-first order wrong: %+v", all[0])
	}
	if all[0]["category"] != "trust" || all[0]["summary"] != "paired" {
		t.Fatalf("enrichment wrong: %+v", all[0])
	}

	// Scoped to Dave: every crossing that named him, however it was recorded —
	// subject, detail (name), actor, and detail (peer id) — but not Erin's.
	dl := get("?peer=dave")
	actions := map[string]bool{}
	for _, e := range dl {
		if e["subject"] == "erin" {
			t.Fatalf("peer filter leaked another peer: %+v", e)
		}
		actions[e["action"].(string)] = true
	}
	for _, want := range []string{"federation.pair", "files.subscribe", "files.contribution", "files.share.peer"} {
		if !actions[want] {
			t.Fatalf("peer filter missed %s — got %v", want, actions)
		}
	}
	if len(dl) != 4 {
		t.Fatalf("Dave's ledger should have 4 crossings, got %d: %+v", len(dl), dl)
	}
}

// A refused update changes nothing: the rename, label and address in the
// same request as a bad capability must not be saved behind a 400.
func TestUpdateFedPeerRefusesWholesale(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[]`)
	before, err := p.b.store.FedPeerByID(p.aPeerOnB.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"name":"renamed","label":"Renamed","url":"https://elsewhere.example","capabilities":[{"capability":"no.such.thing","direction":"use","enabled":true}]}`
	w := do(t, p.bH, "PUT", fmt.Sprintf("/api/federation/peers/%d", before.ID), body, withCookie(p.bCookie))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	after, err := p.b.store.FedPeerByID(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name || after.Label != before.Label || after.URL != before.URL {
		t.Errorf("refused update was partly saved: before %q/%q/%q, after %q/%q/%q",
			before.Name, before.Label, before.URL, after.Name, after.Label, after.URL)
	}
}
