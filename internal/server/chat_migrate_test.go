package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/people"
)

// A room's hub moves from A to B: the log keeps its numbering, both sides
// keep posting (now through B), a hub-moved notice is the first event B
// sequences, and the old hub is a follower that stays in sync.
func TestChatRoomMigratesHub(t *testing.T) {
	p := twoChatFogs(t)
	ctx := context.Background()
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	aInstance, _ := p.a.chat.Instance()
	bInstance, _ := p.b.chat.Instance()

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Movers", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, p.aH, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	km := decode(t, do(t, p.aH, "GET", "/api/chat/keys?addr="+eve.addr, "", withCookie(op.cookie)))
	eveKP, _ := people.DecodeKey(km["devices"].([]any)[0].(map[string]any)["keyPackage"].(string))
	commit, welcome, err := op.c.InviteMember(ctx, gid, eveKP)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, p.aH, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add eve: %d %s", w.Code, w.Body)
	}
	p.a.chat.Fed.Flush(ctx)
	p.b.chat.Fed.WaitInvites()
	inv := decode(t, do(t, p.bH, "GET", "/api/chat/invites", "", withCookie(eve.cookie)))["invites"].([]any)
	wb, _ := people.DecodeKey(inv[0].(map[string]any)["welcome"].(string))
	if _, err := eve.c.JoinGroup(ctx, wb); err != nil {
		t.Fatal(err)
	}
	do(t, p.bH, "POST", "/api/chat/invites/"+itoa(int(inv[0].(map[string]any)["id"].(float64)))+"/accept", "", withCookie(eve.cookie))
	// An attachment on the old hub, to be carried across.
	blob := decode(t, do(t, p.aH, "POST", "/api/chat/blobs?room="+roomID, "sealed bytes", withCookie(op.cookie)))
	sha, _ := blob["sha256"].(string)
	ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, "before the move"))
	submit(t, p.aH, op, roomID, "mls_private", ct, nil)
	p.a.chat.Fed.Flush(ctx)
	p.b.chat.Fed.WaitInvites()
	hubBefore, _ := p.a.store.ChatRoom(roomID)

	// Eve (a member, not an admin) cannot move it; a stranger household is refused.
	if w := do(t, p.bH, "POST", "/api/chat/rooms/"+roomID+"/migrate", fmt.Sprintf(`{"to":%q}`, aInstance), withCookie(eve.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("replica-side migrate: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.aH, "POST", "/api/chat/rooms/"+roomID+"/migrate", `{"to":"01NOPE"}`, withCookie(op.cookie)); w.Code != http.StatusBadRequest {
		t.Errorf("unknown household: %d %s", w.Code, w.Body)
	}
	// B doesn't take the room on because A says so: refused until one of
	// B's own people in it, with the right, agrees.
	if w := do(t, p.aH, "POST", "/api/chat/rooms/"+roomID+"/migrate", fmt.Sprintf(`{"to":%q}`, bInstance), withCookie(op.cookie)); w.Code == http.StatusOK {
		t.Fatalf("B adopted a room nobody there agreed to host: %s", w.Body)
	}
	if w := patchRoom(t, p.bH, eve, roomID, `{"acceptHub":true}`); w.Code != http.StatusForbidden {
		t.Errorf("a plain member agreeing to host: %d %s", w.Code, w.Body)
	}
	if w := patchRoom(t, p.aH, op, roomID, `{"role":{"addr":"`+eve.addr+`","role":3}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if err := p.b.chat.Fed.SyncRoom(ctx, p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	if w := patchRoom(t, p.bH, eve, roomID, `{"acceptHub":true}`); w.Code != http.StatusOK {
		t.Fatalf("an admin agreeing to host: %d %s", w.Code, w.Body)
	}
	// The owner moves it to B.
	w := do(t, p.aH, "POST", "/api/chat/rooms/"+roomID+"/migrate", fmt.Sprintf(`{"to":%q}`, bInstance), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("migrate: %d %s", w.Code, w.Body)
	}
	aRoom, _ := p.a.store.ChatRoom(roomID)
	bRoom, _ := p.b.store.ChatRoom(roomID)
	if aRoom.HubInstance != bInstance || aRoom.ReplicaStatus != "live" {
		t.Fatalf("A after move: hub=%q status=%q", aRoom.HubInstance, aRoom.ReplicaStatus)
	}
	if bRoom.HubInstance != "" || bRoom.Seq != hubBefore.Seq+1 {
		t.Fatalf("B after move: hub=%q seq=%d (was %d)", bRoom.HubInstance, bRoom.Seq, hubBefore.Seq)
	}
	// B's first act as hub is the notice; A already mirrors it.
	last := decode(t, do(t, p.bH, "GET", fmt.Sprintf("/api/chat/rooms/%s/events?after=%d", roomID, hubBefore.Seq), "", withCookie(eve.cookie)))["events"].([]any)
	if len(last) != 1 || last[0].(map[string]any)["kind"] != "system" {
		t.Fatalf("hub-moved notice on B: %v", last)
	}
	var sys map[string]any
	pl, _ := people.DecodeKey(last[0].(map[string]any)["payload"].(string))
	json.Unmarshal(pl, &sys)
	if sys["kind"] != "hub-moved" || sys["from"] != aInstance || sys["to"] != bInstance {
		t.Errorf("notice payload: %v", sys)
	}
	if aRoom.Seq != bRoom.Seq {
		t.Errorf("A did not catch up: %d vs %d", aRoom.Seq, bRoom.Seq)
	}
	// The blob comes across behind the adopt reply, served by A — a
	// follower now — from its copy.
	if sha == "" {
		t.Fatal("no blob uploaded")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := p.b.store.ChatBlob(sha)
		if err == nil && b.RoomID == roomID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blob not on B: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Room views say who hubs it now.
	av := decode(t, do(t, p.aH, "GET", "/api/chat/rooms/"+roomID, "", withCookie(op.cookie)))["room"].(map[string]any)
	bv := decode(t, do(t, p.bH, "GET", "/api/chat/rooms/"+roomID, "", withCookie(eve.cookie)))["room"].(map[string]any)
	if av["hubHere"] != false || bv["hubHere"] != true {
		t.Errorf("hubHere after move: A=%v B=%v", av["hubHere"], bv["hubHere"])
	}

	// Both sides keep talking: eve posts on the new hub, the operator's post
	// on A is relayed to B and mirrored back.
	ct1, _ := eve.c.SendMessage(ctx, gid, mustContent(t, "eve on the new hub"))
	if w := submit(t, p.bH, eve, roomID, "mls_private", ct1, nil); w.Code != http.StatusOK {
		t.Fatalf("eve posts on B: %d %s", w.Code, w.Body)
	}
	p.b.chat.Fed.Flush(ctx)
	ct2, _ := op.c.SendMessage(ctx, gid, mustContent(t, "operator via relay"))
	if w := submit(t, p.aH, op, roomID, "mls_private", ct2, nil); w.Code != http.StatusOK {
		t.Fatalf("operator posts via A: %d %s", w.Code, w.Body)
	}
	aRoom, _ = p.a.store.ChatRoom(roomID)
	bRoom, _ = p.b.store.ChatRoom(roomID)
	if aRoom.Seq != bRoom.Seq || bRoom.Seq != hubBefore.Seq+3 {
		t.Fatalf("seqs after posting: A=%d B=%d", aRoom.Seq, bRoom.Seq)
	}
	evs := decode(t, do(t, p.aH, "GET", fmt.Sprintf("/api/chat/rooms/%s/events?after=%d", roomID, hubBefore.Seq+1), "", withCookie(op.cookie)))["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("A's view of the new posts: %v", evs)
	}
	pl1, _ := people.DecodeKey(evs[0].(map[string]any)["payload"].(string))
	rm, err := op.c.ReceiveMessage(ctx, gid, pl1)
	if err != nil {
		t.Fatalf("operator decrypts eve after move: %v", err)
	}
	if c, _ := chat.DecodeContent(rm.Plaintext); c.Body.Text != "eve on the new hub" {
		t.Errorf("content: %+v", c)
	}
	pl2, _ := people.DecodeKey(evs[1].(map[string]any)["payload"].(string))
	if rm2, err := eve.c.ReceiveMessage(ctx, gid, pl2); err != nil {
		t.Fatalf("eve decrypts operator after move: %v", err)
	} else if c, _ := chat.DecodeContent(rm2.Plaintext); c.Body.Text != "operator via relay" {
		t.Errorf("content: %+v", c)
	}
	// The old hub cannot move it again: it is not the hub.
	if w := do(t, p.aH, "POST", "/api/chat/rooms/"+roomID+"/migrate", fmt.Sprintf(`{"to":%q}`, bInstance), withCookie(op.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("second migrate from a follower: %d %s", w.Code, w.Body)
	}
}

// A replica fetches an attachment it lacks from the hub by digest, and the
// bytes must match the digest: a hub (or anything between) can withhold a
// blob but not substitute one. A name that isn't a digest never reaches the
// filesystem or the hub.
func TestChatReplicaBlobFetchChecksDigest(t *testing.T) {
	p := twoChatFogs(t)
	ctx := context.Background()
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Pics", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	roomID := decode(t, do(t, p.aH, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie)))["room"].(map[string]any)["id"].(string)
	km := decode(t, do(t, p.aH, "GET", "/api/chat/keys?addr="+eve.addr, "", withCookie(op.cookie)))
	eveKP, _ := people.DecodeKey(km["devices"].([]any)[0].(map[string]any)["keyPackage"].(string))
	commit, welcome, err := op.c.InviteMember(ctx, gid, eveKP)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, p.aH, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add eve: %d %s", w.Code, w.Body)
	}
	p.a.chat.Fed.Flush(ctx)
	p.b.chat.Fed.WaitInvites()
	inv := decode(t, do(t, p.bH, "GET", "/api/chat/invites", "", withCookie(eve.cookie)))["invites"].([]any)
	do(t, p.bH, "POST", "/api/chat/invites/"+itoa(int(inv[0].(map[string]any)["id"].(float64)))+"/accept", "", withCookie(eve.cookie))

	sha := decode(t, do(t, p.aH, "POST", "/api/chat/blobs?room="+roomID, "sealed bytes", withCookie(op.cookie)))["sha256"].(string)
	onA := filepath.Join(p.a.chat.DataDir, "chat", "blobs", sha[:2], sha)
	if err := os.WriteFile(onA, []byte("other bytes!"), 0o644); err != nil {
		t.Fatal(err)
	}
	get := "/api/chat/blobs/" + sha + "?room=" + roomID
	if w := do(t, p.bH, "GET", get, "", withCookie(eve.cookie)); w.Code == http.StatusOK {
		t.Fatalf("substituted blob served: %q", w.Body)
	}
	if _, err := p.b.store.ChatBlob(sha); err == nil {
		t.Error("substituted blob recorded on B")
	}
	if _, err := os.Stat(filepath.Join(p.b.chat.DataDir, "chat", "blobs", sha[:2], sha)); err == nil {
		t.Error("substituted blob written on B")
	}

	// With the right bytes back on the hub, the same fetch succeeds.
	if err := os.WriteFile(onA, []byte("sealed bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(t, p.bH, "GET", get, "", withCookie(eve.cookie)); w.Code != http.StatusOK || w.Body.String() != "sealed bytes" {
		t.Fatalf("fetch from hub: %d %q", w.Code, w.Body)
	}

	// ("..", which the mux cleans away before routing, is covered in chat.)
	for _, bad := range []string{"a", "%2e%2e", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if w := do(t, p.bH, "GET", "/api/chat/blobs/"+bad+"?room="+roomID, "", withCookie(eve.cookie)); w.Code != http.StatusNotFound {
			t.Errorf("blob name %q: %d %s", bad, w.Code, w.Body)
		}
	}
}
