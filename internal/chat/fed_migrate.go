// Moving a room's hub to another household (docs/chat-contract.md Phase 6).
//
// The MLS group does not notice: keys live on devices. What moves is the
// ordering role — which fog assigns seq numbers, holds the canonical log and
// blobs, and fans out to followers. The old hub freezes the room, the new hub
// pulls the log to the frozen seq and adopts it, then the old hub becomes a
// follower and tells every other follower where to look. Any step failing
// before adoption unfreezes the room with nothing changed.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// ErrRoomMoving is returned to writers while a hub hand-over is in flight.
var ErrRoomMoving = errors.New("this room is moving to another household; try again in a moment")

const replicaFrozen = "frozen"

// blobCopyTimeout caps the whole background copy after a hand-over.
const blobCopyTimeout = 30 * time.Minute

// Migrate hands a room hubbed here to the household `to`. The actor must be
// allowed to change policy (owner or admin) and `to` must be a paired
// household with a current participant in the room.
func (f *Federation) Migrate(ctx context.Context, actor *Actor, room *store.ChatRoom, to string) error {
	if room.HubInstance != "" {
		return fmt.Errorf("%w: the room is hubbed by another household", ErrForbidden)
	}
	part, err := f.Svc.Participant(room, actor.Addr)
	if err != nil {
		return err
	}
	if !f.Svc.policyOf(room).Can(part.RoleIndex, CapChangePolicy) {
		return ErrForbidden
	}
	if to == "" || to == f.Svc.instanceOrEmpty() {
		return fmt.Errorf("%w: pick another household", ErrBadInput)
	}
	peer, err := fed.PeerForHousehold(f.Svc.Store, to)
	if err != nil || peer.URL == "" {
		return fmt.Errorf("%w: that household is not paired", ErrBadInput)
	}
	if !f.chatGranted(peer) {
		return fmt.Errorf("%w: that household is not granted chat here", ErrBadInput)
	}
	present := false
	for _, fp := range f.followers(room.ID) {
		if fp.InstanceID == to {
			present = true
		}
	}
	if !present {
		return fmt.Errorf("%w: that household has nobody in this room", ErrBadInput)
	}

	// Freeze: no new seq numbers here from now on, by any path (the store
	// refuses appends to a frozen room). Taken under the commit lock, so a
	// commit already being sequenced lands first, and the seq read below is
	// the final one.
	f.Svc.commitMu.Lock()
	err = f.Svc.Store.SetChatRoomHub(room.ID, "", replicaFrozen)
	f.Svc.commitMu.Unlock()
	if err != nil {
		return err
	}
	unfreeze := func() { f.Svc.Store.SetChatRoomHub(room.ID, "", "live") }

	// Push what is queued so the new hub's pull is short, then ask it to
	// adopt at exactly our final seq.
	f.out.FlushPeer(ctx, peer.ID)
	fresh, err := f.Svc.Store.ChatRoom(room.ID)
	if err != nil {
		unfreeze()
		return err
	}
	body, _ := json.Marshal(map[string]any{"seq": fresh.Seq, "epoch": fresh.Epoch, "by": actor.Addr})
	resp, err := f.Fed.SignedPostVerified(ctx, peer, "/fed/v1/chat/rooms/"+room.ID+"/adopt", "application/json", body)
	if err != nil {
		unfreeze()
		return translateRemote(err)
	}
	var ack struct {
		Seq int64 `json:"seq"`
	}
	json.Unmarshal(resp, &ack)
	if ack.Seq < fresh.Seq {
		unfreeze()
		return fmt.Errorf("the new hub only reached seq %d of %d", ack.Seq, fresh.Seq)
	}

	// Adopted. We are a follower now; tell the other followers, then catch
	// up on whatever the new hub has appended (its hub-moved notice). The
	// blobs we hold are kept until the new hub has fetched each.
	f.Svc.Store.HandOffChatBlobs(room.ID)
	if err := f.Svc.Store.SetChatRoomHub(room.ID, to, "live"); err != nil {
		return err
	}
	others := slices.DeleteFunc(f.followers(room.ID), func(fp store.FedPeer) bool { return fp.InstanceID == to })
	f.enqueueAll(others, item{Type: "moved", RoomID: room.ID, Hub: f.Svc.instanceOrEmpty(), NewHub: to})
	f.Svc.Store.Audit(actor.Person.Handle, "chat.room.migrate", room.ID, store.AuditDetail(map[string]any{"to": to}))
	f.SyncRoom(ctx, peer, room.ID)
	for _, p := range f.Svc.localParticipants(room.ID) {
		f.Svc.Bus.PublishPerson(p, Notification{Kind: "rooms", RoomID: room.ID})
	}
	return nil
}

