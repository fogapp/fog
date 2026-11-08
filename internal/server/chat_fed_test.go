package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// chatPair pairs two fogs both ways for chat: each grants the other the chat
// capability, each is reachable over HTTP, and heartbeats have carried each
// household's people to the other.
type chatPair struct {
	*fogPair
	bHTTP *httptest.Server
}

func twoChatFogs(t *testing.T) *chatPair {
	t.Helper()
	p := &chatPair{fogPair: twoFogs(t, `[{"capability":"chat"}]`)}
	// B is reachable too, and grants A chat in return.
	p.bHTTP = httptest.NewServer(p.bH)
	t.Cleanup(p.bHTTP.Close)
	if w := do(t, p.bH, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, p.bHTTP.URL), withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("set b url: %d %s", w.Code, w.Body)
	}
	body := `{"capabilities":[{"capability":"chat","direction":"grant","enabled":true}]}`
	if w := do(t, p.bH, "PUT", fmt.Sprintf("/api/federation/peers/%d", p.aPeerOnB.ID), body, withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("b grants chat: %d %s", w.Code, w.Body)
	}
	return p
}

// heartbeat exchanges both ways so people directories and URLs propagate.
func (p *chatPair) heartbeat(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// A payload stamped in the same second as one already applied may read
	// as stale; the next second's exchange carries everything anyway.
	beat := func(f *fed.Service, peer *store.FedPeer) error {
		var err error
		for range 3 {
			if err = f.HeartbeatPeer(ctx, peer); !errors.Is(err, fed.ErrStalePayload) {
				return err
			}
			time.Sleep(1100 * time.Millisecond)
		}
		return err
	}
	if err := beat(p.b.fed, p.aPeerOnB); err != nil {
		t.Fatalf("b→a heartbeat: %v", err)
	}
	// A now knows B's URL; refresh the row and dial back.
	p.bPeerOnA, _ = p.a.store.FedPeerByID(p.bPeerOnA.ID)
	if err := beat(p.a.fed, p.bPeerOnA); err != nil {
		t.Fatalf("a→b heartbeat: %v", err)
	}
	p.aPeerOnB, _ = p.b.store.FedPeerByID(p.aPeerOnB.ID)
	p.bPeerOnA, _ = p.a.store.FedPeerByID(p.bPeerOnA.ID)
}

