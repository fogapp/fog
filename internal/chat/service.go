// Package chat is the sealed room server (docs/chat-contract.md): the
// hub that orders, authorizes, stores, and delivers MLS ciphertext it cannot
// read. It knows who is in a room from the public half of the group
// (mlsview), enforces the room policy on commits, and hands the log to
// clients and — in Phase 2 — to follower fogs.
package chat

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thomas-vilte/mls-go/framing"
	"github.com/thomas-vilte/mls-go/group"

	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	KindChannel = "channel"
	KindGroup   = "group"
	KindDM      = "dm"

	EventPrivate  = "mls_private"
	EventCommit   = "mls_commit"
	EventProposal = "mls_proposal"
	EventSystem   = "system"

	// PolicyExtensionType is the GroupContext extension that carries the
	// room policy JSON, in the private-use range.
	PolicyExtensionType = 0xF0F0

	// MaxPayload bounds one MLS message on the wire.
	MaxPayload = 1 << 20
	// MaxBlob bounds one encrypted attachment.
	MaxBlob = 200 << 20
	// EpochSlack is how many epochs behind current a private message may be
	// and still be accepted — a sender racing a commit.
	EpochSlack = 2
)

var (
	ErrNotParticipant  = errors.New("you are not in this room")
	ErrEpochStale      = errors.New("the room moved on; sync and retry")
	ErrGroupMismatch   = errors.New("message is for a different group")
	ErrNotYourLeaf     = errors.New("the sender leaf is not your device")
	ErrForbidden       = errors.New("your role does not allow that")
	ErrUnknownDevice   = errors.New("a key package names a device fog does not know")
	ErrUnknownPerson   = errors.New("a key package names a person fog does not know")
	ErrFixedMembership = errors.New("this room's membership is fixed")
	ErrNeedDevice      = errors.New("register a device before using chat")
	ErrUnsupported     = errors.New("standalone proposals are not supported; inline them in a commit")
	ErrBadInput        = errors.New("bad input")
	ErrJoinRefused     = errors.New("this room is not open to you")
	ErrArchived        = errors.New("this room is archived")
	// ErrUnverifiedDevice: a device its person's identity key has not
	// attested — one that fog, or whoever holds the person's sign-in, could
	// have registered — may not be put into a room, by anyone.
	ErrUnverifiedDevice = errors.New("that device is not verified by its person's identity key")
	// ErrNeedGroupInfo: a room from before the hub verified commits has no
	// verified state yet; a member must publish GroupInfo for its epoch.
	ErrNeedGroupInfo = errors.New("this room needs a member to publish its group info for the current epoch first")
)

// System event kinds that take people out of a room. Each names them in
// "left"; the hub has already counted them out, and the first member device
// to see it commits the MLS removal of their leaves.
const (
	SystemHouseholdCut  = "household-cut"
	SystemBanned        = "banned"
	SystemLeft          = "left"
	SystemIdentityReset = "identity-reset"
)

// Service is the chat server for one fog.
type Service struct {
	Store   *store.Store
	Log     *slog.Logger
	DataDir string
	// Instance returns this fog's instance ULID (minting it if needed).
	Instance func() (string, error)
	Bus      *Bus
	// Household names this fog for room labels and impact sentences.
	Household func() string

	// OnAccepted runs after an event is sequenced — federation fan-out hooks
	// here in Phase 2.
	OnAccepted func(room *store.ChatRoom, ev *store.ChatEvent)

	// bot is fog's own seat, once started; nil until main brings it up,
	// which happens while requests are already being served.
	bot atomic.Pointer[Bot]

	// Fed is the cross-household layer. fogd always sets it; the nil
	// checks that remain are for unit tests that build a bare Service.
	Fed *Federation
	// Notify pushes to this household's people; nil means quiet.
	Notify *Notifier

	// commitMu serializes commit application: the hub must see commits in
	// one order, and SQLite has one writer anyway.
	commitMu sync.Mutex

	// Keys bounds how many one-use KeyPackages each asker consumes.
	Keys KeyBudget

	// blobMu makes an attachment's quota check and its storing one step,
	// and guards reserved: the declared totals of chunked uploads under
	// way, held against the quotas from their first piece.
	blobMu   sync.Mutex
	reserved map[string]*reservation
}

// reservation is one chunked upload's declared size, held from its start.
type reservation struct {
	addr, room string
	total      int64
	at         time.Time
}

// Actor is who is speaking: a person, from a device.
type Actor struct {
	Person *store.Person
	Device *store.PersonDevice
	Addr   string
}

// Addr forms a person's address on this fog: fog:<instance>/<handle>.
func (s *Service) Addr(handle string) (string, error) {
	inst, err := s.Instance()
	if err != nil {
		return "", err
	}
	return "fog:" + inst + "/" + handle, nil
}

// ParseAddr splits fog:<instance>/<handle>.
func ParseAddr(addr string) (instance, handle string, ok bool) {
	if !strings.HasPrefix(addr, "fog:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(addr, "fog:")
	i := strings.IndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// ActorFor resolves a person and device into an Actor.
func (s *Service) ActorFor(p *store.Person, d *store.PersonDevice) (*Actor, error) {
	addr, err := s.Addr(p.Handle)
	if err != nil {
		return nil, err
	}
	return &Actor{Person: p, Device: d, Addr: addr}, nil
}

func (a *Actor) leaf() mlsview.Leaf {
	return mlsview.Leaf{Identity: a.Addr, SignatureKey: a.Device.DeviceKey}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ---- rooms ----

type CreateRoomInput struct {
	Kind             string   `json:"kind"`
	Label            string   `json:"label"`
	Topic            string   `json:"topic"`
	GroupID          string   `json:"groupId"` // base64url, client-chosen
	Policy           *Policy  `json:"policy"`
	Participants     []string `json:"participants"` // DM: the other people's addresses
	WorkspaceVisible *bool    `json:"workspaceVisible"`
	// GroupInfo is the new group's GroupInfo at epoch 0 with its ratchet
	// tree (base64url MLSMessage): what the hub verifies every later commit
	// against.
	GroupInfo string `json:"groupInfo"`
}

// CreateRoom opens a room hubbed here with the actor's device as leaf 0. The
// client has already created the MLS group locally with the same group id.
func (s *Service) CreateRoom(actor *Actor, in CreateRoomInput) (*store.ChatRoom, error) {
	if actor.Device == nil {
		return nil, ErrNeedDevice
	}
	// The creator's device is the group's first leaf: one their identity
	// key attests, like every leaf after it.
	if _, err := s.attestedDevice(actor.Addr, actor.Device.DeviceKey, s.instanceOrEmpty()); err != nil {
		return nil, err
	}
	switch in.Kind {
	case KindChannel, KindGroup, KindDM:
	default:
		return nil, fmt.Errorf("%w: kind must be channel, group, or dm", ErrBadInput)
	}
	gid, err := people.DecodeKey(in.GroupID)
	if err != nil || len(gid) < 8 || len(gid) > 64 {
		return nil, fmt.Errorf("%w: groupId must be 8–64 bytes", ErrBadInput)
	}
	if _, err := s.Store.ChatRoomByGroupID(gid); err == nil {
		return nil, fmt.Errorf("%w: that group id is taken", ErrBadInput)
	}
	pol := DefaultPolicy(in.Kind)
	if in.Policy != nil {
		pol = *in.Policy
		if err := pol.Validate(); err != nil {
			return nil, fmt.Errorf("%w: policy: %v", ErrBadInput, err)
		}
	}
	label := people.ValidDisplayName(in.Label)
	if in.Kind == KindChannel && label == "" {
		return nil, fmt.Errorf("%w: a channel needs a label", ErrBadInput)
	}
	visible := in.Kind == KindChannel
	if in.WorkspaceVisible != nil {
		visible = *in.WorkspaceVisible && in.Kind == KindChannel
	}
	dmKey := ""
	if in.Kind == KindDM {
		set := map[string]bool{actor.Addr: true}
		for _, p := range in.Participants {
			c, err := s.CanonicalAddr(p)
			if err != nil {
				return nil, fmt.Errorf("%w: bad participant address %q", ErrBadInput, p)
			}
			set[c] = true
		}
		if len(set) < 2 || len(set) > 9 {
			return nil, fmt.Errorf("%w: a DM has 2–9 people", ErrBadInput)
		}
		addrs := make([]string, 0, len(set))
		for a := range set {
			addrs = append(addrs, a)
		}
		sort.Strings(addrs)
		dmKey = strings.Join(addrs, ",")
		if existing, err := s.Store.ChatRoomByDMKey(dmKey); err == nil {
			// The DM already exists: it is shown only to someone still in
			// it. Someone who left needs a member to add them back.
			if _, err := s.Participant(existing, actor.Addr); err != nil {
				return nil, fmt.Errorf("%w: you are not in this DM any more; someone in it must add you back", ErrNotParticipant)
			}
			return existing, nil
		}
		visible = false
	}
	// The group's public state starts from the creator's GroupInfo: one
	// leaf, the creator's device, at epoch 0 of this group.
	giWire, err := people.DecodeKey(in.GroupInfo)
	if err != nil || len(giWire) == 0 || len(giWire) > MaxPayload {
		return nil, fmt.Errorf("%w: groupInfo: the new group's GroupInfo, with its ratchet tree, is required", ErrBadInput)
	}
	pg, err := mlsview.FromGroupInfo(giWire)
	if err != nil {
		return nil, fmt.Errorf("%w: groupInfo: %v", ErrBadInput, err)
	}
	if m := pg.Members(); pg.Epoch() != 0 || string(pg.GroupID()) != string(gid) || len(m) != 1 ||
		m[0].Identity != actor.Addr || string(m[0].SignatureKey) != string(actor.Device.DeviceKey) {
		return nil, fmt.Errorf("%w: groupInfo is not this group at epoch 0 with only your device in it", ErrBadInput)
	}
	pgJSON, _ := json.Marshal(pg)
	// The room's id is bound to this household (BoundRoomID), so no other
	// can squat it on a fog its invite hasn't reached yet.
	id, err := BoundRoomID(s.instanceOrEmpty())
	if err != nil {
		return nil, err
	}
	tree := mlsview.NewTree(actor.leaf())
	treeJSON, _ := json.Marshal(tree)
	inst, _ := s.Instance()
	room := &store.ChatRoom{
		ID: id, GroupID: gid, Kind: in.Kind, Label: label, TopicLabel: people.ValidDisplayName(in.Topic),
		Policy: pol.JSON(), WorkspaceVisible: visible, Tree: string(treeJSON), DMKey: dmKey,
		ReplicaStatus: "live", CreatedBy: actor.Addr, PublicState: string(pgJSON),
	}
	if err := s.Store.CreateChatRoom(room); err != nil {
		return nil, err
	}
	s.Store.SetChatGroupInfo(id, 0, giWire)
	created, _ := json.Marshal(map[string]any{"kind": in.Kind, "label": label, "by": actor.Addr})
	c := &store.ChatCommit{
		Event: &store.ChatEvent{RoomID: id, ID: id + "-created", Epoch: 0, Kind: EventSystem, SenderAddr: actor.Addr, Payload: created},
		Added: []store.ChatParticipant{{PersonAddr: actor.Addr, HomeInstance: inst, RoleIndex: RoleOwner, IdentityKey: identityOf(actor.Person)}},
	}
	if err := s.Store.AppendChatEvent(c); err != nil {
		return nil, err
	}
	s.Store.Audit(actor.Person.Handle, "chat.room.create", id, store.AuditDetail(map[string]any{"kind": in.Kind, "label": label}))
	room, _ = s.Store.ChatRoom(id)
	s.Bus.PublishPerson(actor.Addr, Notification{Kind: "rooms"})
	return room, nil
}

// CanonicalAddr is the one address a person goes by in rooms:
// fog:<instance>/<handle>. A person's ULID names them too (person
// addresses), so every address that reaches a participant comparison — a
// leaf's identity, an actor, a DM's set, a role's target — is resolved here
// first, or a ban written against one form could be walked round with the
// other.
func (s *Service) CanonicalAddr(addr string) (string, error) {
	home, ref, ok := ParseAddr(addr)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownPerson, addr)
	}
	if home == s.instanceOrEmpty() {
		p, err := s.Store.PersonByRef(ref)
		if err != nil {
			return "", fmt.Errorf("%w: %q", ErrUnknownPerson, addr)
		}
		return "fog:" + home + "/" + p.Handle, nil
	}
	if s.Fed == nil {
		return "", fmt.Errorf("%w: %q", ErrUnknownPerson, addr)
	}
	peer, err := fed.PeerForHousehold(s.Store, home)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrUnknownPerson, addr)
	}
	for _, dp := range PeerPeople(peer) {
		if dp.Names(ref) {
			return "fog:" + home + "/" + dp.Handle, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownPerson, addr)
}

