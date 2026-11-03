package fed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// answerFixture is one paired peer ("them") as this side holds it,
// answering at srv: the proof as the peer, and every other request however
// the test's answer func says, given the opened envelope.
type answerFixture struct {
	s        *Service
	st       *store.Store
	them     *Identity
	stranger *Identity
	peer     *store.FedPeer
	answer   func(w http.ResponseWriter, sr *SealedRequest)
}

func newAnswerFixture(t *testing.T) *answerFixture {
	t.Helper()
	st, vlt := testDeps(t)
	f := &answerFixture{st: st, s: &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler), Version: "test"}}
	st2, vlt2 := testDeps(t)
	var err error
	if f.them, err = EnsureIdentity(st2, vlt2); err != nil {
		t.Fatal(err)
	}
	st3, vlt3 := testDeps(t)
	if f.stranger, err = EnsureIdentity(st3, vlt3); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		sr, _, err := OpenSealedRequest(f.them, r, wire, f.them.InstanceID)
		if err != nil {
			http.Error(w, "unsealed", http.StatusForbidden)
			return
		}
		if sr.Hello {
			WriteRecorded(w, f.them, sr, http.StatusOK, nil, []byte("{}"))
			return
		}
		f.answer(w, sr)
	}))
	t.Cleanup(srv.Close)
	if f.peer, err = st.CreatePairedPeer(f.them.InstanceID, "riverside", srv.URL, f.them.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	return f
}

// forge writes an envelope answer to sr as responder names it, signed by
// signer (nil: unsigned) for the request bound — the variations a squatter,
// a replayer or a buggy peer could produce.
func forge(w http.ResponseWriter, sr *SealedRequest, responder string, signer *Identity, bound Bound, status int, hdr http.Header, body []byte) {
	sum := sha256.Sum256(body)
	ts := uniqueStamp()
	head, _ := json.Marshal(AnswerHead{Status: status, Header: hdr, Digest: hex.EncodeToString(sum[:]), Instance: responder, Time: ts})
	sig := ""
	if signer != nil {
		sig = signer.Sign(methodAnswer, sr.Target, ts, AnswerMaterial(status, bound, head))
	}
	plain := binary.BigEndian.AppendUint32(nil, uint32(len(head)))
	plain = append(plain, head...)
	plain = binary.BigEndian.AppendUint16(plain, uint16(len(sig)))
	plain = append(append(plain, sig...), body...)
	sealed, _ := Seal(sr.Requester, sr.AnswerInfo(responder), plain)
	w.Write(sealed)
}

