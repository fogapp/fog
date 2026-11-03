// Chat across households (docs/chat-contract.md): the hub fans events
// out to every follower fog with a participant in the room, through the
// federation outbox; a follower keeps a replica of the ciphertext log for
// its people, relays their submissions to the hub, and pulls what it missed.
// Pull is the truth; push is a nudge. Nothing here ever holds a key.
package chat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thomas-vilte/mls-go/framing"

	"gitea.lan/matthew/fog/internal/backoff"
	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// CapChat is the federation capability: granted, the peer's people may
	// be in rooms hubbed here, its fog may follow them and fetch key
	// material; used, it names which of our people we advertise.
	CapChat = "chat"
	// Contract is the app-facing name of this wire.
	Contract = "fog-chat/1"

	OutboxKind = "chat"
	// coalesce bounds how long a fan-out waits for more events to batch.
	coalesce    = 250 * time.Millisecond
	notifyBatch = 200
	notifyBytes = 2 << 20
	// hubBackoffMax caps how long an unreachable hub's rooms wait between
	// re-sync attempts.
	hubBackoffMax = time.Hour
)

// The loop's cadences, variables so a test can run them fast: the outbox's
// retry sweep, and the follower's idle re-sync of its replicas.
var (
	flushEvery = 15 * time.Second
	pollEvery  = 5 * time.Minute
)

// GrantConfig is the grant-side configuration of the chat capability.
type GrantConfig struct {
	// AcceptThirdHouseholds allows rooms hubbed by this peer that contain
	// people from households we are not paired with. Default true.
	AcceptThirdHouseholds *bool `json:"acceptThirdHouseholds,omitempty"`
}

func (g GrantConfig) acceptsThird() bool {
	return g.AcceptThirdHouseholds == nil || *g.AcceptThirdHouseholds
}

// UseConfig is the people.directory grant's configuration: which of our
// people the peer sees — {"people": "members"} (or "all") for every active
// member, or a list of handles.
type UseConfig struct {
	People json.RawMessage `json:"people,omitempty"`
}

// allows fails closed: a grant whose people are missing, unreadable or
// anything but "members", "all" or a list shows nobody. Pairing writes
// {"people": "members"} explicitly, so a sound grant never reads empty.
func (u UseConfig) allows(handle string) bool {
	if len(u.People) == 0 {
		return false
	}
	var all string
	if json.Unmarshal(u.People, &all) == nil {
		return all == "all" || all == "members"
	}
	var list []string
	if json.Unmarshal(u.People, &list) == nil {
		for _, h := range list {
			if h == handle {
				return true
			}
		}
	}
	return false
}

// Federation is the cross-household half of the chat service.
type Federation struct {
	Svc *Service
	Fed *fed.Service
	// Now is swappable for tests.
	Now func() time.Time

	out *fed.Outbox[string]
	// departures is the durable queue of our people's departures (an
	// identity reset, a deletion) owed to the hubs of rooms they were in:
	// retried until the hub acknowledges (QueueDeparture).
	departures *fed.Outbox[string]
	// syncRoom is SyncRoom, swappable so a test can stand in for the hubs.
	syncRoom func(ctx context.Context, peer *store.FedPeer, roomID string) error
	// hubs is each hub's re-sync backoff by peer id, and syncing the hubs a
	// poll is still pulling from; both under hubsMu.
	hubsMu  sync.Mutex
	hubs    map[int64]hubBackoff
	syncing map[int64]bool
	// typing limits typing signals crossing to other households to one per
	// person and room every typingEvery, whatever a client sends.
	typing notify.Throttle
	// moves holds this fog's people's standing agreements to host a room
	// hubbed elsewhere (AcceptMove), by room id.
	movesMu sync.Mutex
	moves   map[string]moveConsent
	// invites counts invite workers running behind their answers; inviteQs
	// is each hub's queue of rooms with invites to file (inviteQueue).
	invites     sync.WaitGroup
	inviteMu    sync.Mutex
	inviteQs    map[int64]*inviteQueue
	inviteBytes int64
	// getRoom is how a follower reads a hub's room plane (snapshot, log):
	// the fed client, swappable so a test can see every call a hub sees.
	getRoom func(ctx context.Context, peer *store.FedPeer, path, ifNoneMatch string, out any) (string, error)
}

// getRoomJSON reads one of a hub's room routes.
func (f *Federation) getRoomJSON(ctx context.Context, peer *store.FedPeer, path, ifNoneMatch string, out any) (string, error) {
	if f.getRoom != nil {
		return f.getRoom(ctx, peer, path, ifNoneMatch, out)
	}
	if f.Fed == nil {
		return "", errors.New("chat fed: no federation client")
	}
	return f.Fed.SignedGetJSON(ctx, peer, path, ifNoneMatch, out)
}

// typingEvery matches the web client's own throttle; a client that sends
// more often (a native one without it) is held to the same pace here.
const typingEvery = 3 * time.Second

func NewFederation(svc *Service, f *fed.Service) *Federation {
	fd := &Federation{Svc: svc, Fed: f, Now: time.Now, hubs: map[int64]hubBackoff{}}
	fd.syncRoom = fd.SyncRoom
	fd.out = &fed.Outbox[string]{
		Store: svc.Store, Fed: f, Source: notifySource{fd, fed.OutboxQueue{Store: svc.Store, Kind: OutboxKind}}, Log: svc.Log,
		Kind: OutboxKind, Path: "/fed/v1/chat/notify", ContentType: "application/json",
		BatchRows: notifyBatch, BatchBytes: notifyBytes, RowMax: MaxPayload * 2,
		// A follower we can't dial pulls what it missed; its rows wait for
		// an address rather than backing off.
		HoldUnaddressed: true,
		Now:             func() time.Time { return fd.Now() },
	}
	fd.departures = &fed.Outbox[string]{
		Store: svc.Store, Fed: f, Source: departureSource{fed.OutboxQueue{Store: svc.Store, Kind: DepartureKind}, fd}, Log: svc.Log,
		Kind: DepartureKind, Path: "/fed/v1/chat/departures", ContentType: "application/json",
		BatchRows: notifyBatch, BatchBytes: 1 << 20, RowMax: 4 << 10,
		HoldUnaddressed: true,
		Now:             func() time.Time { return fd.Now() },
	}
	return fd
}

// DepartureKind is the outbox kind of departures owed to rooms' hubs.
const DepartureKind = "chat-departure"

// departure is one of our people's departures from a room hubbed elsewhere.
type departure struct {
	RoomID string `json:"roomId"`
	Person string `json:"person"`
}

// QueueDeparture owes a room's hub word that one of our people left it —
// their identity was reset, or they were deleted — and keeps owing it,
// across restarts and the hub being away, until the hub acknowledges.
func (f *Federation) QueueDeparture(addr string, room *store.ChatRoom) {
	peer, err := f.hubPeer(room)
	if err != nil {
		return
	}
	b, _ := json.Marshal(departure{RoomID: room.ID, Person: addr})
	if err := f.Svc.Store.EnqueueChatDeparture(peer.ID, DepartureKind, room.ID, string(b)); err != nil {
		// Said loudly: this hub won't hear that this person left.
		f.Svc.Log.Error("chat fed: a departure could not be queued for the room's hub", "room", room.ID, "person", addr, "hub", peer.Name, "err", err)
		return
	}
	f.departures.Kick(peer.ID, coalesce)
}

// departureSource is the departure queue as the drainer sees it. The hub
// answers once it has applied the batch; any answer it signed settles it.
type departureSource struct {
	fed.OutboxQueue
	f *Federation
}

func (d departureSource) Encode(_ *store.FedPeer, batch []fed.OutboxRow[string]) ([]byte, error) {
	items := make([]json.RawMessage, len(batch))
	for i, r := range batch {
		items[i] = json.RawMessage(r.Payload)
	}
	return json.Marshal(map[string]any{"items": items})
}

// Settle reads what the hub couldn't apply: a departure from a room that
// moved on goes to its new hub instead; one from a room frozen mid-move
// stays owed (the batch is retried).
func (d departureSource) Settle(_ *store.FedPeer, batch []fed.OutboxRow[string], answer []byte) error {
	var ack struct {
		Moved []struct {
			RoomID string `json:"roomId"`
			Person string `json:"person"`
			Hub    string `json:"hub"`
		} `json:"moved"`
		Retry []departure `json:"retry"`
	}
	if err := json.Unmarshal(answer, &ack); err != nil {
		return fmt.Errorf("%w: %v", fed.ErrBadAnswer, err)
	}
	if len(ack.Retry) > 0 {
		return fmt.Errorf("%w: the hub is mid-move", fed.ErrBadAnswer)
	}
	// A room that moved: once this fog has followed the move to the hub the
	// answer names (verified by that hub's own moved notice), the departure
	// is owed there; until then it stays owed here, and is asked again.
	rowOf := map[[2]string]int64{}
	for _, r := range batch {
		var dep departure
		if json.Unmarshal([]byte(r.Payload), &dep) == nil {
			rowOf[[2]string{dep.RoomID, dep.Person}] = r.ID
		}
	}
	var later []int64
	for _, m := range ack.Moved {
		row, ok := rowOf[[2]string{m.RoomID, m.Person}]
		if !ok || d.f == nil {
			continue
		}
		if room, err := d.f.Svc.Store.ChatRoom(m.RoomID); err == nil && room.HubInstance == m.Hub && m.Hub != "" {
			d.f.QueueDeparture(m.Person, room)
			continue
		}
		later = append(later, row)
	}
	if len(later) == 0 {
		return d.Delete(batch)
	}
	keep := map[int64]bool{}
	for _, id := range later {
		keep[id] = true
	}
	var done []fed.OutboxRow[string]
	for _, r := range batch {
		if !keep[r.ID] {
			done = append(done, r)
		}
	}
	if err := d.Defer(0, later, time.Now().Add(5*time.Minute), nil); err != nil {
		return err
	}
	return d.Delete(done)
}

// ReceiveDepartures is the hub's side: a household's word that its people
// left rooms hubbed here. The household's signature is the authority — the
// person need not be in its directory any more (a deleted one isn't) — and
// it speaks only for its own people.
//
// It answers what it couldn't apply: a room moved on (to its new hub, for
// the household to tell instead), or frozen mid-move (to try again).
func (f *Federation) ReceiveDepartures(peer *store.FedPeer, items []departure) (moved []map[string]string, retry []departure) {
	for _, it := range items {
		home, _, ok := ParseAddr(it.Person)
		if !ok || home != peer.InstanceID {
			continue
		}
		room, err := f.Svc.Store.ChatRoom(it.RoomID)
		if err != nil {
			continue
		}
		if room.HubInstance != "" {
			moved = append(moved, map[string]string{"roomId": it.RoomID, "person": it.Person, "hub": room.HubInstance})
			continue
		}
		if p, err := f.Svc.Store.ChatParticipant(room.ID, it.Person); err != nil || p.LeftSeq != 0 {
			continue
		}
		if err := f.Svc.MarkLeft(room, []string{it.Person}, SystemIdentityReset, "identity reset or removed by their household's operator"); errors.Is(err, ErrRoomMoving) {
			retry = append(retry, it)
		}
	}
	return moved, retry
}