// requireCanonical refuses an address that isn't the person's canonical one.
func (s *Service) requireCanonical(addr string) error {
	c, err := s.CanonicalAddr(addr)
	if err != nil {
		return err
	}
	if c != addr {
		return fmt.Errorf("%w: %s goes by %s in rooms", ErrBadInput, addr, c)
	}
	return nil
}

// identityOf is a local person's identity key as participants record it.
func identityOf(p *store.Person) string {
	if p == nil || len(p.IdentityKey) == 0 {
		return ""
	}
	return people.EncodeKey(p.IdentityKey)
}

// Tree loads the hub's public view of a room.
func (s *Service) Tree(room *store.ChatRoom) (*mlsview.Tree, error) {
	t := &mlsview.Tree{}
	if room.Tree == "" {
		return t, nil
	}
	if err := json.Unmarshal([]byte(room.Tree), t); err != nil {
		return nil, err
	}
	return t, nil
}

// Participant returns the actor's current participation, or ErrNotParticipant.
func (s *Service) Participant(room *store.ChatRoom, addr string) (*store.ChatParticipant, error) {
	p, err := s.Store.ChatParticipant(room.ID, addr)
	if err != nil || p.LeftSeq != 0 {
		return nil, ErrNotParticipant
	}
	return p, nil
}

// Can says whether a current participant's role in a room holds a
// capability.
func (s *Service) Can(roomID, addr, capability string) bool {
	room, err := s.Store.ChatRoom(roomID)
	if err != nil {
		return false
	}
	p, err := s.Participant(room, addr)
	return err == nil && s.policyOf(room).Can(p.RoleIndex, capability)
}

func (s *Service) policyOf(room *store.ChatRoom) Policy {
	p, err := ParsePolicy(room.Policy)
	if err != nil {
		return DefaultPolicy(room.Kind)
	}
	return p
}

// RoomPatch is a change to a room's hub state, by someone in it: labels,
// listing, a role, archiving, or leaving. Notify settings are not here:
// they are the person's own, kept by their home fog.
type RoomPatch struct {
	Label            *string    `json:"label,omitempty"`
	Topic            *string    `json:"topic,omitempty"`
	WorkspaceVisible *bool      `json:"workspaceVisible,omitempty"`
	Role             *RolePatch `json:"role,omitempty"`
	Archive          bool       `json:"archive,omitempty"`
	Leave            bool       `json:"leave,omitempty"`
	// LeaveDevice takes only the actor's device out of the room: the person
	// stays, with their role and their other devices (DeviceLeave).
	LeaveDevice bool `json:"leaveDevice,omitempty"`
}

type RolePatch struct {
	Addr string `json:"addr"`
	Role int    `json:"role"`
}

// Empty says the patch asks for nothing the hub keeps.
func (p RoomPatch) Empty() bool {
	return p.Label == nil && p.Topic == nil && p.WorkspaceVisible == nil && p.Role == nil && !p.Archive && !p.Leave && !p.LeaveDevice
}

// UpdateRoom applies a patch to a room hubbed here. The hub is the one
// authority for this state: a follower relays the patch here
// (Federation.RelayUpdate) rather than write its replica, which the next
// snapshot would overwrite. Every check runs before anything is written.
func (s *Service) UpdateRoom(actor *Actor, room *store.ChatRoom, p RoomPatch) error {
	// A room frozen for a hand-over takes no change here: what the new hub
	// adopts is what the room was when it froze.
	if room.HubInstance == "" && room.ReplicaStatus == replicaFrozen {
		return ErrRoomMoving
	}
	if room.HubInstance != "" {
		return fmt.Errorf("%w: the room is hubbed by another household", ErrForbidden)
	}
	if p.LeaveDevice {
		return s.DeviceLeave(actor, room)
	}
	part, err := s.Participant(room, actor.Addr)
	if err != nil {
		return err
	}
	pol := s.policyOf(room)
	if p.Label != nil && !pol.Can(part.RoleIndex, CapChangeRoomName) {
		return ErrForbidden
	}
	if p.Topic != nil && !pol.Can(part.RoleIndex, CapChangeRoomTopic) {
		return ErrForbidden
	}
	if p.WorkspaceVisible != nil && room.Kind == KindChannel && !pol.Can(part.RoleIndex, CapChangePolicy) {
		return ErrForbidden
	}
	if p.Archive && !pol.Can(part.RoleIndex, CapChangePolicy) {
		return ErrForbidden
	}
	if p.Role != nil {
		target, err := s.CanonicalAddr(p.Role.Addr)
		if err != nil {
			return ErrNotParticipant
		}
		p.Role.Addr = target
		if err := s.checkRole(part, pol, room, p.Role.Addr, p.Role.Role); err != nil {
			return err
		}
	}
	if p.Label != nil || p.Topic != nil {
		label, topic := room.Label, room.TopicLabel
		if p.Label != nil {
			label = people.ValidDisplayName(*p.Label)
		}
		if p.Topic != nil {
			topic = people.ValidDisplayName(*p.Topic)
		}
		if err := s.Store.UpdateChatRoomLabels(room.ID, label, topic); err != nil {
			return err
		}
	}
	if p.WorkspaceVisible != nil && room.Kind == KindChannel {
		if err := s.Store.SetChatRoomWorkspaceVisible(room.ID, *p.WorkspaceVisible); err != nil {
			return err
		}
	}
	if p.Role != nil {
		if err := s.setRole(actor, room, p.Role.Addr, p.Role.Role); err != nil {
			return err
		}
	}
	if p.Archive {
		if err := s.Store.ArchiveChatRoom(room.ID); err != nil {
			return err
		}
	}
	if p.Leave {
		s.MarkLeft(room, []string{actor.Addr}, SystemLeft, "")
	}
	s.Bus.PublishRoom(room.ID, Notification{Kind: "rooms", RoomID: room.ID}, nil)
	return nil
}

