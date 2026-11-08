package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// Pushes carry metadata only, go to the people who asked for them, and
// honour the per-room setting, mention hints, and do-not-disturb.
func TestChatNotificationsDispatch(t *testing.T) {

	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()

	var mu sync.Mutex
	var got []chat.PushPayload
	srv.chat.Notify.SendPush = func(sub store.PushSub, payload []byte) error {
		var p chat.PushPayload
		json.Unmarshal(payload, &p)
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		return nil
	}
	pushes := func() []chat.PushPayload {
		mu.Lock()
		defer mu.Unlock()
		return append([]chat.PushPayload(nil), got...)
	}
	waitPushes := func(n int) []chat.PushPayload {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if len(pushes()) >= n {
				return pushes()
			}
			time.Sleep(20 * time.Millisecond)
		}
		return pushes()
	}

	// Dave subscribes his device and turns push on.
	sub := `{"endpoint":"https://push.example/abc","keys":{"p256dh":"BP","auth":"AA"}}`
	if w := do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(dave.id))+"/push", sub, withCookie(dave.cookie)); w.Code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/chat/me/prefs", `{"pushEnabled":true}`, withCookie(dave.cookie)); w.Code != http.StatusOK {
		t.Fatalf("prefs: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/chat/push/vapid", "", withCookie(dave.cookie)); w.Code != http.StatusOK || decode(t, w)["publicKey"] == "" {
		t.Fatalf("vapid: %d %s", w.Code, w.Body)
	}

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Family", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	commit, welcome, _ := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "dave"))
	submit(t, h, op, roomID, "mls_commit", commit, welcome)
	dave.c.JoinGroup(ctx, welcome)

	// A plain message: one push, kind message, no content anywhere in it.
	ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, "secret words"))
	submit(t, h, op, roomID, "mls_private", ct, nil)
	ps := waitPushes(1)
	if len(ps) != 1 || ps[0].Kind != "message" || ps[0].RoomID != roomID || ps[0].Label != "Family" || ps[0].Sender != op.addr {
		t.Fatalf("first push: %+v", ps)
	}
	raw, _ := json.Marshal(ps[0])
	if strings.Contains(string(raw), "secret") {
		t.Fatal("push carried content")
	}

	// Mentions-only: a message without a hint is silent; with a hint it
	// arrives as a mention.
	do(t, h, "PATCH", "/api/chat/rooms/"+roomID, `{"notify":"mentions"}`, withCookie(dave.cookie))
	ct, _ = op.c.SendMessage(ctx, gid, mustContent(t, "no hint"))
	submit(t, h, op, roomID, "mls_private", ct, nil)
	time.Sleep(150 * time.Millisecond)
	if len(pushes()) != 1 {
		t.Fatalf("mentions-only room pushed a plain message: %+v", pushes())
	}
	ct, _ = op.c.SendMessage(ctx, gid, mustContent(t, "hey dave"))
	in := map[string]any{"id": ulid(t), "kind": "mls_private", "payload": people.EncodeKey(ct), "hints": map[string]any{"mentions": []string{dave.addr}}}
	b, _ := json.Marshal(in)
	if w := do(t, h, "POST", "/api/chat/rooms/"+roomID+"/events", string(b), withCookie(op.cookie)); w.Code != http.StatusOK {
		t.Fatalf("mention post: %s", w.Body)
	}
	ps = waitPushes(2)
	if len(ps) != 2 || ps[1].Kind != "mention" {
		t.Fatalf("mention push: %+v", ps)
	}

	// Do not disturb silences plain messages but not mentions.
	do(t, h, "PATCH", "/api/chat/rooms/"+roomID, `{"notify":"all"}`, withCookie(dave.cookie))
	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format("15:04")
	end := now.Add(time.Hour).Format("15:04")
	prefs := `{"pushEnabled":true,"dnd":{"enabled":true,"start":"` + start + `","end":"` + end + `","tz":"UTC"}}`
	do(t, h, "PUT", "/api/chat/me/prefs", prefs, withCookie(dave.cookie))
	ct, _ = op.c.SendMessage(ctx, gid, mustContent(t, "late night"))
	submit(t, h, op, roomID, "mls_private", ct, nil)
	time.Sleep(150 * time.Millisecond)
	if len(pushes()) != 2 {
		t.Fatalf("DND let a plain message through: %+v", pushes())
	}
	// A gone subscription is dropped.
	srv.chat.Notify.SendPush = func(sub store.PushSub, payload []byte) error { return errGoneForTest() }
	do(t, h, "PUT", "/api/chat/me/prefs", `{"pushEnabled":true}`, withCookie(dave.cookie))
	ct, _ = op.c.SendMessage(ctx, gid, mustContent(t, "bye"))
	submit(t, h, op, roomID, "mls_private", ct, nil)
	subsLeft := func() int {
		var n int
		srv.store.DB().QueryRow(`SELECT COUNT(*) FROM chat_push_subs WHERE device_id = ?`, dave.id).Scan(&n)
		return n
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && subsLeft() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := subsLeft(); n != 0 {
		t.Errorf("gone subscription kept: %d left", n)
	}
}

