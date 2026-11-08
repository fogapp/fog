package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// A deleted person leaves every room, and whoever is given their handle
// next inherits nothing: no room, no role, no ban, no history, no DM.
func TestChatDeletedPersonsHandleStartsFresh(t *testing.T) {

	srv, h := newTestServer(t)
	admin := setup(t, h)
	aliceS := inviteAndJoin(t, h, admin, "alice", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	alice := registerDevice(t, h, aliceS, "alice")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, alice)
	// alice is an admin there, with a webhook of her own.
	if w := patchRoom(t, h, op, roomID, `{"role":{"addr":"`+alice.addr+`","role":3}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if _, err := srv.store.CreateChatWebhook(roomID, "alice's", "digest-alice", alice.addr); err != nil {
		t.Fatal(err)
	}
	dgid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "dm", "participants": []string{alice.addr}, "groupId": people.EncodeKey(dgid), "groupInfo": groupInfoOf(t, op, dgid)})
	w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	dmID := decode(t, w)["room"].(map[string]any)["id"].(string)
	addToRoom(t, h, op, dgid, dmID, alice)

	if w := do(t, h, "DELETE", "/api/people/"+personID(t, h, admin, "alice"), "", withCookie(admin)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	for _, id := range []string{roomID, dmID} {
		if kinds := systemKinds(t, h, op, id); len(kinds) == 0 || kinds[len(kinds)-1] != chat.SystemIdentityReset {
			t.Errorf("no departure line in %s: %v", id, kinds)
		}
	}
	if hooks, _ := srv.store.ChatWebhooks(roomID); len(hooks) != 0 {
		t.Errorf("a deleted person's webhook still posts: %+v", hooks)
	}
	if dm, _ := srv.store.ChatRoom(dmID); dm.ArchivedAt == "" {
		t.Error("the deleted person's DM is still open")
	}

	// Someone new is given the handle.
	newS := inviteAndJoin(t, h, admin, "alice", "another horse battery")
	heir := registerDevice(t, h, newS, "alice")
	if heir.addr != alice.addr {
		t.Fatalf("setup: the heir's address %s", heir.addr)
	}
	rooms := decode(t, do(t, h, "GET", "/api/chat/rooms", "", withCookie(heir.cookie)))
	for _, r := range rooms["rooms"].([]any) {
		if id := r.(map[string]any)["id"]; id == roomID || id == dmID {
			t.Errorf("the heir lists the deleted person's room %v", id)
		}
	}
	for _, path := range []string{"/api/chat/rooms/" + roomID, "/api/chat/rooms/" + roomID + "/events?after=0", "/api/chat/rooms/" + dmID + "/events?after=0"} {
		if w := do(t, h, "GET", path, "", withCookie(heir.cookie)); w.Code != http.StatusForbidden {
			t.Errorf("the heir reads %s: %d %.200s", path, w.Code, w.Body)
		}
	}
	if w := patchRoom(t, h, heir, roomID, `{"label":"mine now"}`); w.Code != http.StatusForbidden {
		t.Errorf("the heir acts as the deleted admin: %d %s", w.Code, w.Body)
	}
	if w := patchRoom(t, h, heir, roomID, `{"role":{"addr":"`+op.addr+`","role":1}}`); w.Code != http.StatusForbidden {
		t.Errorf("the heir bans a member: %d %s", w.Code, w.Body)
	}
	// Nor is the heir the deleted person's DM partner.
	hgid, err := heir.c.CreateGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"kind": "dm", "participants": []string{op.addr}, "groupId": people.EncodeKey(hgid), "groupInfo": groupInfoOf(t, heir, hgid)})
	if w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(heir.cookie)); w.Code == http.StatusOK {
		if id := decode(t, w)["room"].(map[string]any)["id"]; id == dmID {
			t.Error("the heir was handed the deleted person's DM")
		}
	}
}

// A hub that never heard of a person's deletion (or reset) at home still
// tells the new holder of the address from the participant who joined: a
// different identity key acting under their address is counted out and
// refused.
func TestChatHubRefusesAnotherIdentityUnderAParticipantsAddress(t *testing.T) {

	p := twoChatFogs(t)
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	_, roomID := crossRoom(t, p, op, eve, "Cousins")
	if w := patchRoom(t, p.aH, op, roomID, `{"role":{"addr":"`+eve.addr+`","role":3}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if err := p.b.chat.Fed.SyncRoom(context.Background(), p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	// B deletes eve without a word to the hub, and gives the handle again.
	old, _ := p.b.store.PersonByHandle("eve")
	if err := p.b.store.DeletePerson(old.ID); err != nil {
		t.Fatal(err)
	}
	heirS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "another horse battery")
	heir := registerDevice(t, p.bH, heirS, "eve")
	time.Sleep(1100 * time.Millisecond)
	p.heartbeat(t)
	// As the admin eve was, through B: the hub refuses, and counts her out.
	if w := patchRoom(t, p.bH, heir, roomID, `{"label":"mine now"}`); w.Code == http.StatusOK {
		t.Fatalf("the hub let another identity act as eve: %s", w.Body)
	}
	if room, _ := p.a.store.ChatRoom(roomID); room.Label == "mine now" {
		t.Fatal("the label changed")
	}
	if row, _ := p.a.store.ChatParticipant(roomID, eve.addr); row == nil || row.LeftSeq == 0 {
		t.Errorf("the hub still counts eve in: %+v", row)
	}
	if kinds := systemKinds(t, p.aH, op, roomID); len(kinds) == 0 || kinds[len(kinds)-1] != chat.SystemIdentityReset {
		t.Errorf("no identity-reset line on the hub: %v", kinds)
	}
}

// The hub takes a room's first leaf, and every committer, only from a
// device its person's identity key attests.
func TestChatUnattestedDevicesCantCreateOrCommit(t *testing.T) {

	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	registerDevice(t, h, daveS, "dave") // dave's real, attested device
	ghost := registerUnattested(t, h, daveS, "dave")
	gid, err := ghost.c.CreateGroup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"kind": "group", "label": "Ghost's", "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, ghost, gid)})
	if w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(ghost.cookie)); w.Code != http.StatusForbidden {
		t.Fatalf("a room made from an unattested device: %d %s", w.Code, w.Body)
	}
	// A committer whose device no longer verifies (its attestation gone
	// from under it) commits nothing.
	op := registerDevice(t, h, admin, "admin")
	ogid, roomID := newGroupRoom(t, h, op, "group", "Us")
	if _, err := srv.store.DB().Exec(`UPDATE people_devices SET attestation = NULL WHERE device_key = ?`, mustDeviceKey(t, h, op)); err != nil {
		t.Fatal(err)
	}
	commit, err := op.c.SelfUpdate(context.Background(), ogid)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, nil); w.Code != http.StatusForbidden {
		t.Errorf("a commit from an unattested device: %d %s", w.Code, w.Body)
	}
}