// DecodeDepartures reads a departures request body.
func DecodeDepartures(body []byte) ([]departure, error) {
	var req struct {
		Items []departure `json:"items"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if len(req.Items) > notifyBatch {
		return nil, fmt.Errorf("%w: too many departures", ErrBadInput)
	}
	return req.Items, nil
}

// item is one entry in a notify envelope.
type item struct {
	Type   string `json:"type"` // event | invite | moved
	RoomID string `json:"roomId"`
	Hub    string `json:"hub"`
	// NewHub names the household a room moved to (type "moved").
	NewHub string           `json:"newHub,omitempty"`
	Event  *store.ChatEvent `json:"event,omitempty"`
	Invite *inviteItem      `json:"invite,omitempty"`
}

type inviteItem struct {
	Person    string        `json:"person"`
	DeviceKey string        `json:"deviceKey"`
	InvitedBy string        `json:"invitedBy"`
	Seq       int64         `json:"seq"`
	Welcome   []byte        `json:"welcome"`
	Room      *RoomSnapshot `json:"room"`
}

// RoomSnapshot is what a follower needs to mirror a room: everything public.
type RoomSnapshot struct {
	Room store.ChatRoom `json:"room"`
	// GroupID travels explicitly: the room view hides it from clients.
	GroupID      []byte                  `json:"groupId"`
	Policy       string                  `json:"policy"`
	Participants []store.ChatParticipant `json:"participants"`
	Tree         string                  `json:"tree"`
	GroupInfo    []byte                  `json:"groupInfo,omitempty"`
	GroupInfoEp  int64                   `json:"groupInfoEpoch"`
	// PublicState is the hub's verified group state, so a hand-over keeps
	// verifying; DM is a DM's fixed set, so a replica's people hold the room
	// to it too.
	PublicState string `json:"publicState,omitempty"`
	DM          string `json:"dm,omitempty"`
	// Departed says the asking household has no one left in the room: the
	// snapshot carries only its own people's rows, to mark them out, and
	// nothing of the room as it is since.
	Departed bool `json:"departed,omitempty"`
}

// ---- hub side ----

// followers lists the peers with a current participant in a room hubbed here.
func (f *Federation) followers(roomID string) []store.FedPeer {
	parts, err := f.Svc.Store.ChatParticipants(roomID)
	if err != nil {
		return nil
	}
	me := f.Svc.instanceOrEmpty()
	seen := map[string]bool{}
	var homes []string
	for _, p := range parts {
		if p.LeftSeq != 0 || p.HomeInstance == "" || p.HomeInstance == me || seen[p.HomeInstance] {
			continue
		}
		seen[p.HomeInstance] = true
		homes = append(homes, p.HomeInstance)
	}
	out, _ := f.Svc.Store.FedPeersByInstanceIDs(homes)
	return out
}

// OnAccepted is the hub's hook: every sequenced event is queued for each
// follower. Invites for newly added remote people ride the same queue.
func (f *Federation) OnAccepted(room *store.ChatRoom, ev *store.ChatEvent) {
	if room.HubInstance != "" {
		return
	}
	f.enqueueAll(f.followers(room.ID), item{Type: "event", RoomID: room.ID, Hub: f.Svc.instanceOrEmpty(), Event: ev})
}

// SendInvites queues a Welcome to each remote person a commit added.
func (f *Federation) SendInvites(room *store.ChatRoom, invites []store.ChatInvite) {
	me := f.Svc.instanceOrEmpty()
	if room.HubInstance != "" || len(invites) == 0 {
		return
	}
	snap, err := f.Snapshot(room)
	if err != nil {
		return
	}
	for _, inv := range invites {
		home, _, ok := ParseAddr(inv.PersonAddr)
		if !ok || home == me {
			continue
		}
		peer, err := fed.PeerForHousehold(f.Svc.Store, home)
		if err != nil {
			continue
		}
		f.enqueue(peer.ID, item{Type: "invite", RoomID: room.ID, Hub: me, Invite: &inviteItem{
			Person: inv.PersonAddr, DeviceKey: inv.DeviceKey, InvitedBy: inv.InvitedBy, Seq: inv.Seq, Welcome: inv.Welcome, Room: snap,
		}})
	}
}

// Snapshot renders a room's public state for a follower.
func (f *Federation) Snapshot(room *store.ChatRoom) (*RoomSnapshot, error) {
	parts, err := f.Svc.Store.ChatParticipants(room.ID)
	if err != nil {
		return nil, err
	}
	gi, ep, _ := f.Svc.Store.ChatGroupInfo(room.ID)
	return &RoomSnapshot{Room: *room, GroupID: room.GroupID, Policy: room.Policy, Participants: parts, Tree: room.Tree, GroupInfo: gi, GroupInfoEp: ep,
		PublicState: room.PublicState, DM: room.DMKey}, nil
}

// notifyDeparted sends ev to each household some of left belong to that
// has no one left in the room: the push that tells its fog its people are
// out, which the ordinary fan-out (to followers, read after ev) skips.
func (f *Federation) notifyDeparted(room *store.ChatRoom, ev *store.ChatEvent, left []string) {
	me := f.Svc.instanceOrEmpty()
	still := map[string]bool{}
	for _, p := range f.followers(room.ID) {
		still[p.InstanceID] = true
	}
	var homes []string
	seen := map[string]bool{}
	for _, a := range left {
		home, _, ok := ParseAddr(a)
		if !ok || home == me || still[home] || seen[home] {
			continue
		}
		seen[home] = true
		homes = append(homes, home)
	}
	if len(homes) == 0 {
		return
	}
	peers, _ := f.Svc.Store.FedPeersByInstanceIDs(homes)
	f.enqueueAll(peers, item{Type: "event", RoomID: room.ID, Hub: me, Event: ev})
}

// Stake is what a household may read of a room hubbed here: everything
// (cap 0) while one of its people is in the room or invited to it; only the
// log up to where its last person left (cap > 0) once all have gone; nothing
// (ok false) if it never had anyone there.
func (f *Federation) Stake(room *store.ChatRoom, instance string) (cap int64, ok bool) {
	parts, _ := f.Svc.Store.ChatParticipants(room.ID)
	for _, p := range parts {
		// A ban on someone never in the room (left_seq -1) is no stake.
		if p.HomeInstance != instance || p.LeftSeq < 0 {
			continue
		}
		if p.LeftSeq == 0 {
			return 0, true
		}
		ok = true
		cap = max(cap, p.LeftSeq)
	}
	addrs, _ := f.Svc.Store.ChatPendingInviteAddrs(room.ID)
	for _, addr := range addrs {
		if home, _, k := ParseAddr(addr); k && home == instance {
			return 0, true
		}
	}
	return cap, ok
}

// SnapshotFor renders a room's snapshot for the household asking. One with
// nobody left in the room gets a departed snapshot: its own people's rows,
// so its replica marks them out, and nothing newer about the room.
func (f *Federation) SnapshotFor(room *store.ChatRoom, instance string) (*RoomSnapshot, error) {
	cap, ok := f.Stake(room, instance)
	if !ok {
		return nil, ErrNotParticipant
	}
	if cap == 0 {
		return f.Snapshot(room)
	}
	parts, err := f.Svc.Store.ChatParticipants(room.ID)
	if err != nil {
		return nil, err
	}
	snap := &RoomSnapshot{Departed: true, GroupID: room.GroupID, Participants: []store.ChatParticipant{}}
	snap.Room = store.ChatRoom{ID: room.ID, Kind: room.Kind, Seq: cap, CreatedBy: room.CreatedBy, CreatedAt: room.CreatedAt}
	for _, p := range parts {
		if p.HomeInstance == instance {
			snap.Participants = append(snap.Participants, p)
		}
	}
	return snap, nil
}

func (f *Federation) enqueue(peerID int64, it item) {
	f.enqueueAll([]store.FedPeer{{ID: peerID}}, it)
}

// enqueueAll queues one item for several peers: marshalled once, written
// in one transaction, each peer's flush coalesced as enqueue's is.
func (f *Federation) enqueueAll(peers []store.FedPeer, it item) {
	if len(peers) == 0 {
		return
	}
	b, err := json.Marshal(it)
	if err != nil {
		return
	}
	ids := make([]int64, len(peers))
	for i, p := range peers {
		ids[i] = p.ID
	}
	if err := f.Svc.Store.EnqueueFedOutboxAll(ids, OutboxKind, it.RoomID, string(b)); err != nil {
		f.Svc.Log.Warn("chat fed: enqueue", "err", err)
		return
	}
	for _, peerID := range ids {
		f.out.Kick(peerID, coalesce)
	}
}

// Flush delivers every due outbox row to every peer. The loop calls it on a
// cadence; enqueue flushes one peer after a short coalescing wait.
func (f *Federation) Flush(ctx context.Context) {
	f.out.Flush(ctx)
	f.departures.Flush(ctx)
}

// notifySource is the chat outbox as the drainer sees it: each row is one
// notify item, and a batch is {"items": [...]}.
type notifySource struct {
	f *Federation
	fed.OutboxQueue
}

func (n notifySource) Encode(_ *store.FedPeer, batch []fed.OutboxRow[string]) ([]byte, error) {
	items := make([]json.RawMessage, len(batch))
	for i, r := range batch {
		items[i] = json.RawMessage(r.Payload)
	}
	return json.Marshal(map[string]any{"items": items})
}

// Settle reads the follower's answer — it may refuse individual invites
// (third households), and says which, so the hub records those people as
// out — then takes the batch off the queue.
//
// A follower speaks for its own people only, and only about the invites it
// was just sent: a refusal naming anyone else, or a room it was sent no
// invite to in this batch, is ignored — it would otherwise let any paired
// household take anyone out of any room hubbed here.
func (n notifySource) Settle(peer *store.FedPeer, batch []fed.OutboxRow[string], answer []byte) error {
	var ack struct {
		Refused []struct {
			RoomID string `json:"roomId"`
			Person string `json:"person"`
			Code   string `json:"code"`
		} `json:"refused"`
	}
	var later []int64
	if json.Unmarshal(answer, &ack) == nil && len(ack.Refused) > 0 {
		invited := map[[2]string]int64{}
		for _, r := range batch {
			var it item
			if json.Unmarshal([]byte(r.Payload), &it) == nil && it.Type == "invite" && it.Invite != nil {
				invited[[2]string{it.RoomID, it.Invite.Person}] = r.ID
			}
		}
		for _, r := range ack.Refused {
			if c, err := n.f.Svc.CanonicalAddr(r.Person); err == nil {
				r.Person = c
			}
			home, _, ok := ParseAddr(r.Person)
			row, sent := invited[[2]string{r.RoomID, r.Person}]
			if !ok || peer == nil || home != peer.InstanceID || !sent {
				continue
			}
			// A follower that is full for now hasn't refused anyone: the
			// invitee stays in the room, and the invite goes again later.
			if r.Code == "invites_full" {
				later = append(later, row)
				continue
			}
			n.f.markLeft(r.RoomID, r.Person, "refused:"+r.Code)
		}
	}
	if len(later) > 0 && peer != nil {
		keep := map[int64]bool{}
		for _, id := range later {
			keep[id] = true
		}
		var done []fed.OutboxRow[string]
		for _, r := range batch {
			if !keep[r.ID] {
				done = append(done, r)
			}
		}
		if err := n.Defer(peer.ID, later, n.f.Now().Add(invitesFullRetry), nil); err != nil {
			return err
		}
		return n.Delete(done)
	}
	return n.Delete(batch)
}

// invitesFullRetry is when an invite a follower was too full to take goes
// again.
const invitesFullRetry = time.Hour

// markLeft records that a person is no longer in a room hubbed here, with a
// system event members act on (an admin commits the MLS removal).
func (f *Federation) markLeft(roomID, addr, reason string) {
	room, err := f.Svc.Store.ChatRoom(roomID)
	if err != nil || room.HubInstance != "" {
		return
	}
	f.Svc.MarkLeft(room, []string{addr}, SystemHouseholdCut, reason)
}

// Loop drains the outbox and re-syncs replicas, each on its own cadence: a
// re-sync pass waits on hubs, and a slow one must not hold up retries of
// what we owe our followers.
func (f *Federation) Loop(ctx context.Context) {
	go f.out.Loop(ctx, flushEvery)
	go f.departures.Loop(ctx, flushEvery)
	loop.Every(ctx, pollEvery, pollEvery, f.SyncAll)
}

// PeopleFor is the heartbeat hook: our directory as this peer may see it
// (people.directory, docs/signin-contract.md). Guests are never
// advertised, nor bots; identity keys and devices ride along only when the
// peer is granted chat too — a directory is who, chat is how to reach them.
func (f *Federation) PeopleFor(peer *store.FedPeer) []fed.DirectoryPerson {
	cfg, granted, _ := f.Svc.Store.FedCapabilityConfig(peer.ID, store.CapPeopleDirectory, store.DirGrant)
	if !granted {
		return nil
	}
	var use UseConfig
	if json.Unmarshal([]byte(cfg), &use) != nil {
		return nil
	}
	_, chatGranted, _ := f.Svc.Store.FedCapabilityConfig(peer.ID, CapChat, store.DirGrant)
	list, err := f.Svc.Store.ListPeople()
	if err != nil {
		return nil
	}
	// Everyone's devices in one read rather than one per person; a failed
	// read advertises no devices, as a failed per-person read did.
	var devices map[int64][]store.PersonDevice
	if chatGranted {
		devices, _ = f.Svc.Store.AllPersonDevices()
	}
	instance, _ := f.Svc.Instance()
	out := []fed.DirectoryPerson{}
	for _, p := range list {
		if p.Status != store.PersonStatusActive || !use.allows(p.Handle) {
			continue
		}
		if p.Role != store.PersonRoleOperator && p.Role != store.PersonRoleMember {
			continue
		}
		dp := fed.DirectoryPerson{Handle: p.Handle, DisplayName: p.DisplayName, Role: p.Role, Devices: []fed.DirectoryDevice{}}
		if instance != "" && p.UID != "" {
			dp.Address = "person:" + instance + "/" + p.UID
		}
		if chatGranted {
			if len(p.IdentityKey) > 0 {
				dp.IdentityKey = people.EncodeKey(p.IdentityKey)
			}
			for _, d := range devices[p.ID] {
				dd := fed.DirectoryDevice{ID: d.ID, Name: d.Name, Kind: d.Kind, DeviceKey: people.EncodeKey(d.DeviceKey), Verified: d.Verified()}
				if d.Verified() {
					dd.Attestation = people.EncodeKey(d.Attestation)
				}
				dp.Devices = append(dp.Devices, dd)
			}
		}
		out = append(out, dp)
	}
	return out
}

// PeerPeople decodes a peer's advertised directory.
func PeerPeople(peer *store.FedPeer) []fed.DirectoryPerson {
	var out []fed.DirectoryPerson
	if len(peer.People) > 0 {
		json.Unmarshal(peer.People, &out)
	}
	return out
}

// RemoteDevice looks a remote person's device up in the directory the peer
// advertised. The hub uses it to check that a leaf a member wants to add is
// a device the person's own fog vouches for.
func (f *Federation) RemoteDevice(addr string, key []byte) (*fed.DirectoryPerson, *fed.DirectoryDevice, error) {
	home, handle, ok := ParseAddr(addr)
	if !ok {
		return nil, nil, ErrUnknownPerson
	}
	peer, err := fed.PeerForHousehold(f.Svc.Store, home)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s is not a paired household", ErrUnknownPerson, home)
	}
	if _, granted, _ := f.Svc.Store.FedCapabilityConfig(peer.ID, CapChat, store.DirGrant); !granted {
		return nil, nil, fmt.Errorf("%w: %s is not granted chat here", ErrUnknownPerson, peer.DisplayName())
	}
	for _, p := range PeerPeople(peer) {
		if !p.Names(handle) {
			continue
		}
		for i := range p.Devices {
			if k, err := people.DecodeKey(p.Devices[i].DeviceKey); err == nil && string(k) == string(key) {
				return &p, &p.Devices[i], nil
			}
		}
		return &p, nil, fmt.Errorf("%w: %s", ErrUnknownDevice, addr)
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrUnknownPerson, addr)
}

// KeysFor fetches key material for a remote person from their home fog.
func (f *Federation) KeysFor(ctx context.Context, addr string) (json.RawMessage, error) {
	home, handle, ok := ParseAddr(addr)
	if !ok {
		return nil, ErrUnknownPerson
	}
	peer, err := fed.PeerForHousehold(f.Svc.Store, home)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a paired household", ErrUnknownPerson, home)
	}
	var out json.RawMessage
	if _, err := f.Fed.SignedGetJSON(ctx, peer, "/fed/v1/chat/people/"+url.PathEscape(handle)+"/keys", "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- follower side ----

// Receive applies a notify envelope from a hub. Events land in the replica in
// hub order; a gap triggers a pull. Returns the invites refused.
func (f *Federation) Receive(ctx context.Context, peer *store.FedPeer, items []item) (refused []map[string]string) {
	var invites []item
	defer func() { f.enqueueInvites(peer, invites) }()
	for _, it := range items {
		if it.Hub != peer.InstanceID {
			// A hub may only speak for rooms it hubs.
			continue
		}
		switch it.Type {
		case "invite":
			if it.Invite == nil {
				continue
			}
			if code, err := f.receiveInvite(ctx, peer, it); err != nil {
				f.Svc.Log.Warn("chat fed: invite refused", "room", it.RoomID, "person", it.Invite.Person, "err", err)
				if code != "" {
					refused = append(refused, map[string]string{"roomId": it.RoomID, "person": it.Invite.Person, "code": code})
				}
				continue
			}
			invites = append(invites, it)
		case "event":
			if it.Event == nil {
				continue
			}
			f.receiveEvent(ctx, peer, it.RoomID, it.Event)
		case "moved":
			if it.NewHub != "" {
				f.moved(ctx, peer, it.RoomID, it.NewHub)
			}
		}
	}
	return refused
}

// receiveInvite mirrors the room if needed and files the Welcome for the
// person's device.
func (f *Federation) receiveInvite(ctx context.Context, peer *store.FedPeer, it item) (string, error) {
	inv := it.Invite
	home, _, ok := ParseAddr(inv.Person)
	if !ok || home != f.Svc.instanceOrEmpty() {
		return "", errors.New("invite is not for this household")
	}
	if inv.Room == nil || inv.Room.Room.ID != it.RoomID {
		return "", errors.New("invite without its room")
	}
	// The room's rules first, which say nothing about who lives here.
	if code, err := f.thirdHouseholdCheck(peer, inv.Room); err != nil {
		return code, err
	}
	if code, err := f.claimRoomID(peer, it.RoomID); err != nil {
		return code, err
	}
	// A room this fog would start watching for an invite nobody has
	// accepted — a real one or a decoy, alike — counts against a cap per hub
	// and overall; past it every such invite is refused the same way.
	if _, err := f.Svc.Store.ChatRoom(it.RoomID); err != nil && f.watchFull(peer, it.RoomID, inv.InvitedBy) {
		return "invites_full", errors.New("too many invites from this hub are waiting")
	}
	// From here the answer is the same whoever the invite names, and so is
	// what this fog does where the hub can see it: the work happens behind
	// the answer (Receive queues it), and an invite for someone who isn't
	// here, or whom this household doesn't show that hub, makes the same
	// calls back as one filed. The hub learns nothing about which handles
	// exist.
	return "", nil
}

// The invite queue. Invites are filed behind their answer, by at most
// inviteWorkers workers per hub; a hub's invites for one room wait as one
// job, so a room is called back about once however many invites name it;
// and a hub has at most inviteMaxRooms rooms waiting, and inviteMaxItems
// invites per room, past which its invites are dropped (logged). One notify
// request, or many, can't make this fog do unbounded work.
const (
	inviteWorkers  = 2
	inviteMaxRooms = 256
	inviteMaxItems = 200
	inviteBudget   = 2 * time.Minute
	// What one hub's waiting invites, and everyone's, may weigh.
	inviteMaxHubBytes = 8 << 20
	inviteMaxBytes    = 32 << 20
)

type inviteQueue struct {
	peer    *store.FedPeer
	pending map[string][]item
	weight  map[string]int64
	order   []string
	workers int
	bytes   int64
}

// itemBytes is what one queued invite weighs: its Welcome and the room
// snapshot it carries.
func itemBytes(it item) int64 {
	b, _ := json.Marshal(it)
	return int64(len(b))
}

func (f *Federation) enqueueInvites(peer *store.FedPeer, items []item) {
	if len(items) == 0 {
		return
	}
	f.inviteMu.Lock()
	defer f.inviteMu.Unlock()
	for _, it := range items {
		f.enqueueInviteLocked(peer, it)
	}
}

func (f *Federation) enqueueInviteLocked(peer *store.FedPeer, it item) {
	if f.inviteQs == nil {
		f.inviteQs = map[int64]*inviteQueue{}
	}
	q := f.inviteQs[peer.ID]
	if q == nil {
		q = &inviteQueue{pending: map[string][]item{}, weight: map[string]int64{}}
		f.inviteQs[peer.ID] = q
	}
	q.peer = peer
	items, held := q.pending[it.RoomID]
	size := itemBytes(it)
	switch {
	case !held && len(q.order) >= inviteMaxRooms, held && len(items) >= inviteMaxItems,
		q.bytes+size > inviteMaxHubBytes, f.inviteBytes+size > inviteMaxBytes:
		f.Svc.Log.Warn("chat fed: too many invites waiting; dropped", "hub", peer.Name, "room", it.RoomID)
		return
	case !held:
		q.order = append(q.order, it.RoomID)
	}
	q.pending[it.RoomID] = append(items, it)
	q.weight[it.RoomID] += size
	q.bytes += size
	f.inviteBytes += size
	if q.workers < inviteWorkers {
		q.workers++
		f.invites.Add(1)
		go f.inviteWorker(q)
	}
}

func (f *Federation) inviteWorker(q *inviteQueue) {
	defer f.invites.Done()
	for {
		f.inviteMu.Lock()
		if len(q.order) == 0 {
			q.workers--
			f.inviteMu.Unlock()
			return
		}
		roomID := q.order[0]
		q.order = q.order[1:]
		items := q.pending[roomID]
		delete(q.pending, roomID)
		q.bytes -= q.weight[roomID]
		f.inviteBytes -= q.weight[roomID]
		delete(q.weight, roomID)
		peer := q.peer
		f.inviteMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), inviteBudget)
		f.fileInvites(ctx, peer, roomID, items)
		cancel()
	}
}

// fileInvites files a room's invites for the people this household shows
// the hub, and then makes the one call back any invite gets — the room's
// snapshot, then its log — whoever they named.
func (f *Federation) fileInvites(ctx context.Context, peer *store.FedPeer, roomID string, items []item) {
	// A hub cut, or no longer granted chat, since it sent these gets
	// nothing filed.
	if fresh, err := f.Svc.Store.FedPeerByID(peer.ID); err != nil || !f.chatGranted(fresh) {
		return
	}
	var filed []string
	// Who this household shows the hub is read once for the job: every
	// invite, for someone here or not, hidden or shown, then costs the same.
	shown := f.shownTo(peer)
	for _, it := range items {
		_, handle, _ := ParseAddr(it.Invite.Person)
		if !f.visibleIn(shown, handle) {
			f.fileDecoy(peer, it)
			continue
		}
		addr, err := f.fileInvite(peer, it, handle)
		if err != nil {
			f.Svc.Log.Warn("chat fed: invite not filed", "room", roomID, "err", err)
			continue
		}
		if addr != "" {
			filed = append(filed, addr)
		}
	}
	f.callBack(ctx, peer, roomID)
	for _, addr := range filed {
		f.Svc.Bus.PublishPerson(addr, Notification{Kind: "invite", RoomID: roomID})
		f.Svc.Bus.PublishPerson(addr, Notification{Kind: "rooms", RoomID: roomID})
	}
}

// fileDecoy is what an invite for someone who doesn't live here, or whom
// this household doesn't show the hub, sets up: the replica a filed one
// would — synced and polled the same, so the hub sees the same calls then
// and after — held by a decoy invite that names nobody, and dropped once
// its watch is over (watch). It holds ciphertext the hub chose to send and
// shows nobody anything.
func (f *Federation) fileDecoy(peer *store.FedPeer, it item) {
	if _, err := f.Svc.Store.ChatRoom(it.RoomID); err == nil {
		return // held anyway: its sync is the same
	}
	if _, err := f.claimRoomID(peer, it.RoomID); err != nil {
		return
	}
	if err := f.mirror(peer, it.Invite.Room, true); err != nil {
		return
	}
	f.Svc.Store.AddChatInvite(&store.ChatInvite{
		RoomID: it.RoomID, PersonAddr: decoyPrefix + it.RoomID, InvitedBy: it.Invite.InvitedBy,
		Seq: it.Invite.Seq, Welcome: []byte{}, CreatedAt: f.Now().UTC().Format(time.RFC3339),
	})
}

// Caps on the rooms watched for unaccepted invites (real or decoy): per
// hub, per person sending them (so one person making rooms can't fill a
// household's cap), and in all.
const (
	watchPerHub     = 64
	watchPerInviter = 8
	watchTotal      = 512
)

// watchFull says one more watched room from peer, sent by inviter, would
// pass a cap. Only rooms in their watch window, that none of this fog's
// people has accepted on any device, count — and rooms queued to be.
func (f *Federation) watchFull(peer *store.FedPeer, roomID, inviter string) bool {
	since := f.Now().Add(-inviteWatch).UTC().Format(time.RFC3339)
	byHub, byInviter, total, err := f.Svc.Store.ChatWatchedRooms(peer.InstanceID, inviter, since)
	if err != nil {
		return true
	}
	f.inviteMu.Lock()
	for id, q := range f.inviteQs {
		for _, r := range q.order {
			if r == roomID {
				f.inviteMu.Unlock()
				return false // already on its way: no new watch
			}
			if id == peer.ID && len(q.pending[r]) > 0 && q.pending[r][0].Invite != nil && q.pending[r][0].Invite.InvitedBy == inviter {
				byInviter++
			}
		}
		total += len(q.order)
		if id == peer.ID {
			byHub += len(q.order)
		}
	}
	f.inviteMu.Unlock()
	return byHub >= watchPerHub || byInviter >= watchPerInviter || total >= watchTotal
}

// decoyPrefix names a decoy invite's person: no address, so no one's.
const decoyPrefix = "decoy:"

// inviteWatch is how long a replica held only by invites nobody has
// accepted — real or decoy — is kept in step with its hub.
const inviteWatch = 7 * 24 * time.Hour

// watch decides whether SyncAll polls a replica. One of our people in it
// who has accepted (or never needed) an invite: always. One held only by
// unaccepted invites: while the newest is within inviteWatch, then not —
// the same for a real invite nobody accepted and a decoy, so the hub can't
// tell them apart by our polling; a decoy-only replica is then dropped.
func (f *Federation) watch(room *store.ChatRoom) (poll, drop bool) {
	invs, err := f.Svc.Store.ChatRoomInvites(room.ID)
	if err != nil {
		return true, false
	}
	// Per person: an invite pending, and one accepted (on any device).
	pending, accepted := map[string]bool{}, map[string]bool{}
	newest, real := "", false
	for _, inv := range invs {
		if !strings.HasPrefix(inv.PersonAddr, decoyPrefix) {
			real = true
		}
		if inv.AcceptedAt == "" {
			pending[inv.PersonAddr] = true
			newest = max(newest, inv.CreatedAt)
		} else {
			accepted[inv.PersonAddr] = true
		}
	}
	if len(pending) == 0 {
		return true, false
	}
	// Anyone of ours who is a member — in without an invite, or having
	// accepted one on any device — keeps the room polled, whatever else is
	// still pending.
	me := f.Svc.instanceOrEmpty()
	parts, _ := f.Svc.Store.ChatParticipants(room.ID)
	for _, p := range parts {
		if p.LeftSeq == 0 && p.HomeInstance == me && (accepted[p.PersonAddr] || !pending[p.PersonAddr]) {
			return true, false
		}
	}
	if t, err := time.Parse(time.RFC3339, newest); err == nil && f.Now().Sub(t) < inviteWatch {
		return true, false
	}
	return false, !real
}

// WaitInvites waits for invites still being filed (tests use it).
func (f *Federation) WaitInvites() { f.invites.Wait() }

// shownTo is the handles this household's people.directory grant shows a
// peer, read once.
func (f *Federation) shownTo(peer *store.FedPeer) map[string]bool {
	out := map[string]bool{}
	for _, dp := range f.PeopleFor(peer) {
		out[dp.Handle] = true
	}
	return out
}

// visibleIn says handle names an active person of ours among shown. It does
// the same work whether the handle is someone's, someone hidden's, or no
// one's: one lookup, one set check.
func (f *Federation) visibleIn(shown map[string]bool, handle string) bool {
	p, err := f.Svc.Store.PersonByRef(handle)
	name := handle
	if err == nil {
		name = p.Handle
	}
	listed := shown[name]
	return err == nil && p.Status == store.PersonStatusActive && listed
}

// callBack is what follows a hub's invites for a room: a sync of the
// replica when this fog holds one (filed invites set it up), and otherwise
// the same calls — the room's snapshot, then its log — keeping nothing.
func (f *Federation) callBack(ctx context.Context, peer *store.FedPeer, roomID string) {
	if _, err := f.Svc.Store.ChatRoom(roomID); err == nil {
		if err := f.syncRoom(ctx, peer, roomID); err != nil {
			f.Svc.Log.Warn("chat fed: sync of an invited room", "room", roomID, "err", err)
		}
		return
	}
	var snap RoomSnapshot
	if _, err := f.getRoomJSON(ctx, peer, "/fed/v1/chat/rooms/"+roomID, "", &snap); err != nil {
		return // as a sync stops when the snapshot can't be had
	}
	var res struct {
		Events []store.ChatEvent `json:"events"`
	}
	f.getRoomJSON(ctx, peer, fmt.Sprintf("/fed/v1/chat/rooms/%s/events?after=%d&limit=500", roomID, 0), "", &res)
}

// claimRoomID decides whether peer may invite into a room by this id here.
// A room this fog already holds takes invites only from its current hub —
// the one it was made by, or moved to by a hand-over this fog followed from
// the hub before (moved) — and an invite never displaces it: any other
// household's invite is refused. A room this fog doesn't hold yet may be
// set up by the household its id is bound to (BoundRoomID); an id bound to
// this fog or to another household paired with it is refused from anyone
// else, so no peer can squat on a room id that household may later invite
// under. An id bound to no household this fog knows is taken as offered.
func (f *Federation) claimRoomID(peer *store.FedPeer, roomID string) (string, error) {
	if cur, err := f.Svc.Store.ChatRoom(roomID); err == nil {
		if cur.HubInstance == peer.InstanceID {
			return "", nil
		}
		return "room_id_taken", fmt.Errorf("room %s is already held here, hubbed by another household", roomID)
	}
	if RoomIDBoundTo(roomID, peer.InstanceID) {
		return "", nil
	}
	known := []string{f.Svc.instanceOrEmpty()}
	if peers, err := f.Svc.Store.ListFedPeers(); err == nil {
		for _, p := range peers {
			known = append(known, p.InstanceID)
		}
	}
	for _, inst := range known {
		if inst != peer.InstanceID && RoomIDBoundTo(roomID, inst) {
			return "room_id_taken", fmt.Errorf("room %s belongs to another household this fog knows", roomID)
		}
	}
	return "", nil
}

// fileInvite mirrors the room if needed and files the Welcome for one of
// our people.
// It returns the address filed under, or "" for a Welcome already on file.
func (f *Federation) fileInvite(peer *store.FedPeer, it item, handle string) (string, error) {
	inv := *it.Invite
	home, _, _ := ParseAddr(inv.Person)
	p, err := f.Svc.Store.PersonByRef(handle)
	if err != nil {
		return "", err
	}
	// Filed under the person's own address, whichever form the hub used.
	inv.Person = "fog:" + home + "/" + p.Handle
	// The hub may have moved on, or another taken the id, since the answer.
	if code, err := f.claimRoomID(peer, it.RoomID); err != nil {
		return "", fmt.Errorf("%s: %w", code, err)
	}
	if err := f.mirror(peer, inv.Room, true); err != nil {
		return "", err
	}
	room, err := f.Svc.Store.ChatRoom(it.RoomID)
	if err != nil {
		return "", err
	}
	// Idempotent: the same Welcome twice files once.
	existing, _ := f.Svc.Store.ChatInvitesFor(inv.Person)
	for _, e := range existing {
		if e.RoomID == room.ID && e.Seq == inv.Seq && e.DeviceKey == inv.DeviceKey {
			return "", nil
		}
	}
	if err := f.Svc.Store.AddChatInvite(&store.ChatInvite{
		RoomID: room.ID, PersonAddr: inv.Person, DeviceKey: inv.DeviceKey, InvitedBy: inv.InvitedBy,
		Seq: inv.Seq, Welcome: inv.Welcome, CreatedAt: f.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return "", err
	}
	// Now that the invite is on file, the snapshot may show them in it,
	// and the room's log is ours to hold.
	if err := f.Svc.refreshReplicaMeta(room, inv.Room); err != nil {
		return "", err
	}
	f.Svc.Store.Audit(store.FederationActor(peer.Name), "chat.invite.received", room.ID, store.AuditDetail(map[string]any{"person": inv.Person}))
	return inv.Person, nil
}

// thirdHouseholdCheck applies the grant's acceptThirdHouseholds setting to a
// room snapshot: refuse when it holds people from a household we are not
// paired with and the operator said no.
func (f *Federation) thirdHouseholdCheck(hub *store.FedPeer, snap *RoomSnapshot) (string, error) {
	cfg, _, _ := fed.CapConfig[GrantConfig](f.Svc.Store, hub.ID, CapChat, store.DirGrant)
	if cfg.acceptsThird() {
		return "", nil
	}
	me := f.Svc.instanceOrEmpty()
	for _, p := range snap.Participants {
		// A participant's household is the one their address names, not
		// what the snapshot says it is.
		home, _, _ := ParseAddr(p.PersonAddr)
		if p.LeftSeq != 0 || home == me || home == hub.InstanceID {
			continue
		}
		if _, err := fed.PeerForHousehold(f.Svc.Store, home); err != nil {
			return "third_household", fmt.Errorf("room holds people from an unpaired household (%s) and this fog refuses that", home)
		}
	}
	return "", nil
}

// mirror refreshes a replica room from a hub snapshot, or, with create —
// only when an invite for one of our people brought the snapshot — makes it.
// A hub can't put a room into our people's lists that none of them was
// invited to.
func (f *Federation) mirror(peer *store.FedPeer, snap *RoomSnapshot, create bool) error {
	r := snap.Room
	existing, err := f.Svc.Store.ChatRoom(r.ID)
	if snap.Departed {
		// Nobody of ours is in the room any more: mark them out where the
		// hub says they left and keep the replica, read-only, as it was.
		if err != nil {
			return err
		}
		if existing.HubInstance != peer.InstanceID {
			return errors.New("room is hubbed elsewhere")
		}
		return f.Svc.departReplica(existing, snap.Participants)
	}
	if errors.Is(err, store.ErrNotFound) {
		if !create {
			return errors.New("no one here was invited to that room")
		}
		if len(snap.GroupID) == 0 {
			return errors.New("snapshot without a group id")
		}
		row := &store.ChatRoom{
			ID: r.ID, GroupID: snap.GroupID, HubInstance: peer.InstanceID, Kind: r.Kind, Label: r.Label, TopicLabel: r.TopicLabel,
			Policy: snap.Policy, WorkspaceVisible: false, Epoch: r.Epoch, Seq: 0, Tree: snap.Tree, ReplicaStatus: "live", CreatedBy: r.CreatedBy,
			DMKey: snap.DM, PublicState: snap.PublicState,
		}
		if row.Policy == "" {
			row.Policy = DefaultPolicy(r.Kind).JSON()
		}
		if err := f.Svc.Store.CreateChatRoom(row); err != nil {
			return err
		}
		existing = row
	} else if err != nil {
		return err
	}
	if existing.HubInstance != peer.InstanceID {
		return errors.New("room is hubbed elsewhere")
	}
	return f.Svc.refreshReplicaMeta(existing, snap)
}

// receiveEvent appends one hub-sequenced event to the replica, pulling any
// gap first so the log never has holes.
func (f *Federation) receiveEvent(ctx context.Context, peer *store.FedPeer, roomID string, ev *store.ChatEvent) {
	room, err := f.Svc.Store.ChatRoom(roomID)
	if errors.Is(err, store.ErrNotFound) {
		// A room none of our people was invited to: not ours to hold.
		return
	}
	if err != nil || room.HubInstance != peer.InstanceID {
		return
	}
	// The hub's own push waits on none of this past one room's budget.
	ctx, cancel := context.WithTimeout(ctx, syncRoomBudget)
	defer cancel()
	if ev.Seq > room.Seq+1 {
		if err := f.SyncRoom(ctx, peer, roomID); err != nil {
			f.Svc.Log.Warn("chat fed: gap fill", "room", roomID, "err", err)
		}
		return
	}
	if ev.Seq <= room.Seq {
		return
	}
	f.Svc.appendReplica(room, ev)
	if ev.Kind == EventCommit || ev.Kind == EventSystem {
		f.refreshMeta(ctx, peer, room)
	}
}

func (f *Federation) refreshMeta(ctx context.Context, peer *store.FedPeer, room *store.ChatRoom) {
	snap, tag, err := f.pullSnapshot(ctx, peer, room.ID)
	if err != nil || snap == nil {
		return
	}
	if f.Svc.refreshReplicaMeta(room, snap) == nil {
		f.Svc.Store.SetChatRoomHubETag(room.ID, tag.etag, tag.seq)
	}
}

// SnapshotETag tags a marshaled room snapshot by its content: any event,
// membership change or new GroupInfo moves it.
func SnapshotETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// hubTag is what a replica remembers of the hub snapshot it last applied:
// its ETag, and the hub's seq it named (0 when not known).
type hubTag struct {
	etag string
	seq  int64
}

// pullSnapshot fetches a room's snapshot from its hub, conditional on the
// one the replica last applied. A nil snapshot (and no error) means the
// hub answered 304: the replica already holds this state, and the tag is
// the one it remembered. A hub too old to tag snapshots always answers in
// full, with no ETag to remember.
func (f *Federation) pullSnapshot(ctx context.Context, peer *store.FedPeer, roomID string) (*RoomSnapshot, hubTag, error) {
	var prev hubTag
	prev.etag, prev.seq, _ = f.Svc.Store.ChatRoomHubETag(roomID)
	var snap RoomSnapshot
	etag, err := f.getRoomJSON(ctx, peer, "/fed/v1/chat/rooms/"+roomID, prev.etag, &snap)
	if errors.Is(err, fed.ErrNotModified) {
		return nil, prev, nil
	}
	if err != nil {
		return nil, hubTag{}, err
	}
	return &snap, hubTag{etag: etag, seq: snap.Room.Seq}, nil
}

// errHubDown marks a re-sync that failed reaching the hub rather than on
// the one room: every other room on that hub would fail the same way.
var errHubDown = errors.New("hub unreachable")

// hubFailure wraps a failed hub request. A refusal the hub itself wrote is
// about the room; anything else — no answer, an answer we can't verify, a
// proxy's 5xx for a hub that's down — is about the hub.
func hubFailure(err error) error {
	var re *fed.RemoteError
	// Only a refusal the hub signed is the hub's word about the room; an
	// unsigned one may come from whoever answers at its address.
	if errors.As(err, &re) && re.Verified && re.Status < 500 {
		return err
	}
	return fmt.Errorf("%w: %w", errHubDown, err)
}

// SyncRoom pulls a replica up to date from its hub.
func (f *Federation) SyncRoom(ctx context.Context, peer *store.FedPeer, roomID string) error {
	snap, tag, err := f.pullSnapshot(ctx, peer, roomID)
	if err != nil {
		return hubFailure(err)
	}
	if snap == nil {
		// Unchanged since it was mirrored, but the operator may have turned
		// third households away since: check what the replica holds.
		parts, err := f.Svc.Store.ChatParticipants(roomID)
		if err != nil {
			return err
		}
		if code, err := f.thirdHouseholdCheck(peer, &RoomSnapshot{Participants: parts}); err != nil {
			return fmt.Errorf("%s: %w", code, err)
		}
	} else {
		if code, err := f.thirdHouseholdCheck(peer, snap); err != nil {
			return fmt.Errorf("%s: %w", code, err)
		}
		if snap.Room.ID != roomID {
			return errors.New("hub answered with another room's snapshot")
		}
		if err := f.mirror(peer, snap, false); err != nil {
			return err
		}
		f.Svc.Store.SetChatRoomHubETag(roomID, tag.etag, tag.seq)
	}
	room, err := f.Svc.Store.ChatRoom(roomID)
	if err != nil {
		return err
	}
	// A 304 says the hub's log still ends where the remembered snapshot
	// said; a replica already that far along has nothing to fetch.
	if snap == nil && tag.seq > 0 && room.Seq >= tag.seq {
		return nil
	}
	// The hub's answer is its log: events it no longer holds (expired,
	// pruned) are holes it declares by leaving them out, and the replica
	// steps over them. Every page must move the replica forward, or the
	// pull stops — a hub can't hold it in a loop.
	for {
		var res struct {
			Events []store.ChatEvent `json:"events"`
			Seq    int64             `json:"seq"`
		}
		if _, err := f.getRoomJSON(ctx, peer, fmt.Sprintf("/fed/v1/chat/rooms/%s/events?after=%d&limit=500", roomID, room.Seq), "", &res); err != nil {
			return hubFailure(err)
		}
		if len(res.Events) == 0 {
			// Nothing left to fetch: whatever the hub's log ended with since
			// is gone from it.
			if res.Seq > room.Seq {
				f.Svc.skipReplicaTo(room, res.Seq)
			}
			break
		}
		start := room.Seq
		for i := range res.Events {
			ev := res.Events[i]
			if ev.Seq <= room.Seq {
				continue
			}
			f.Svc.appendReplicaPulled(room, &ev)
			if room.Seq != ev.Seq {
				break // not taken: the rest would land on a hole of our own
			}
		}
		if room.Seq == start {
			break
		}
		if len(res.Events) < 500 {
			// The last page: the log ends where the hub says, whatever
			// after it is gone.
			if res.Seq > room.Seq {
				f.Svc.skipReplicaTo(room, res.Seq)
			}
			break
		}
	}
	return nil
}

// hubBackoff is one unreachable hub's place on the retry ladder.
type hubBackoff struct {
	fails int
	next  time.Time // none of its rooms re-sync before this
}

// SyncAll re-syncs every replica; the idle poll. A hub that can't be
// reached is tried once a pass, not once per room — its first failure
// skips the rest of its rooms — and then backs off, so a household that is
// away for a weekend costs an attempt now and then rather than a timeout
// per room every pass.
func (f *Federation) SyncAll(ctx context.Context) {
	rooms, err := f.Svc.Store.AllChatRooms()
	if err != nil {
		return
	}
	now := f.Now()
	// Each hub's rooms in turn, a few hubs at once, each room within its
	// own budget and each hub within one for the pass: one slow or hostile
	// hub, or one bad room, holds up no other. A pass waits for its hubs
	// only so long; one still pulling is left to finish, and the next pass
	// skips it.
	type hubRooms struct {
		peer  *store.FedPeer
		rooms []string
	}
	var order []int64
	byHub := map[int64]*hubRooms{}
	f.hubsMu.Lock()
	for i := range rooms {
		r := &rooms[i]
		if r.HubInstance == "" || r.ReplicaStatus != "live" {
			continue
		}
		peer, err := f.hubPeer(r)
		if err != nil || peer.URL == "" || f.syncing[peer.ID] {
			continue
		}
		if b, ok := f.hubs[peer.ID]; ok && now.Before(b.next) {
			continue
		}
		if poll, drop := f.watch(r); !poll {
			if drop {
				f.Svc.Store.DeleteChatDecoy(r.ID, f.Svc.instanceOrEmpty())
			}
			continue
		}
		h := byHub[peer.ID]
		if h == nil {
			h = &hubRooms{peer: peer}
			byHub[peer.ID] = h
			order = append(order, peer.ID)
		}
		h.rooms = append(h.rooms, r.ID)
	}
	if f.syncing == nil {
		f.syncing = map[int64]bool{}
	}
	for _, id := range order {
		f.syncing[id] = true
	}
	f.hubsMu.Unlock()
	// The whole pass, hubs it left running included, has a budget too.
	ctx, cancelPass := context.WithTimeout(ctx, syncPassBudget)
	sem := make(chan struct{}, syncWorkers)
	var wg sync.WaitGroup
	for _, id := range order {
		h := byHub[id]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				f.hubsMu.Lock()
				delete(f.syncing, h.peer.ID)
				f.hubsMu.Unlock()
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			hctx, cancel := context.WithTimeout(ctx, syncHubBudget)
			defer cancel()
			answered := false
			for _, roomID := range h.rooms {
				rctx, rcancel := context.WithTimeout(hctx, syncRoomBudget)
				err := f.syncRoom(rctx, h.peer, roomID)
				rcancel()
				if hctx.Err() != nil {
					return
				}
				if errors.Is(err, errHubDown) {
					// Its first failure skips the rest of its rooms.
					f.hubsMu.Lock()
					b := f.hubs[h.peer.ID]
					b.next = now.Add(backoff.Exp(pollEvery, hubBackoffMax, b.fails))
					b.fails++
					f.hubs[h.peer.ID] = b
					f.hubsMu.Unlock()
					f.Svc.Log.Debug("chat fed: hub unreachable; its rooms wait", "hub", h.peer.Name, "retry_at", b.next, "err", err)
					return
				}
				answered = true
				if err != nil {
					f.Svc.Log.Debug("chat fed: sync", "room", roomID, "err", err)
				}
			}
			// A hub that answered and never failed this pass is off the
			// ladder.
			if answered {
				f.hubsMu.Lock()
				delete(f.hubs, h.peer.ID)
				f.hubsMu.Unlock()
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		cancelPass()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(syncPassWait):
	case <-ctx.Done():
	}
}

// syncWorkers is how many hubs SyncAll pulls from at once, and
// syncRoomBudget how long one room's pull may take before it gives way.
const (
	syncWorkers    = 4
	syncRoomBudget = 2 * time.Minute
	syncHubBudget  = 4 * time.Minute
	syncPassBudget = 10 * time.Minute
)

// syncPassWait is how long a poll pass waits for its hubs before the next
// can begin; a var so tests can shorten it.
var syncPassWait = 30 * time.Second

// hubPeer is the paired household hosting room. A hub we hold no pairing
// with leaves us nothing to relay through, so it reads as not taking part.
func (f *Federation) hubPeer(room *store.ChatRoom) (*store.FedPeer, error) {
	peer, err := fed.PeerForHousehold(f.Svc.Store, room.HubInstance)
	if err != nil {
		return nil, fmt.Errorf("%w: the room's hub is not a paired household", ErrNotParticipant)
	}
	return peer, nil
}

// RelaySubmit forwards a local member's event to the room's hub and mirrors
// the accepted event into the replica.
func (f *Federation) RelaySubmit(ctx context.Context, actor *Actor, room *store.ChatRoom, in SubmitInput) (*Result, error) {
	peer, err := f.hubPeer(room)
	if err != nil {
		return nil, err
	}
	if actor.Device == nil {
		return nil, ErrNeedDevice
	}
	body, _ := json.Marshal(map[string]any{"actor": actor.Addr, "deviceKey": people.EncodeKey(actor.Device.DeviceKey), "input": in})
	resp, err := f.Fed.SignedPostVerified(ctx, peer, "/fed/v1/chat/rooms/"+room.ID+"/submit", "application/json", body)
	if err != nil {
		return nil, translateRemote(err)
	}
	var out struct {
		Event    store.ChatEvent `json:"event"`
		Replayed bool            `json:"replayed"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, err
	}
	fresh, _ := f.Svc.Store.ChatRoom(room.ID)
	if fresh != nil && out.Event.Seq == fresh.Seq+1 {
		f.Svc.appendReplica(fresh, &out.Event)
		if out.Event.Kind == EventCommit {
			f.refreshMeta(ctx, peer, fresh)
		}
	} else if fresh != nil && out.Event.Seq > fresh.Seq+1 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), syncRoomBudget)
			defer cancel()
			f.SyncRoom(ctx, peer, room.ID)
		}()
	}
	return &Result{Event: &out.Event, Replayed: out.Replayed}, nil
}

