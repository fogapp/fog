// Chat tools: what an assistant may read and say. Scoped to rooms the fog
// bot is in — the operator's own rooms are on the operator's devices, not
// here — and posting is off unless the token was minted with that right.
package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitea.lan/matthew/fog/internal/store"
)

var errNoChat = errors.New("chat is not available")

func (s *server) addChatTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_chat_rooms",
		Description: "List the chat rooms fog itself is a member of — the only rooms an assistant can read or post in. People add fog to a room deliberately; every other room is end-to-end encrypted away from this server."},
		s.chatRooms)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_chat_read",
		Description: "Read the most recent messages in a room fog is in (text only, newest last)."},
		s.chatRead)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_chat_search",
		Description: "Search message text across the rooms fog is in."},
		s.chatSearch)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_chat_send",
		Description: "Post a message as fog into a room fog is in. Only tokens created with 'may post to chat' can do this; the message shows in the room as coming from fog on behalf of this assistant."},
		s.chatSend)
}

type chatRoomView struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Topic string `json:"topic,omitempty"`
}

func (s *server) chatRooms(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct {
	Rooms []chatRoomView `json:"rooms"`
}, error) {
	var out struct {
		Rooms []chatRoomView `json:"rooms"`
	}
	out.Rooms = []chatRoomView{}
	if s.Chat == nil || s.Chat.Bot() == nil {
		return nil, out, errNoChat
	}
	for _, id := range s.Chat.Bot().Rooms() {
		if room, err := s.Store.ChatRoom(id); err == nil && room.ArchivedAt == "" {
			out.Rooms = append(out.Rooms, chatRoomView{ID: room.ID, Kind: room.Kind, Label: room.Label, Topic: room.TopicLabel})
		}
	}
	return nil, out, nil
}

type chatReadIn struct {
	RoomID string `json:"roomId" jsonschema:"the room id from fog_chat_rooms"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many recent messages (default 50, max 200)"`
}

type chatMessageView struct {
	Seq    int64  `json:"seq"`
	Sender string `json:"sender"`
	At     string `json:"at"`
	Text   string `json:"text"`
}

func (s *server) chatRead(_ context.Context, _ *mcp.CallToolRequest, in chatReadIn) (*mcp.CallToolResult, struct {
	Messages []chatMessageView `json:"messages"`
}, error) {
	var out struct {
		Messages []chatMessageView `json:"messages"`
	}
	out.Messages = []chatMessageView{}
	if s.Chat == nil || s.Chat.Bot() == nil {
		return nil, out, errNoChat
	}
	if !s.Chat.Bot().InRoom(in.RoomID) {
		return nil, out, errors.New("fog is not in that room")
	}
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	msgs, err := s.Store.BotMessages(in.RoomID, limit)
	if err != nil {
		return nil, out, err
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, chatMessageView{Seq: m.Seq, Sender: m.Sender, At: m.At, Text: m.Text})
	}
	return nil, out, nil
}

type chatSearchIn struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

func (s *server) chatSearch(_ context.Context, _ *mcp.CallToolRequest, in chatSearchIn) (*mcp.CallToolResult, struct {
	Hits []store.BotMessage `json:"hits"`
}, error) {
	var out struct {
		Hits []store.BotMessage `json:"hits"`
	}
	out.Hits = []store.BotMessage{}
	if s.Chat == nil || s.Chat.Bot() == nil {
		return nil, out, errNoChat
	}
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return nil, out, errors.New("query required")
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	hits, err := s.Store.SearchBotMessages(q, limit)
	if err != nil {
		return nil, out, err
	}
	for _, h := range hits {
		if s.Chat.Bot().InRoom(h.RoomID) {
			out.Hits = append(out.Hits, h)
		}
	}
	return nil, out, nil
}

type chatSendIn struct {
	RoomID string `json:"roomId"`
	Text   string `json:"text"`
}

func (s *server) chatSend(ctx context.Context, _ *mcp.CallToolRequest, in chatSendIn) (*mcp.CallToolResult, struct {
	Seq int64 `json:"seq"`
}, error) {
	var out struct {
		Seq int64 `json:"seq"`
	}
	if s.Chat == nil || s.Chat.Bot() == nil {
		return nil, out, errNoChat
	}
	if !canPostChat(ctx) {
		return nil, out, errors.New("this token was not created with permission to post to chat")
	}
	actor := actorFor(ctx)
	name := strings.TrimPrefix(actor, ActorPrefix+":")
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, out, errors.New("text required")
	}
	if !s.Chat.Bot().InRoom(in.RoomID) {
		return nil, out, errors.New("fog is not in that room; someone in it must add fog first")
	}
	ev, err := s.Chat.Bot().SendAs(in.RoomID, text, "mcp:"+name)
	if err != nil {
		return nil, out, err
	}
	s.Store.Audit(actor, "chat.mcp.post", in.RoomID, "")
	out.Seq = ev.Seq
	return nil, out, nil
}