func TestChatAcrossHouseholds(t *testing.T) {

	p := twoChatFogs(t)
	ctx := context.Background()
	// People: the operator on A; eve, a member, on B.
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)

	// A's directory lists B's household and eve, with her device.
	dir := decode(t, do(t, p.aH, "GET", "/api/chat/people", "", withCookie(op.cookie)))
	hh := dir["households"].([]any)
	if len(hh) != 1 {
		t.Fatalf("households in A's directory: %v", hh)
	}
	bPeople := hh[0].(map[string]any)["people"].([]any)
	var eveEntry map[string]any
	for _, x := range bPeople {
		if x.(map[string]any)["handle"] == "eve" {
			eveEntry = x.(map[string]any)
		}
	}
	if eveEntry == nil || len(eveEntry["devices"].([]any)) != 1 {
		t.Fatalf("eve in A's directory: %v", bPeople)
	}

	// The operator creates a room on A and adds eve: her key material comes
	// from B through A.
	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Cousins", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, p.aH, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	w := do(t, p.aH, "GET", "/api/chat/keys?addr="+eve.addr, "", withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("eve's keys via A: %d %s", w.Code, w.Body)
	}
	km := decode(t, w)
	kpS, ok := km["devices"].([]any)[0].(map[string]any)["keyPackage"].(string)
	if !ok {
		t.Fatalf("no key package in %v", km)
	}
	eveKP, _ := people.DecodeKey(kpS)
	commit, welcome, err := op.c.InviteMember(ctx, gid, eveKP)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, p.aH, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add eve: %d %s", w.Code, w.Body)
	}
	// The hub queued eve's Welcome for B; deliver it.
	p.a.chat.Fed.Flush(ctx)
	p.b.chat.Fed.WaitInvites()

	// B now mirrors the room and holds eve's invite.
	rooms := decode(t, do(t, p.bH, "GET", "/api/chat/rooms", "", withCookie(eve.cookie)))
	if len(rooms["rooms"].([]any)) != 1 {
		t.Fatalf("eve's rooms on B: %v", rooms["rooms"])
	}
	rv := rooms["rooms"].([]any)[0].(map[string]any)
	if rv["hubHere"] != false || rv["id"] != roomID || rv["participants"].(float64) != 2 || rv["households"].(float64) != 2 {
		t.Fatalf("replica view: %v", rv)
	}
	inv := decode(t, do(t, p.bH, "GET", "/api/chat/invites", "", withCookie(eve.cookie)))["invites"].([]any)
	if len(inv) != 1 {
		t.Fatalf("eve's invites on B: %v", inv)
	}
	wb, _ := people.DecodeKey(inv[0].(map[string]any)["welcome"].(string))
	if _, err := eve.c.JoinGroup(ctx, wb); err != nil {
		t.Fatalf("eve joins: %v", err)
	}
	do(t, p.bH, "POST", "/api/chat/invites/"+itoa(int(inv[0].(map[string]any)["id"].(float64)))+"/accept", "", withCookie(eve.cookie))

	// Eve posts through B; B relays to the hub on A; the operator reads it.
	ct, _ := eve.c.SendMessage(ctx, gid, mustContent(t, "hello from B"))
	w = submit(t, p.bH, eve, roomID, "mls_private", ct, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("eve posts via B: %d %s", w.Code, w.Body)
	}
	evs := decode(t, do(t, p.aH, "GET", "/api/chat/rooms/"+roomID+"/events?after=2", "", withCookie(op.cookie)))["events"].([]any)
	if len(evs) != 1 || evs[0].(map[string]any)["sender"] != eve.addr {
		t.Fatalf("hub log after eve's post: %v", evs)
	}
	payload, _ := people.DecodeKey(evs[0].(map[string]any)["payload"].(string))
	rm, err := op.c.ReceiveMessage(ctx, gid, payload)
	if err != nil {
		t.Fatalf("operator decrypts eve: %v", err)
	}
	if c, _ := chat.DecodeContent(rm.Plaintext); c.Body.Text != "hello from B" {
		t.Fatalf("content: %+v", c)
	}
	// The relay mirrored eve's own event into B's replica.
	bev := decode(t, do(t, p.bH, "GET", "/api/chat/rooms/"+roomID+"/events?after=2", "", withCookie(eve.cookie)))["events"].([]any)
	if len(bev) != 1 {
		t.Fatalf("B replica after relay: %v", bev)
	}

	// The operator answers on A; fan-out carries it to B; eve decrypts.
	ct2, _ := op.c.SendMessage(ctx, gid, mustContent(t, "hello from A"))
	if w := submit(t, p.aH, op, roomID, "mls_private", ct2, nil); w.Code != http.StatusOK {
		t.Fatalf("operator posts: %s", w.Body)
	}
	p.a.chat.Fed.Flush(ctx)
	p.b.chat.Fed.WaitInvites()
	bev = decode(t, do(t, p.bH, "GET", "/api/chat/rooms/"+roomID+"/events?after=3", "", withCookie(eve.cookie)))["events"].([]any)
	if len(bev) != 1 {
		t.Fatalf("B replica after fan-out: %v", bev)
	}
	payload2, _ := people.DecodeKey(bev[0].(map[string]any)["payload"].(string))
	rm2, err := eve.c.ReceiveMessage(ctx, gid, payload2)
	if err != nil {
		t.Fatalf("eve decrypts operator: %v", err)
	}
	if c, _ := chat.DecodeContent(rm2.Plaintext); c.Body.Text != "hello from A" {
		t.Fatalf("content: %+v", c)
	}
	// B's copy is ciphertext too.
	if strings.Contains(string(payload2), "hello from A") {
		t.Fatal("plaintext in the replica")
	}

	// Gap fill: events land on the hub while B is unreachable, then a sync
	// catches the replica up in order.
	for i := 0; i < 3; i++ {
		ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, fmt.Sprintf("burst %d", i)))
		submit(t, p.aH, op, roomID, "mls_private", ct, nil)
	}
	p.a.store.DB().Exec(`DELETE FROM fed_outbox`) // pretend the pushes were lost
	hubRoom, _ := p.a.store.ChatRoom(roomID)
	if err := p.b.chat.Fed.SyncRoom(ctx, p.aPeerOnB, roomID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	room2, _ := p.b.store.ChatRoom(roomID)
	if room2.Seq != hubRoom.Seq {
		t.Fatalf("replica seq after sync: %d, hub %d", room2.Seq, hubRoom.Seq)
	}

	// An idle re-sync of a room nothing happened in is a signed 304, and
	// nobody on B is told to refetch their room list. A hub too old to tag
	// its snapshots answers in full; the replica sees nothing changed and
	// stays quiet too.
	etag, _, _ := p.b.store.ChatRoomHubETag(roomID)
	if etag == "" {
		t.Fatal("the replica did not remember the hub's snapshot tag")
	}
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/chat/rooms/"+roomID, etag, nil); err != fed.ErrNotModified {
		t.Fatalf("conditional snapshot of an unchanged room: %v", err)
	}
	sub := p.b.chat.Bus.Subscribe(eve.addr, nil)
	for _, forget := range []bool{false, true} {
		if forget {
			p.b.store.SetChatRoomHubETag(roomID, "", 0)
		}
		if err := p.b.chat.Fed.SyncRoom(ctx, p.aPeerOnB, roomID); err != nil {
			t.Fatalf("idle sync: %v", err)
		}
		select {
		case n := <-sub.C:
			t.Fatalf("an idle sync (etag forgotten: %v) told eve to refetch: %+v", forget, n)
		default:
		}
	}
	p.b.chat.Bus.Unsubscribe(sub)

	// Typing crosses to the follower household at most once per throttle
	// window, however often a client sends it.
	tsub := p.b.chat.Bus.Subscribe(eve.addr, []string{roomID})
	for range 3 {
		if w := do(t, p.aH, "POST", "/api/chat/rooms/"+roomID+"/typing", "", withCookie(op.cookie)); w.Code != http.StatusNoContent {
			t.Fatalf("typing: %d %s", w.Code, w.Body)
		}
	}
	typed := 0
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case n := <-tsub.C:
			if n.Kind == "ephemeral" && n.Signal == "typing" {
				typed++
			}
		case <-deadline:
			break collect
		}
	}
	p.b.chat.Bus.Unsubscribe(tsub)
	if typed != 1 {
		t.Errorf("typing reached the follower %d times, want 1", typed)
	}

	// Eve's impact sentence names A as the host.
	detail := decode(t, do(t, p.bH, "GET", "/api/chat/rooms/"+roomID, "", withCookie(eve.cookie)))
	if imp := detail["impact"].(string); !strings.Contains(imp, "from 2 households") {
		t.Errorf("impact: %s", imp)
	}

	// A unpairs B: eve leaves the room on the hub with a household-cut, and
	// B's replica is archived and read-only.
	if w := do(t, p.aH, "DELETE", fmt.Sprintf("/api/federation/peers/%d", p.bPeerOnA.ID), "", withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("unpair: %d %s", w.Code, w.Body)
	}
	parts := decode(t, do(t, p.aH, "GET", "/api/chat/rooms/"+roomID, "", withCookie(op.cookie)))["participants"].([]any)
	for _, x := range parts {
		pm := x.(map[string]any)
		if pm["addr"] == eve.addr && pm["leftSeq"] == nil {
			t.Errorf("eve still a participant on the hub after unpair: %v", pm)
		}
	}
	last := decode(t, do(t, p.aH, "GET", "/api/chat/rooms/"+roomID+"/events?after=7", "", withCookie(op.cookie)))["events"].([]any)
	if len(last) != 1 || last[0].(map[string]any)["kind"] != "system" {
		t.Errorf("household-cut event: %v", last)
	}
	// The hub notices B is gone before B does; B's own view archives once it
	// learns (here: when its operator unpairs too).
	if w := do(t, p.bH, "DELETE", fmt.Sprintf("/api/federation/peers/%d", p.aPeerOnB.ID), "", withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("b unpair: %d %s", w.Code, w.Body)
	}
	rv = decode(t, do(t, p.bH, "GET", "/api/chat/rooms", "", withCookie(eve.cookie)))["rooms"].([]any)[0].(map[string]any)
	if rv["replicaStatus"] != "archived" {
		t.Errorf("replica after unpair: %v", rv["replicaStatus"])
	}
	ct3, _ := eve.c.SendMessage(ctx, gid, mustContent(t, "too late"))
	if w := submit(t, p.bH, eve, roomID, "mls_private", ct3, nil); w.Code != http.StatusGone {
		t.Errorf("posting into an archived replica: %d %s", w.Code, w.Body)
	}
}

func TestChatFedRefusesUngrantedPeer(t *testing.T) {
	t.Parallel()

	// A grants B nothing chat-related: B's fog cannot fetch A's key material
	// or read A's rooms, and A's people are absent from B's directory.
	p := twoFogs(t, `[{"capability":"apps.directory"}]`)
	ctx := context.Background()
	var out map[string]any
	_, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/chat/people/admin/keys", "", &out)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("ungranted key fetch: %v", err)
	}
	if err := p.b.fed.HeartbeatPeer(ctx, p.aPeerOnB); err != nil {
		t.Fatal(err)
	}
	// Who is in the household is shared by default (people.directory,
	// docs/signin-contract.md); how to reach them in chat is not.
	peer, _ := p.b.store.FedPeerByID(p.aPeerOnB.ID)
	for _, dp := range chat.PeerPeople(peer) {
		if dp.IdentityKey != "" || len(dp.Devices) != 0 {
			t.Fatalf("chat key material advertised without a chat grant: %s", peer.People)
		}
		if !strings.HasPrefix(dp.Address, "person:") {
			t.Fatalf("a directory entry without a person address: %+v", dp)
		}
	}
}
