package chat

import (
	"sync"

	"gitea.lan/matthew/fog/internal/store"
)

// Notification is what a connected client hears: an event landed in a room
// it is in, its room list changed, or an ephemeral signal (typing, presence)
// passed by.
type Notification struct {
	Kind   string           `json:"kind"` // event | rooms | ephemeral | invite
	RoomID string           `json:"roomId,omitempty"`
	Event  *store.ChatEvent `json:"event,omitempty"`
	// Ephemeral payload, never stored.
	Person string `json:"person,omitempty"`
	Signal string `json:"signal,omitempty"` // typing | presence:online | presence:away
}

// Bus is the in-process fan-out from the hub to connected clients. A
// subscriber names the person it speaks for and the rooms it follows; the
// room set is refreshed by the connection when its membership changes.
type Bus struct {
	mu   sync.RWMutex
	subs map[*Sub]struct{}
}

type Sub struct {
	C      chan Notification
	person string
	mu     sync.RWMutex
	rooms  map[string]bool
}

func NewBus() *Bus { return &Bus{subs: map[*Sub]struct{}{}} }

// Subscribe registers a listener. The channel is buffered; a slow reader
// drops notifications rather than blocking the hub — the client's next sync
// catches it up, since the log is the truth and the push is a nudge.
func (b *Bus) Subscribe(person string, rooms []string) *Sub {
	s := &Sub{C: make(chan Notification, 256), person: person, rooms: map[string]bool{}}
	for _, r := range rooms {
		s.rooms[r] = true
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *Bus) Unsubscribe(s *Sub) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// SetRooms replaces the rooms a subscriber follows.
func (s *Sub) SetRooms(rooms []string) {
	s.mu.Lock()
	s.rooms = map[string]bool{}
	for _, r := range rooms {
		s.rooms[r] = true
	}
	s.mu.Unlock()
}

func (s *Sub) follows(room string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms[room]
}

func (s *Sub) offer(n Notification) {
	select {
	case s.C <- n:
	default:
	}
}

// PublishRoom sends to every subscriber following the room, except the one
// given (the sender's own connection already knows).
func (b *Bus) PublishRoom(roomID string, n Notification, except *Sub) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if s == except || !s.follows(roomID) {
			continue
		}
		s.offer(n)
	}
}

// Leave stops a person's connections following a room, at once: whatever
// the room publishes next does not reach them, even before a connection
// gets round to refreshing its room list.
func (b *Bus) Leave(person, roomID string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if s.person == person {
			s.mu.Lock()
			delete(s.rooms, roomID)
			s.mu.Unlock()
		}
	}
}

// PublishPerson sends to every connection a person holds.
func (b *Bus) PublishPerson(person string, n Notification) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if s.person == person {
			s.offer(n)
		}
	}
}

// Online reports whether a person has any live connection.
func (b *Bus) Online(person string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if s.person == person {
			return true
		}
	}
	return false
}

// OnlinePeople lists people with a live connection.
func (b *Bus) OnlinePeople() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for s := range b.subs {
		if !seen[s.person] {
			seen[s.person] = true
			out = append(out, s.person)
		}
	}
	return out
}

// roomLister is the one store method PublishRoomsOf needs.
type roomLister interface {
	ChatRoomsFor(addr string) ([]store.ChatRoom, error)
}

// PublishRoomsOf sends a notification into every room a person is in —
// presence changes ride this.
func (b *Bus) PublishRoomsOf(person string, n Notification, st roomLister) {
	rooms, err := st.ChatRoomsFor(person)
	if err != nil {
		return
	}
	for _, r := range rooms {
		nn := n
		nn.RoomID = r.ID
		b.PublishRoom(r.ID, nn, nil)
	}
}