func errGoneForTest() error { return chat.ErrSubscriptionGone }

// Webhooks and assistants speak through the bot, and only where it sits.
func TestChatWebhooksAndMCPThroughTheBot(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	admin := setup(t, h)
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()
	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	srv.chat.SetBot(bot)
	if err := bot.PublishKeyPackagesNow(); err != nil {
		t.Fatal(err)
	}

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Ops", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)

	// No bot in the room yet: a webhook cannot be made.
	if w := do(t, h, "POST", "/api/chat/rooms/"+roomID+"/webhooks", `{"name":"ci"}`, withCookie(op.cookie)); w.Code != http.StatusConflict {
		t.Fatalf("webhook without bot: %d %s", w.Code, w.Body)
	}
	commit, welcome, err := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "fog"))
	if err != nil {
		t.Fatal(err)
	}
	submit(t, h, op, roomID, "mls_commit", commit, welcome)
	bot.CatchUp()

	w := do(t, h, "POST", "/api/chat/rooms/"+roomID+"/webhooks", `{"name":"ci"}`, withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("create webhook: %d %s", w.Code, w.Body)
	}
	url := decode(t, w)["url"].(string)
	path := url[strings.Index(url, "/hooks/"):]
	if w := do(t, h, "POST", path, `{"text":"build passed"}`); w.Code != http.StatusAccepted {
		t.Fatalf("hook post: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/hooks/chat/nope", `{"text":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("bad hook token: %d", w.Code)
	}
	// The operator decrypts the webhook's message.
	evs := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=2", "", withCookie(op.cookie)))["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events after hook: %d", len(evs))
	}
	payload, _ := people.DecodeKey(evs[0].(map[string]any)["payload"].(string))
	rm, err := op.c.ReceiveMessage(ctx, gid, payload)
	if err != nil {
		t.Fatal(err)
	}
	hookContent, _ := chat.DecodeContent(rm.Plaintext)
	// Stamped as the webhook, never as fog itself.
	if hookContent.Body.Text != "build passed" || hookContent.System != "webhook:ci" {
		t.Fatalf("hook content: %+v", hookContent)
	}
	// The bot's record of it carries the message's id and the real speaker.
	if msgs, _ := srv.store.BotMessages(roomID, 10); len(msgs) != 1 || msgs[0].ID == "" || msgs[0].ID != hookContent.ID || !strings.HasSuffix(msgs[0].Sender, "#webhook:ci") {
		t.Fatalf("hook record: %+v (content id %q)", msgs, hookContent.ID)
	}

	// The bot recorded what the operator posts, so an assistant can read it.
	ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, "deploy at noon"))
	submit(t, h, op, roomID, "mls_private", ct, nil)
	bot.CatchUp()

	// MCP: a token without the flag reads but cannot post; one with it can.
	w = do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"reader"}`, withCookie(admin))
	reader := decode(t, w)["token"].(string)
	w = do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"poster","canPostChat":true}`, withCookie(admin))
	poster := decode(t, w)["token"].(string)
	call := func(token, name, args string) map[string]any {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
		w := do(t, h, "POST", "/mcp", body, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Accept", "application/json, text/event-stream")
		})
		if w.Code != http.StatusOK {
			t.Fatalf("mcp %s: %d %s", name, w.Code, w.Body)
		}
		return decode(t, w)
	}
	rooms := call(reader, "fog_chat_rooms", `{}`)
	if !strings.Contains(w.Body.String(), "") || !strings.Contains(mustJSON(rooms), roomID) {
		t.Fatalf("fog_chat_rooms: %v", rooms)
	}
	read := call(reader, "fog_chat_read", `{"roomId":"`+roomID+`"}`)
	if !strings.Contains(mustJSON(read), "deploy at noon") || !strings.Contains(mustJSON(read), "build passed") {
		t.Fatalf("fog_chat_read: %v", read)
	}
	search := call(reader, "fog_chat_search", `{"query":"noon"}`)
	if !strings.Contains(mustJSON(search), "deploy at noon") {
		t.Fatalf("fog_chat_search: %v", search)
	}
	denied := call(reader, "fog_chat_send", `{"roomId":"`+roomID+`","text":"hi"}`)
	if !strings.Contains(mustJSON(denied), "permission") {
		t.Fatalf("reader token posted: %v", denied)
	}
	sent := call(poster, "fog_chat_send", `{"roomId":"`+roomID+`","text":"from the assistant"}`)
	if strings.Contains(mustJSON(sent), "isError\":true") {
		t.Fatalf("poster token refused: %v", sent)
	}
	evs = decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=4", "", withCookie(op.cookie)))["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events after mcp send: %d", len(evs))
	}
	payload, _ = people.DecodeKey(evs[0].(map[string]any)["payload"].(string))
	rm, err = op.c.ReceiveMessage(ctx, gid, payload)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := chat.DecodeContent(rm.Plaintext); c.Body.Text != "from the assistant" {
		t.Fatalf("mcp content: %+v", c)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
