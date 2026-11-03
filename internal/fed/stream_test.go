package fed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowReader yields left bytes of 'x', a pause before each read after the
// first, and blocks for good after stallAfter reads when stallAfter > 0.
type slowReader struct {
	left, stallAfter, given int
	ctx                     context.Context
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	if s.stallAfter > 0 && s.given == s.stallAfter {
		<-s.ctx.Done()
		return 0, s.ctx.Err()
	}
	if s.given > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	n := min(len(p), s.left)
	for i := range p[:n] {
		p[i] = 'x'
	}
	s.left -= n
	s.given++
	return n, nil
}

// A streamed file may take far longer than any fixed timeout as long as it
// keeps moving; one that stalls is cut off by the idle watchdog.
func TestSignedDoStreamsPastIdleButNotStalls(t *testing.T) {
	old := streamIdle
	streamIdle = 150 * time.Millisecond
	t.Cleanup(func() { streamIdle = old })

	st, vlt := testDeps(t)
	s := &Service{Store: st, Vault: vlt}
	pst, pvlt := testDeps(t)
	them, _ := EnsureIdentity(pst, pvlt)
	whole := []byte(strings.Repeat("x", 10*SealChunk))
	sum := sha256.Sum256(whole)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		sr, _, err := OpenSealedRequest(them, r, wire, them.InstanceID)
		if err != nil {
			return
		}
		if sr.Hello {
			WriteRecorded(w, them, sr, http.StatusOK, nil, []byte("{}"))
			return
		}
		src := &slowReader{left: len(whole), ctx: r.Context()}
		if sr.Inner.URL.Path == "/stall" {
			src.stallAfter = 4
		}
		WriteSealedAnswer(w, them, sr.Requester, sr.AnswerInfo(them.InstanceID), sr.Target, sr.Bound, http.StatusOK, nil, hex.EncodeToString(sum[:]), src)
	}))
	defer srv.Close()
	peer, _ := st.CreatePairedPeer(them.InstanceID, "p", srv.URL, them.PublicKeyB64())

	resp, err := s.SignedDo(context.Background(), peer, "GET", "/slow", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(b) != len(whole) {
		t.Fatalf("slow but steady body: %d bytes, %v", len(b), err)
	}

	resp, err = s.SignedDo(context.Background(), peer, "GET", "/stall", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled body read to completion")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled body was never cut off")
	}
}