// RemoteError is a hub's refusal, carried back to the local client with its
// code intact so epoch_stale still means "sync and retry".
type RemoteError struct {
	Status int
	Code   string
	Msg    string
}

func (e *RemoteError) Error() string { return e.Msg }

func translateRemote(err error) error {
	var re *fed.RemoteError
	if !errors.As(err, &re) {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	// A refusal's code steers the client — epoch_stale means sync and
	// retry, forbidden means stop — so only one the hub signed is read as
	// the hub's. Anything else is an answer from nobody we can name.
	if !re.Verified {
		return fmt.Errorf("hub unreachable: an unsigned answer (%d): %w", re.Status, err)
	}
	switch re.Code {
	case "epoch_stale":
		return ErrEpochStale
	case "forbidden":
		return fmt.Errorf("%w: %s", ErrForbidden, re.Message)
	case "need_device":
		return ErrNeedDevice
	case "archived":
		return ErrArchived
	case "bad_request":
		return fmt.Errorf("%w: %s", ErrBadInput, re.Message)
	case "moving":
		return ErrRoomMoving
	case "need_group_info":
		return ErrNeedGroupInfo
	case "adopt_failed":
		return fmt.Errorf("the other household could not take the room over: %s", re.Message)
	}
	return &RemoteError{Status: re.Status, Code: re.Code, Msg: re.Message}
}

// RelayGroupInfo forwards a member's GroupInfo to the hub.
func (f *Federation) RelayGroupInfo(ctx context.Context, actor *Actor, room *store.ChatRoom, epoch int64, info []byte) error {
	peer, err := f.hubPeer(room)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"actor": actor.Addr, "epoch": epoch, "groupInfo": people.EncodeKey(info)})
	if _, err := f.Fed.SignedPostVerified(ctx, peer, "/fed/v1/chat/rooms/"+room.ID+"/groupinfo", "application/json", body); err != nil {
		return translateRemote(err)
	}
	return nil
}

