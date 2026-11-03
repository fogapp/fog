package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// Events the hub no longer holds leave holes in its log. A follower steps
// over the holes the hub's answer declares — between events, and at the end
// — rather than wait forever for them, and a hub that answers without ever
// moving the replica forward can't hold the pull in a loop.
func TestSyncStepsOverHolesAndAlwaysEnds(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	replicaOf(t, st, hub, "R", 0)
	fd := testFederation(st, nil)
	ev := func(seq int64) store.ChatEvent {
		return store.ChatEvent{Seq: seq, ID: fmt.Sprintf("e%d", seq), Kind: EventSystem, HubAt: "2026-01-01T00:00:00Z", Payload: []byte("{}")}
	}
	// The hub's log: 1, 2, then 3–4 expired, 5, and 6–7 expired at the end.
	log := []store.ChatEvent{ev(1), ev(2), ev(5)}
	calls := 0
	fd.getRoom = func(_ context.Context, _ *store.FedPeer, path, _ string, out any) (string, error) {
		calls++
		if !strings.Contains(path, "/events") {
			return "", fed.ErrNotModified
		}
		after, _ := strconv.ParseInt(path[strings.Index(path, "after=")+6:strings.Index(path, "&")], 10, 64)
		var evs []store.ChatEvent
		for _, e := range log {
			if e.Seq > after {
				evs = append(evs, e)
			}
		}
		b, _ := json.Marshal(map[string]any{"events": evs, "seq": 7})
		return "", json.Unmarshal(b, out)
	}
	if err := fd.SyncRoom(context.Background(), hub, "R"); err != nil {
		t.Fatal(err)
	}
	r, _ := st.ChatRoom("R")
	if r.Seq != 7 {
		t.Fatalf("the replica stopped at seq %d, want 7 past the holes", r.Seq)
	}
	if evs, _ := st.ChatEventsAfter("R", 0, 10); len(evs) != 3 {
		t.Errorf("the replica holds %d events, want the 3 the hub has", len(evs))
	}

	// A hub that pages forever without moving the replica: the pull ends.
	replicaOf(t, st, hub, "S", 10)
	calls = 0
	fd.getRoom = func(_ context.Context, _ *store.FedPeer, path, _ string, out any) (string, error) {
		calls++
		if !strings.Contains(path, "/events") {
			return "", fed.ErrNotModified
		}
		evs := make([]store.ChatEvent, 500)
		for i := range evs {
			evs[i] = ev(int64(i%10) + 1) // all at or behind the replica
		}
		b, _ := json.Marshal(map[string]any{"events": evs, "seq": 99})
		return "", json.Unmarshal(b, out)
	}
	done := make(chan struct{})
	go func() {
		fd.SyncRoom(context.Background(), hub, "S")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hub that never moves the replica held the pull in a loop")
	}
	if calls > 3 {
		t.Errorf("%d calls for a pull that made no progress", calls)
	}
}

// One notify request's invites are filed by a few workers, not one
// goroutine each; a room is called back about once however many invites
// name it; and a hub's waiting rooms are capped.
func TestInvitesAreFiledWithBoundedWork(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	st.SetFedCapability(hub.ID, CapChat, store.DirGrant, "")
	st.SetFedCapability(hub.ID, store.CapPeopleDirectory, store.DirGrant, `{"people":"members"}`)
	fd := testFederation(st, nil)
	var mu sync.Mutex
	running, most := 0, 0
	perRoom := map[string]int{}
	release := make(chan struct{})
	fd.getRoom = func(_ context.Context, _ *store.FedPeer, path, _ string, out any) (string, error) {
		mu.Lock()
		running++
		most = max(most, running)
		if !strings.Contains(path, "/events") {
			perRoom[strings.TrimPrefix(path, "/fed/v1/chat/rooms/")]++
		}
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return "", fmt.Errorf("unreachable")
	}
	var items []item
	for r := 0; r < inviteMaxRooms+50; r++ {
		id := fmt.Sprintf("R%03d", r)
		for i := 0; i < 5; i++ {
			items = append(items, inviteFor(hub, id, fmt.Sprintf("fog:us/nobody%d", i)))
		}
	}
	fd.Receive(context.Background(), hub, items)
	time.Sleep(50 * time.Millisecond)
	close(release)
	fd.WaitInvites()
	if most > inviteWorkers {
		t.Errorf("%d invite callbacks ran at once, want at most %d", most, inviteWorkers)
	}
	if len(perRoom) > inviteMaxRooms+inviteWorkers {
		t.Errorf("%d rooms called back from one request, want at most about %d", len(perRoom), inviteMaxRooms)
	}
	for room, n := range perRoom {
		if n != 1 {
			t.Errorf("room %s called back %d times for 5 invites", room, n)
		}
	}
}