func (f *answerFixture) reload(t *testing.T) *store.FedPeer {
	t.Helper()
	p, err := f.st.FedPeerByID(f.peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// forged is what an address squatter, or anyone on the path of a plain-http
// address, would answer a heartbeat with: its own device keys for the
// peer's people, its own app URLs, its own version.
var forged = HeartbeatPayload{
	FogVersion: "forged",
	URL:        "https://evil.example",
	Grants:     []string{"chat"},
	Directory:  []DirectoryApp{{App: "feed", URL: "https://evil.example/feed"}},
	People: []DirectoryPerson{{Address: "person:x/alice", Handle: "alice", DisplayName: "Alice",
		Devices: []DirectoryDevice{{ID: 1, Name: "evil", DeviceKey: "attacker-key", Verified: true}}}},
}

// Finding 2: a heartbeat answer that the peer's key did not sign — unsigned,
// or signed by someone else — changes nothing: not people (which a chat hub
// checks remote devices against), not the directory, version or address,
// and it does not count as having seen the peer.
func TestUnprovenHeartbeatAnswerChangesNothing(t *testing.T) {
	f := newAnswerFixture(t)
	body, _ := json.Marshal(forged)
	for name, answer := range map[string]func(w http.ResponseWriter, sr *SealedRequest){
		"unsigned": func(w http.ResponseWriter, sr *SealedRequest) {
			forge(w, sr, f.them.InstanceID, nil, sr.Bound, http.StatusOK, nil, body)
		},
		"signed by another": func(w http.ResponseWriter, sr *SealedRequest) {
			forge(w, sr, f.them.InstanceID, f.stranger, sr.Bound, http.StatusOK, nil, body)
		},
		// The peer's real answer to an earlier heartbeat, replayed into this
		// one inside the signature window.
		"signed for another request": func(w http.ResponseWriter, sr *SealedRequest) {
			b := sr.Bound
			b.Sig = "an-earlier-heartbeat"
			forge(w, sr, f.them.InstanceID, f.them, b, http.StatusOK, nil, body)
		},
		"sealed for another exchange": func(w http.ResponseWriter, sr *SealedRequest) {
			moved := *sr
			moved.Bound.Sig = "an-earlier-heartbeat"
			forge(w, &moved, f.them.InstanceID, f.them, moved.Bound, http.StatusOK, nil, body)
		},
		"not sealed": func(w http.ResponseWriter, sr *SealedRequest) { w.Write(body) },
	} {
		f.answer = answer
		if err := f.s.HeartbeatPeer(context.Background(), f.peer); err == nil {
			t.Fatalf("%s: a heartbeat answer the peer did not sign was accepted", name)
		}
		got := f.reload(t)
		if got.LastSeenAt != "" || got.FogVersion != "" || string(got.People) != "[]" ||
			got.URL != f.peer.URL || string(got.Directory) == string(mustJSON(forged.Directory)) {
			t.Fatalf("%s: an unproven answer changed the peer: %+v", name, got)
		}
		if ls := f.s.LinkStatus(f.peer.ID); ls.Answered != 0 {
			t.Fatalf("%s: an unproven answer counted as the peer answering", name)
		}
	}

	// The peer's own signed answer is recorded.
	f.answer = func(w http.ResponseWriter, sr *SealedRequest) {
		WriteRecorded(w, f.them, sr, http.StatusOK, nil, body)
	}
	if err := f.s.HeartbeatPeer(context.Background(), f.peer); err != nil {
		t.Fatal(err)
	}
	if got := f.reload(t); got.FogVersion != "forged" || got.LastSeenAt == "" {
		t.Fatalf("the peer's signed answer was not recorded: %+v", got)
	}
}

// A signed payload is good for five minutes either side; one older than a
// payload already applied — a replay, or two exchanges crossing — must not
// roll the peer back.
func TestStaleHeartbeatPayloadIsNotApplied(t *testing.T) {
	f := newAnswerFixture(t)
	newer := HeartbeatPayload{FogVersion: "2", URL: "https://new.example", Grants: []string{}, Directory: []DirectoryApp{}}
	older := HeartbeatPayload{FogVersion: "1", URL: "https://old.example", Grants: []string{}, Directory: []DirectoryApp{}}
	if err := f.s.RecordPeerPayload(f.peer.ID, &newer, "2026-11-02T10:00:10Z"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordPeerPayload(f.peer.ID, &older, "2026-11-02T10:00:05Z"); !errors.Is(err, ErrStalePayload) {
		t.Fatalf("an older payload: err = %v, want ErrStalePayload", err)
	}
	if got := f.reload(t); got.FogVersion != "2" || got.URL != "https://new.example" {
		t.Fatalf("a replayed payload rolled the peer back: %+v", got)
	}
	if err := f.s.RecordPeerPayload(f.peer.ID, &older, "not a time"); err == nil {
		t.Fatal("a payload with no usable timestamp was applied")
	}
}

// Finding 3: a 304 is a claim — "the version you hold is current" — and an
// unproven one would let whoever answers at the address freeze what this
// side holds. It must be signed by the peer, for the entity tag sent, and
// for this request.
func TestNotModifiedMustBeProven(t *testing.T) {
	f := newAnswerFixture(t)
	ctx := context.Background()
	etag := func(v string) http.Header { return http.Header{"Etag": {v}} }
	for name, answer := range map[string]func(w http.ResponseWriter, sr *SealedRequest){
		"unsigned": func(w http.ResponseWriter, sr *SealedRequest) {
			forge(w, sr, f.them.InstanceID, nil, sr.Bound, http.StatusNotModified, etag("v1"), nil)
		},
		"signed by another": func(w http.ResponseWriter, sr *SealedRequest) {
			forge(w, sr, f.them.InstanceID, f.stranger, sr.Bound, http.StatusNotModified, etag("v1"), nil)
		},
		"for another version": func(w http.ResponseWriter, sr *SealedRequest) {
			forge(w, sr, f.them.InstanceID, f.them, sr.Bound, http.StatusNotModified, etag("v0"), nil)
		},
		// A 304 the peer really signed, captured from some other exchange
		// and replayed into this one.
		"for another request": func(w http.ResponseWriter, sr *SealedRequest) {
			b := sr.Bound
			b.Sig = "captured"
			forge(w, sr, f.them.InstanceID, f.them, b, http.StatusNotModified, etag("v1"), nil)
		},
		"not sealed": func(w http.ResponseWriter, sr *SealedRequest) { w.WriteHeader(http.StatusNotModified) },
	} {
		f.answer = answer
		var out map[string]any
		if _, err := f.s.SignedGetJSON(ctx, f.peer, "/fed/v1/files/shares/1/manifest", "v1", &out); err == nil || errors.Is(err, ErrNotModified) {
			t.Fatalf("%s: an unproven 304 was taken as not-modified (%v)", name, err)
		}
	}
	f.answer = func(w http.ResponseWriter, sr *SealedRequest) {
		WriteRecorded(w, f.them, sr, http.StatusNotModified, etag("v1"), nil)
	}
	if _, err := f.s.SignedGetJSON(ctx, f.peer, "/fed/v1/files/shares/1/manifest", "v1", nil); !errors.Is(err, ErrNotModified) {
		t.Fatalf("the peer's own 304: %v", err)
	}
}

// "Unknown peer" is the whole disconnect protocol, so only the peer saying
// it — for this request — counts. A bare code from a proxy or a stranger,
// or a refusal the peer signed for somebody else's request carrying a copy
// of our signature, stamps nothing.
func TestOnlyAProvenRefusalIsAnUnpairing(t *testing.T) {
	f := newAnswerFixture(t)
	body := []byte(`{"error":"unknown peer","code":"unknown_peer"}`)
	for name, answer := range map[string]func(w http.ResponseWriter, sr *SealedRequest){
		"unsigned": func(w http.ResponseWriter, sr *SealedRequest) {
			forge(w, sr, f.them.InstanceID, nil, sr.Bound, http.StatusForbidden, nil, body)
		},
		"signed by another": func(w http.ResponseWriter, sr *SealedRequest) {
			forge(w, sr, f.them.InstanceID, f.stranger, sr.Bound, http.StatusForbidden, nil, body)
		},
		"for another caller": func(w http.ResponseWriter, sr *SealedRequest) {
			b := sr.Bound
			b.Instance = "someone-else"
			forge(w, sr, f.them.InstanceID, f.them, b, http.StatusForbidden, nil, body)
		},
		"a bare 403": func(w http.ResponseWriter, sr *SealedRequest) {
			w.WriteHeader(http.StatusForbidden)
			w.Write(body)
		},
	} {
		f.answer = answer
		for i := 0; i < UnpairThreshold+1; i++ {
			err := f.s.HeartbeatPeer(context.Background(), f.peer)
			if err == nil || Unpaired(err) {
				t.Fatalf("%s: read as an unpairing: %v", name, err)
			}
		}
		if got := f.reload(t); got.RejectStreak != 0 || got.UnpairedAt != "" {
			t.Fatalf("%s: an unproven refusal counted: streak %d, stamp %q", name, got.RejectStreak, got.UnpairedAt)
		}
	}
	f.answer = func(w http.ResponseWriter, sr *SealedRequest) {
		WriteRecorded(w, f.them, sr, http.StatusForbidden, nil, body)
	}
	for i := 0; i < UnpairThreshold; i++ {
		if err := f.s.HeartbeatPeer(context.Background(), f.peer); !Unpaired(err) {
			t.Fatalf("the peer's own refusal not read as an unpairing: %v", err)
		}
	}
	if got := f.reload(t); got.UnpairedAt == "" {
		t.Fatal("the peer's own refusals did not stamp it unpaired")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// A stale address: whatever holds it now answers, but can't prove it holds
// the peer's key — so no request is ever written to it. Not the heartbeat
// (which carries the people and apps published to the peer), not a GET
// (whose path names a file), not a streamed upload.
func TestNothingIsSentToAnAddressThatCannotProveThePeer(t *testing.T) {
	f := newAnswerFixture(t)
	var mu sync.Mutex
	var got []string
	for name, hello := range map[string]func(w http.ResponseWriter, r *http.Request, wire []byte){
		"a stranger's fog": func(w http.ResponseWriter, r *http.Request, wire []byte) {
			sr, _, _ := OpenSealedRequest(f.stranger, r, wire, f.stranger.InstanceID)
			WriteRecorded(w, f.stranger, sr, http.StatusOK, nil, []byte("{}"))
		},
		"a web server": func(w http.ResponseWriter, r *http.Request, wire []byte) { w.Write([]byte("<html>")) },
		"the peer's proof, replayed": func(w http.ResponseWriter, r *http.Request, wire []byte) {
			sr, _, _ := OpenSealedRequest(f.them, r, wire, f.them.InstanceID)
			b := sr.Bound
			b.Sig = "captured"
			forge(w, sr, f.them.InstanceID, f.them, b, http.StatusOK, nil, []byte("{}"))
		},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wire, _ := io.ReadAll(r.Body)
			if len(wire) == 0 {
				hello(w, r, wire)
				return
			}
			mu.Lock()
			got = append(got, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}))
		peer := *f.peer
		peer.URL = srv.URL
		ctx := context.Background()
		if err := f.s.HeartbeatPeer(ctx, &peer); !errors.Is(err, ErrUnproven) {
			t.Errorf("%s: heartbeat: %v", name, err)
		}
		if _, err := f.s.SignedGetJSON(ctx, &peer, "/fed/v1/files/shares/1/blob/Diary/secret.pdf", "", nil); !errors.Is(err, ErrUnproven) {
			t.Errorf("%s: GET: %v", name, err)
		}
		if _, err := f.s.SignedDo(ctx, &peer, "PATCH", "/fed/v1/files/shares/1/contributions/u", "application/octet-stream", []byte("bytes")); !errors.Is(err, ErrUnproven) {
			t.Errorf("%s: upload: %v", name, err)
		}
		srv.Close()
	}
	if len(got) != 0 {
		t.Fatalf("requests reached an address that never proved the peer: %v", got)
	}
}