// RelayUpdate forwards a local member's change to a room hubbed elsewhere:
// the hub alone decides labels, roles, archiving and who has left, and the
// replica takes the result from its snapshot, never from a local write.
func (f *Federation) RelayUpdate(ctx context.Context, actor *Actor, room *store.ChatRoom, patch RoomPatch) error {
	body := map[string]any{"actor": actor.Addr, "patch": patch}
	// A device leaving by itself is named, by the household that holds its
	// session.
	if patch.LeaveDevice && actor.Device != nil {
		body["deviceKey"] = people.EncodeKey(actor.Device.DeviceKey)
	}
	return f.relayUpdate(ctx, actor, room, body)
}

// RelayIdentityReset tells a room's hub that one of our people's identity
// was reset here, so it takes them out of the room as a hub does for its own.
func (f *Federation) relayUpdate(ctx context.Context, actor *Actor, room *store.ChatRoom, body map[string]any) error {
	peer, err := f.hubPeer(room)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(body)
	if _, err := f.Fed.SignedPostVerified(ctx, peer, "/fed/v1/chat/rooms/"+room.ID+"/update", "application/json", b); err != nil {
		return translateRemote(err)
	}
	if fresh, err := f.Svc.Store.ChatRoom(room.ID); err == nil {
		f.refreshMeta(ctx, peer, fresh)
	}
	return nil
}