// DeviceLeave takes one device of a participant out of a room hubbed here —
// the way out for a device that went stale there. Its leaf is marked
// departed in the hub's tree, which lets any member remove it, and a
// device-left line tells members' devices to. The person stays in the room
// with their role and other devices; the device comes back like any new
// device of theirs, added by a sibling or any member.
func (s *Service) DeviceLeave(actor *Actor, room *store.ChatRoom) error {
	if actor.Device == nil {
		return ErrNeedDevice
	}
	if _, err := s.Participant(room, actor.Addr); err != nil {
		return err
	}
	s.commitMu.Lock()
	fresh, err := s.Store.ChatRoom(room.ID)
	if err != nil {
		s.commitMu.Unlock()
		return err
	}
	if fresh.ReplicaStatus == replicaFrozen {
		s.commitMu.Unlock()
		return ErrRoomMoving
	}
	tree, err := s.Tree(fresh)
	if err != nil {
		s.commitMu.Unlock()
		return err
	}
	l := tree.FindKey(actor.Device.DeviceKey)
	if l == nil || l.Identity != actor.Addr || l.Departed {
		s.commitMu.Unlock()
		return nil // nothing of this device's left to go
	}
	l.Departed = true
	treeJSON, _ := json.Marshal(tree)
	err = s.Store.SetChatRoomTree(room.ID, string(treeJSON))
	s.commitMu.Unlock()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"kind": SystemDeviceLeft, "person": actor.Addr, "devices": []string{b64(actor.Device.DeviceKey)}})
	s.appendSystem(fresh, payload)
	return nil
}

// SystemDeviceLeft names a device that left a room by itself: any member
// may remove its leaf.
const SystemDeviceLeft = "device-left"

// checkRole decides whether the participant part may give target the role.
// Roles move only below the actor's own rank: the target must rank below
// them now, and so must the role, unless they hold every capability and
// make a peer. Nobody changes their own role, so an owner is changed by no
// one, and a room never loses its last one to a lower rank.
func (s *Service) checkRole(part *store.ChatParticipant, pol Policy, room *store.ChatRoom, target string, role int) error {
	if !pol.Can(part.RoleIndex, CapChangeRoles) {
		return ErrForbidden
	}
	if !pol.HasRole(role) {
		return fmt.Errorf("%w: the room has no role %d", ErrBadInput, role)
	}
	if target == part.PersonAddr {
		return fmt.Errorf("%w: you cannot change your own role", ErrForbidden)
	}
	cur, err := s.Store.ChatParticipant(room.ID, target)
	if err != nil {
		// Never in the room: a ban may still keep them out, for anyone
		// of this household.
		if role != RoleBanned || !s.inHousehold(target) {
			return ErrNotParticipant
		}
		cur = &store.ChatParticipant{PersonAddr: target, RoleIndex: RoleMember, LeftSeq: -1}
	}
	// Someone out of the room keeps no role but a ban, which may be lifted
	// (so a member can add them again) or left as it is; and anyone who was
	// ever in it may be banned from coming back.
	if cur.LeftSeq != 0 && cur.RoleIndex != RoleBanned {
		if role != RoleBanned {
			return ErrNotParticipant
		}
		cur = &store.ChatParticipant{PersonAddr: target, RoleIndex: RoleMember, LeftSeq: cur.LeftSeq}
	}
	mine := pol.Rank(part.RoleIndex)
	if pol.Rank(cur.RoleIndex) >= mine {
		return fmt.Errorf("%w: only someone who outranks them can change their role", ErrForbidden)
	}
	if pol.Rank(role) >= mine && !pol.Can(part.RoleIndex, CapAll) {
		return fmt.Errorf("%w: you cannot grant a role at or above your own", ErrForbidden)
	}
	return nil
}

// inHousehold says addr is a person of this fog, by their canonical address.
func (s *Service) inHousehold(addr string) bool {
	home, handle, ok := ParseAddr(addr)
	if !ok || home != s.instanceOrEmpty() {
		return false
	}
	p, err := s.Store.PersonByHandle(handle)
	return err == nil && p.Handle == handle
}

// setRole writes a role checkRole allowed. A ban takes the person out of
// the room on the spot, with a line every member's client acts on: the
// first device to see it removes their leaves, so the ban is a removal, not
// a mute.
func (s *Service) setRole(actor *Actor, room *store.ChatRoom, target string, role int) error {
	if _, err := s.Store.ChatParticipant(room.ID, target); err != nil && role == RoleBanned {
		// Banned before ever joining: a row that says so, out from the start.
		home, _, _ := ParseAddr(target)
		if err := s.Store.AddChatBan(room.ID, target, home); err != nil {
			return err
		}
	} else if err := s.Store.SetChatParticipantRole(room.ID, target, role); err != nil {
		return err
	}
	s.Store.Audit(actor.Person.Handle, "chat.role", room.ID, store.AuditDetail(map[string]any{"person": target, "role": role}))
	// A webhook posts with the right its maker held when they made it; it
	// goes when they no longer hold it.
	if !s.policyOf(room).Can(role, CapChangePolicy) {
		s.Store.DeleteChatWebhooksBy(room.ID, target)
	}
	if role == RoleBanned {
		s.MarkLeft(room, []string{target}, SystemBanned, "banned by "+actor.Addr)
	}
	return nil
}

// ---- events ----

type SubmitInput struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Payload   string          `json:"payload"` // base64url MLSMessage
	SentAt    string          `json:"sentAt"`
	Hints     json.RawMessage `json:"hints"`
	Welcome   string          `json:"welcome"` // base64url MLSMessage(Welcome), commits only
	ExpiresAt string          `json:"expiresAt"`
	// GroupInfo is the post-commit GroupInfo for external joins, sent along
	// with a commit so open rooms stay joinable without a second round trip.
	GroupInfo string `json:"groupInfo"`
}

// Result is what a submit returns: the sequenced event, and whether it was
// a replay of one the hub already had.
type Result struct {
	Event    *store.ChatEvent
	Replayed bool
}

// Submit accepts one event from a device into a room hubbed here.
func (s *Service) Submit(actor *Actor, room *store.ChatRoom, in SubmitInput) (*Result, error) {
	if actor.Device == nil {
		return nil, ErrNeedDevice
	}
	if room.ArchivedAt != "" {
		return nil, ErrArchived
	}
	if room.HubInstance == "" && room.ReplicaStatus == replicaFrozen {
		return nil, ErrRoomMoving
	}
	if !validULID(in.ID) {
		return nil, fmt.Errorf("%w: id must be a ULID", ErrBadInput)
	}
	if existing, err := s.Store.ChatEventByID(room.ID, in.ID); err == nil {
		return &Result{Event: existing, Replayed: true}, nil
	}
	payload, err := people.DecodeKey(in.Payload)
	if err != nil || len(payload) == 0 || len(payload) > MaxPayload {
		return nil, fmt.Errorf("%w: payload", ErrBadInput)
	}
	msg, err := mlsview.Parse(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	if string(msg.GroupID) != string(room.GroupID) {
		return nil, ErrGroupMismatch
	}
	switch in.Kind {
	case EventPrivate:
		if !msg.IsPrivate() {
			return nil, fmt.Errorf("%w: payload is not a private message", ErrBadInput)
		}
		// Only application data travels encrypted. A commit or proposal in
		// a private message would change who is in the group behind the
		// hub's back: membership changes are public, so the hub checks them.
		if !msg.IsApplication() {
			return nil, fmt.Errorf("%w: a private message must carry application data; commits travel as public messages and proposals are not accepted", ErrBadInput)
		}
		return s.submitPrivate(actor, room, in, msg, payload)
	case EventCommit:
		if !msg.IsCommit() {
			return nil, fmt.Errorf("%w: payload is not a commit", ErrBadInput)
		}
		return s.submitCommit(actor, room, in, msg, payload)
	case EventProposal:
		return nil, ErrUnsupported
	}
	return nil, fmt.Errorf("%w: kind", ErrBadInput)
}

func validULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
			return false
		}
	}
	return true
}

