package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// The fog bot is a member like any other: it creates #fog, people join it
// externally, it keeps up with their commits, mirrors notices as encrypted
// messages they can read, and reads what they post — and nothing else on
// the box can.
func TestFogBotIsAMember(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()

	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	var received []chat.Content
	bot.OnMessage = func(room *store.ChatRoom, ev *store.ChatEvent, c chat.Content) { received = append(received, c) }
	fog, err := bot.EnsureFogChannel()
	if err != nil {
		t.Fatal(err)
	}
	if fog.Label != "fog" || fog.Kind != "channel" {
		t.Fatalf("#fog: %+v", fog)
	}
	// Idempotent: a second call finds the same room.
	if again, _ := bot.EnsureFogChannel(); again.ID != fog.ID {
		t.Fatalf("second EnsureFogChannel made %s, want %s", again.ID, fog.ID)
	}

	// The bot is in the directory as a person, verified by its own key.
	dir := decode(t, do(t, h, "GET", "/api/chat/people", "", withCookie(dave.cookie)))
	var botEntry map[string]any
	for _, p := range dir["people"].([]any) {
		if p.(map[string]any)["handle"] == "fog" {
			botEntry = p.(map[string]any)
		}
	}
	if botEntry == nil || botEntry["role"] != "bot" || botEntry["devices"].([]any)[0].(map[string]any)["verified"] != true {
		t.Fatalf("bot in directory: %v", botEntry)
	}

	// Dave sees #fog as joinable (the bot published group info) and joins.
	rooms := decode(t, do(t, h, "GET", "/api/chat/rooms", "", withCookie(dave.cookie)))
	avail := rooms["available"].([]any)
	if len(avail) != 1 || avail[0].(map[string]any)["joinable"] != true {
		t.Fatalf("available: %v", avail)
	}
	gi := decode(t, do(t, h, "GET", "/api/chat/rooms/"+fog.ID+"/groupinfo", "", withCookie(dave.cookie)))
	giWire, _ := people.DecodeKey(gi["groupInfo"].(string))
	giBytes, err := mlsview.UnwrapGroupInfo(giWire)
	if err != nil {
		t.Fatal(err)
	}
	gid, commit, err := dave.c.ExternalJoin(ctx, giBytes)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, dave, fog.ID, "mls_commit", commit, nil); w.Code != http.StatusOK {
		t.Fatalf("dave joins #fog: %d %s", w.Code, w.Body)
	}
	// The bot catches up on dave's commit and republishes group info for
	// the new epoch.
	bot.CatchUp()
	gi2 := decode(t, do(t, h, "GET", "/api/chat/rooms/"+fog.ID+"/groupinfo", "", withCookie(dave.cookie)))
	if gi2["epoch"].(float64) != 1 {
		t.Fatalf("group info epoch after join: %v", gi2["epoch"])
	}

	// A notice lands in the inbox; the bot mirrors it as an encrypted
	// message dave can decrypt.
	srv.store.CreateNotice("test", "", "Backup finished", "12 files, 3 MB")
	bot.CatchUp()
	bot.MirrorNoticesNow()
	evs := decode(t, do(t, h, "GET", "/api/chat/rooms/"+fog.ID+"/events?after=2", "", withCookie(dave.cookie)))["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events after join: %d", len(evs))
	}
	payload, _ := people.DecodeKey(evs[0].(map[string]any)["payload"].(string))
	rm, err := dave.c.ReceiveMessage(ctx, gid, payload)
	if err != nil {
		t.Fatalf("dave decrypting the bot: %v", err)
	}
	c, err := chat.DecodeContent(rm.Plaintext)
	if err != nil || c.Body == nil || c.Body.Text != "**Backup finished**\n12 files, 3 MB" || c.System != "fog" {
		t.Fatalf("bot content: %+v %v", c, err)
	}
	// Mirrored once, not again.
	bot.MirrorNoticesNow()
	evs = decode(t, do(t, h, "GET", "/api/chat/rooms/"+fog.ID+"/events?after=2", "", withCookie(dave.cookie)))["events"].([]any)
	if len(evs) != 1 {
		t.Errorf("notice mirrored twice")
	}

	// Dave posts; the bot reads it.
	plain, _ := chat.EncodeContent(chat.TextMessage(ulid(t), "thanks fog"))
	ct, _ := dave.c.SendMessage(ctx, gid, plain)
	if w := submit(t, h, dave, fog.ID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatalf("dave posts: %s", w.Body)
	}
	bot.CatchUp()
	if len(received) != 1 || received[0].Body.Text != "thanks fog" {
		t.Fatalf("bot received: %+v", received)
	}

	// Restart: a new bot over the same vault and directory holds the same
	// key, the same rooms, and keeps reading.
	bot2, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	if bot2.Addr() != bot.Addr() {
		t.Fatalf("bot address changed across restart")
	}
	var again []chat.Content
	bot2.OnMessage = func(room *store.ChatRoom, ev *store.ChatEvent, c chat.Content) { again = append(again, c) }
	ct2, _ := dave.c.SendMessage(ctx, gid, mustContent(t, "after restart"))
	submit(t, h, dave, fog.ID, "mls_private", ct2, nil)
	bot2.CatchUp()
	if len(again) != 1 || again[0].Body.Text != "after restart" {
		t.Fatalf("bot after restart received: %+v", again)
	}
	// And the directory still shows exactly one bot device.
	dir = decode(t, do(t, h, "GET", "/api/chat/people", "", withCookie(dave.cookie)))
	for _, p := range dir["people"].([]any) {
		if p.(map[string]any)["handle"] == "fog" && len(p.(map[string]any)["devices"].([]any)) != 1 {
			t.Errorf("bot devices after restart: %v", p.(map[string]any)["devices"])
		}
	}
}