// Copies of other hubs' blobs are a cache held to QuotaCache: the oldest
// make room, and one too large to fit isn't kept.
func TestCachedBlobsAreHeldToTheirQuota(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	replicaOf(t, st, hub, "R", 0)
	fd := testFederation(st, nil)
	fd.Svc.DataDir = t.TempDir()
	defer func(q int64) { QuotaCache = q }(QuotaCache)
	QuotaCache = 100
	for i, sha := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		st.PutChatBlob(&store.ChatBlob{SHA256: sha, RoomID: "R", Size: 40, UploaderAddr: "hub"})
		st.DB().Exec(`UPDATE chat_blobs SET stored_at = ? WHERE sha256 = ?`, fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1), sha)
	}
	fd.Svc.blobMu.Lock()
	err := fd.Svc.makeCacheRoom(50)
	fd.Svc.blobMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ChatBlob(strings.Repeat("a", 64)); err == nil {
		t.Error("the oldest copy wasn't evicted to make room")
	}
	if _, err := st.ChatBlob(strings.Repeat("b", 64)); err != nil {
		t.Error("a copy that left room enough was evicted")
	}
	fd.Svc.blobMu.Lock()
	err = fd.Svc.makeCacheRoom(101)
	fd.Svc.blobMu.Unlock()
	if !errors.Is(err, ErrQuota) {
		t.Errorf("a copy larger than the cache: %v", err)
	}
}

// One hub that never answers holds up no other, nor the pass: the pass
// returns, and the next skips the hub still being pulled from.
func TestASlowHubHoldsUpNoPoll(t *testing.T) {
	st, _ := testFedStore(t)
	slow, _ := st.CreatePairedPeer("slow", "slow", "http://slow.invalid", "")
	fast, _ := st.CreatePairedPeer("fast", "fast", "http://fast.invalid", "")
	replicaOf(t, st, slow, "S", 0)
	replicaOf(t, st, fast, "F", 0)
	fd := testFederation(st, nil)
	defer func(w time.Duration) { syncPassWait = w }(syncPassWait)
	syncPassWait = 100 * time.Millisecond
	release := make(chan struct{})
	var mu sync.Mutex
	tried := map[string]int{}
	fd.syncRoom = func(ctx context.Context, peer *store.FedPeer, _ string) error {
		mu.Lock()
		tried[peer.InstanceID]++
		mu.Unlock()
		if peer.InstanceID == slow.InstanceID {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	}
	start := time.Now()
	fd.SyncAll(context.Background())
	if time.Since(start) > 5*time.Second {
		t.Fatal("the pass waited on the slow hub")
	}
	fd.SyncAll(context.Background())
	close(release)
	mu.Lock()
	defer mu.Unlock()
	if tried[fast.InstanceID] != 2 {
		t.Errorf("the fast hub was pulled %d times in two passes", tried[fast.InstanceID])
	}
	if tried[slow.InstanceID] != 1 {
		t.Errorf("a hub still being pulled was pulled again: %d", tried[slow.InstanceID])
	}
}

// An archived replica stays archived: its old hub can't revive it with a
// move.
func TestAMoveDoesntReviveAnArchivedReplica(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	next, _ := st.CreatePairedPeer("next", "next", "http://next.invalid", "")
	st.SetFedCapability(next.ID, CapChat, store.DirGrant, "")
	replicaOf(t, st, hub, "R", 0)
	st.SetChatRoomReplicaStatus("R", "archived")
	fd := testFederation(st, nil)
	fd.syncRoom = func(context.Context, *store.FedPeer, string) error { return nil }
	fd.Receive(context.Background(), hub, []item{{Type: "moved", RoomID: "R", Hub: hub.InstanceID, NewHub: next.InstanceID}})
	if r, _ := st.ChatRoom("R"); r.ReplicaStatus != "archived" || r.HubInstance != hub.InstanceID {
		t.Errorf("a move revived an archived replica: %+v", r)
	}
}

// A household's word that its people left reaches a room hubbed here by its
// signature alone — a deleted person is in no directory — and only for its
// own people.
func TestDeparturesAreTheHouseholdsWord(t *testing.T) {
	st, _ := testFedStore(t)
	peer, _ := st.CreatePairedPeer("them", "them", "http://them.invalid", "")
	fd := testFederation(st, nil)
	room := &store.ChatRoom{ID: "R", GroupID: []byte("group-R"), Kind: KindGroup, Policy: DefaultPolicy(KindGroup).JSON(), ReplicaStatus: "live", CreatedBy: "fog:us/op"}
	st.CreateChatRoom(room)
	gone, kept := "fog:"+peer.InstanceID+"/gone", "fog:us/op"
	st.AppendChatEvent(&store.ChatCommit{Event: &store.ChatEvent{RoomID: "R", ID: "c", Kind: EventSystem, Payload: []byte("{}")},
		Added: []store.ChatParticipant{{PersonAddr: kept, HomeInstance: "us", RoleIndex: RoleOwner}, {PersonAddr: gone, HomeInstance: peer.InstanceID, RoleIndex: RoleMember}}})
	items, err := DecodeDepartures([]byte(`{"items":[{"roomId":"R","person":"` + gone + `"},{"roomId":"R","person":"` + kept + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	fd.ReceiveDepartures(peer, items)
	if p, _ := st.ChatParticipant("R", gone); p.LeftSeq == 0 {
		t.Error("a household's departure for its own person, absent from any directory, was ignored")
	}
	if p, _ := st.ChatParticipant("R", kept); p.LeftSeq != 0 {
		t.Error("a household took someone else's person out")
	}
}