func (s *Service) submitPrivate(actor *Actor, room *store.ChatRoom, in SubmitInput, msg *mlsview.Message, payload []byte) (*Result, error) {
	part, err := s.Participant(room, actor.Addr)
	if err != nil {
		return nil, err
	}
	pol := s.policyOf(room)
	if !pol.Can(part.RoleIndex, CapSendMessage) {
		return nil, ErrForbidden
	}
	// Re-read the room under the lock so epoch and tree are current, and
	// the sender's participation and role, which the event records.
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	room, err = s.Store.ChatRoom(room.ID)
	if err != nil {
		return nil, err
	}
	if part, err = s.Participant(room, actor.Addr); err != nil {
		return nil, err
	}
	if !pol.Can(part.RoleIndex, CapSendMessage) {
		return nil, ErrForbidden
	}
	if int64(msg.Epoch) > room.Epoch || int64(msg.Epoch)+EpochSlack < room.Epoch {
		return nil, ErrEpochStale
	}
	tree, err := s.Tree(room)
	if err != nil {
		return nil, err
	}
	if l := tree.FindKey(actor.Device.DeviceKey); l == nil || l.Identity != actor.Addr {
		return nil, ErrNotYourLeaf
	}
	hints := s.filterHints(pol, in.Hints)
	ev := &store.ChatEvent{
		RoomID: room.ID, ID: in.ID, Epoch: int64(msg.Epoch), Kind: EventPrivate,
		SenderAddr: actor.Addr, SenderDevice: b64(actor.Device.DeviceKey),
		SentAt: in.SentAt, Payload: payload, Hints: hints, ExpiresAt: validTime(in.ExpiresAt),
		SenderRole: part.RoleIndex,
	}
	if err := s.Store.AppendChatEvent(&store.ChatCommit{Event: ev}); err != nil {
		if errors.Is(err, store.ErrDuplicateEvent) {
			existing, _ := s.Store.ChatEventByID(room.ID, in.ID)
			return &Result{Event: existing, Replayed: true}, nil
		}
		return nil, err
	}
	s.announce(room, ev)
	return &Result{Event: ev}, nil
}

func validTime(s string) string {
	if s == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return ""
	}
	return s
}