// A ban reaches anyone who was ever in the room and anyone of the
// household, never-joined included; a banned person, like any outsider,
// gets a channel's summary and nothing of who is in it.
func TestChatBansReachFormerMembersAndOutsiders(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	erinS := inviteAndJoin(t, h, admin, "erin", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	erin := registerDevice(t, h, erinS, "erin")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "channel", "Town square")
	addToRoom(t, h, op, gid, roomID, dave)
	if w := patchRoom(t, h, dave, roomID, `{"leave":true}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	for _, who := range []*testDevice{dave, erin} {
		if w := patchRoom(t, h, op, roomID, `{"role":{"addr":"`+who.addr+`","role":1}}`); w.Code != http.StatusOK {
			t.Fatalf("banning %s: %d %s", who.handle, w.Code, w.Body)
		}
	}
	// Neither gets back in: not by an Add, not by joining the channel.
	commit, welcome, err := op.c.InviteMember(ctx, gid, freshKeys(t, h, erin, op.cookie))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusForbidden {
		t.Errorf("a banned outsider added: %d %s", w.Code, w.Body)
	}
	gi, _ := op.c.GroupInfo(ctx, gid)
	if _, ext, err := erin.c.ExternalJoin(ctx, gi); err == nil {
		if w := submit(t, h, erin, roomID, "mls_commit", ext, nil); w.Code == http.StatusOK {
			t.Errorf("a banned outsider joined the channel: %d %s", w.Code, w.Body)
		}
	}
	// The ban gives erin nothing of the room's past.
	if w := do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?after=0", "", withCookie(erin.cookie)); w.Code == http.StatusOK && len(decode(t, w)["events"].([]any)) != 0 {
		t.Errorf("a ban opened the log: %s", w.Body)
	}
	for _, who := range []*testDevice{dave, erin} {
		w := do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(who.cookie))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: the channel's summary: %d %s", who.handle, w.Code, w.Body)
		}
		d := decode(t, w)
		if parts := d["participants"].([]any); len(parts) != 0 {
			t.Errorf("%s (banned) sees who is in the room: %v", who.handle, parts)
		}
		if leaves := d["leaves"].([]any); len(leaves) != 0 {
			t.Errorf("%s (banned) sees the room's devices", who.handle)
		}
		if roles := d["room"].(map[string]any)["roles"].(map[string]any); len(roles) != 0 {
			t.Errorf("%s (banned) sees the room's roles: %v", who.handle, roles)
		}
	}
}

// Attachments are held to per-person, per-room and overall quotas.
func TestChatAttachmentQuotas(t *testing.T) {
	defer func(p, r, a int64) { chat.QuotaPerPerson, chat.QuotaPerRoom, chat.QuotaTotal = p, r, a }(chat.QuotaPerPerson, chat.QuotaPerRoom, chat.QuotaTotal)
	for name, q := range map[string]*int64{"person": &chat.QuotaPerPerson, "room": &chat.QuotaPerRoom, "total": &chat.QuotaTotal} {
		t.Run(name, func(t *testing.T) {
			_, h := newTestServer(t)
			admin := setup(t, h)
			op := registerDevice(t, h, admin, "admin")
			_, roomID := newGroupRoom(t, h, op, "group", "Us")
			chat.QuotaPerPerson, chat.QuotaPerRoom, chat.QuotaTotal = 1<<30, 1<<30, 1<<30
			*q = 40
			if w := do(t, h, "POST", "/api/chat/blobs?room="+roomID, strings.Repeat("a", 30), withCookie(op.cookie)); w.Code != http.StatusOK {
				t.Fatalf("within the quota: %d %s", w.Code, w.Body)
			}
			w := do(t, h, "POST", "/api/chat/blobs?room="+roomID, strings.Repeat("b", 30), withCookie(op.cookie))
			if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), `"quota"`) {
				t.Errorf("past the %s quota: %d %s", name, w.Code, w.Body)
			}
		})
	}
}

// An address that has held another identity key here — reset, or a deleted
// person's handle given again — can't walk into an open channel by itself,
// even one it was never in: members' clients that pinned the old key would
// refuse the leaf and go stale. A member's Add is the way in.
func TestChatOpenChannelsTakeNoChangedIdentityByItself(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	op := registerDevice(t, h, admin, "admin")
	ctx := context.Background()
	for _, how := range []string{"reset", "deleted"} {
		t.Run(how, func(t *testing.T) {
			handle := "dave" + how
			s := inviteAndJoin(t, h, admin, handle, "correct horse battery")
			dave := registerDevice(t, h, s, handle)
			gid, id := newGroupRoom(t, h, op, "group", "Us "+how)
			addToRoom(t, h, op, gid, id, dave)
			switch how {
			case "reset":
				if w := do(t, h, "POST", "/api/people/"+personID(t, h, admin, handle)+"/reset-identity", "", withCookie(admin)); w.Code != http.StatusOK {
					t.Fatal(w.Body)
				}
			case "deleted":
				if w := do(t, h, "DELETE", "/api/people/"+personID(t, h, admin, handle), "", withCookie(admin)); w.Code != http.StatusOK {
					t.Fatal(w.Body)
				}
				s = inviteAndJoin(t, h, admin, handle, "another horse battery")
			}
			heir := registerDevice(t, h, s, handle)
			// A channel created after, which no one under this address was in.
			cgid, cid := newGroupRoom(t, h, op, "channel", "Square "+how)
			gi, _ := op.c.GroupInfo(ctx, cgid)
			_, ext, err := heir.c.ExternalJoin(ctx, gi)
			if err != nil {
				t.Fatal(err)
			}
			if w := submit(t, h, heir, cid, "mls_commit", ext, nil); w.Code == http.StatusOK {
				t.Errorf("a %s address's new identity let itself into an open channel", how)
			}
		})
	}
}

// A reset at home reaches the room's hub even when the hub can't be reached
// at the time: the departure is owed until the hub takes it, and the hub
// takes it for a person no longer in their household's directory.
func TestChatDeparturesReachTheHubLater(t *testing.T) {

	p := twoChatFogs(t)
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	_, roomID := crossRoom(t, p, op, eve, "Cousins")
	if err := p.b.chat.Fed.SyncRoom(context.Background(), p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	// B deletes eve while the hub is away.
	aURL := p.aPeerOnB.URL
	p.b.store.UpdateFedPeerURL(p.aPeerOnB.ID, "http://127.0.0.1:1")
	if w := do(t, p.bH, "DELETE", "/api/people/"+personID(t, p.bH, p.bCookie, "eve"), "", withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	p.b.chat.Fed.Flush(context.Background())
	var queued int
	p.b.store.DB().QueryRow(`SELECT COUNT(*) FROM fed_outbox WHERE kind = ?`, chat.DepartureKind).Scan(&queued)
	if queued == 0 {
		t.Fatal("the departure wasn't kept while the hub was away")
	}
	if row, _ := p.a.store.ChatParticipant(roomID, eve.addr); row == nil || row.LeftSeq != 0 {
		t.Fatalf("setup: the hub heard while away: %+v", row)
	}
	// The hub is back; its retry falls due.
	p.b.store.UpdateFedPeerURL(p.aPeerOnB.ID, aURL)
	p.b.store.DB().Exec(`UPDATE fed_outbox SET next_attempt_at = '' WHERE kind = ?`, chat.DepartureKind)
	// The directories move on: eve is in B's no longer.
	time.Sleep(1100 * time.Millisecond)
	p.heartbeat(t)
	p.b.chat.Fed.Flush(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if row, _ := p.a.store.ChatParticipant(roomID, eve.addr); row != nil && row.LeftSeq != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hub never took the deleted person out")
		}
		p.b.chat.Fed.Flush(context.Background())
		time.Sleep(50 * time.Millisecond)
	}
	p.b.store.DB().QueryRow(`SELECT COUNT(*) FROM fed_outbox WHERE kind = ?`, chat.DepartureKind).Scan(&queued)
	if queued != 0 {
		t.Errorf("%d departures still owed after the hub took them", queued)
	}
}

// A gap in a household's directory — a narrowed grant, a person no longer
// listed — says nothing about a device: it doesn't let a plain member remove
// a remote admin's leaf as "revoked".
func TestChatDirectoryGapsDontMakeLeavesRemovable(t *testing.T) {

	p := twoChatFogs(t)
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	daveS := inviteAndJoin(t, p.aH, p.aCookie, "dave", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	dave := registerDevice(t, p.aH, daveS, "dave")
	p.heartbeat(t)
	ctx := context.Background()
	gid, roomID := crossRoom(t, p, op, eve, "Cousins")
	addToRoom(t, p.aH, op, gid, roomID, dave)
	if w := patchRoom(t, p.aH, op, roomID, `{"role":{"addr":"`+eve.addr+`","role":3}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	// B stops showing eve to A.
	p.b.store.SetFedCapability(p.aPeerOnB.ID, store.CapPeopleDirectory, store.DirGrant, `{"people":["nobody"]}`)
	time.Sleep(1100 * time.Millisecond)
	p.heartbeat(t)
	// dave, a plain member, removes eve's leaf.
	commit, err := dave.c.RemoveMember(ctx, gid, []byte(eve.addr))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, p.aH, dave, roomID, "mls_commit", commit, nil); w.Code != http.StatusForbidden {
		t.Errorf("a plain member removed a remote admin her household stopped listing: %d %s", w.Code, w.Body)
	}
}

// Every commit carries the GroupInfo of the epoch it makes: a member can't
// leave the hub without a current one for newcomers checking a Welcome.
func TestChatCommitsMustCarryGroupInfo(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	commit, welcome, err := op.c.InviteMember(context.Background(), gid, freshKeys(t, h, dave, op.cookie))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"id": ulid(t), "kind": "mls_commit", "payload": people.EncodeKey(commit), "welcome": people.EncodeKey(welcome)})
	w := do(t, h, "POST", "/api/chat/rooms/"+roomID+"/events", string(body), withCookie(op.cookie))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "GroupInfo") {
		t.Errorf("a commit without its GroupInfo: %d %s", w.Code, w.Body)
	}
}