// FetchGroupInfo asks the hub for the current GroupInfo.
func (f *Federation) FetchGroupInfo(ctx context.Context, actor *Actor, room *store.ChatRoom) ([]byte, int64, error) {
	peer, err := f.hubPeer(room)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		GroupInfo string `json:"groupInfo"`
		Epoch     int64  `json:"epoch"`
	}
	q := url.Values{"actor": {actor.Addr}}
	if actor.Device != nil {
		q.Set("deviceKey", people.EncodeKey(actor.Device.DeviceKey))
	}
	if _, err := f.Fed.SignedGetJSON(ctx, peer, "/fed/v1/chat/rooms/"+room.ID+"/groupinfo?"+q.Encode(), "", &out); err != nil {
		return nil, 0, translateRemote(err)
	}
	gi, err := people.DecodeKey(out.GroupInfo)
	if err != nil {
		return nil, 0, err
	}
	// The log this fog serves its people must reach what that GroupInfo
	// says: pulled now, after it was read, so a newcomer checking a Welcome,
	// or a device rejoining, never finds this copy a commit short of it.
	if room.Epoch < out.Epoch {
		sctx, cancel := context.WithTimeout(ctx, syncRoomBudget)
		f.syncRoom(sctx, peer, room.ID)
		cancel()
	}
	return gi, out.Epoch, nil
}