// filterHints keeps only the hints the policy permits, and only well-formed
// ones: mentions are a list of person addresses.
func (s *Service) filterHints(pol Policy, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	out := map[string]any{}
	if m, ok := in[HintMentions]; ok && pol.HintAllowed(HintMentions) {
		var addrs []string
		if json.Unmarshal(m, &addrs) == nil {
			var clean []string
			for _, a := range addrs {
				if _, _, ok := ParseAddr(a); ok && len(clean) < 50 {
					clean = append(clean, a)
				}
			}
			if len(clean) > 0 {
				out[HintMentions] = clean
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// submitCommit validates a membership commit against the room's policy and
// the hub's public tree, then sequences it.
func (s *Service) submitCommit(actor *Actor, room *store.ChatRoom, in SubmitInput, msg *mlsview.Message, payload []byte) (*Result, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	room, err := s.Store.ChatRoom(room.ID)
	if err != nil {
		return nil, err
	}
	if int64(msg.Epoch) != room.Epoch {
		return nil, ErrEpochStale
	}
	tree, err := s.Tree(room)
	if err != nil {
		return nil, err
	}
	pol := s.policyOf(room)
	inst, _ := s.Instance()

	// Who is committing, and is that really them?
	var part *store.ChatParticipant
	external := false
	// joinerKey is the identity key behind an external joiner's device.
	joinerKey := ""
	switch msg.SenderType {
	case framing.SenderTypeMember:
		leaf := tree.Leaf(msg.SenderLeaf)
		if leaf == nil || string(leaf.SignatureKey) != string(actor.Device.DeviceKey) || leaf.Identity != actor.Addr {
			return nil, ErrNotYourLeaf
		}
		// A leaf's signature key is its device's registered key: a path
		// refreshes the leaf's encryption keys, never moves it to another.
		if pl := msg.Commit.PathLeaf; pl != nil && string(pl.SignatureKey) != string(actor.Device.DeviceKey) {
			return nil, ErrNotYourLeaf
		}
		part, err = s.Participant(room, actor.Addr)
		if err != nil {
			return nil, err
		}
		// Whoever commits does it from a device their identity key
		// attests, as every leaf in the room must be.
		if _, err := s.attestedDevice(actor.Addr, actor.Device.DeviceKey, inst); err != nil {
			return nil, err
		}
	case framing.SenderTypeNewMemberCommit:
		external = true
		if msg.Commit.PathLeaf == nil || string(msg.Commit.PathLeaf.SignatureKey) != string(actor.Device.DeviceKey) || msg.Commit.PathLeaf.Identity != actor.Addr {
			return nil, ErrNotYourLeaf
		}
		joinerKey, err = s.authorizeExternalJoin(room, pol, actor, inst, true)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unsupported sender type", ErrBadInput)
	}
	// The structural rules every member's client applies (RFC 9420 §12.2):
	// a commit they would all refuse must not move the hub's view.
	if err := tree.Check(msg); err != nil {
		if errors.Is(err, mlsview.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}

	// Authorize every proposal before touching the tree.
	var newPolicy *string
	dmSet := dmMembers(room)
	// identities is the identity key behind each person an Add (or the
	// external joiner) puts in the room.
	identities := map[string]string{}
	if external {
		identities[actor.Addr] = joinerKey
	}
	for _, p := range msg.Commit.Proposals {
		switch p.Type {
		case group.ProposalTypeAdd:
			if err := s.requireCanonical(p.Add.Identity); err != nil {
				return nil, err
			}
			idk, err := s.attestedDevice(p.Add.Identity, p.Add.SignatureKey, inst)
			if err != nil {
				return nil, err
			}
			identities[p.Add.Identity] = idk
			if p.Add.Identity != actor.Addr {
				// Adding a device of someone already in the room — under the
				// identity key they joined with — adds a device, not a person:
				// any member may. Adding a person takes canAddParticipant.
				cur, err := s.Store.ChatParticipant(room.ID, p.Add.Identity)
				device := err == nil && cur.LeftSeq == 0 && (cur.IdentityKey == "" || cur.IdentityKey == idk)
				if !device && !pol.Can(part.RoleIndex, CapAddParticipant) {
					return nil, ErrForbidden
				}
				if room.Kind == KindDM && !dmSet[p.Add.Identity] {
					return nil, ErrFixedMembership
				}
			}
			// A ban holds until someone who may change roles lifts it.
			if prev, err := s.Store.ChatParticipant(room.ID, p.Add.Identity); err == nil && prev.RoleIndex == RoleBanned {
				return nil, fmt.Errorf("%w: %s is banned from this room", ErrForbidden, p.Add.Identity)
			}
		case group.ProposalTypeRemove:
			leaf := tree.Leaf(p.Remove)
			if external {
				// A resync: the joiner may remove only its own old leaf.
				if leaf.Identity != actor.Addr {
					return nil, ErrForbidden
				}
				continue
			}
			if leaf.Identity == actor.Addr || leaf.Departed {
				continue
			}
			// The leaves of someone the hub already counts out of the room
			// — they left, were banned, cut or reset — and of a device its
			// person revoked are anyone's to remove: that only brings the
			// group into line with the room.
			other, err := s.Store.ChatParticipant(room.ID, leaf.Identity)
			if err != nil || other.LeftSeq != 0 {
				continue
			}
			// A revoked device's leaf is anyone's to remove — but one of
			// someone who outranks the remover only by their own household.
			if s.revokedLeaf(leaf, inst) {
				home, _, _ := ParseAddr(leaf.Identity)
				actorHome, _, _ := ParseAddr(actor.Addr)
				if pol.Rank(other.RoleIndex) < pol.Rank(part.RoleIndex) || home == actorHome {
					continue
				}
				return nil, fmt.Errorf("%w: only their own household, or someone who outranks them, removes their revoked device", ErrForbidden)
			}
			// Removing someone still in the room takes the right, and
			// reaches only below one's own rank: no admin removes an owner.
			if !pol.Can(part.RoleIndex, CapRemoveParticipant) {
				return nil, ErrForbidden
			}
			if pol.Rank(other.RoleIndex) >= pol.Rank(part.RoleIndex) {
				return nil, fmt.Errorf("%w: only someone who outranks them can remove them", ErrForbidden)
			}
		case group.ProposalTypeGroupContextExtensions:
			if !pol.Can(part.RoleIndex, CapChangePolicy) {
				return nil, ErrForbidden
			}
			for _, ext := range p.Extensions {
				if uint16(ext.Type) != PolicyExtensionType {
					return nil, fmt.Errorf("%w: group context extension %d", ErrUnsupported, ext.Type)
				}
				np, err := parseNewPolicy(string(ext.Data))
				if err != nil {
					return nil, fmt.Errorf("%w: policy: %v", ErrBadInput, err)
				}
				// Only someone holding every capability may change the
				// roles themselves; anyone else would be writing their own
				// promotion, or the owner's demotion.
				if !pol.Can(part.RoleIndex, CapAll) && !sameRoles(pol.Roles, np.Roles) {
					return nil, fmt.Errorf("%w: only an owner may change the room's roles", ErrForbidden)
				}
				js := np.JSON()
				newPolicy = &js
			}
		}
	}

	// Everything the hub can check from public data, cryptographically:
	// the signature over the content and this epoch's GroupContext, each new
	// leaf's signature, the new tree's parent hashes, and so the next
	// GroupContext. A commit that doesn't verify is refused before anyone
	// is asked to apply it.
	pg, err := s.publicState(room, tree)
	if err != nil {
		return nil, err
	}
	nextPG, err := pg.Next(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}

	// Apply to a copy; the store write is what makes it real.
	before := map[string]bool{}
	// stays: people all of whose leaves are devices that left by themselves.
	// Losing those leaves doesn't take the person out: they keep their
	// place and role, for their devices to come back to.
	stays := map[string]bool{}
	for _, l := range tree.Members() {
		if !before[l.Identity] {
			stays[l.Identity] = true
		}
		before[l.Identity] = true
		stays[l.Identity] = stays[l.Identity] && l.Departed
	}
	changes, err := tree.Apply(msg)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	after := map[string]bool{}
	for _, l := range tree.Members() {
		after[l.Identity] = true
	}
	if !sameLeaves(tree.Members(), nextPG.Members()) {
		return nil, fmt.Errorf("%w: the commit's leaves don't match the verified tree", ErrBadInput)
	}
	// Every commit carries the GroupInfo of the epoch it makes, which must
	// be that epoch's: so the hub always holds a current one, and a newcomer
	// checking a Welcome, or a device rejoining, never finds it missing.
	if in.GroupInfo == "" {
		return nil, fmt.Errorf("%w: groupInfo: a commit must carry the GroupInfo of the epoch it makes", ErrBadInput)
	}
	giWire, err := people.DecodeKey(in.GroupInfo)
	if err != nil || len(giWire) > MaxPayload {
		return nil, fmt.Errorf("%w: groupInfo", ErrBadInput)
	}
	if err := nextPG.CheckGroupInfo(giWire); err != nil {
		return nil, fmt.Errorf("%w: groupInfo: %v", ErrBadInput, err)
	}
	treeJSON, _ := json.Marshal(tree)
	pgJSON, _ := json.Marshal(nextPG)
	pgState := string(pgJSON)

	commit := &store.ChatCommit{
		Event: &store.ChatEvent{
			RoomID: room.ID, ID: in.ID, Epoch: int64(msg.Epoch), Kind: EventCommit,
			SenderAddr: actor.Addr, SenderDevice: b64(actor.Device.DeviceKey),
			SentAt: in.SentAt, Payload: payload,
		},
		AdvanceEpoch:   true,
		NewTree:        string(treeJSON),
		NewPublicState: &pgState,
		Policy:         newPolicy,
	}
	var welcome []byte
	if in.Welcome != "" {
		if welcome, err = people.DecodeKey(in.Welcome); err != nil || len(welcome) > MaxPayload {
			return nil, fmt.Errorf("%w: welcome", ErrBadInput)
		}
	}
	// Whoever gains a leaf and is not a current participant joins: someone
	// new, or someone the hub counted out whose old leaves still linger.
	joined := map[string]bool{}
	for _, ch := range changes {
		if ch.Kind != "add" || joined[ch.Leaf.Identity] {
			continue
		}
		if cur, err := s.Store.ChatParticipant(room.ID, ch.Leaf.Identity); err == nil && cur.LeftSeq == 0 {
			continue
		}
		joined[ch.Leaf.Identity] = true
		home, _, _ := ParseAddr(ch.Leaf.Identity)
		commit.Added = append(commit.Added, store.ChatParticipant{PersonAddr: ch.Leaf.Identity, HomeInstance: home, RoleIndex: RoleMember, IdentityKey: identities[ch.Leaf.Identity]})
	}
	for addr := range before {
		if !after[addr] && !stays[addr] {
			commit.Left = append(commit.Left, addr)
		}
	}
	if welcome != nil {
		for _, ch := range changes {
			if ch.Kind == "add" && ch.Leaf.Identity != actor.Addr {
				commit.Invites = append(commit.Invites, store.ChatInvite{PersonAddr: ch.Leaf.Identity, DeviceKey: b64(ch.Leaf.SignatureKey), InvitedBy: actor.Addr, Welcome: welcome})
			} else if ch.Kind == "add" && string(ch.Leaf.SignatureKey) != string(actor.Device.DeviceKey) {
				// Adding one's own other device still needs the Welcome.
				commit.Invites = append(commit.Invites, store.ChatInvite{PersonAddr: ch.Leaf.Identity, DeviceKey: b64(ch.Leaf.SignatureKey), InvitedBy: actor.Addr, Welcome: welcome})
			}
		}
	}
	if err := s.Store.AppendChatEvent(commit); err != nil {
		if errors.Is(err, store.ErrDuplicateEvent) {
			existing, _ := s.Store.ChatEventByID(room.ID, in.ID)
			return &Result{Event: existing, Replayed: true}, nil
		}
		return nil, err
	}
	if giWire != nil {
		s.Store.SetChatGroupInfo(room.ID, room.Epoch+1, giWire)
	}
	// Mark pending invites for the joiner as accepted on an external join.
	if external {
		if invs, err := s.Store.ChatInvitesFor(actor.Addr); err == nil {
			for _, inv := range invs {
				if inv.RoomID == room.ID {
					s.Store.AcceptChatInvite(inv.ID, actor.Addr)
				}
			}
		}
	}
	fresh, _ := s.Store.ChatRoom(room.ID)
	s.announce(fresh, commit.Event)
	s.departed(fresh, commit.Event, commit.Left)
	if s.Fed != nil && len(commit.Invites) > 0 {
		invs := make([]store.ChatInvite, 0, len(commit.Invites))
		for _, inv := range commit.Invites {
			inv.Seq = commit.Event.Seq
			invs = append(invs, inv)
		}
		s.Fed.SendInvites(fresh, invs)
	}
	for _, p := range commit.Added {
		s.Bus.PublishPerson(p.PersonAddr, Notification{Kind: "rooms", RoomID: room.ID})
	}
	for _, inv := range commit.Invites {
		s.Bus.PublishPerson(inv.PersonAddr, Notification{Kind: "invite", RoomID: room.ID})
	}
	s.Store.Audit(actor.Person.Handle, "chat.commit", room.ID,
		store.AuditDetail(map[string]any{"added": len(commit.Added), "left": len(commit.Left), "epoch": room.Epoch + 1}))
	return &Result{Event: commit.Event}, nil
}

// sameRoles reports whether two policies define exactly the same roles:
// the same indexes, names and capability sets, compared as sets (so
// repeating a capability neither pads nor hides a change).
func sameRoles(a, b []Role) bool {
	canon := func(rs []Role) map[int]string {
		out := map[int]string{}
		for _, r := range rs {
			caps := map[string]bool{}
			for _, c := range r.Capabilities {
				caps[c] = true
			}
			keys := make([]string, 0, len(caps))
			for c := range caps {
				keys = append(keys, c)
			}
			sort.Strings(keys)
			out[r.Index] = strings.TrimSpace(r.Name) + "\x00" + strings.Join(keys, ",")
		}
		return out
	}
	ca, cb := canon(a), canon(b)
	if len(ca) != len(cb) || len(ca) != len(a) || len(cb) != len(b) {
		return false
	}
	for i, v := range ca {
		if cb[i] != v {
			return false
		}
	}
	return true
}

func dmMembers(room *store.ChatRoom) map[string]bool {
	out := map[string]bool{}
	if room.DMKey == "" {
		return out
	}
	for _, a := range strings.Split(room.DMKey, ",") {
		out[a] = true
	}
	return out
}

// attestedDevice checks that key is a live device of the person at addr —
// one of this household's, or one a paired household advertises for its
// person — and that the person's identity key attests it. It returns that
// identity key, base64url. Fog verifies the attestation itself rather than
// take a "verified" flag on trust, and refuses an unattested device to
// everyone: it is what someone holding only the person's sign-in can make.
func (s *Service) attestedDevice(addr string, key []byte, inst string) (string, error) {
	home, handle, ok := ParseAddr(addr)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownPerson, addr)
	}
	var identity, attestation []byte
	if home != inst {
		if s.Fed == nil {
			return "", fmt.Errorf("%w: %q is not in this household", ErrUnknownPerson, addr)
		}
		dp, dd, err := s.Fed.RemoteDevice(addr, key)
		if err != nil {
			return "", err
		}
		identity, _ = people.DecodeKey(dp.IdentityKey)
		attestation, _ = people.DecodeKey(dd.Attestation)
	} else {
		p, err := s.Store.PersonByHandle(handle)
		if err != nil || p.Status != store.PersonStatusActive {
			return "", fmt.Errorf("%w: %q", ErrUnknownPerson, addr)
		}
		d, err := s.Store.PersonDeviceByKey(key)
		if err != nil || d.PersonID != p.ID || d.RevokedAt != "" {
			return "", fmt.Errorf("%w: %s", ErrUnknownDevice, addr)
		}
		identity, attestation = p.IdentityKey, d.Attestation
	}
	if len(identity) == 0 || len(attestation) == 0 || people.VerifyAttestation(identity, key, attestation) != nil {
		return "", fmt.Errorf("%w (%s)", ErrUnverifiedDevice, addr)
	}
	return people.EncodeKey(identity), nil
}

// revokedLeaf says a leaf's device is no longer one its person holds: a
// local device revoked (or never registered), or a remote one its home fog
// no longer advertises.
func (s *Service) revokedLeaf(l *mlsview.Leaf, inst string) bool {
	home, _, ok := ParseAddr(l.Identity)
	if !ok {
		return true
	}
	if home != inst {
		// Only an explicit answer: the person listed in their household's
		// current directory (it heartbeated recently), without this device.
		// A household away, a narrowed grant, a person no longer listed or
		// an error says nothing about the device, and makes no one's leaf
		// anyone's to remove.
		if s.Fed == nil {
			return false
		}
		peer, err := fed.PeerForHousehold(s.Store, home)
		if err != nil || !fed.Online(peer, time.Now()) {
			return false
		}
		dp, dev, err := s.Fed.RemoteDevice(l.Identity, l.SignatureKey)
		return dp != nil && dev == nil && errors.Is(err, ErrUnknownDevice)
	}
	d, err := s.Store.PersonDeviceByKey(l.SignatureKey)
	return err != nil || d.RevokedAt != ""
}

// ErrQuota: storing the attachment would pass a storage quota.
var ErrQuota = errors.New("attachment storage is full")

// Attachment storage quotas: what one person's uploads, one room's and all
// rooms' together may hold on this fog, and how much unfinished chunked
// uploads may. Variables so tests can shrink them.
var (
	QuotaPerPerson int64 = 10 << 30
	QuotaPerRoom   int64 = 20 << 30
	QuotaTotal     int64 = 200 << 30
	QuotaUploads   int64 = 2 << 30
	// What one person's, and one household's, uploads under way may hold
	// reserved of QuotaUploads.
	QuotaUploadsPerPerson    int64 = 512 << 20
	QuotaUploadsPerHousehold int64 = 1 << 30
)

// checkQuota says whether n more bytes from addr in room fit, beside what
// is stored and what uploads under way have reserved (except skip's own).
// The caller holds blobMu.
func (s *Service) checkQuota(roomID, addr string, n int64, skip string) error {
	byRoom, byPerson, total, err := s.Store.ChatBlobUsage(roomID, addr)
	if err != nil {
		return err
	}
	for k, r := range s.reserved {
		if k == skip {
			continue
		}
		total += r.total
		if r.room == roomID {
			byRoom += r.total
		}
		if r.addr == addr {
			byPerson += r.total
		}
	}
	switch {
	case byPerson+n > QuotaPerPerson:
		return fmt.Errorf("%w: your attachments here have reached their limit", ErrQuota)
	case byRoom+n > QuotaPerRoom:
		return fmt.Errorf("%w: this room's attachments have reached their limit", ErrQuota)
	case total+n > QuotaTotal:
		return fmt.Errorf("%w: this fog's chat attachments have reached their limit", ErrQuota)
	}
	return nil
}

// ErrAwaitingRemoval: the group still holds leaves that must go first.
var ErrAwaitingRemoval = errors.New("the room is waiting for a member to remove someone who left; try again in a moment")

// departedLeaves says the group still holds a leaf of someone the hub
// counts out of the room, or of a revoked device.
func (s *Service) departedLeaves(room *store.ChatRoom) bool {
	tree, err := s.Tree(room)
	if err != nil {
		return true
	}
	inst := s.instanceOrEmpty()
	for _, l := range tree.Members() {
		if p, err := s.Store.ChatParticipant(room.ID, l.Identity); err != nil || p.LeftSeq != 0 || l.Departed || s.revokedLeaf(&l, inst) {
			return true
		}
	}
	return false
}

// errIdentityReplaced is authorizeExternalJoin's word that the person now
// holds a different identity key than the one they joined with.
var errIdentityReplaced = errors.New("identity replaced")

// authorizeExternalJoin decides whether a device may commit itself in, and
// returns the identity key behind it. locked says the caller holds
// commitMu (submitCommit does; a GroupInfo fetch doesn't).
//
// Every self-join needs a device the person's identity key attests: an
// unattested device is what someone holding only the person's sign-in can
// make — the ghost-device attack. A current participant's device must also
// carry the identity key they joined with: a different one is someone else
// under their name (an operator's identity reset, say). That takes them out
// of the room, with a line every member sees; a member must add them again.
func (s *Service) authorizeExternalJoin(room *store.ChatRoom, pol Policy, actor *Actor, inst string, locked bool) (string, error) {
	if actor.Device == nil {
		return "", ErrNeedDevice
	}
	// A DM's people are fixed when it is made: nobody outside the set joins
	// one, by an Add or by themselves.
	if set := dmMembers(room); room.Kind == KindDM && len(set) > 0 && !set[actor.Addr] {
		return "", ErrFixedMembership
	}
	idk, err := s.attestedDevice(actor.Addr, actor.Device.DeviceKey, inst)
	if errors.Is(err, ErrUnverifiedDevice) {
		return "", fmt.Errorf("%w: this device is not verified; verify it from another of your devices, which then adds it", ErrJoinRefused)
	}
	if err != nil {
		return "", err
	}
	if p, err := s.Store.ChatParticipant(room.ID, actor.Addr); err == nil {
		if p.RoleIndex == RoleBanned {
			return "", ErrJoinRefused
		}
		if p.LeftSeq == 0 {
			switch p.IdentityKey {
			case idk:
				return idk, nil
			case "":
				// Joined before the hub kept identity keys: this one is
				// the key from here on.
				s.Store.SetChatParticipantIdentity(room.ID, actor.Addr, idk)
				return idk, nil
			}
			if locked {
				s.markLeftLocked(room, []string{actor.Addr}, SystemIdentityReset, "identity key changed")
			} else {
				s.MarkLeft(room, []string{actor.Addr}, SystemIdentityReset, "identity key changed")
			}
			return "", fmt.Errorf("%w: %w: this person's identity key changed since they joined; someone in the room must add them again", ErrJoinRefused, errIdentityReplaced)
		}
		// Someone who was in the room and has since changed identity key is
		// someone else under their name, in any kind of room: not by an
		// invite, not by an open channel's door. Only a member's Add, which
		// records the new key, brings them back.
		if p.IdentityKey != "" && p.IdentityKey != idk {
			return "", fmt.Errorf("%w: %w: this person's identity key changed since they were in this room; someone in it must add them again", ErrJoinRefused, errIdentityReplaced)
		}
	}
	// A Welcome waiting for this very device: it may join on its own instead.
	if invs, err := s.Store.ChatInvitesFor(actor.Addr); err == nil {
		mine := b64(actor.Device.DeviceKey)
		for _, inv := range invs {
			if inv.RoomID == room.ID && (inv.DeviceKey == "" || inv.DeviceKey == mine) {
				return idk, nil
			}
		}
	}
	home, _, _ := ParseAddr(actor.Addr)
	if pol.Join == JoinOpenHousehold && room.Kind == KindChannel && home == inst && room.HubInstance == "" {
		// An open door only for the identity this address has always had
		// here: one that ever held another key, in any room — a reset, a
		// deleted person whose handle was given again — would be refused
		// by every member's client that pinned the old one, so only a
		// member's Add, which their clients take as vouching, lets it in.
		if keys, err := s.Store.ChatIdentityKeysFor(actor.Addr); err == nil {
			for _, k := range keys {
				if k != idk {
					return "", fmt.Errorf("%w: %w: this address has had another identity key here; someone in the room must add them", ErrJoinRefused, errIdentityReplaced)
				}
			}
		}
		return idk, nil
	}
	return "", ErrJoinRefused
}

// publicState is a room's verified group state. A room from before the hub
// kept one seeds it from the GroupInfo on file for its current epoch.
// Callers hold commitMu.
func (s *Service) publicState(room *store.ChatRoom, tree *mlsview.Tree) (*mlsview.PublicGroup, error) {
	if room.PublicState != "" {
		return mlsview.UnmarshalPublicGroup([]byte(room.PublicState))
	}
	info, epoch, err := s.Store.ChatGroupInfo(room.ID)
	if err != nil || info == nil || epoch != room.Epoch {
		return nil, ErrNeedGroupInfo
	}
	return s.seedPublicState(room, tree, info)
}

// seedPublicState verifies a GroupInfo for the room's current epoch whose
// tree holds exactly the leaves the hub knows, and keeps it as the room's
// state. Callers hold commitMu.
func (s *Service) seedPublicState(room *store.ChatRoom, tree *mlsview.Tree, info []byte) (*mlsview.PublicGroup, error) {
	pg, err := mlsview.FromGroupInfo(info)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	if string(pg.GroupID()) != string(room.GroupID) || int64(pg.Epoch()) != room.Epoch || !sameLeaves(tree.Members(), pg.Members()) {
		return nil, fmt.Errorf("%w: group info doesn't match the room's group, epoch or members", ErrBadInput)
	}
	js, _ := json.Marshal(pg)
	if err := s.Store.SetChatPublicState(room.ID, string(js)); err != nil {
		return nil, err
	}
	room.PublicState = string(js)
	return pg, nil
}

// sameLeaves says two leaf lists name the same identities and keys at the
// same indexes.
func sameLeaves(a, b []mlsview.Leaf) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Index != b[i].Index || a[i].Identity != b[i].Identity || string(a[i].SignatureKey) != string(b[i].SignatureKey) {
			return false
		}
	}
	return true
}

// checkGroupInfo holds a GroupInfo to the room it is filed under: an
// MLSMessage-framed GroupInfo for this group at this epoch. A member can
// still publish one that lies about the rest (the hub can't check the tree
// hash); that is a member breaking their own room.
func checkGroupInfo(wire []byte, groupID []byte, epoch int64) error {
	raw, err := mlsview.UnwrapGroupInfo(wire)
	if err != nil {
		return err
	}
	gi, err := group.UnmarshalGroupInfo(raw)
	if err != nil {
		return err
	}
	gc := gi.GroupContext
	if gc == nil || gc.GroupID == nil || string(gc.GroupID.Value) != string(groupID) || int64(gc.Epoch) != epoch {
		return errors.New("group info is for another group or epoch")
	}
	return nil
}

func (s *Service) announce(room *store.ChatRoom, ev *store.ChatEvent) {
	s.Bus.PublishRoom(room.ID, Notification{Kind: "event", RoomID: room.ID, Event: ev}, nil)
	if s.OnAccepted != nil {
		s.OnAccepted(room, ev)
	}
	if s.Notify != nil {
		go s.Notify.Dispatch(room, ev)
	}
}

// Events reads a slice of the log for an actor, honoring the history rule.
func (s *Service) Events(actor *Actor, room *store.ChatRoom, after, before int64, limit int) ([]store.ChatEvent, error) {
	part, err := s.Store.ChatParticipant(room.ID, actor.Addr)
	// A row out from before the start — a ban on someone never in, a
	// deleted person's tombstone — is no one's way in.
	if err != nil || part.LeftSeq < 0 {
		return nil, ErrNotParticipant
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	pol := s.policyOf(room)
	floor := int64(0)
	if pol.History == HistorySinceJoin {
		floor = part.JoinedSeq - 1
	}
	if after < floor {
		after = floor
	}
	var evs []store.ChatEvent
	if before > 0 {
		evs, err = s.Store.ChatEventsBefore(room.ID, before, limit)
		// Reverse to oldest-first and drop anything under the floor or past
		// the seq at which the person left.
		for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
			evs[i], evs[j] = evs[j], evs[i]
		}
	} else {
		evs, err = s.Store.ChatEventsAfter(room.ID, after, limit)
	}
	if err != nil {
		return nil, err
	}
	out := evs[:0]
	for _, e := range evs {
		if e.Seq <= floor {
			continue
		}
		if part.LeftSeq != 0 && e.Seq > part.LeftSeq {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ---- group info ----

// SetGroupInfo stores a member's post-commit GroupInfo for external joins.
func (s *Service) SetGroupInfo(actor *Actor, room *store.ChatRoom, epoch int64, info []byte) error {
	if _, err := s.Participant(room, actor.Addr); err != nil {
		return err
	}
	if len(info) == 0 || len(info) > MaxPayload {
		return fmt.Errorf("%w: group info", ErrBadInput)
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	fresh, err := s.Store.ChatRoom(room.ID)
	if err != nil {
		return err
	}
	if epoch != fresh.Epoch {
		return ErrEpochStale
	}
	if err := checkGroupInfo(info, fresh.GroupID, epoch); err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	tree, err := s.Tree(fresh)
	if err != nil {
		return err
	}
	if fresh.PublicState == "" {
		// A room from before the hub verified commits takes its state
		// from the first GroupInfo published for its epoch that matches
		// the members it knows.
		if _, err := s.seedPublicState(fresh, tree, info); err != nil {
			return err
		}
	} else {
		pg, err := mlsview.UnmarshalPublicGroup([]byte(fresh.PublicState))
		if err != nil {
			return err
		}
		if err := pg.CheckGroupInfo(info); err != nil {
			return fmt.Errorf("%w: %v", ErrBadInput, err)
		}
	}
	return s.Store.SetChatGroupInfo(room.ID, epoch, info)
}

// GroupInfoFor hands the current GroupInfo to someone allowed to join.
func (s *Service) GroupInfoFor(actor *Actor, room *store.ChatRoom) ([]byte, int64, error) {
	inst, _ := s.Instance()
	if _, err := s.Participant(room, actor.Addr); err != nil {
		if _, err := s.authorizeExternalJoin(room, s.policyOf(room), actor, inst, false); err != nil {
			return nil, 0, err
		}
	} else if actor.Device != nil {
		// A participant's device fetching group info means to join by it:
		// the same rules apply as when it submits the join.
		if _, err := s.authorizeExternalJoin(room, s.policyOf(room), actor, inst, false); err != nil {
			return nil, 0, err
		}
	}
	info, epoch, err := s.Store.ChatGroupInfo(room.ID)
	if err != nil {
		return nil, 0, err
	}
	if info == nil || epoch != room.Epoch {
		return nil, epoch, fmt.Errorf("%w: no current group info; a member must publish one", ErrEpochStale)
	}
	return info, epoch, nil
}

// Bot is fog's own seat, or nil until it has started. It goes from nil to
// set once and never back, so a caller that saw it non-nil may use it again.
func (s *Service) Bot() *Bot { return s.bot.Load() }

// SetBot installs the started bot.
func (s *Service) SetBot(b *Bot) { s.bot.Store(b) }

// ---- blobs ----

// validBlobSHA reports whether sha is a lowercase hex SHA-256 — the only
// shape a blob name may take. Names arrive from clients and from peers (a
// hub's blob list, a fetch path), so this is what keeps them from naming a
// file outside the blob store.
func validBlobSHA(sha string) bool {
	if len(sha) != sha256.Size*2 {
		return false
	}
	for _, c := range sha {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// blobPath is where a blob lives on disk. A name that isn't a digest is
// not found: no such blob can exist in the store.
func (s *Service) blobPath(sha string) (string, error) {
	if !validBlobSHA(sha) {
		return "", store.ErrNotFound
	}
	return filepath.Join(s.DataDir, "chat", "blobs", sha[:2], sha), nil
}

// BlobChunk is how much of an attachment one relayed request carries: the
// federation plane seals each request whole, so a large attachment crosses
// in pieces and is put together, and checked, at the hub.
const BlobChunk = 16 << 20

// PutBlobChunk takes one piece of an attachment a follower relays, at
// offset into an upload of total bytes the caller names. The pieces must
// come in order (a repeat of the last is answered with where it stands);
// the last one stores the blob, content-addressed, and returns it. Before
// then it returns nil and the bytes held so far. Pieces of an upload
// abandoned for an hour are dropped.
func (s *Service) PutBlobChunk(actor *Actor, room *store.ChatRoom, upload string, offset, total int64, r io.Reader) (*store.ChatBlob, int64, error) {
	if _, err := s.Participant(room, actor.Addr); err != nil {
		return nil, 0, err
	}
	if total <= 0 || total > MaxBlob || offset < 0 || offset > total || len(upload) == 0 || len(upload) > 64 {
		return nil, 0, fmt.Errorf("%w: upload", ErrBadInput)
	}
	dir := filepath.Join(s.DataDir, "chat", "blobs", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, err
	}
	sweepUploads(dir)
	sum := sha256.Sum256([]byte(room.ID + "\x00" + actor.Addr + "\x00" + upload))
	path := filepath.Join(dir, "chunks-"+hex.EncodeToString(sum[:16]))
	have := int64(0)
	if st, err := os.Stat(path); err == nil {
		have = st.Size()
	}
	if offset != have {
		return nil, have, fmt.Errorf("%w: the upload is at byte %d", ErrBadInput, have)
	}
	// A new upload reserves its declared total, whole, before its first
	// byte is kept: in the quotas, and beside the uploads under way. Every
	// piece after holds to the total it declared.
	key := filepath.Base(path)
	if err := s.reserve(key, room.ID, actor.Addr, total, have == 0); err != nil {
		if have == 0 {
			os.Remove(path)
		}
		return nil, have, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, have, err
	}
	n, err := io.Copy(f, io.LimitReader(r, min(BlobChunk, total-have)))
	f.Close()
	if err != nil {
		os.Remove(path)
		s.release(key)
		return nil, 0, err
	}
	have += n
	if n > 0 {
		s.progress(key)
	}
	if have < total {
		return nil, have, nil
	}
	defer os.Remove(path)
	whole, err := os.Open(path)
	if err != nil {
		s.release(key)
		return nil, have, err
	}
	defer whole.Close()
	b, err := s.putBlob(actor, room, whole, total, key)
	return b, have, err
}

// reserve holds a chunked upload's declared total against the quotas: at
// its start (fresh) a new reservation, which must fit — within what one
// person and one household may hold reserved, as well as the quotas; after,
// the one it made, whose total the piece must declare again.
func (s *Service) reserve(key, roomID, addr string, total int64, fresh bool) error {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	if s.reserved == nil {
		s.reserved = map[string]*reservation{}
	}
	for k, r := range s.reserved {
		if time.Since(r.at) > time.Hour {
			delete(s.reserved, k)
		}
	}
	if !fresh {
		r := s.reserved[key]
		if r == nil || r.total != total || r.addr != addr || r.room != roomID {
			return fmt.Errorf("%w: no upload under way with that total", ErrBadInput)
		}
		return nil
	}
	delete(s.reserved, key)
	if err := s.checkQuota(roomID, addr, total, ""); err != nil {
		return err
	}
	home, _, _ := ParseAddr(addr)
	var pending, mine, theirs int64
	for _, r := range s.reserved {
		pending += r.total
		if r.addr == addr {
			mine += r.total
		}
		if h, _, _ := ParseAddr(r.addr); h == home {
			theirs += r.total
		}
	}
	switch {
	case mine+total > QuotaUploadsPerPerson:
		return fmt.Errorf("%w: you have too many large attachments uploading; let one finish", ErrQuota)
	case theirs+total > QuotaUploadsPerHousehold:
		return fmt.Errorf("%w: your household has too many large attachments uploading; try again soon", ErrQuota)
	case pending+total > QuotaUploads:
		return fmt.Errorf("%w: too many large attachments are uploading; try again soon", ErrQuota)
	}
	s.reserved[key] = &reservation{addr: addr, room: roomID, total: total, at: time.Now()}
	return nil
}

// progress marks a reservation's upload as moving: only a piece that added
// bytes keeps it from expiring.
func (s *Service) progress(key string) {
	s.blobMu.Lock()
	if r := s.reserved[key]; r != nil {
		r.at = time.Now()
	}
	s.blobMu.Unlock()
}

// AbortBlobChunks ends an upload its sender gave up on: its pieces and its
// reservation go at once.
func (s *Service) AbortBlobChunks(actor *Actor, room *store.ChatRoom, upload string) {
	dir := filepath.Join(s.DataDir, "chat", "blobs", "tmp")
	sum := sha256.Sum256([]byte(room.ID + "\x00" + actor.Addr + "\x00" + upload))
	path := filepath.Join(dir, "chunks-"+hex.EncodeToString(sum[:16]))
	os.Remove(path)
	s.release(filepath.Base(path))
}

func (s *Service) release(key string) {
	s.blobMu.Lock()
	delete(s.reserved, key)
	s.blobMu.Unlock()
}

// sweepUploads drops pieces of uploads nobody has added to for an hour.
func sweepUploads(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), "chunks-") && time.Since(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// PutBlob stores an encrypted attachment, content-addressed. declared is
// its length when the request says it (-1 when it doesn't): checked against
// the quotas before a byte is read, and held to.
func (s *Service) PutBlob(actor *Actor, room *store.ChatRoom, r io.Reader, declared int64) (*store.ChatBlob, error) {
	return s.putBlob(actor, room, r, declared, "")
}

// putBlob is PutBlob; reservation names a chunked upload's reservation,
// which its finished bytes take the place of.
func (s *Service) putBlob(actor *Actor, room *store.ChatRoom, r io.Reader, declared int64, reservation string) (*store.ChatBlob, error) {
	if reservation != "" {
		defer s.release(reservation)
	}
	if _, err := s.Participant(room, actor.Addr); err != nil {
		return nil, err
	}
	room, err := s.Store.ChatRoom(room.ID)
	if err != nil {
		return nil, err
	}
	if declared > MaxBlob {
		return nil, fmt.Errorf("%w: attachment too large", ErrBadInput)
	}
	if declared > 0 {
		s.blobMu.Lock()
		err := s.checkQuota(room.ID, actor.Addr, declared, reservation)
		s.blobMu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	limit := int64(MaxBlob)
	if declared > 0 {
		limit = declared
	}
	dir := filepath.Join(s.DataDir, "chat", "blobs", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "up-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	f.Close()
	if err != nil || n == 0 || n > limit || (declared > 0 && n != declared) {
		os.Remove(tmp)
		if n > limit {
			return nil, fmt.Errorf("%w: attachment larger than it said", ErrBadInput)
		}
		return nil, fmt.Errorf("%w: empty, short or unreadable attachment", ErrBadInput)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	// The quota check and the storing are one step: two uploads can't both
	// fit in the room one of them leaves.
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	if _, err := s.Store.ChatBlob(sha); err != nil {
		if err := s.checkQuota(room.ID, actor.Addr, n, reservation); err != nil {
			os.Remove(tmp)
			return nil, err
		}
	}
	dst, _ := s.blobPath(sha) // a digest just computed is always valid
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if _, err := os.Stat(dst); err == nil {
		os.Remove(tmp)
	} else if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	b := &store.ChatBlob{SHA256: sha, RoomID: room.ID, Size: n, UploaderAddr: actor.Addr, RoomSeq: room.Seq}
	if err := s.Store.PutChatBlob(b); err != nil {
		return nil, err
	}
	return s.Store.ChatBlob(sha)
}

// OpenBlob returns a reader for a blob the actor may see.
func (s *Service) OpenBlob(actor *Actor, sha string) (*os.File, *store.ChatBlob, error) {
	path, err := s.blobPath(sha)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.Store.ChatBlob(sha)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.Store.ChatParticipant(b.RoomID, actor.Addr)
	if err != nil || !BlobVisible(b, p.LeftSeq) {
		return nil, nil, ErrNotParticipant
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, b, nil
}

// PruneHistory applies each room's retention policy to its events and
// blobs, and reports how many rows went; internal/retention runs it. An
// attachment lives as long as its room keeps history: the hub can't see
// which sealed message references it, so it ages out with the messages.
// The rules that need no room policy (disappearing messages, accepted
// invites) are plain store passes in retention's own list.
func (s *Service) PruneHistory(now time.Time) (int64, error) {
	rooms, err := s.Store.AllChatRooms()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, r := range rooms {
		pol := s.policyOf(&r)
		if pol.Retention.Days <= 0 {
			continue
		}
		cut := now.Add(-time.Duration(pol.Retention.Days) * 24 * time.Hour).UTC().Format(time.RFC3339)
		n, err := s.Store.PruneChatEvents(r.ID, cut)
		total += n
		if err != nil {
			return total, err
		}
		shas, err := s.Store.ChatBlobsBefore(r.ID, cut)
		if err != nil {
			return total, err
		}
		for _, sha := range shas {
			if path, err := s.blobPath(sha); err == nil {
				os.Remove(path)
			}
			if s.Store.DeleteChatBlob(sha) == nil {
				total++
			}
		}
	}
	return total, nil
}

// BlobVisible says whether someone who left a room at leftSeq (0: still in
// it) may fetch a blob of it: everything while they are in the room, and
// afterwards only what was stored before they left — the attachments of
// messages they were sent. A blob of unknown age is not theirs once gone.
func BlobVisible(b *store.ChatBlob, leftSeq int64) bool {
	return leftSeq == 0 || (b.RoomSeq > 0 && b.RoomSeq < leftSeq)
}

// MaySee says whether a person may be sent an event of a room: a current
// participant may, and a former one only up to the event that took them
// out. Live delivery asks this as well as replay, so nothing sequenced after
// someone left reaches a connection opened before.
func (s *Service) MaySee(addr, roomID string, seq int64) bool {
	p, err := s.Store.ChatParticipant(roomID, addr)
	return err == nil && (p.LeftSeq == 0 || seq <= p.LeftSeq)
}

// OpenBlobRaw opens a stored blob by digest with no participant check — for
// the fed plane, which has already established the caller's stake.
func (s *Service) OpenBlobRaw(sha string) (*os.File, error) {
	path, err := s.blobPath(sha)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}
