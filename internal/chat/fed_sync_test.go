package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

func testFedStore(t *testing.T) (*store.Store, *vault.Vault) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st, v
}

// replicaOf files a live replica room hubbed by peer, at seq.
func replicaOf(t *testing.T, st *store.Store, peer *store.FedPeer, id string, seq int64) {
	t.Helper()
	if err := st.CreateChatRoom(&store.ChatRoom{
		ID: id, GroupID: []byte("group-" + id), HubInstance: peer.InstanceID, Kind: KindGroup,
		Policy: DefaultPolicy(KindGroup).JSON(), Seq: seq, ReplicaStatus: "live",
	}); err != nil {
		t.Fatal(err)
	}
}

func testFederation(st *store.Store, f *fed.Service) *Federation {
	svc := &Service{Store: st, Log: slog.New(slog.DiscardHandler), Bus: NewBus(), Instance: func() (string, error) { return "us", nil }}
	return NewFederation(svc, f)
}

// A hub that can't be reached costs one attempt a pass, not one per room:
// its other rooms are skipped, another hub's rooms still sync, and the next
// passes leave it alone until its backoff runs out. Once it answers again it
// is off the ladder.
func TestSyncAllBacksOffAnUnreachableHub(t *testing.T) {
	st, _ := testFedStore(t)
	down, err := st.CreatePairedPeer("hub-down", "down", "http://down.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	up, err := st.CreatePairedPeer("hub-up", "up", "http://up.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		replicaOf(t, st, down, fmt.Sprintf("down-%d", i), 1)
		replicaOf(t, st, up, fmt.Sprintf("up-%d", i), 1)
	}
	fd := testFederation(st, nil)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	fd.Now = func() time.Time { return now }

	var mu sync.Mutex
	tried := map[string]int{} // hub → rooms tried
	reachable := false
	fd.syncRoom = func(_ context.Context, peer *store.FedPeer, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		tried[peer.InstanceID]++
		if peer.ID == down.ID && !reachable {
			return hubFailure(errors.New("dial tcp: connection refused"))
		}
		return nil
	}
	pass := func() map[string]int {
		clear(tried)
		fd.SyncAll(context.Background())
		return map[string]int{"down": tried["hub-down"], "up": tried["hub-up"]}
	}

	if got := pass(); got["down"] != 1 || got["up"] != 3 {
		t.Fatalf("first pass tried %v; want the unreachable hub once and every room of the other", got)
	}
	// Within the backoff the hub is left alone entirely.
	now = now.Add(pollEvery - time.Second)
	if got := pass(); got["down"] != 0 || got["up"] != 3 {
		t.Fatalf("pass inside the backoff tried %v", got)
	}
	// Past it, one more attempt; a second failure doubles the wait.
	now = now.Add(2 * time.Second)
	if got := pass(); got["down"] != 1 {
		t.Fatalf("pass after the backoff tried the hub %d times, want 1", got["down"])
	}
	now = now.Add(pollEvery + time.Second)
	if got := pass(); got["down"] != 0 {
		t.Fatalf("the second failure did not lengthen the wait: tried %d", got["down"])
	}
	now = now.Add(pollEvery)
	reachable = true
	if got := pass(); got["down"] != 3 {
		t.Fatalf("a hub answering again synced %d of its 3 rooms", got["down"])
	}
	if _, ok := fd.hubs[down.ID]; ok {
		t.Error("a hub that answered is still on the backoff ladder")
	}

	// A room the hub refuses, by a refusal it signed, is about that room:
	// the hub's other rooms go on.
	reachable = false
	verified := true
	fd.syncRoom = func(_ context.Context, peer *store.FedPeer, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		tried[peer.InstanceID]++
		return hubFailure(&fed.RemoteError{Status: http.StatusForbidden, Code: "forbidden", Verified: verified})
	}
	if got := pass(); got["down"] != 3 || got["up"] != 3 {
		t.Fatalf("refusals stopped the pass early: %v", got)
	}
	// An unsigned "refusal" is nobody's word about the room: the hub is
	// treated as unreachable, once a pass.
	verified = false
	clear(fd.hubs)
	if got := pass(); got["down"] != 1 || got["up"] != 1 {
		t.Fatalf("unsigned refusals were taken as the hub's: %v", got)
	}
}