// RelayBlob uploads a member's encrypted attachment to the hub, keeping a
// local copy so the follower can serve it without a round trip.
//
// declared is the upload's stated length (-1 if none), held to here as on
// the hub.
func (f *Federation) RelayBlob(ctx context.Context, actor *Actor, room *store.ChatRoom, r io.Reader, declared int64) (*store.ChatBlob, error) {
	peer, err := f.hubPeer(room)
	if err != nil {
		return nil, err
	}
	local, err := f.Svc.PutBlob(actor, room, r, declared)
	if err != nil {
		return nil, err
	}
	// Our copy stays only if the hub took the blob: a copy the hub refused
	// would be an attachment nobody can reference.
	fresh := local.UploaderAddr == actor.Addr && local.RoomID == room.ID
	fail := func(err error) (*store.ChatBlob, error) {
		if fresh {
			if p, perr := f.Svc.blobPath(local.SHA256); perr == nil {
				os.Remove(p)
			}
			f.Svc.Store.DeleteChatBlob(local.SHA256)
		}
		return nil, err
	}
	path, err := f.Svc.blobPath(local.SHA256)
	if err != nil {
		return fail(err)
	}
	fh, err := os.Open(path)
	if err != nil {
		return fail(err)
	}
	defer fh.Close()
	// In pieces: the federation plane seals each request whole, path and
	// headers included, under a cap of BlobChunk; each piece leaves room
	// for them.
	upload, _ := fed.NewULID()
	// A relay that fails partway tells the hub, so what it reserved for the
	// whole goes at once rather than when it expires.
	abort := func() {
		actx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		q := url.Values{"actor": {actor.Addr}, "upload": {upload}, "abort": {"1"}}
		if resp, err := f.Fed.SignedDoHeaders(actx, peer, "POST", "/fed/v1/chat/rooms/"+room.ID+"/blobs?"+q.Encode(), "application/octet-stream", nil, nil); err == nil {
			resp.Body.Close()
		}
	}
	buf := make([]byte, BlobChunk-relayHeadroom)
	for off := int64(0); off < local.Size; {
		n, err := io.ReadFull(fh, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			if off > 0 {
				abort()
			}
			return fail(err)
		}
		q := url.Values{"actor": {actor.Addr}, "upload": {upload}, "offset": {strconv.FormatInt(off, 10)}, "total": {strconv.FormatInt(local.Size, 10)}}
		resp, err := f.Fed.SignedDoHeaders(ctx, peer, "POST", "/fed/v1/chat/rooms/"+room.ID+"/blobs?"+q.Encode(), "application/octet-stream", buf[:n], nil)
		if err != nil {
			abort()
			return fail(translateRemote(err))
		}
		resp.Body.Close()
		off += int64(n)
	}
	return local, nil
}

