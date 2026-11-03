package chat

import (
	"encoding/json"
	"fmt"

	"gitea.lan/matthew/fog/internal/store"
)

// IdentityReset is chat's half of an operator resetting a person's identity
// (POST /api/people/{id}/reset-identity): the person's devices are revoked
// and the next one they register starts a new identity key. That new key is
// not the person the rooms' members verified, and may not be the person at
// all — whoever reset it can sign in under their name. So the reset is a
// departure from every room they are in: the hub counts them out with an
// identity-reset line that every member's client shows as a warning, drops
// its verification mark for, and removes the old leaves on; Welcomes still
// waiting for the old devices go; and getting back in takes a member adding
// the new identity, never a self-join. Rooms hubbed by another household are
// told through their hub, which does the same.
func (s *Service) IdentityReset(p *store.Person) {
	addr, err := s.Addr(p.Handle)
	if err != nil {
		return
	}
	s.Store.DropChatInvitesFor(addr)
	rooms, err := s.Store.ChatRoomsFor(addr)
	if err != nil {
		return
	}
	for i := range rooms {
		r := rooms[i]
		if r.HubInstance == "" {
			s.markLeftOrAfterMove(&r, []string{addr}, SystemIdentityReset, "identity reset by an operator")
			continue
		}
		if s.Fed == nil {
			continue
		}
		// The hub may be slow or away; the reset doesn't wait on it, and the
		// word is owed until the hub acknowledges it (QueueDeparture). A hub
		// that hasn't heard yet still refuses the new identity anything
		// under the old one's address (SameIdentity).
		s.Fed.QueueDeparture(addr, &r)
	}
}

// RemoteIdentityReset is the hub's side of an older follower's relayed reset
// (POST …/update with identityReset; followers now queue departures):
// one of that household's people had their identity reset there.
func (s *Service) RemoteIdentityReset(actor *Actor, room *store.ChatRoom) error {
	if _, err := s.Participant(room, actor.Addr); err != nil {
		return err
	}
	s.MarkLeft(room, []string{actor.Addr}, SystemIdentityReset, "identity reset by their household's operator")
	return nil
}

// SameIdentity says actor, acting under a participant's address, holds the
// identity key that participant joined with. Someone else under the address
// — a new person given a deleted one's handle, or a reset whose word never
// reached this hub — is counted out of the room with an identity-reset line,
// and refused. A participant who joined before keys were kept passes.
func (s *Service) SameIdentity(room *store.ChatRoom, actor *Actor) error {
	p, err := s.Store.ChatParticipant(room.ID, actor.Addr)
	if err != nil || p.LeftSeq != 0 || p.IdentityKey == "" || identityOf(actor.Person) == p.IdentityKey {
		return nil
	}
	s.MarkLeft(room, []string{actor.Addr}, SystemIdentityReset, "identity key changed")
	return fmt.Errorf("%w: %w", ErrNotParticipant, errIdentityReplaced)
}

// PersonDeleted is chat's half of an operator deleting a person: a full
// departure from every room, and nothing of theirs left for whoever takes
// the handle next. Rooms hubbed here count them out with an identity-reset
// line (members' clients warn and drop what they trusted about them); rooms
// hubbed elsewhere are told through their hub, which does the same, and
// whose rows this fog's replicas keep only for the identity key that joined
// (keepOwnRows). Their DMs here are archived, their webhooks revoked and
// their pending Welcomes dropped. Then every chat row naming them is moved
// to their retired address, fog:<instance>/<ULID>, which no living person
// answers to: a new person with the same handle starts with no rooms, no
// bans, no history, and must be added like anyone new.
func (s *Service) PersonDeleted(p *store.Person) {
	addr, err := s.Addr(p.Handle)
	if err != nil || p.UID == "" {
		return
	}
	retired, _ := s.Addr(p.UID)
	s.Store.DropChatInvitesFor(addr)
	s.Store.DeleteChatWebhooksByPerson(addr)
	if rooms, err := s.Store.ChatRoomsFor(addr); err == nil {
		for i := range rooms {
			r := rooms[i]
			if r.HubInstance == "" {
				s.markLeftOrAfterMove(&r, []string{addr}, SystemIdentityReset, "removed from their household")
				if r.Kind == KindDM {
					s.Store.ArchiveChatRoom(r.ID)
				}
				continue
			}
			if s.Fed == nil {
				continue
			}
			s.Fed.QueueDeparture(addr, &r)
		}
	}
	if err := s.Store.RetireChatAddr(addr, retired); err != nil {
		s.Log.Error("chat: retiring a deleted person's rooms", "err", err)
	}
}

// SystemDeviceRevoked names a revoked device's leaves for members to remove.
const SystemDeviceRevoked = "device-revoked"

// DeviceRevoked is chat's half of a person revoking one of their devices: in
// every room hubbed here where that device still holds a leaf, a
// device-revoked line names its key, and the first member device to see it
// commits the leaf's removal — anyone may remove a revoked device's leaf.
// Until then the device can still decrypt what it is sent, though fog no
// longer serves it anything. A person's own other devices remove such
// leaves everywhere, rooms hubbed elsewhere included, on their next sync.
func (s *Service) DeviceRevoked(p *store.Person, deviceKey []byte) {
	addr, err := s.Addr(p.Handle)
	if err != nil {
		return
	}
	rooms, err := s.Store.ChatRoomsFor(addr)
	if err != nil {
		return
	}
	for i := range rooms {
		r := &rooms[i]
		if r.HubInstance != "" {
			continue
		}
		tree, err := s.Tree(r)
		if err != nil || tree.FindKey(deviceKey) == nil {
			continue
		}
		payload, _ := json.Marshal(map[string]any{"kind": SystemDeviceRevoked, "person": addr, "devices": []string{b64(deviceKey)}})
		s.appendSystem(r, payload)
	}
}