// Adopt is the new hub's side: the room must already be a replica of the
// asking peer. It syncs to the frozen seq, copies the blobs it lacks, and
// becomes the hub, appending a hub-moved notice as its first act.
func (f *Federation) Adopt(ctx context.Context, peer *store.FedPeer, roomID string, seq int64, by string) (int64, error) {
	room, err := f.Svc.Store.ChatRoom(roomID)
	if err != nil || room.HubInstance != peer.InstanceID {
		return 0, fmt.Errorf("%w: not a replica of that household", ErrForbidden)
	}
	// A household doesn't take a room on because another says so: one of
	// this fog's own people in the room, holding canChangePolicy, must have
	// agreed to host it (AcceptMove) within the hour.
	if !f.hasAcceptance(roomID, peer.InstanceID) {
		return 0, fmt.Errorf("%w: nobody here has agreed to host this room", ErrForbidden)
	}
	if err := f.SyncRoom(ctx, peer, roomID); err != nil {
		return 0, fmt.Errorf("sync before adopt: %w", err)
	}
	room, _ = f.Svc.Store.ChatRoom(roomID)
	if room.Seq != seq {
		return room.Seq, fmt.Errorf("replica at seq %d, hub froze at %d", room.Seq, seq)
	}
	// What the old hub hands over is checked against what this fog can see
	// for itself before it is kept.
	if err := f.Svc.vetAdoption(room); err != nil {
		return room.Seq, err
	}
	// The list is cheap and must be read while the old hub still answers as
	// hub; the bytes are not. Copying them inside this request would outlast
	// the old hub's timeout, and it would give up on a hand-over that went on
	// to complete here — two hubs. So adopt now and copy behind the reply.
	missing := f.missingBlobs(ctx, peer, room)
	if err := f.Svc.Store.SetChatRoomHub(roomID, "", "live"); err != nil {
		return room.Seq, err
	}
	// An open-household channel is open to the household that made it. Here
	// "household" would read as this one, opening the channel to people its
	// members never chose and closing it to theirs: it becomes invite-only.
	if pol := f.Svc.policyOf(room); pol.Join == JoinOpenHousehold {
		if home, _, _ := ParseAddr(room.CreatedBy); home != f.Svc.instanceOrEmpty() {
			pol.Join = JoinInvite
			f.Svc.Store.SetChatRoomPolicy(roomID, pol.JSON())
		}
	}
	go f.copyBlobs(peer, roomID, missing)
	room, _ = f.Svc.Store.ChatRoom(roomID)
	payload, _ := json.Marshal(map[string]any{"kind": "hub-moved", "from": peer.InstanceID, "to": f.Svc.instanceOrEmpty(), "by": by})
	f.Svc.appendSystem(room, payload)
	f.Svc.Store.Audit(store.FederationActor(peer.Name), "chat.room.adopt", roomID, store.AuditDetail(map[string]any{"from": peer.InstanceID}))
	// The agreement is used up by the move it was for, and only by one
	// that happened.
	f.movesMu.Lock()
	delete(f.moves, roomID)
	f.movesMu.Unlock()
	room, _ = f.Svc.Store.ChatRoom(roomID)
	for _, p := range f.Svc.localParticipants(room.ID) {
		f.Svc.Bus.PublishPerson(p, Notification{Kind: "rooms", RoomID: room.ID})
	}
	return room.Seq, nil
}

// moveAcceptWindow is how long a member's agreement to host a room stands.
const moveAcceptWindow = time.Hour

// AcceptMove records that actor, one of this fog's people in a room hubbed
// elsewhere and holding canChangePolicy there, agrees to this fog becoming
// its hub. The old hub's adopt request is refused without it.
func (f *Federation) AcceptMove(actor *Actor, room *store.ChatRoom) error {
	if room.HubInstance == "" || room.ReplicaStatus != "live" {
		return fmt.Errorf("%w: this room is not hosted by another household", ErrBadInput)
	}
	part, err := f.Svc.Participant(room, actor.Addr)
	if err != nil {
		return err
	}
	if !f.Svc.policyOf(room).Can(part.RoleIndex, CapChangePolicy) {
		return ErrForbidden
	}
	f.movesMu.Lock()
	defer f.movesMu.Unlock()
	if f.moves == nil {
		f.moves = map[string]moveConsent{}
	}
	f.moves[room.ID] = moveConsent{at: f.Now(), from: room.HubInstance}
	f.Svc.Store.Audit(actor.Person.Handle, "chat.room.accept_move", room.ID, "")
	return nil
}