func mustContent(t *testing.T, text string) []byte {
	t.Helper()
	b, err := chat.EncodeContent(chat.TextMessage(ulid(t), text))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A member adding the bot to a private group by Welcome: the bot joins from
// one of its published KeyPackages.
func TestFogBotJoinsByWelcome(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()
	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.PublishKeyPackagesNow(); err != nil {
		t.Fatal(err)
	}
	var got []chat.Content
	bot.OnMessage = func(room *store.ChatRoom, ev *store.ChatEvent, c chat.Content) { got = append(got, c) }

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Ops", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	commit, welcome, err := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "fog"))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add bot: %s", w.Body)
	}
	bot.CatchUp()
	ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, "hello bot"))
	submit(t, h, op, roomID, "mls_private", ct, nil)
	bot.CatchUp()
	if len(got) != 1 || got[0].Body.Text != "hello bot" {
		t.Fatalf("bot in group received: %+v", got)
	}
	// The bot answers, and the operator can read it.
	ev, err := bot.Send(roomID, chat.TextMessage("", "hello operator"))
	if err != nil {
		t.Fatal(err)
	}
	rm, err := op.c.ReceiveMessage(ctx, gid, ev.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := chat.DecodeContent(rm.Plaintext); c.Body.Text != "hello operator" {
		t.Fatalf("bot reply: %+v", c)
	}
}

