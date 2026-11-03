package fed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// A failed delivery POST is a failed dial like any other: it counts toward
// "peer unreachable", and the notice waits for the streak to have lasted
// DialFailWindow as well as reaching the threshold, so a burst of queued
// sends toward a restarting peer can't raise it in seconds. Any HTTP
// answer — even a rejection — ends the streak.
func TestSignedPostFailureCountsTowardUnreachable(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}

	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	peer := &store.FedPeer{ID: 7, Name: "attic", URL: dead}

	ctx := context.Background()
	for i := 0; i < DialFailThreshold+2; i++ {
		if _, err := s.SignedPostVerified(ctx, peer, "/fed/v1/chat/notify", "application/json", []byte(`{}`)); err == nil {
			t.Fatal("post to a dead address succeeded")
		}
	}
	if _, _, failing := s.DialStatus(peer.Name); failing {
		t.Fatal("a burst of failures inside the window raised the notice")
	}
	if n, _ := st.CountActiveNotices(); n != 0 {
		t.Fatalf("%d notices raised inside the window", n)
	}

	// The streak has now lasted the window.
	s.dialFails().Now = func() time.Time { return time.Now().Add(DialFailWindow) }
	if _, err := s.SignedPost(ctx, peer, "/fed/v1/flare/envelope", "application/x-sentry-envelope", nil); err == nil {
		t.Fatal("post to a dead address succeeded")
	}
	if _, _, failing := s.DialStatus(peer.Name); !failing {
		t.Fatal("a streak past threshold and window is not reported failing")
	}
	if n, _ := st.CountActiveNotices(); n != 1 {
		t.Fatalf("want one unreachable notice, have %d", n)
	}

	// Whatever answers at the address without the peer's key — a proxy's
	// error page, a stranger on a recycled lease — is not the peer
	// answering, and clears nothing.
	st2, vlt2 := testDeps(t)
	them, err := EnsureIdentity(st2, vlt2)
	if err != nil {
		t.Fatal(err)
	}
	peer.PublicKey, peer.InstanceID = them.PublicKeyB64(), them.InstanceID
	signed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"error":"no","code":"bad_signature"}`)
		if signed {
			wire, _ := io.ReadAll(r.Body)
			sr, _, _ := OpenSealedRequest(them, r, wire, them.InstanceID)
			WriteRecorded(w, them, sr, http.StatusForbidden, nil, body)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write(body)
	}))
	defer srv.Close()
	peer.URL = srv.URL
	var re *RemoteError
	// It can't even prove the connection, so the report is never sent.
	if _, err := s.SignedPostVerified(ctx, peer, "/fed/v1/feedback", "application/json", []byte(`{}`)); !errors.Is(err, ErrUnproven) {
		t.Fatalf("want ErrUnproven, got %v", err)
	}
	if _, _, failing := s.DialStatus(peer.Name); !failing {
		t.Fatal("an unsigned refusal cleared the unreachable state")
	}

	// A refusal the peer signed for this request is an answer: reachable
	// again, notice cleared.
	signed = true
	if _, err := s.SignedPostVerified(ctx, peer, "/fed/v1/feedback", "application/json", []byte(`{}`)); !errors.As(err, &re) || !re.Verified {
		t.Fatalf("want a verified RemoteError, got %v", err)
	}
	if _, _, failing := s.DialStatus(peer.Name); failing {
		t.Fatal("an answered dial left the peer failing")
	}
	if n, _ := st.CountActiveNotices(); n != 0 {
		t.Fatalf("notice not cleared: %d active", n)
	}
}

// A caller abandoning its own request says nothing about the peer.
func TestCancelledRequestIsNotADialFailure(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	peer := &store.FedPeer{ID: 3, Name: "p", URL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.SignedPost(ctx, peer, "/x", "application/json", nil); err == nil {
		t.Fatal("cancelled request succeeded")
	}
	if s.dialFails().Streak(peer.Name) != 0 {
		t.Fatal("a cancelled request counted as a failed dial")
	}
}

// Every helper takes only an answer signed for the request it sent: the
// peer's real answer to some other request, captured and replayed, is
// refused by each.
func TestHelpersVerifyTheAnswerIsForThisRequest(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}
	st2, vlt2 := testDeps(t)
	them, err := EnsureIdentity(st2, vlt2)
	if err != nil {
		t.Fatal(err)
	}
	var lie bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		sr, _, err := OpenSealedRequest(them, r, wire, them.InstanceID)
		if err != nil {
			return
		}
		body := []byte(`{"ok":true}`)
		if sr.Hello || !lie {
			WriteRecorded(w, them, sr, http.StatusOK, nil, body)
			return
		}
		b := sr.Bound
		b.Sig = "another-request"
		forge(w, sr, them.InstanceID, them, b, http.StatusOK, nil, body)
	}))
	defer srv.Close()
	peer, err := st.CreatePairedPeer(them.InstanceID, "p", srv.URL, them.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	calls := map[string]func() error{
		"SignedPostVerified": func() error {
			_, err := s.SignedPostVerified(ctx, peer, "/fed/v1/chat/rooms/r1/groupinfo?actor=a@b", "application/json", []byte(`{}`))
			return err
		},
		"SignedGetJSON": func() error {
			var out struct{ OK bool }
			_, err := s.SignedGetJSON(ctx, peer, "/fed/v1/chat/rooms/r1/events?after=3&limit=500", "", &out)
			if err == nil && !out.OK {
				return errors.New("decoded nothing")
			}
			return err
		},
		"SignedDo": func() error {
			resp, err := s.SignedDo(ctx, peer, "GET", "/fed/v1/files/shares/1/thumb/a.jpg", "", nil)
			if err == nil {
				resp.Body.Close()
			}
			return err
		},
	}
	for name, call := range calls {
		lie = false
		if err := call(); err != nil {
			t.Errorf("%s: an answer signed for this request: %v", name, err)
		}
		lie = true
		if err := call(); err == nil || errors.Is(err, ErrUnsignedResponse) {
			t.Errorf("%s: an answer signed for another request: %v", name, err)
		}
	}
}

// The fed plane follows no redirects: a 3xx is an answer, unsigned, and
// the request it would have sent on is never sent — not to another path,
// not to another host.
func TestFedPlaneFollowsNoRedirects(t *testing.T) {
	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)}
	st2, vlt2 := testDeps(t)
	them, _ := EnsureIdentity(st2, vlt2)
	var followed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		if r.URL.Path != SealedPath {
			followed.Add(1)
			return
		}
		sr, _, _ := OpenSealedRequest(them, r, wire, them.InstanceID)
		if sr != nil && sr.Hello {
			WriteRecorded(w, them, sr, http.StatusOK, nil, []byte("{}"))
			return
		}
		http.Redirect(w, r, "/elsewhere?x=1", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	ctx := context.Background()
	paired, _ := st.CreatePairedPeer(them.InstanceID, "paired", srv.URL, them.PublicKeyB64())
	var re *RemoteError
	if _, err := s.SignedDo(ctx, paired, "GET", "/fed/v1/a?w=64", "", nil); !errors.As(err, &re) || re.Status != http.StatusTemporaryRedirect {
		t.Errorf("streamed GET through a redirect: %v", err)
	}
	if _, err := s.SignedPostVerified(ctx, paired, "/fed/v1/a", "application/json", []byte(`{"secret":1}`)); !errors.As(err, &re) || re.Status != http.StatusTemporaryRedirect {
		t.Errorf("POST through a redirect: %v", err)
	}
	if followed.Load() != 0 {
		t.Fatalf("a redirect was followed %d times", followed.Load())
	}
}
