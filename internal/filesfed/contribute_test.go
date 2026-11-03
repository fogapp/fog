package filesfed

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// A session nobody finished or aborted is dropped once it has sat idle
// past the upload sweep; a slow one still receiving chunks is kept even
// though it started long ago.
func TestBeginContributionPrunesAbandonedSessions(t *testing.T) {
	s, peer, _ := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	sh.Mode = ModeContribute

	old, err := s.BeginContribution(peer, sh, "old.txt", 3, "", "")
	if err != nil {
		t.Fatal(err)
	}
	slow, err := s.BeginContribution(peer, sh, "slow.txt", 3, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.contribMu.Lock()
	long := time.Now().Add(-contribIdleTTL - time.Hour)
	s.contribs[old.ID] = contribSession{peerID: peer.ID, shareID: sh.ID, shareCreated: sh.CreatedAt, inner: "old.txt", size: 3, startedAt: long, touched: long}
	s.contribs[slow.ID] = contribSession{peerID: peer.ID, shareID: sh.ID, shareCreated: sh.CreatedAt, inner: "slow.txt", size: 3, startedAt: long, touched: long}
	s.contribMu.Unlock()
	// A chunk arriving refreshes the slow session.
	if _, err := s.contribFor(peer, sh, slow.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.BeginContribution(peer, sh, "new.txt", 3, "", ""); err != nil {
		t.Fatal(err)
	}
	s.contribMu.Lock()
	defer s.contribMu.Unlock()
	if _, ok := s.contribs[old.ID]; ok {
		t.Error("an abandoned session outlived the upload sweep")
	}
	if _, ok := s.contribs[slow.ID]; !ok {
		t.Error("a session still receiving chunks was pruned")
	}
	if len(s.contribs) != 2 {
		t.Errorf("sessions = %d, want 2", len(s.contribs))
	}
}

// A session is bound to the share as it is when each chunk arrives: once
// the share stops taking contributions, or the file is marked keep on this
// box, the session ends instead of landing the file; a session can't carry
// over to a new share that reuses a deleted one's id; and the contribute
// quota holds at landing, however long a session was kept alive.
func TestContributionSessionFollowsTheShare(t *testing.T) {
	s, peer, _ := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	sh.Mode = ModeContribute

	cb, err := s.BeginContribution(peer, sh, "a.txt", 3, "", "")
	if err != nil {
		t.Fatal(err)
	}
	narrowed := *sh
	narrowed.Mode = ModeRead
	if _, err := s.CompleteContribution(peer, &narrowed, cb.ID); !errors.Is(err, ErrNotContribute) {
		t.Fatalf("completing after the share went read-only: %v", err)
	}
	if _, err := s.contribFor(peer, sh, cb.ID); err == nil {
		t.Fatal("the session outlived the narrowing")
	}

	cb, _ = s.BeginContribution(peer, sh, "b.txt", 3, "", "")
	if err := s.Store.AddFileShareExclude("Drop/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContribution(peer, sh, cb.ID, 0, strings.NewReader("bbb")); !errors.Is(err, ErrKeptHere) {
		t.Fatalf("a chunk for a file since kept on this box: %v", err)
	}

	cb, _ = s.BeginContribution(peer, sh, "c.txt", 3, "", "")
	reused := *sh
	reused.CreatedAt = "2099-01-01T00:00:00Z"
	if _, err := s.contribFor(peer, &reused, cb.ID); err == nil {
		t.Fatal("a session carried over to another share with the same id")
	}
	if err := s.AbortContribution(peer, sh, cb.ID); err != nil {
		t.Fatal(err)
	}

	capped := *sh
	capped.ContributeQuotaBytes = 4
	cb, err = s.BeginContribution(peer, &capped, "d.txt", 3, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertFileFedOrigin(store.FileFedOrigin{Path: "Drop/other.bin", PeerID: peer.ID, ShareID: sh.ID,
		InstanceID: peer.InstanceID, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteContribution(peer, &capped, cb.ID); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("landing past the quota: %v", err)
	}
}
