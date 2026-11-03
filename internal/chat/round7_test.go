package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

func uploadRoom(t *testing.T) (*store.Store, *Service, *store.ChatRoom) {
	t.Helper()
	st, _ := testFedStore(t)
	svc := &Service{Store: st, Log: slog.New(slog.DiscardHandler), Bus: NewBus(), DataDir: t.TempDir(), Instance: func() (string, error) { return "us", nil }}
	room := &store.ChatRoom{ID: "R", GroupID: []byte("group-R"), Kind: KindGroup, Policy: DefaultPolicy(KindGroup).JSON(), ReplicaStatus: "live", CreatedBy: "fog:us/eve"}
	st.CreateChatRoom(room)
	var added []store.ChatParticipant
	for _, a := range []string{"fog:us/eve", "fog:them/mal", "fog:them/mal2"} {
		home, _, _ := ParseAddr(a)
		added = append(added, store.ChatParticipant{PersonAddr: a, HomeInstance: home, RoleIndex: RoleMember})
	}
	st.AppendChatEvent(&store.ChatCommit{Event: &store.ChatEvent{RoomID: "R", ID: "c", Kind: EventSystem, Payload: []byte("{}")}, Added: added})
	return st, svc, room
}

// One person, or one household, can't hold the shared pool of uploads
// under way; an empty piece doesn't keep a reservation alive; and an upload
// its sender gives up on releases what it reserved at once.
func TestUploadReservationsAreBounded(t *testing.T) {
	_, svc, room := uploadRoom(t)
	defer func(a, p, h int64) { QuotaUploads, QuotaUploadsPerPerson, QuotaUploadsPerHousehold = a, p, h }(QuotaUploads, QuotaUploadsPerPerson, QuotaUploadsPerHousehold)
	QuotaUploads, QuotaUploadsPerPerson, QuotaUploadsPerHousehold = 1000, 100, 150
	mal := &Actor{Person: &store.Person{Handle: "mal"}, Addr: "fog:them/mal"}
	mal2 := &Actor{Person: &store.Person{Handle: "mal2"}, Addr: "fog:them/mal2"}
	eve := &Actor{Person: &store.Person{Handle: "eve"}, Addr: "fog:us/eve"}
	if _, _, err := svc.PutBlobChunk(mal, room, "a", 0, 100, bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.PutBlobChunk(mal, room, "b", 0, 10, bytes.NewReader(make([]byte, 1))); !errors.Is(err, ErrQuota) {
		t.Errorf("one person past their share of uploads under way: %v", err)
	}
	if _, _, err := svc.PutBlobChunk(mal2, room, "c", 0, 60, bytes.NewReader(make([]byte, 1))); !errors.Is(err, ErrQuota) {
		t.Errorf("one household past its share: %v", err)
	}
	if _, _, err := svc.PutBlobChunk(eve, room, "d", 0, 100, bytes.NewReader(make([]byte, 1))); err != nil {
		t.Errorf("another household was crowded out: %v", err)
	}
	// An empty piece keeps nothing alive.
	key := ""
	for k, r := range svc.reserved {
		if r.addr == mal.Addr {
			key = k
		}
	}
	svc.reserved[key].at = time.Now().Add(-time.Hour + time.Second)
	before := svc.reserved[key].at
	svc.PutBlobChunk(mal, room, "a", 10, 100, bytes.NewReader(nil))
	if r := svc.reserved[key]; r != nil && !r.at.Equal(before) {
		t.Error("an empty piece extended the reservation")
	}
	// Abort: released at once.
	svc.AbortBlobChunks(mal, room, "a")
	if _, ok := svc.reserved[key]; ok {
		t.Error("an aborted upload kept its reservation")
	}
}

// Whether a handle is someone's, someone hidden's, or no one's, checking it
// is the same work: one lookup, one set check, against a snapshot of what
// the hub is shown read once per job.
func TestInviteVisibilityCostsTheSame(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	st.SetFedCapability(hub.ID, store.CapPeopleDirectory, store.DirGrant, `{"people":["eve"]}`)
	st.CreatePerson("eve", "Eve", store.PersonRoleMember)
	st.CreatePerson("zed", "Zed", store.PersonRoleMember)
	fd := testFederation(st, nil)
	shown := fd.shownTo(hub)
	measure := func(handle string) time.Duration {
		const n = 300
		start := time.Now()
		for range n {
			fd.visibleIn(shown, handle)
		}
		return time.Since(start) / n
	}
	for range 2 { // warm
		measure("zed")
		measure("nobody")
	}
	hidden, unknown := measure("zed"), measure("nobody")
	ratio := float64(hidden) / float64(unknown)
	if ratio > 3 || ratio < 1.0/3 {
		t.Errorf("hidden %v vs unknown %v per check: the timings tell them apart", hidden, unknown)
	}
	if !fd.visibleIn(shown, "eve") || fd.visibleIn(shown, "zed") || fd.visibleIn(shown, "nobody") {
		t.Error("visibility")
	}
}

// Watched rooms — held for invites nobody accepted, real or decoy — are
// capped per hub; past the cap every new one is refused alike, whoever it
// names.
func TestWatchedRoomsAreCapped(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	st.SetFedCapability(hub.ID, CapChat, store.DirGrant, "")
	st.SetFedCapability(hub.ID, store.CapPeopleDirectory, store.DirGrant, `{"people":["eve"]}`)
	st.CreatePerson("eve", "Eve", store.PersonRoleMember)
	fd := testFederation(st, nil)
	fd.syncRoom = func(context.Context, *store.FedPeer, string) error { return nil }
	ctx := context.Background()
	for i := 0; i < watchPerHub; i++ {
		fd.Receive(ctx, hub, []item{inviteFor(hub, fmt.Sprintf("W%03d", i), "fog:us/nobody")})
		fd.WaitInvites()
	}
	for _, who := range []string{"fog:us/eve", "fog:us/nobody"} {
		refused := fd.Receive(ctx, hub, []item{inviteFor(hub, "OVER-"+who[7:], who)})
		fd.WaitInvites()
		if len(refused) != 1 || refused[0]["code"] != "invites_full" {
			t.Errorf("%s past the cap: %v", who, refused)
		}
	}
}

// A room frozen for a hand-over sequences nothing — not a departure, not a
// room change — and a departure for a room that moved is pointed at its new
// hub rather than acknowledged.
func TestAFrozenRoomSequencesNothing(t *testing.T) {
	st, svc, room := uploadRoom(t)
	st.SetChatRoomHub("R", "", replicaFrozen)
	room, _ = st.ChatRoom("R")
	seq := room.Seq
	if err := svc.MarkLeft(room, []string{"fog:us/eve"}, SystemBanned, "x"); !errors.Is(err, ErrRoomMoving) {
		t.Errorf("a departure in a frozen room: %v", err)
	}
	eve := &Actor{Person: &store.Person{Handle: "eve"}, Addr: "fog:us/eve"}
	label := "frozen?"
	if err := svc.UpdateRoom(eve, room, RoomPatch{Label: &label}); !errors.Is(err, ErrRoomMoving) {
		t.Errorf("a room change in a frozen room: %v", err)
	}
	svc.appendSystem(room, []byte(`{"kind":"x"}`))
	if r, _ := st.ChatRoom("R"); r.Seq != seq {
		t.Errorf("a frozen room's seq moved: %d → %d", seq, r.Seq)
	}
	// The departures endpoint: frozen → retry; moved → redirected.
	peer, _ := st.CreatePairedPeer("them", "them", "http://them.invalid", "")
	fd := NewFederation(svc, nil)
	items := []departure{{RoomID: "R", Person: "fog:" + peer.InstanceID + "/mal"}}
	if _, retry := fd.ReceiveDepartures(peer, items); len(retry) != 1 {
		t.Errorf("a departure for a frozen room was acknowledged: retry %v", retry)
	}
	st.SetChatRoomHub("R", "elsewhere", "live")
	moved, _ := fd.ReceiveDepartures(peer, items)
	if len(moved) != 1 || moved[0]["hub"] != "elsewhere" {
		t.Errorf("a departure for a room that moved wasn't pointed on: %v", moved)
	}
}

// Invites a hub sent before it was cut are never filed after.
func TestInvitesQueuedBeforeACutAreDropped(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	st.SetFedCapability(hub.ID, CapChat, store.DirGrant, "")
	st.SetFedCapability(hub.ID, store.CapPeopleDirectory, store.DirGrant, `{"people":"members"}`)
	st.CreatePerson("eve", "Eve", store.PersonRoleMember)
	fd := testFederation(st, nil)
	fd.syncRoom = func(context.Context, *store.FedPeer, string) error { return nil }
	// Queue without workers running, as a backlog would be.
	fd.inviteMu.Lock()
	fd.inviteQs = map[int64]*inviteQueue{hub.ID: {peer: hub, pending: map[string][]item{}, weight: map[string]int64{}, workers: inviteWorkers}}
	fd.enqueueInviteLocked(hub, inviteFor(hub, "R1", "fog:us/eve"))
	fd.inviteMu.Unlock()
	fd.PeerCut(hub, "cut")
	if q := fd.inviteQs[hub.ID]; len(q.order) != 0 {
		t.Error("a cut hub's queued invites survived")
	}
	// And a hub whose grant was revoked gets nothing filed.
	st.SetFedCapability(hub.ID, CapChat, store.DirGrant, "off")
	st.DB().Exec(`DELETE FROM fed_capabilities WHERE peer_id = ? AND capability = ?`, hub.ID, CapChat)
	fd.fileInvites(context.Background(), hub, "R2", []item{inviteFor(hub, "R2", "fog:us/eve")})
	if invs, _ := st.ChatInvitesFor("fog:us/eve"); len(invs) != 0 {
		t.Errorf("an invite was filed for a hub no longer granted chat: %v", invs)
	}
}

// A person who is a member keeps the room polled, whatever other invites
// (another device's) are still pending; and dropping decoys never takes a
// room any of our people were in.
func TestMembersKeepTheirRoomPolled(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	replicaOf(t, st, hub, "R", 0)
	fd := testFederation(st, nil)
	st.AppendChatEvent(&store.ChatCommit{Event: &store.ChatEvent{RoomID: "R", ID: "c", Kind: EventSystem, Payload: []byte("{}")},
		Added: []store.ChatParticipant{{PersonAddr: "fog:us/eve", HomeInstance: "us", RoleIndex: RoleMember}}})
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	st.AddChatInvite(&store.ChatInvite{RoomID: "R", PersonAddr: "fog:us/eve", InvitedBy: "x", Welcome: []byte("w"), DeviceKey: "phone", CreatedAt: old})
	st.AddChatInvite(&store.ChatInvite{RoomID: "R", PersonAddr: "fog:us/eve", InvitedBy: "x", Welcome: []byte("w"), DeviceKey: "tablet", CreatedAt: old})
	invs, _ := st.ChatRoomInvites("R")
	st.DB().Exec(`UPDATE chat_invites SET accepted_at = ? WHERE id = ?`, old, invs[0].ID)
	room, _ := st.ChatRoom("R")
	if poll, _ := fd.watch(room); !poll {
		t.Error("a member's room stopped being polled because another device's invite is pending")
	}
	// A room with a decoy invite and one of our people in it isn't a decoy.
	st.AddChatInvite(&store.ChatInvite{RoomID: "R", PersonAddr: decoyPrefix + "R", InvitedBy: "x", Welcome: []byte{}, CreatedAt: old})
	st.DB().Exec(`DELETE FROM chat_invites WHERE room_id = 'R' AND person_addr = 'fog:us/eve'`)
	st.DeleteChatDecoy("R", "us")
	if _, err := st.ChatRoom("R"); err != nil {
		t.Error("dropping a decoy took a room one of our people was in")
	}
}

// After a hand-over, the old hub keeps its copies until the new hub has
// fetched them; only then are they a cache to evict.
func TestHandedOffBlobsAreKeptUntilFetched(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	replicaOf(t, st, hub, "R", 0)
	fd := testFederation(st, nil)
	fd.Svc.DataDir = t.TempDir()
	sha := strings.Repeat("c", 64)
	st.PutChatBlob(&store.ChatBlob{SHA256: sha, RoomID: "R", Size: 40, UploaderAddr: "hub"})
	st.HandOffChatBlobs("R")
	defer func(q int64) { QuotaCache = q }(QuotaCache)
	QuotaCache = 50
	fd.Svc.blobMu.Lock()
	fd.Svc.makeCacheRoom(30)
	fd.Svc.blobMu.Unlock()
	if _, err := st.ChatBlob(sha); err != nil {
		t.Fatal("a handed-off blob was evicted before the new hub had it")
	}
	st.ConfirmChatBlobHandedOff(sha)
	fd.Svc.blobMu.Lock()
	fd.Svc.makeCacheRoom(30)
	fd.Svc.blobMu.Unlock()
	if _, err := st.ChatBlob(sha); err == nil {
		t.Error("a confirmed copy is still not a cache")
	}
}

// A hub's owed departures are never dropped to make room: past the cap a
// new one is refused, loudly, and the old ones stay.
func TestDeparturesAreNeverDroppedSilently(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	defer func(c int) { store.ChatDepartureCap = c }(store.ChatDepartureCap)
	store.ChatDepartureCap = 2
	for i := 0; i < 2; i++ {
		if err := st.EnqueueChatDeparture(hub.ID, DepartureKind, "R", fmt.Sprintf(`{"n":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EnqueueChatDeparture(hub.ID, DepartureKind, "R", `{"n":3}`); !errors.Is(err, store.ErrDepartureQueueFull) {
		t.Errorf("past the cap: %v", err)
	}
	var first string
	st.DB().QueryRow(`SELECT payload FROM fed_outbox WHERE kind = ? ORDER BY id LIMIT 1`, DepartureKind).Scan(&first)
	if first != `{"n":0}` {
		t.Errorf("an owed departure was dropped: oldest now %s", first)
	}
}

// The watch cap counts only rooms in their watch window that none of our
// people has accepted — acceptance kept past the prune of old invites — and
// caps each inviting person on their own.
func TestTheWatchCapCountsOnlyLiveWatches(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	fd := testFederation(st, nil)
	now := time.Now().UTC()
	mk := func(id, by string, created time.Time, accepted bool) {
		replicaOf(t, st, hub, id, 0)
		st.AddChatInvite(&store.ChatInvite{RoomID: id, PersonAddr: "fog:us/eve", InvitedBy: by, Welcome: []byte("w"), CreatedAt: created.Format(time.RFC3339)})
		if accepted {
			st.AddChatInvite(&store.ChatInvite{RoomID: id, PersonAddr: "fog:us/eve", InvitedBy: by, Welcome: []byte("w"), DeviceKey: "phone", CreatedAt: created.Format(time.RFC3339)})
			invs, _ := st.ChatRoomInvites(id)
			st.DB().Exec(`UPDATE chat_invites SET accepted_at = ? WHERE id = ?`, now.Add(-60*24*time.Hour).Format(time.RFC3339), invs[len(invs)-1].ID)
		}
	}
	mk("OLD", "fog:hub/a", now.Add(-30*24*time.Hour), false)
	mk("ACC", "fog:hub/a", now, true)
	mk("LIVE", "fog:hub/a", now, false)
	st.PruneChatInvites(now.Add(-30 * 24 * time.Hour).Format(time.RFC3339))
	byHub, _, _, err := st.ChatWatchedRooms(hub.InstanceID, "fog:hub/a", now.Add(-inviteWatch).Format(time.RFC3339))
	if err != nil || byHub != 1 {
		t.Errorf("watched rooms: %d (%v), want only the live one", byHub, err)
	}
	// One inviter can't fill the household's cap.
	for i := 0; i < watchPerInviter; i++ {
		mk(fmt.Sprintf("M%d", i), "fog:hub/mal", now, false)
	}
	if !fd.watchFull(hub, "NEXT", "fog:hub/mal") {
		t.Error("one inviting person went past their own cap")
	}
	if fd.watchFull(hub, "NEXT", "fog:hub/someone-else") {
		t.Error("one person's invites filled everyone's cap")
	}
}

// A follower answering invites_full hasn't refused anyone: the hub keeps
// the invitee in the room, and sends the invite again later.
func TestHubsDontTakeAFullFollowerAsARefusal(t *testing.T) {
	st, _ := testFedStore(t)
	peer, _ := st.CreatePairedPeer("them", "them", "http://them.invalid", "")
	fd := testFederation(st, nil)
	room := &store.ChatRoom{ID: "R", GroupID: []byte("group-R"), Kind: KindGroup, Policy: DefaultPolicy(KindGroup).JSON(), ReplicaStatus: "live", CreatedBy: "fog:us/op"}
	st.CreateChatRoom(room)
	eve := "fog:" + peer.InstanceID + "/eve"
	st.AppendChatEvent(&store.ChatCommit{Event: &store.ChatEvent{RoomID: "R", ID: "c", Kind: EventSystem, Payload: []byte("{}")},
		Added: []store.ChatParticipant{{PersonAddr: eve, HomeInstance: peer.InstanceID, RoleIndex: RoleMember}}})
	b, _ := json.Marshal(inviteFor(&store.FedPeer{InstanceID: ""}, "R", eve))
	st.EnqueueFedOutbox(peer.ID, OutboxKind, "R", string(b))
	src := fd.out.Source
	rows, _ := src.Due(peer.ID, 10)
	answer := []byte(`{"ok":true,"refused":[{"roomId":"R","person":"` + eve + `","code":"invites_full"}]}`)
	if err := src.Settle(peer, rows, answer); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.ChatParticipant("R", eve); p.LeftSeq != 0 {
		t.Error("a full follower's answer took the invitee out of the room")
	}
	var n int
	st.DB().QueryRow(`SELECT COUNT(*) FROM fed_outbox WHERE kind = ?`, OutboxKind).Scan(&n)
	if n != 1 {
		t.Errorf("the invite wasn't kept to send again: %d rows", n)
	}
}

// A departure the hub answers "moved" for is held until this fog has
// followed the move — never dropped.
func TestAMovedDepartureIsHeldNotDropped(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	replicaOf(t, st, hub, "R", 0)
	fd := testFederation(st, nil)
	b, _ := json.Marshal(departure{RoomID: "R", Person: "fog:us/eve"})
	st.EnqueueChatDeparture(hub.ID, DepartureKind, "R", string(b))
	src := fd.departures.Source
	rows, _ := src.Due(hub.ID, 10)
	answer := []byte(`{"ok":true,"moved":[{"roomId":"R","person":"fog:us/eve","hub":"newhub"}]}`)
	if err := src.Settle(hub, rows, answer); err != nil {
		t.Fatal(err)
	}
	var n int
	st.DB().QueryRow(`SELECT COUNT(*) FROM fed_outbox WHERE kind = ?`, DepartureKind).Scan(&n)
	if n != 1 {
		t.Errorf("a departure for a room whose move this fog hasn't followed was dropped: %d rows", n)
	}
}

// Unpairing during a hand-over takes the household's people out once the
// move settles, as a reset does.
func TestACutDuringAMoveAppliesAfterIt(t *testing.T) {
	st, svc, room := uploadRoom(t)
	peer, _ := st.CreatePairedPeer("them", "them", "http://them.invalid", "")
	fd := NewFederation(svc, nil)
	svc.Fed = fd
	st.SetChatRoomHub("R", "", replicaFrozen)
	fd.PeerCut(peer, "unpaired")
	if p, _ := st.ChatParticipant("R", "fog:them/mal"); p.LeftSeq != 0 {
		t.Fatal("a frozen room sequenced a cut")
	}
	st.SetChatRoomHub("R", "", "live")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, _ := st.ChatParticipant("R", "fog:them/mal"); p.LeftSeq != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cut never applied after the move settled")
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = room
}

// A hub can't hand a follower's people something that isn't what it says:
// an "mls_commit" that isn't a commit of the room's group is kept only as a
// note that the hub sent something bad.
func TestReplicasKeepOnlySoundMLSEvents(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	replicaOf(t, st, hub, "R", 0)
	fd := testFederation(st, nil)
	fd.getRoom = func(_ context.Context, _ *store.FedPeer, path, _ string, out any) (string, error) {
		if !strings.Contains(path, "/events") {
			return "", fed.ErrNotModified
		}
		evs := []store.ChatEvent{
			{Seq: 1, ID: "x", Kind: EventCommit, Epoch: 0, HubAt: "2026-01-01T00:00:00Z", Payload: []byte("not a commit at all")},
			{Seq: 2, ID: "y", Kind: EventPrivate, Epoch: 0, HubAt: "2026-01-01T00:00:00Z", Payload: []byte{0, 1, 2, 3}},
		}
		b, _ := json.Marshal(map[string]any{"events": evs, "seq": 2})
		return "", json.Unmarshal(b, out)
	}
	if err := fd.SyncRoom(context.Background(), hub, "R"); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.ChatEventsAfter("R", 0, 10)
	if len(evs) != 2 {
		t.Fatalf("the replica holds %d events, want both seqs kept", len(evs))
	}
	for _, e := range evs {
		if e.Kind != EventSystem || !strings.Contains(string(e.Payload), "hub-fault") {
			t.Errorf("seq %d: a mislabelled event was stored as %s %q", e.Seq, e.Kind, e.Payload)
		}
	}
}