// takeAcceptance consumes a standing agreement to host a room.
// moveConsent is a member's agreement to host one room, moved from the one
// hub that hosted it when they agreed.
type moveConsent struct {
	at   time.Time
	from string
}

// hasAcceptance says a standing agreement covers a move of roomID offered
// by hub from. It is used up only by an adoption that completes.
func (f *Federation) hasAcceptance(roomID, from string) bool {
	f.movesMu.Lock()
	defer f.movesMu.Unlock()
	c, ok := f.moves[roomID]
	return ok && c.from == from && f.Now().Sub(c.at) < moveAcceptWindow
}

// vetAdoption checks what an old hub handed over against what this fog can
// see: a DM's set is a set of 2-9 people covering everyone who was ever in
// it, and no other kind has one; the creator was a participant; every role
// held exists in the policy; and the verified group state is this group at
// this epoch with the leaves the room's tree holds.
func (s *Service) vetAdoption(room *store.ChatRoom) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: the room handed over doesn't hold together: %s", ErrBadInput, why)
	}
	parts, err := s.Store.ChatParticipants(room.ID)
	if err != nil {
		return err
	}
	pol, err := ParsePolicy(room.Policy)
	if err != nil {
		return bad("its policy")
	}
	creator := false
	for _, p := range parts {
		if p.PersonAddr == room.CreatedBy {
			creator = true
		}
		if !pol.HasRole(p.RoleIndex) {
			return bad("a role its policy lacks")
		}
	}
	if !creator {
		return bad("its creator was never in it")
	}
	if room.Kind == KindDM {
		set := dmMembers(room)
		if len(set) < 2 || len(set) > 9 {
			return bad("its DM set")
		}
		for a := range set {
			if _, _, ok := ParseAddr(a); !ok {
				return bad("its DM set")
			}
		}
		for _, p := range parts {
			if !set[p.PersonAddr] {
				return bad("someone in it outside its DM set")
			}
		}
	} else if room.DMKey != "" {
		return bad("a DM set on a room that isn't a DM")
	}
	if room.PublicState == "" {
		return bad("no verified group state")
	}
	pg, err := mlsview.UnmarshalPublicGroup([]byte(room.PublicState))
	if err != nil {
		return bad("its group state")
	}
	tree, err := s.Tree(room)
	if err != nil {
		return bad("its tree")
	}
	if string(pg.GroupID()) != string(room.GroupID) || int64(pg.Epoch()) != room.Epoch || !sameLeaves(tree.Members(), pg.Members()) {
		return bad("its group state is not this group as it stands")
	}
	return nil
}

// Bounds on what an adopted room's attachment copy may bring in.
const (
	adoptMaxBlobs = 10000
	adoptMaxBytes = int64(20) << 30
)

// missingBlobs lists the attachments the old hub holds that this fog lacks.
// Names that aren't digests are dropped: the list is the peer's word.
func (f *Federation) missingBlobs(ctx context.Context, peer *store.FedPeer, room *store.ChatRoom) []string {
	var res struct {
		Blobs []string `json:"blobs"`
	}
	if _, err := f.Fed.SignedGetJSON(ctx, peer, "/fed/v1/chat/rooms/"+room.ID+"/blobs", "", &res); err != nil {
		f.Svc.Log.Warn("chat fed: blob list for adopt", "room", room.ID, "err", err)
		return nil
	}
	var out []string
	if len(res.Blobs) > adoptMaxBlobs {
		f.Svc.Log.Warn("chat fed: blob list for adopt is longer than this fog takes; the rest are left", "room", room.ID, "listed", len(res.Blobs))
		res.Blobs = res.Blobs[:adoptMaxBlobs]
	}
	seen := map[string]bool{}
	for _, sha := range res.Blobs {
		if seen[sha] {
			continue
		}
		seen[sha] = true
		path, err := f.Svc.blobPath(sha)
		if err != nil {
			f.Svc.Log.Warn("chat fed: blob list for adopt names a non-digest", "room", room.ID, "peer", peer.InstanceID)
			continue
		}
		if _, err := os.Stat(path); err == nil {
			continue
		}
		out = append(out, sha)
	}
	return out
}

