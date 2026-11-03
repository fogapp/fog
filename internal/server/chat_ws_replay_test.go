package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/store"
)

// A room someone is added to while their stream is open is replayed in
// full, not cut off after the first 500 events.
func TestChatStreamReplaysAddedRoomPastOnePage(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	dave := registerDevice(t, h, daveS, "dave")
	daveAddr, err := srv.chat.Addr("dave")
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(sameTestClient(h)) // one client in-process and over the socket
	defer ts.Close()
	hdr := http.Header{}
	hdr.Add("Cookie", dave.cookie.Name+"="+dave.cookie.Value)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/chat/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)
	conn.Write(ctx, websocket.MessageText, []byte(`{"since":{}}`))
	read := func() streamFrame {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var f streamFrame
		json.Unmarshal(data, &f)
		return f
	}
	if f := read(); f.Kind != "synced" {
		t.Fatalf("first frame %q, want synced (dave is in no rooms yet)", f.Kind)
	}

	// A room with 700 events, dave a participant from the first, so its
	// whole history is his to see.
	room := &store.ChatRoom{ID: "01ROOMREPLAYTEST000000000A", GroupID: []byte("group"), Kind: "group",
		Label: "Big", CreatedBy: daveAddr}
	if err := srv.store.CreateChatRoom(room); err != nil {
		t.Fatal(err)
	}
	const total = 700
	for i := 1; i <= total; i++ {
		c := &store.ChatCommit{Event: &store.ChatEvent{RoomID: room.ID, ID: fmt.Sprintf("ev%04d", i),
			Kind: "mls_private", SenderAddr: daveAddr, SentAt: "2026-01-01T00:00:00Z", Payload: []byte("x")}}
		if i == 1 {
			c.Added = []store.ChatParticipant{{RoomID: room.ID, PersonAddr: daveAddr, RoleIndex: 4, JoinedSeq: 1}}
		}
		if err := srv.store.AppendChatEvent(c); err != nil {
			t.Fatal(err)
		}
	}
	srv.chat.Bus.PublishPerson(daveAddr, chat.Notification{Kind: "rooms", RoomID: room.ID})

	var last int64
	for last < total {
		f := read()
		if f.Kind == "event" && f.RoomID == room.ID {
			if f.Event.Seq != last+1 {
				t.Fatalf("event seq %d after %d", f.Event.Seq, last)
			}
			last = f.Event.Seq
		}
	}
}
