// The chat stream: one WebSocket per client, carrying every event for the
// rooms the person is in plus ephemera. The first message from the client is
// its cursors ({since: {roomId: seq}}); the server replays what it missed,
// then relays live. A slow client is disconnected rather than allowed to
// stall the hub — it reconnects with cursors and loses nothing.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/store"
)

// streamRecheck is how often a stream checks that its session, device and
// person still stand; a variable so a test can run it fast.
var streamRecheck = 10 * time.Second

type streamHello struct {
	Since map[string]int64 `json:"since"`
}

type streamFrame struct {
	Kind    string            `json:"kind"`
	RoomID  string            `json:"roomId,omitempty"`
	Event   *eventView        `json:"event,omitempty"`
	Person  string            `json:"person,omitempty"`
	Signal  string            `json:"signal,omitempty"`
	Invite  int64             `json:"inviteId,omitempty"`
	Cursors map[string]int64  `json:"cursors,omitempty"`
	Rooms   []string          `json:"rooms,omitempty"`
	Online  []string          `json:"online,omitempty"`
	Error   string            `json:"error,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	actor := s.chatActor(w, r, false)
	if actor == nil {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()
	conn.SetReadLimit(1 << 20)

	// Hello: the client's cursors.
	var hello streamHello
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, data, err := conn.Read(hctx)
	cancel()
	if err != nil {
		return
	}
	json.Unmarshal(data, &hello)
	if hello.Since == nil {
		hello.Since = map[string]int64{}
	}

	roomIDs := func() []string {
		rooms, _ := s.store.ChatRoomsFor(actor.Addr)
		ids := make([]string, 0, len(rooms))
		for _, rm := range rooms {
			ids = append(ids, rm.ID)
		}
		return ids
	}
	sub := s.chat.Bus.Subscribe(actor.Addr, roomIDs())
	defer s.chat.Bus.Unsubscribe(sub)
	s.chat.Bus.PublishRoomsOf(actor.Addr, chat.Notification{Kind: "ephemeral", Person: actor.Addr, Signal: "presence:online"}, s.store)
	defer s.chat.Bus.PublishRoomsOf(actor.Addr, chat.Notification{Kind: "ephemeral", Person: actor.Addr, Signal: "presence:offline"}, s.store)

	send := func(f streamFrame) bool {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		b, _ := json.Marshal(f)
		return conn.Write(wctx, websocket.MessageText, b) == nil
	}

	// replayRoom sends a room's events after the given seq, a page at a
	// time, advancing hello.Since as it goes. False means the client is gone.
	replayRoom := func(rm *store.ChatRoom, after int64) bool {
		const page = 500
		for {
			evs, err := s.chat.Events(actor, rm, after, 0, page)
			if err != nil || len(evs) == 0 {
				return true
			}
			for _, e := range evs {
				ev := eventView1(e)
				if !send(streamFrame{Kind: "event", RoomID: rm.ID, Event: &ev}) {
					return false
				}
				after = e.Seq
				hello.Since[rm.ID] = e.Seq
			}
			if len(evs) < page {
				return true
			}
		}
	}

	// Replay: everything after the client's cursor, per room. A room whose
	// newest seq the client already holds has nothing to send, and skipping
	// it saves a participant check and an events query per room on every
	// reconnect.
	replay := func() bool {
		rooms, _ := s.store.ChatRoomsFor(actor.Addr)
		cursors := map[string]int64{}
		for i := range rooms {
			rm := &rooms[i]
			if after := hello.Since[rm.ID]; rm.Seq > after {
				if !replayRoom(rm, after) {
					return false
				}
			}
			cursors[rm.ID] = rm.Seq
			hello.Since[rm.ID] = rm.Seq
		}
		return send(streamFrame{Kind: "synced", Cursors: cursors, Online: s.chat.Bus.OnlinePeople()})
	}
	if !replay() {
		return
	}

	// A stream outlives nothing that let it open: the session it rode in on
	// signed out or revoked, the device revoked, the person suspended — each
	// closes it within a beat, as it would refuse the next HTTP call.
	pc := s.currentPerson(r)
	var sessionID int64
	if pc.Session != nil {
		sessionID = pc.Session.ID
	}
	stillAllowed := func() bool {
		if sessionID != 0 {
			if _, p, err := s.store.SessionByID(sessionID); err != nil || p.Status != store.PersonStatusActive {
				return false
			}
		} else if p, err := s.store.PersonByID(actor.Person.ID); err != nil || p.Status != store.PersonStatusActive {
			return false
		}
		if actor.Device != nil {
			if d, err := s.store.PersonDeviceByID(actor.Device.ID); err != nil || d.RevokedAt != "" {
				return false
			}
		}
		return true
	}
	recheck := time.NewTicker(streamRecheck)
	defer recheck.Stop()

	// Client → server: pings and cursor updates only; everything else is HTTP.
	go func() {
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-recheck.C:
			if !stillAllowed() {
				conn.Close(websocket.StatusPolicyViolation, "session ended")
				return
			}
		case n, ok := <-sub.C:
			if !ok {
				return
			}
			switch n.Kind {
			case "event":
				if n.Event == nil {
					continue
				}
				// The bus stops a departed person's connections following
				// the room, but a room list refreshed late must not let a
				// later event through: they see up to their departure.
				if !s.chat.MaySee(actor.Addr, n.RoomID, n.Event.Seq) {
					continue
				}
				ev := eventView1(*n.Event)
				hello.Since[n.RoomID] = n.Event.Seq
				if !send(streamFrame{Kind: "event", RoomID: n.RoomID, Event: &ev}) {
					return
				}
			case "rooms":
				sub.SetRooms(roomIDs())
				if !send(streamFrame{Kind: "rooms", RoomID: n.RoomID}) {
					return
				}
				// A room we were just added to: replay it from the start,
				// all of it — a room older than one page is not cut short.
				if n.RoomID != "" {
					if rm, err := s.store.ChatRoom(n.RoomID); err == nil {
						if _, err := s.chat.Participant(rm, actor.Addr); err == nil {
							if _, seen := hello.Since[rm.ID]; !seen {
								if !replayRoom(rm, 0) {
									return
								}
							}
						}
					}
				}
			case "invite":
				if !send(streamFrame{Kind: "invite", RoomID: n.RoomID}) {
					return
				}
			case "ephemeral":
				if !send(streamFrame{Kind: "ephemeral", RoomID: n.RoomID, Person: n.Person, Signal: n.Signal}) {
					return
				}
			}
		}
	}
}