// relayHeadroom is what a relayed piece leaves of BlobChunk for the sealed
// request's own framing.
const relayHeadroom = 64 << 10

// FetchBlob pulls an attachment from the hub into the local cache. The name
// is the blob's digest, so the bytes are checked against it before they
// land: a hub can withhold a blob, but not substitute one.
func (f *Federation) FetchBlob(ctx context.Context, room *store.ChatRoom, sha string) error {
	peer, err := f.hubPeer(room)
	if err != nil {
		return err
	}
	return f.fetchBlobFrom(ctx, peer, room.ID, sha)
}

// fetchBlobFrom pulls one blob of a room from a peer: the hub, or after a
// hand-over the old hub serving its copy to the new one.
func (f *Federation) fetchBlobFrom(ctx context.Context, peer *store.FedPeer, roomID, sha string) error {
	dst, err := f.Svc.blobPath(sha)
	if err != nil {
		return err
	}
	resp, err := f.Fed.SignedDo(ctx, peer, "GET", "/fed/v1/chat/rooms/"+roomID+"/blob/"+sha, "", nil)
	if err != nil {
		return translateRemote(err)
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// A unique temp name: two fetches of one blob (a reader and a hand-over
	// copy) must not write into each other's file.
	fh, err := os.CreateTemp(filepath.Dir(dst), ".fetch-*")
	if err != nil {
		return err
	}
	tmp := fh.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(fh, h), io.LimitReader(resp.Body, MaxBlob+1))
	fh.Close()
	switch {
	case err != nil:
	case n > MaxBlob:
		err = fmt.Errorf("%w: hub sent more than %d bytes for blob %s", ErrBadInput, MaxBlob, sha)
	case hex.EncodeToString(h.Sum(nil)) != sha:
		err = fmt.Errorf("%w: hub sent bytes that don't match blob %s", ErrBadInput, sha)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// Copies of other hubs' blobs are a cache, held to QuotaCache: the
	// oldest go to make room, and one that can't fit isn't kept.
	f.Svc.blobMu.Lock()
	defer f.Svc.blobMu.Unlock()
	if err := f.Svc.makeCacheRoom(n); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	// The hub says when in the room's life the blob was stored, so this fog
	// can hold its own former members to what they may still fetch.
	seq, _ := strconv.ParseInt(resp.Header.Get(BlobSeqHeader), 10, 64)
	return f.Svc.Store.PutChatBlob(&store.ChatBlob{SHA256: sha, RoomID: roomID, Size: n, UploaderAddr: "hub", RoomSeq: seq})
}

// QuotaCache bounds the copies of other hubs' attachments this fog keeps.
var QuotaCache int64 = 50 << 30

// makeCacheRoom evicts the oldest cached copies of other hubs' blobs until
// n more bytes fit in QuotaCache. The caller holds blobMu.
func (s *Service) makeCacheRoom(n int64) error {
	if n > QuotaCache {
		return fmt.Errorf("%w: too large to keep a copy of", ErrQuota)
	}
	used, err := s.Store.ChatCacheUsage()
	if err != nil {
		return err
	}
	for used+n > QuotaCache {
		old, err := s.Store.OldestCachedChatBlobs(32)
		if err != nil || len(old) == 0 {
			return fmt.Errorf("%w: no room to keep a copy", ErrQuota)
		}
		for _, b := range old {
			if p, perr := s.blobPath(b.SHA256); perr == nil {
				os.Remove(p)
			}
			s.Store.DeleteChatBlob(b.SHA256)
			used -= b.Size
			if used+n <= QuotaCache {
				break
			}
		}
	}
	return nil
}

// BlobSeqHeader carries a blob's room seq on the fed blob fetch.
const BlobSeqHeader = "X-Fog-Blob-Seq"

// SendEphemeral relays typing/presence to a room's hub or, on the hub, to
// every follower — best effort, never queued. Followers are dialled at
// once, each with its own timeout, so one unreachable household neither
// delays the rest nor eats their time.
func (f *Federation) SendEphemeral(room *store.ChatRoom, person, signal string) {
	if signal == "typing" && !f.typing.Allow(room.ID+"\x00"+person, typingEvery, f.Now()) {
		return
	}
	body, _ := json.Marshal(map[string]string{"roomId": room.ID, "person": person, "signal": signal})
	post := func(peer *store.FedPeer) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.Fed.SignedPost(ctx, peer, "/fed/v1/chat/ephemeral", "application/json", body)
	}
	if room.HubInstance != "" {
		if peer, err := f.hubPeer(room); err == nil {
			post(peer)
		}
		return
	}
	home, _, _ := ParseAddr(person)
	var wg sync.WaitGroup
	for _, peer := range f.followers(room.ID) {
		if peer.InstanceID == home {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(&peer)
		}()
	}
	wg.Wait()
}

// PeerCut handles a peer being unpaired or losing the chat grant: their
// people leave every room hubbed here, and every replica of their rooms is
// archived (read-only; the copy stays until the operator decides).
func (f *Federation) PeerCut(peer *store.FedPeer, reason string) {
	// Its invites still waiting to be filed are dropped: nothing it sent
	// before the cut is acted on after it.
	f.inviteMu.Lock()
	if q := f.inviteQs[peer.ID]; q != nil {
		f.inviteBytes -= q.bytes
		q.pending, q.weight, q.order, q.bytes = map[string][]item{}, map[string]int64{}, nil, 0
	}
	f.inviteMu.Unlock()
	rooms, err := f.Svc.Store.AllChatRooms()
	if err != nil {
		return
	}
	for i := range rooms {
		r := &rooms[i]
		if r.HubInstance == peer.InstanceID {
			f.Svc.Store.SetChatRoomReplicaStatus(r.ID, "archived")
			f.Svc.Bus.PublishRoom(r.ID, Notification{Kind: "rooms", RoomID: r.ID}, nil)
			continue
		}
		if r.HubInstance != "" {
			continue
		}
		parts, _ := f.Svc.Store.ChatParticipants(r.ID)
		var gone []string
		for _, p := range parts {
			if p.LeftSeq == 0 && p.HomeInstance == peer.InstanceID {
				gone = append(gone, p.PersonAddr)
			}
		}
		if len(gone) > 0 {
			// Frozen mid-move: applied once the move settles, if the room
			// stays here. A room that moved on is its new hub's to judge.
			f.Svc.markLeftOrAfterMove(r, gone, SystemHouseholdCut, reason)
		}
	}
	// Whatever was queued for them will never be read.
	f.Svc.Store.DropFedOutboxKind(peer.ID, OutboxKind)
}

// ---- helpers on the service the federation layer needs ----

// MarkLeft records people as out of a room hubbed here and appends a system
// event of the given kind naming them, so members' clients commit the MLS
// removal. Their access to what follows ends with that event.
func (s *Service) MarkLeft(room *store.ChatRoom, addrs []string, kind, reason string) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	return s.markLeftLocked(room, addrs, kind, reason)
}

// markLeftLocked is MarkLeft for a caller already holding commitMu. It
// fails with ErrRoomMoving while the room is frozen for a hand-over.
func (s *Service) markLeftLocked(room *store.ChatRoom, addrs []string, kind, reason string) error {
	var out []string
	for _, a := range addrs {
		if p, err := s.Store.ChatParticipant(room.ID, a); err == nil && p.LeftSeq == 0 {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	fresh, err := s.Store.ChatRoom(room.ID)
	if err != nil {
		return err
	}
	body := map[string]any{"kind": kind, "left": out}
	if reason != "" {
		body["reason"] = reason
	}
	payload, _ := json.Marshal(body)
	id, _ := fed.NewULID()
	c := &store.ChatCommit{
		Event: &store.ChatEvent{RoomID: room.ID, ID: id, Epoch: fresh.Epoch, Kind: EventSystem, Payload: payload},
		Left:  out,
	}
	if err := s.Store.AppendChatEvent(c); err != nil {
		if errors.Is(err, store.ErrChatRoomFrozen) {
			return ErrRoomMoving
		}
		s.Log.Warn("chat: mark left", "room", room.ID, "err", err)
		return err
	}
	fresh, _ = s.Store.ChatRoom(room.ID)
	s.announce(fresh, c.Event)
	s.departed(fresh, c.Event, out)
	s.Store.Audit("fog", "chat.left", room.ID, store.AuditDetail(map[string]any{"kind": kind, "people": out}))
	return nil
}

// markLeftOrAfterMove takes people out of a room hubbed here, and when the
// room is frozen for a hand-over, does so once the move settles: here if it
// stayed, or by word to the new hub (QueueDeparture) if it went — for this
// household's people, the only ones it may speak for there.
func (s *Service) markLeftOrAfterMove(room *store.ChatRoom, addrs []string, kind, reason string) {
	if err := s.MarkLeft(room, addrs, kind, reason); !errors.Is(err, ErrRoomMoving) {
		return
	}
	go func() {
		deadline := time.Now().Add(10 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			fresh, err := s.Store.ChatRoom(room.ID)
			if err != nil {
				return
			}
			if fresh.HubInstance == "" && fresh.ReplicaStatus == replicaFrozen {
				continue
			}
			if fresh.HubInstance == "" {
				s.MarkLeft(fresh, addrs, kind, reason)
				return
			}
			me := s.instanceOrEmpty()
			for _, a := range addrs {
				if home, _, _ := ParseAddr(a); home == me && s.Fed != nil {
					s.Fed.QueueDeparture(a, fresh)
				}
			}
			return
		}
	}()
}

// departed ends what people taken out of a room by ev still receive: their
// open connections stop following the room now (they have just been sent
// ev), and a household that has no one left in the room is sent ev too —
// it is no longer a follower, but it must learn its people are out.
func (s *Service) departed(room *store.ChatRoom, ev *store.ChatEvent, left []string) {
	if len(left) == 0 {
		return
	}
	for _, a := range left {
		s.Bus.Leave(a, room.ID)
		s.Bus.PublishPerson(a, Notification{Kind: "rooms", RoomID: room.ID})
		s.Store.DeleteChatWebhooksBy(room.ID, a) // a webhook leaves with its maker
	}
	if s.Fed != nil && room.HubInstance == "" {
		s.Fed.notifyDeparted(room, ev, left)
	}
}

// appendReplica stores a hub-sequenced event as-is and tells local clients.
func (s *Service) appendReplica(room *store.ChatRoom, ev *store.ChatEvent) {
	s.appendReplicaAt(room, ev, false)
}

// appendReplicaPulled applies an event a pull from the hub returned: the
// next the hub holds, which may skip seqs it no longer has.
func (s *Service) appendReplicaPulled(room *store.ChatRoom, ev *store.ChatEvent) {
	s.appendReplicaAt(room, ev, true)
}

// skipReplicaTo moves a replica past the end of a hub's log whose last
// events are gone from it.
func (s *Service) skipReplicaTo(room *store.ChatRoom, seq int64) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := s.Store.SkipChatReplicaSeq(room.ID, seq); err == nil && seq > room.Seq {
		room.Seq = seq
	}
}

func (s *Service) appendReplicaAt(room *store.ChatRoom, ev *store.ChatEvent, pulled bool) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	fresh, err := s.Store.ChatRoom(room.ID)
	if err != nil || ev.Seq <= fresh.Seq || (!pulled && ev.Seq != fresh.Seq+1) {
		return
	}
	// What the hub says an MLS event is must be what its bytes are: a commit
	// a public commit of this group at the epoch the hub gives it, a
	// private message a private application message of this group. Anything
	// else is the hub's fault, kept in its place (the seq) as a system note
	// that it was dropped, never handed to our people's clients.
	if !replicaEventSound(fresh, ev) {
		note, _ := json.Marshal(map[string]any{"kind": "hub-fault", "seq": ev.Seq})
		bad := *ev
		ev = &bad
		ev.Kind, ev.Payload, ev.Hints = EventSystem, note, ""
	}
	epoch := fresh.Epoch
	if ev.Kind == EventCommit {
		epoch = ev.Epoch + 1
	}
	if err := s.Store.ApplyChatReplicaEvent(room.ID, ev, epoch); err != nil {
		return
	}
	room.Seq = ev.Seq
	room.Epoch = epoch
	e := *ev
	e.RoomID = room.ID
	s.Bus.PublishRoom(room.ID, Notification{Kind: "event", RoomID: room.ID, Event: &e}, nil)
	if s.Notify != nil {
		go s.Notify.Dispatch(room, &e)
	}
}