// Fog's plaintext copy of a room follows the room's rules: an author's
// delete takes the message out of it, and once the bot is removed from the
// room it forgets the room and all it read there, so webhooks, apps and MCP
// can't reach it any more.
func TestFogBotLetsGoOfWhatTheRoomTookBack(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()
	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.PublishKeyPackagesNow(); err != nil {
		t.Fatal(err)
	}
	gid, roomID := newGroupRoom(t, h, op, "group", "Ops")
	commit, welcome, err := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "fog"))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add bot: %s", w.Body)
	}
	bot.CatchUp()
	post := func(c chat.Content) {
		b, _ := chat.EncodeContent(c)
		ct, _ := op.c.SendMessage(ctx, gid, b)
		if w := submit(t, h, op, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
			t.Fatal(w.Body)
		}
	}
	keep, gone := chat.TextMessage(ulid(t), "keep me"), chat.TextMessage(ulid(t), "delete me")
	post(keep)
	post(gone)
	post(chat.Content{V: 1, Type: "delete", ID: ulid(t), Replaces: gone.ID})
	bot.CatchUp()
	msgs, _ := srv.store.BotMessages(roomID, 10)
	if len(msgs) != 1 || msgs[0].Text != "keep me" {
		t.Fatalf("bot's copy after a delete: %+v", msgs)
	}
	rm, err := op.c.RemoveMember(ctx, gid, []byte(bot.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", rm, nil); w.Code != http.StatusOK {
		t.Fatalf("remove bot: %s", w.Body)
	}
	bot.CatchUp()
	if bot.InRoom(roomID) {
		t.Error("the bot still holds a room it was removed from")
	}
	if msgs, _ := srv.store.BotMessages(roomID, 10); len(msgs) != 0 {
		t.Errorf("the bot kept the plaintext of a room it was removed from: %+v", msgs)
	}
}

// Fog's bot, like members' clients, sends nothing into a group that still
// holds a departed person's leaves: it waits for a member to remove them.
func TestFogBotWaitsForDepartedLeaves(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()
	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	bot.PublishKeyPackagesNow()
	gid, roomID := newGroupRoom(t, h, op, "group", "Ops")
	addToRoom(t, h, op, gid, roomID, dave)
	commit, welcome, _ := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "fog"))
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	dave.c.ProcessCommit(ctx, gid, commit)
	bot.CatchUp()
	if w := patchRoom(t, h, op, roomID, `{"role":{"addr":"`+dave.addr+`","role":1}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	bot.CatchUp()
	if _, err := bot.Send(roomID, chat.TextMessage("", "while dave's leaf remains")); !errors.Is(err, chat.ErrAwaitingRemoval) {
		t.Fatalf("bot posting before the removal: %v", err)
	}
	rm, _ := op.c.RemoveMember(ctx, gid, []byte(dave.addr))
	if w := submit(t, h, op, roomID, "mls_commit", rm, nil); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	bot.CatchUp()
	if _, err := bot.Send(roomID, chat.TextMessage("", "after")); err != nil {
		t.Fatalf("bot posting after the removal: %v", err)
	}
}

// Fog's bot moves a room it talks in to a fresh epoch well before its
// sending chain gets too long for members' clients to follow: the members
// apply its commit, and its messages after read as before.
func TestFogBotRollsTheEpoch(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()
	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.PublishKeyPackagesNow(); err != nil {
		t.Fatal(err)
	}
	defer func(n int) { chat.BotRollMessages = n }(chat.BotRollMessages)
	chat.BotRollMessages = 3
	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Ops", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	commit, welcome, err := op.c.InviteMember(ctx, gid, keysFor(t, h, op.cookie, "fog"))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add bot: %s", w.Body)
	}
	bot.CatchUp()
	before, _ := srv.store.ChatRoom(roomID)
	for i := 0; i < 3; i++ {
		ev, err := bot.Send(roomID, chat.TextMessage("", fmt.Sprintf("notice %d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := op.c.ReceiveMessage(ctx, gid, ev.Payload); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := srv.store.ChatRoom(roomID)
	if after.Epoch != before.Epoch+1 {
		t.Fatalf("the bot didn't roll the epoch: %d → %d", before.Epoch, after.Epoch)
	}
	// The member applies the bot's commit, and reads what it says next.
	evs, _ := srv.store.ChatEventsAfter(roomID, after.Seq-1, 1)
	if err := op.c.ProcessCommit(ctx, gid, evs[0].Payload); err != nil {
		t.Fatalf("the bot's roll couldn't be applied: %v", err)
	}
	ev, err := bot.Send(roomID, chat.TextMessage("", "after the roll"))
	if err != nil {
		t.Fatal(err)
	}
	rm, err := op.c.ReceiveMessage(ctx, gid, ev.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := chat.DecodeContent(rm.Plaintext); c.Body.Text != "after the roll" {
		t.Fatalf("after the roll: %+v", c)
	}
}