// translateRemote reads a refusal's code — epoch_stale, forbidden — only
// when the hub signed it.
func TestTranslateRemoteTrustsOnlySignedRefusals(t *testing.T) {
	signed := translateRemote(&fed.RemoteError{Status: http.StatusConflict, Code: "epoch_stale", Verified: true})
	if !errors.Is(signed, ErrEpochStale) {
		t.Errorf("signed epoch_stale: %v", signed)
	}
	unsigned := translateRemote(&fed.RemoteError{Status: http.StatusForbidden, Code: "forbidden"})
	if errors.Is(unsigned, ErrForbidden) || errors.Is(unsigned, ErrEpochStale) {
		t.Errorf("an unsigned refusal was read as the hub's: %v", unsigned)
	}
	var re *RemoteError
	if errors.As(unsigned, &re) {
		t.Errorf("an unsigned refusal's status passed through: %v", re)
	}
}

// A re-sync that hangs on a hub must not hold up the outbox: retries of what
// we owe our followers go out on their own cadence regardless.
func TestLoopFlushesWhileAHubHangs(t *testing.T) {
	st, _ := testFedStore(t)
	hub, err := st.CreatePairedPeer("hub-slow", "slow", "http://slow.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	follower, err := st.CreatePairedPeer("follower", "follower", "http://follower.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	replicaOf(t, st, hub, "slow-room", 1)
	if err := st.EnqueueFedOutbox(follower.ID, OutboxKind, "room", `{"type":"event"}`); err != nil {
		t.Fatal(err)
	}

	defer func(f, p time.Duration) { flushEvery, pollEvery = f, p }(flushEvery, pollEvery)
	pollEvery, flushEvery = 5*time.Millisecond, 20*time.Millisecond

	// No federation service: every delivery fails and is deferred, which is
	// what shows a flush ran.
	fd := testFederation(st, nil)
	syncing := make(chan struct{}, 1)
	fd.syncRoom = func(ctx context.Context, _ *store.FedPeer, _ string) error {
		select {
		case syncing <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fd.Loop(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	select {
	case <-syncing:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-sync never started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var attempts int
		if err := st.DB().QueryRow(`SELECT attempts FROM fed_outbox WHERE peer_id = ?`, follower.ID).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the outbox was never flushed while a re-sync hung on its hub")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A 304 on the snapshot of a replica already as far along as the hub was
// then needs no events fetch; one whose hub seq isn't known, or that is
// behind it, still asks.
func TestSyncRoomNotModifiedSkipsEvents(t *testing.T) {
	st, vlt := testFedStore(t)
	if _, err := fed.EnsureIdentity(st, vlt); err != nil {
		t.Fatal(err)
	}
	hst, hvlt := testFedStore(t)
	hubID, err := fed.EnsureIdentity(hst, hvlt)
	if err != nil {
		t.Fatal(err)
	}
	const tag = `"snap-1"`
	var eventFetches atomic.Int32
	// The hub's fed plane: sealed and signed by fed.SealedHandler, which also
	// answers each new connection's proof.
	srv := httptest.NewServer(fed.SealedHandler(hubID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			eventFetches.Add(1)
			w.Write([]byte(`{"events":[]}`))
		case r.Header.Get("If-None-Match") == tag:
			w.Header().Set("ETag", tag)
			w.WriteHeader(http.StatusNotModified)
		default:
			http.Error(w, "unexpected full snapshot", http.StatusTeapot)
		}
	})))
	defer srv.Close()
	hub, err := st.CreatePairedPeer(hubID.InstanceID, "hub", srv.URL, hubID.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	replicaOf(t, st, hub, "room", 7)
	fd := testFederation(st, &fed.Service{Store: st, Vault: vlt, Log: slog.New(slog.DiscardHandler)})

	for _, c := range []struct {
		hubSeq int64
		fetch  bool
	}{
		{7, false}, // the hub was at our seq: nothing to fetch
		{0, true},  // a tag from before seqs were kept: fetch as before
		{9, true},  // the hub was ahead of us: fetch
	} {
		if err := st.SetChatRoomHubETag("room", tag, c.hubSeq); err != nil {
			t.Fatal(err)
		}
		eventFetches.Store(0)
		if err := fd.SyncRoom(context.Background(), hub, "room"); err != nil {
			t.Fatalf("hub seq %d: %v", c.hubSeq, err)
		}
		if got := eventFetches.Load() > 0; got != c.fetch {
			t.Errorf("hub seq %d, replica at 7: fetched events = %v, want %v", c.hubSeq, got, c.fetch)
		}
	}
}