// blobCopyFanout bounds how many attachments a new hub pulls at once.
const blobCopyFanout = 4

// copyBlobs pulls the listed attachments from the old hub, now a follower
// that serves its cache to us, so the new hub can serve them to the rest.
// Best effort: a blob that does not arrive is gone.
func (f *Federation) copyBlobs(peer *store.FedPeer, roomID string, shas []string) {
	if len(shas) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), blobCopyTimeout)
	defer cancel()
	var g errgroup.Group
	g.SetLimit(blobCopyFanout)
	var total atomic.Int64
	for _, sha := range shas {
		g.Go(func() error {
			if total.Load() >= adoptMaxBytes {
				return nil
			}
			// fetchBlobFrom checks the digest and the per-blob cap.
			if err := f.fetchBlobFrom(ctx, peer, roomID, sha); err != nil {
				f.Svc.Log.Warn("chat fed: blob copy for adopt", "room", roomID, "sha", sha, "err", err)
				return nil
			}
			if b, err := f.Svc.Store.ChatBlob(sha); err == nil {
				total.Add(b.Size)
			}
			return nil
		})
	}
	g.Wait()
}

// moved is a follower's side of a hand-over the old hub announced.
func (f *Federation) moved(ctx context.Context, peer *store.FedPeer, roomID, newHub string) {
	room, err := f.Svc.Store.ChatRoom(roomID)
	// An archived replica stays archived: a hub can't revive it by moving.
	if err != nil || room.HubInstance != peer.InstanceID || room.ReplicaStatus == "archived" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, syncRoomBudget)
	defer cancel()
	next, err := fed.PeerForHousehold(f.Svc.Store, newHub)
	if err != nil || !f.chatGranted(next) {
		// A hub we are not paired with, or haven't granted chat: the old
		// hub doesn't get to choose who our people's room lives with, so
		// the room is out of reach now. Archived, never followed.
		f.Svc.Store.SetChatRoomReplicaStatus(roomID, "archived")
		f.Svc.Log.Warn("chat fed: a hub moved a room to a household not granted chat here; replica archived", "room", roomID, "to", newHub)
		for _, p := range f.Svc.localParticipants(room.ID) {
			f.Svc.Bus.PublishPerson(p, Notification{Kind: "rooms", RoomID: roomID})
		}
		return
	}
	if err := f.Svc.Store.SetChatRoomHub(roomID, newHub, "live"); err != nil {
		return
	}
	if err := f.syncRoom(ctx, next, roomID); err != nil {
		f.Svc.Log.Warn("chat fed: sync after hub move", "room", roomID, "err", err)
	}
	for _, p := range f.Svc.localParticipants(room.ID) {
		f.Svc.Bus.PublishPerson(p, Notification{Kind: "rooms", RoomID: roomID})
	}
}

// localParticipants lists the addresses of this household's current members.
func (s *Service) localParticipants(roomID string) []string {
	parts, _ := s.Store.ChatParticipants(roomID)
	me := s.instanceOrEmpty()
	var out []string
	for _, p := range parts {
		if p.LeftSeq == 0 && p.HomeInstance == me {
			out = append(out, p.PersonAddr)
		}
	}
	return out
}

// appendSystem sequences a system event on a room hubbed here.
func (s *Service) appendSystem(room *store.ChatRoom, payload []byte) {
	id, _ := fed.NewULID()
	c := &store.ChatCommit{Event: &store.ChatEvent{RoomID: room.ID, ID: id, Epoch: room.Epoch, Kind: EventSystem, Payload: payload}}
	s.commitMu.Lock()
	err := s.Store.AppendChatEvent(c)
	s.commitMu.Unlock()
	if err != nil {
		s.Log.Warn("chat: system event", "room", room.ID, "err", err)
		return
	}
	fresh, _ := s.Store.ChatRoom(room.ID)
	s.announce(fresh, c.Event)
}

// chatGranted says this fog has granted peer the chat capability.
func (f *Federation) chatGranted(peer *store.FedPeer) bool {
	_, granted, _ := f.Svc.Store.FedCapabilityConfig(peer.ID, CapChat, store.DirGrant)
	return granted
}