// A follower serving the hub's GroupInfo serves a log that reaches it: a
// newcomer whose fog's copy lagged the hub by a commit isn't left short.
func TestChatFollowerServesALogThatReachesTheGroupInfo(t *testing.T) {

	p := twoChatFogs(t)
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	ctx := context.Background()
	gid, roomID := crossRoom(t, p, op, eve, "Cousins")
	if err := p.b.chat.Fed.SyncRoom(ctx, p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	// The hub can't push to B for now: B's copy will lag.
	bURL := p.bPeerOnA.URL
	p.a.store.UpdateFedPeerURL(p.bPeerOnA.ID, "http://127.0.0.1:1")
	daveS := inviteAndJoin(t, p.aH, p.aCookie, "dave", "correct horse battery")
	dave := registerDevice(t, p.aH, daveS, "dave")
	addToRoom(t, p.aH, op, gid, roomID, dave)
	hub, _ := p.a.store.ChatRoom(roomID)
	if r, _ := p.b.store.ChatRoom(roomID); r.Epoch >= hub.Epoch {
		t.Skip("setup: B's copy didn't lag")
	}
	w := do(t, p.bH, "GET", "/api/chat/rooms/"+roomID+"/groupinfo", "", withCookie(eve.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("groupinfo via B: %d %s", w.Code, w.Body)
	}
	epoch := int64(decode(t, w)["epoch"].(float64))
	if r, _ := p.b.store.ChatRoom(roomID); r.Epoch < epoch {
		t.Errorf("B served GroupInfo for epoch %d with its log at epoch %d", epoch, r.Epoch)
	}
	p.a.store.UpdateFedPeerURL(p.bPeerOnA.ID, bURL)
}

// A stale device leaves a room on that device only: its person stays, with
// their role, and comes back on a new device any member may add — in a DM,
// where no member may add a person, and for a sole owner, who stays owner.
func TestChatADeviceLeavesWithoutItsPerson(t *testing.T) {

	for _, kind := range []string{"dm", "group"} {
		t.Run(kind, func(t *testing.T) {
			srv, h := newTestServer(t)
			admin := setup(t, h)
			daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
			op := registerDevice(t, h, admin, "admin")
			dave := registerDevice(t, h, daveS, "dave")
			ctx := context.Background()
			var gid []byte
			var roomID string
			if kind == "dm" {
				gid, _ = op.c.CreateGroup(ctx)
				body, _ := json.Marshal(map[string]any{"kind": "dm", "participants": []string{dave.addr}, "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
				w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
				if w.Code != http.StatusOK {
					t.Fatal(w.Body)
				}
				roomID = decode(t, w)["room"].(map[string]any)["id"].(string)
			} else {
				gid, roomID = newGroupRoom(t, h, op, "group", "Us")
			}
			addToRoom(t, h, op, gid, roomID, dave)
			// op — the sole owner — went stale on this device: it leaves the
			// room on this device.
			if w := patchRoom(t, h, op, roomID, `{"leaveDevice":true}`); w.Code != http.StatusOK {
				t.Fatalf("leave on this device: %d %s", w.Code, w.Body)
			}
			if kinds := systemKinds(t, h, dave, roomID); kinds[len(kinds)-1] != chat.SystemDeviceLeft {
				t.Fatalf("no device-left line: %v", kinds)
			}
			// dave — a plain member, who may not remove or add people —
			// removes the leaf op's device left, and adds op's new device.
			rm, err := dave.c.RemoveMember(ctx, gid, []byte(op.addr))
			if err != nil {
				t.Fatal(err)
			}
			if w := submit(t, h, dave, roomID, "mls_commit", rm, nil); w.Code != http.StatusOK {
				t.Fatalf("a member removing a leaf its device left: %d %s", w.Code, w.Body)
			}
			if p := participantRow(t, h, dave, roomID, op.addr); p == nil || p["leftSeq"] != nil || p["roleIndex"].(float64) != 4 {
				t.Fatalf("op lost their place or their role when their device left: %v", p)
			}
			op2 := registerDevice(t, h, admin, "admin")
			commit, welcome, err := dave.c.InviteMember(ctx, gid, freshKeys(t, h, op2, dave.cookie))
			if err != nil {
				t.Fatal(err)
			}
			if w := submit(t, h, dave, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
				t.Fatalf("a member adding a device of someone in the room: %d %s", w.Code, w.Body)
			}
			if _, err := op2.c.JoinGroup(ctx, welcome); err != nil {
				t.Fatalf("op's new device joins: %v", err)
			}
			if p := participantRow(t, h, dave, roomID, op.addr); p == nil || p["leftSeq"] != nil || p["roleIndex"].(float64) != 4 {
				t.Errorf("op isn't owner after their device came back: %v", p)
			}
			room, _ := srv.store.ChatRoom(roomID)
			if room.Kind != kind {
				t.Errorf("the room changed kind: %s", room.Kind)
			}
			// In a DM, where no member may add a person, adding someone else
			// is still refused: a device of someone in it is all that came in.
			if kind == "dm" {
				erinS := inviteAndJoin(t, h, admin, "erin", "correct horse battery")
				erin := registerDevice(t, h, erinS, "erin")
				c2, w2, _ := dave.c.InviteMember(ctx, gid, freshKeys(t, h, erin, dave.cookie))
				if w := submit(t, h, dave, roomID, "mls_commit", c2, w2); w.Code == http.StatusOK {
					t.Errorf("a DM member added a new person")
				}
			}
		})
	}
}

// A device restored from a backup replaces the key packages it published
// before, whose private keys it no longer holds: a member's Welcome then
// goes to one it can open.
func TestChatARestoredDeviceReplacesItsKeyPackages(t *testing.T) {

	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	old := freshKeys(t, h, dave, op.cookie)
	fresh, _ := dave.c.FreshKeyPackageBytes(context.Background())
	last, _ := dave.c.FreshKeyPackageBytes(context.Background())
	body, _ := json.Marshal(map[string]any{"ciphersuite": 1, "keyPackages": []string{people.EncodeKey(fresh)}, "lastResort": people.EncodeKey(last), "replace": true})
	if w := do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(dave.id))+"/keypackages", string(body), withCookie(dave.cookie)); w.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", w.Code, w.Body)
	}
	got := deviceKeys(t, h, op.cookie, dave)
	if string(got) == string(old) || string(got) != string(fresh) {
		t.Error("a member was handed a package the restored device replaced")
	}
	got2 := deviceKeys(t, h, op.cookie, dave)
	if string(got2) != string(last) {
		t.Error("past the fresh one, the old packages still came out before the new last-resort one")
	}
}