// replicaEventSound says an MLS event a hub sequenced is what it says it
// is: an mls_commit a public commit, an mls_private a private application
// message, of the room's group, the commit at the epoch the hub gives it.
func replicaEventSound(room *store.ChatRoom, ev *store.ChatEvent) bool {
	switch ev.Kind {
	case EventCommit, EventPrivate:
	default:
		return true
	}
	msg, err := mlsview.Parse(ev.Payload)
	if err != nil || (len(room.GroupID) > 0 && string(msg.GroupID) != string(room.GroupID)) {
		return false
	}
	if ev.Kind == EventCommit {
		return msg.WireFormat == framing.WireFormatPublicMessage && msg.Commit != nil && int64(msg.Epoch) == ev.Epoch
	}
	return msg.WireFormat == framing.WireFormatPrivateMessage && msg.ContentType == framing.ContentTypeApplication
}

// departReplica marks this household's people out of a replica where the
// hub says they left, and archives it: it has no one left to serve, and the
// hub serves it nothing newer.
func (s *Service) departReplica(room *store.ChatRoom, rows []store.ChatParticipant) error {
	me := s.instanceOrEmpty()
	for _, p := range rows {
		if p.HomeInstance != me || p.LeftSeq == 0 {
			continue
		}
		if err := s.Store.MarkChatParticipantLeft(room.ID, p.PersonAddr, p.LeftSeq); err != nil {
			return err
		}
		s.Bus.Leave(p.PersonAddr, room.ID)
		s.Bus.PublishPerson(p.PersonAddr, Notification{Kind: "rooms", RoomID: room.ID})
	}
	return s.Store.SetChatRoomReplicaStatus(room.ID, "archived")
}

// refreshReplicaMeta copies the hub's public room state into the replica:
// policy, labels, epoch, tree, participants, group info.
func (s *Service) refreshReplicaMeta(room *store.ChatRoom, snap *RoomSnapshot) error {
	if snap.Departed {
		return s.departReplica(room, snap.Participants)
	}
	snap = s.vetSnapshot(room, snap)
	// A hub that doesn't tag its snapshots answers every re-sync in full;
	// one that says nothing new must not make every local member (and the
	// bot) refetch their room list.
	if snap.DM != "" && room.Kind == KindDM {
		s.Store.SetChatRoomDMKey(room.ID, snap.DM)
	}
	if s.replicaHolds(room.ID, snap) {
		return nil
	}
	// A decoy keeps nothing it would only weigh with: no tree, no verified
	// state, no group info — what its polls fetch is all it is for.
	if s.Store.ChatDecoyOnly(room.ID) {
		lite := *snap
		lite.Tree, lite.PublicState, lite.GroupInfo, lite.GroupInfoEp = "", "", nil, 0
		snap = &lite
	}
	if err := s.Store.ReplaceChatReplicaMeta(room.ID, &store.ChatReplicaMeta{
		Label: snap.Room.Label, TopicLabel: snap.Room.TopicLabel, Policy: snap.Policy,
		Epoch: snap.Room.Epoch, Tree: snap.Tree, ArchivedAt: snap.Room.ArchivedAt,
		Participants: snap.Participants, PublicState: snap.PublicState,
		GroupInfo: snap.GroupInfo, GroupInfoEpoch: snap.GroupInfoEp,
	}); err != nil {
		return err
	}
	me := s.instanceOrEmpty()
	for _, p := range snap.Participants {
		if p.HomeInstance == me {
			s.Bus.PublishPerson(p.PersonAddr, Notification{Kind: "rooms", RoomID: room.ID})
		}
	}
	return nil
}

// vetSnapshot holds a hub's snapshot to what it may say about our people.
// Each participant's household is the one their address names. One of our
// people shows as in the room only if they already are here, or were sent
// an invite to it: a hub can't put someone into a room they never joined.
func (s *Service) vetSnapshot(room *store.ChatRoom, snap *RoomSnapshot) *RoomSnapshot {
	me := s.instanceOrEmpty()
	out := *snap
	out.Participants = make([]store.ChatParticipant, 0, len(snap.Participants))
	for _, p := range snap.Participants {
		home, _, ok := ParseAddr(p.PersonAddr)
		if !ok {
			continue
		}
		p.HomeInstance = home
		if home == me && p.LeftSeq == 0 {
			cur, err := s.Store.ChatParticipant(room.ID, p.PersonAddr)
			if (err != nil || cur.LeftSeq != 0) && !s.Store.ChatInvitedTo(room.ID, p.PersonAddr) {
				continue
			}
		}
		// A row for one of our people under an identity key they don't hold
		// is someone before them under the same address — a deleted person
		// whose handle was given again, an identity since reset — and gives
		// whoever holds the address now nothing: not the room, not its past.
		if home == me && p.IdentityKey != "" && !s.holdsIdentity(p.PersonAddr, p.IdentityKey) {
			p.LeftSeq = -1
		}
		out.Participants = append(out.Participants, p)
	}
	return &out
}

// holdsIdentity says one of our people, by address, holds identity key idk.
func (s *Service) holdsIdentity(addr, idk string) bool {
	_, handle, _ := ParseAddr(addr)
	p, err := s.Store.PersonByHandle(handle)
	return err == nil && identityOf(p) == idk
}

// replicaHolds reports whether a replica already holds everything a
// snapshot would write: its metadata, its participants, its GroupInfo.
func (s *Service) replicaHolds(roomID string, snap *RoomSnapshot) bool {
	cur, err := s.Store.ChatRoom(roomID)
	if err != nil {
		return false
	}
	if cur.Label != snap.Room.Label || cur.TopicLabel != snap.Room.TopicLabel || cur.Policy != snap.Policy ||
		cur.Epoch < snap.Room.Epoch || cur.Tree != snap.Tree || cur.ArchivedAt != snap.Room.ArchivedAt ||
		(snap.PublicState != "" && cur.PublicState != snap.PublicState) {
		return false
	}
	if len(snap.GroupInfo) > 0 {
		gi, ep, err := s.Store.ChatGroupInfo(roomID)
		if err != nil || ep != snap.GroupInfoEp || !bytes.Equal(gi, snap.GroupInfo) {
			return false
		}
	}
	parts, err := s.Store.ChatParticipants(roomID)
	if err != nil || len(parts) != len(snap.Participants) {
		return false
	}
	// Notify is the person's own setting, kept here, never the hub's.
	key := func(p store.ChatParticipant) store.ChatParticipant {
		p.RoomID = ""
		p.Notify = ""
		return p
	}
	have := make(map[string]store.ChatParticipant, len(parts))
	for _, p := range parts {
		have[p.PersonAddr] = key(p)
	}
	for _, p := range snap.Participants {
		if h, ok := have[p.PersonAddr]; !ok || h != key(p) {
			return false
		}
	}
	return true
}

// instanceOrEmpty is this fog's instance id, or "" when it cannot be known.
func (s *Service) instanceOrEmpty() string {
	id, _ := s.Instance()
	return id
}

// IsLocal says whether an address belongs to this household.
func (s *Service) IsLocal(addr string) bool {
	home, _, ok := ParseAddr(addr)
	return ok && home == s.instanceOrEmpty()
}

// ReceiveRaw decodes a notify envelope's items and applies them.
func (f *Federation) ReceiveRaw(ctx context.Context, peer *store.FedPeer, raw []json.RawMessage) []map[string]string {
	items := make([]item, 0, len(raw))
	for _, r := range raw {
		var it item
		if json.Unmarshal(r, &it) == nil {
			items = append(items, it)
		}
	}
	return f.Receive(ctx, peer, items)
}

// BoundRoomID mints a room id bound to the household that makes it: a
// random head and a tail that hashes the household's instance id with it.
// Another household can't mint an id bound to this one by chance, so a
// replica can tell a room's own hub from a squatter (claimRoomID).
func BoundRoomID(instance string) (string, error) {
	u, err := fed.NewULID()
	if err != nil {
		return "", err
	}
	return u[:16] + roomIDTail(instance, u[:16]), nil
}

// RoomIDBoundTo says a room id was minted by BoundRoomID for instance.
func RoomIDBoundTo(id, instance string) bool {
	return len(id) == 26 && instance != "" && id[16:] == roomIDTail(instance, id[:16])
}

func roomIDTail(instance, head string) string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	sum := sha256.Sum256([]byte("fog-chat-room/1\n" + instance + "\n" + head))
	var out [10]byte
	acc, bits, j := uint64(0), 0, 0
	for _, b := range sum {
		acc = acc<<8 | uint64(b)
		bits += 8
		for bits >= 5 && j < len(out) {
			bits -= 5
			out[j] = crockford[(acc>>bits)&31]
			j++
		}
		if j == len(out) {
			break
		}
	}
	return string(out[:])
}
