package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mls "github.com/thomas-vilte/mls-go"
	"github.com/thomas-vilte/mls-go/ciphersuite"
	"github.com/thomas-vilte/mls-go/keypackages"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// remoteKeys fetches a person's key material through h's fog and returns
// the KeyPackage of their device with the given key.
func remoteKeys(t *testing.T, h http.Handler, as *http.Cookie, addr string, deviceKey []byte) []byte {
	t.Helper()
	w := do(t, h, "GET", "/api/chat/keys?addr="+addr, "", withCookie(as))
	if w.Code != http.StatusOK {
		t.Fatalf("keys for %s: %d %s", addr, w.Code, w.Body)
	}
	for _, x := range decode(t, w)["devices"].([]any) {
		d := x.(map[string]any)
		if d["deviceKey"] == people.EncodeKey(deviceKey) {
			kpS, ok := d["keyPackage"].(string)
			if !ok {
				t.Fatalf("%s's device has no key package left", addr)
			}
			kp, _ := people.DecodeKey(kpS)
			return kp
		}
	}
	t.Fatalf("no device %x for %s", deviceKey, addr)
	return nil
}

// crossRoom makes a group on A with A's operator and B's eve in it.
func crossRoom(t *testing.T, p *chatPair, op, eve *testDevice, label string) ([]byte, string) {
	t.Helper()
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, p.aH, op, "group", label)
	commit, welcome, err := op.c.InviteMember(ctx, gid, remoteKeys(t, p.aH, op.cookie, eve.addr, mustDeviceKey(t, p.bH, eve)))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, p.aH, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add eve: %d %s", w.Code, w.Body)
	}
	p.a.chat.Fed.Flush(ctx)
	p.b.chat.Fed.WaitInvites()
	if _, err := eve.c.JoinGroup(ctx, welcome); err != nil {
		t.Fatalf("eve joins: %v", err)
	}
	return gid, roomID
}

// Findings 4, 9 and 10 across households: a remote person's unattested
// device can't let itself in; a follower relays its people's room edits to
// the hub, which alone decides them; and a household whose people have all
// left a room reads nothing of it from then on.
func TestChatAcrossHouseholdsHoldsTheRules(t *testing.T) {

	p := twoChatFogs(t)
	ctx := context.Background()
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	gid, roomID := crossRoom(t, p, op, eve, "Cousins")

	// Finding 4: eve's second device, which her identity key never
	// attested, tries to let itself in through B.
	tablet := registerUnattested(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	if w := do(t, p.bH, "GET", "/api/chat/rooms/"+roomID+"/groupinfo", "", withCookie(tablet.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("remote unverified device fetching group info: %d %s", w.Code, w.Body)
	}
	gi, _ := op.c.GroupInfo(ctx, gid)
	_, ext, err := tablet.c.ExternalJoin(ctx, gi)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, p.bH, tablet, roomID, "mls_commit", ext, nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not verified") {
		t.Fatalf("remote unverified device self-joining: %d %s", w.Code, w.Body)
	}

	// Finding 9: eve, made admin on the hub, renames the room from B. The
	// hub takes it, and B's replica shows what the hub decided.
	if w := patchRoom(t, p.aH, op, roomID, `{"role":{"addr":"`+eve.addr+`","role":3}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if err := p.b.chat.Fed.SyncRoom(ctx, p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	if w := patchRoom(t, p.bH, eve, roomID, `{"label":"Cousins & co"}`); w.Code != http.StatusOK {
		t.Fatalf("follower rename: %d %s", w.Code, w.Body)
	}
	if hub, _ := p.a.store.ChatRoom(roomID); hub.Label != "Cousins & co" {
		t.Errorf("the hub never heard of the rename: %q", hub.Label)
	}
	if rep, _ := p.b.store.ChatRoom(roomID); rep.Label != "Cousins & co" {
		t.Errorf("the replica doesn't show the hub's label: %q", rep.Label)
	}
	// What the hub refuses stays refused, on B as on A: an admin demoting
	// the owner, from a replica that would once have written it locally.
	if w := patchRoom(t, p.bH, eve, roomID, `{"role":{"addr":"`+op.addr+`","role":2}}`); w.Code != http.StatusForbidden {
		t.Errorf("follower demoting the hub's owner: %d %s", w.Code, w.Body)
	}
	for name, st := range map[string]*store.Store{"hub": p.a.store, "replica": p.b.store} {
		if row, _ := st.ChatParticipant(roomID, op.addr); row == nil || row.RoleIndex != chat.RoleOwner {
			t.Errorf("owner's role on the %s: %+v", name, row)
		}
	}

	// Finding 10: eve leaves from B. B has no one left in the room: it
	// learns she is out, archives its copy, and the hub serves it nothing
	// sequenced after.
	putBlob := func(body string) string {
		w := doRaw(t, p.aH, "POST", "/api/chat/blobs?room="+roomID, body, op.cookie)
		return decode(t, w)["sha256"].(string)
	}
	before := putBlob("before")
	if w := patchRoom(t, p.bH, eve, roomID, `{"leave":true}`); w.Code != http.StatusOK {
		t.Fatalf("leave via B: %d %s", w.Code, w.Body)
	}
	hubRow, _ := p.a.store.ChatParticipant(roomID, eve.addr)
	if hubRow == nil || hubRow.LeftSeq == 0 {
		t.Fatalf("eve still in on the hub: %+v", hubRow)
	}
	rep, _ := p.b.store.ChatRoom(roomID)
	repRow, _ := p.b.store.ChatParticipant(roomID, eve.addr)
	if rep.ReplicaStatus != "archived" || repRow == nil || repRow.LeftSeq != hubRow.LeftSeq {
		t.Errorf("B after eve left: status %q, row %+v (hub says left at %d)", rep.ReplicaStatus, repRow, hubRow.LeftSeq)
	}
	ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, "after B is gone"))
	if w := submit(t, p.aH, op, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	after := putBlob("after")
	var evs struct {
		Events []store.ChatEvent `json:"events"`
		Seq    int64             `json:"seq"`
	}
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/chat/rooms/"+roomID+"/events?after=0", "", &evs); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs.Events {
		if e.Seq > hubRow.LeftSeq {
			t.Errorf("hub served B seq %d, after its last person left at %d", e.Seq, hubRow.LeftSeq)
		}
	}
	if evs.Seq != hubRow.LeftSeq {
		t.Errorf("hub told B the room is at seq %d", evs.Seq)
	}
	var snap chat.RoomSnapshot
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/chat/rooms/"+roomID, "", &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Departed || snap.Room.Label != "" || snap.Tree != "" || len(snap.Participants) != 1 {
		t.Errorf("B's snapshot after it left: %+v", snap)
	}
	if err := p.b.chat.Fed.FetchBlob(ctx, rep, after); err == nil {
		t.Errorf("B fetched an attachment stored after its people left")
	}
	if err := p.b.chat.Fed.FetchBlob(ctx, rep, before); err != nil {
		t.Errorf("B can't fetch an attachment from while eve was in: %v", err)
	}
}

// Finding 3 across households: an operator resets a person's identity on
// their home fog; the hub of a room they are in hears of it and takes them
// out, with the warning line.
func TestChatIdentityResetReachesTheHub(t *testing.T) {

	p := twoChatFogs(t)
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	_, roomID := crossRoom(t, p, op, eve, "Cousins")
	if err := p.b.chat.Fed.SyncRoom(context.Background(), p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	if w := do(t, p.bH, "POST", "/api/people/"+personID(t, p.bH, p.bCookie, "eve")+"/reset-identity", "", withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if row, _ := p.a.store.ChatParticipant(roomID, eve.addr); row != nil && row.LeftSeq != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hub never took eve out after her identity was reset at home")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if kinds := systemKinds(t, p.aH, op, roomID); len(kinds) == 0 || kinds[len(kinds)-1] != chat.SystemIdentityReset {
		t.Errorf("no identity-reset line on the hub: %v", kinds)
	}
}

// doRaw posts a raw body with a cookie.
func doRaw(t *testing.T, h http.Handler, method, path, body string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	w := do(t, h, method, path, body, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
	}
	return w
}

// A DM's people are fixed at its hub wherever the hub is: a federated DM's
// replica holds its set (so its people's clients check it too), and after
// the hub moves, the new hub refuses anyone outside it.
func TestChatDMStaysFixedAcrossHouseholds(t *testing.T) {

	p := twoChatFogs(t)
	ctx := context.Background()
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	frankS := inviteAndJoin(t, p.bH, p.bCookie, "frank", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	frank := registerDevice(t, p.bH, frankS, "frank")
	p.heartbeat(t)
	bInstance, _ := p.b.chat.Instance()

	gid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "dm", "participants": []string{eve.addr}, "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	w := do(t, p.aH, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("dm: %d %s", w.Code, w.Body)
	}
	roomID := decode(t, w)["room"].(map[string]any)["id"].(string)
	commit, welcome, err := op.c.InviteMember(ctx, gid, remoteKeys(t, p.aH, op.cookie, eve.addr, mustDeviceKey(t, p.bH, eve)))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, p.aH, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add eve: %d %s", w.Code, w.Body)
	}
	p.a.chat.Fed.Flush(ctx)
	p.b.chat.Fed.WaitInvites()
	if _, err := eve.c.JoinGroup(ctx, welcome); err != nil {
		t.Fatal(err)
	}
	gi, _ := op.c.GroupInfo(ctx, gid) // this epoch's, for frank to try a self-join by
	if err := p.b.chat.Fed.SyncRoom(ctx, p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	rv := decode(t, do(t, p.bH, "GET", "/api/chat/rooms/"+roomID, "", withCookie(eve.cookie)))["room"].(map[string]any)
	if dm, _ := rv["dm"].([]any); len(dm) != 1 || dm[0] != op.addr {
		t.Fatalf("B's replica doesn't hold the DM's set: %v", rv["dm"])
	}
	// On the hub, eve adding frank is refused.
	add, wel, _ := eve.c.InviteMember(ctx, gid, freshKeys(t, p.bH, frank, eve.cookie))
	if w := submit(t, p.bH, eve, roomID, "mls_commit", add, wel); w.Code != http.StatusForbidden {
		t.Fatalf("adding outside the DM via its hub: %d %s", w.Code, w.Body)
	}
	// The hub moves to B; B as hub refuses the same.
	// In a DM only an owner holds canChangePolicy: op makes eve one too, so
	// she may agree to B hosting it.
	if w := patchRoom(t, p.aH, op, roomID, `{"role":{"addr":"`+eve.addr+`","role":4}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if err := p.b.chat.Fed.SyncRoom(ctx, p.aPeerOnB, roomID); err != nil {
		t.Fatal(err)
	}
	if w := patchRoom(t, p.bH, eve, roomID, `{"acceptHub":true}`); w.Code != http.StatusOK {
		t.Fatalf("eve agreeing to host: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.aH, "POST", "/api/chat/rooms/"+roomID+"/migrate", fmt.Sprintf(`{"to":%q}`, bInstance), withCookie(op.cookie)); w.Code != http.StatusOK {
		t.Fatalf("migrate: %d %s", w.Code, w.Body)
	}
	if r, _ := p.b.store.ChatRoom(roomID); r.HubInstance != "" || r.DMKey == "" || r.PublicState == "" {
		t.Fatalf("B as hub: hub %q, dm %q, verified state %v", r.HubInstance, r.DMKey, r.PublicState != "")
	}
	freshKeys(t, p.bH, frank, frank.cookie) // one for B's own fetch; the next for A's
	kp, _ := frank.c.FreshKeyPackageBytes(ctx)
	pub, _ := json.Marshal(map[string]any{"keyPackages": []string{people.EncodeKey(kp)}})
	do(t, p.bH, "POST", "/api/chat/me/devices/"+itoa(int(frank.id))+"/keypackages", string(pub), withCookie(frank.cookie))
	add2, wel2, _ := op.c.InviteMember(ctx, gid, remoteKeys(t, p.aH, op.cookie, frank.addr, mustDeviceKey(t, p.bH, frank)))
	if w := submit(t, p.aH, op, roomID, "mls_commit", add2, wel2); w.Code != http.StatusForbidden {
		t.Fatalf("adding outside the DM after the hub moved: %d %s", w.Code, w.Body)
	}
	// Nor may frank let himself in.
	if _, ext, err := frank.c.ExternalJoin(ctx, gi); err == nil {
		if w := submit(t, p.bH, frank, roomID, "mls_commit", ext, nil); w.Code != http.StatusForbidden {
			t.Errorf("self-joining a DM: %d %s", w.Code, w.Body)
		}
	}
}

// A ban holds against a person's other names: a hub adds people only under
// the address their household advertises, never their ULID alias, so a
// banned remote person can't be added back as "someone else".
func TestChatBanHoldsAgainstAliases(t *testing.T) {

	p := twoChatFogs(t)
	ctx := context.Background()
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	gid, roomID := crossRoom(t, p, op, eve, "Cousins")
	if w := patchRoom(t, p.aH, op, roomID, `{"role":{"addr":"`+eve.addr+`","role":1}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	// A device of eve's whose leaf names her by her person ULID.
	pe, _ := p.b.store.PersonByHandle("eve")
	bInst, _ := p.b.chat.Instance()
	alias := "fog:" + bInst + "/" + pe.UID
	c, _ := mls.NewClient([]byte(alias), ciphersuite.MLS128DHKEMX25519)
	kp, _ := c.FreshKeyPackageBytes(ctx)
	pkg, _ := keypackages.UnmarshalKeyPackage(kp)
	idPriv, _ := testIdentities.Load(eve.addr)
	att := ed25519.Sign(idPriv.(ed25519.PrivateKey), people.AttestationMessage(pkg.LeafNode.SignatureKeyBytes))
	body, _ := json.Marshal(map[string]any{"name": "alias", "deviceKey": people.EncodeKey(pkg.LeafNode.SignatureKeyBytes), "attestation": people.EncodeKey(att), "keyPackages": []string{people.EncodeKey(kp)}})
	if w := do(t, p.bH, "POST", "/api/chat/me/devices", string(body), withCookie(eveS)); w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	p.heartbeat(t)
	add, welcome, err := op.c.InviteMember(ctx, gid, kp)
	if err != nil {
		t.Fatal(err)
	}
	w := submit(t, p.aH, op, roomID, "mls_commit", add, welcome)
	if w.Code == http.StatusOK {
		t.Fatalf("a banned person added back under her ULID alias: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), "goes by") {
		t.Errorf("refused, but not for the alias: %d %s", w.Code, w.Body)
	}
}

// A large attachment from a follower crosses to the hub in pieces and
// lands whole; one the hub refuses leaves no copy behind on the follower.
func TestChatLargeAttachmentRelaysWithoutOrphans(t *testing.T) {

	p := twoChatFogs(t)
	ctx := context.Background()
	eveS := inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")
	op := registerDevice(t, p.aH, p.aCookie, "admin")
	eve := registerDevice(t, p.bH, eveS, "eve")
	p.heartbeat(t)
	_, roomID := crossRoom(t, p, op, eve, "Big")
	big := make([]byte, chat.BlobChunk+chat.BlobChunk/2)
	for i := range big {
		big[i] = byte(i * 7)
	}
	w := do(t, p.bH, "POST", "/api/chat/blobs?room="+roomID, string(big), withCookie(eve.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("large upload via B: %d %.300s", w.Code, w.Body)
	}
	sha := decode(t, w)["sha256"].(string)
	if b, err := p.a.store.ChatBlob(sha); err != nil || b.Size != int64(len(big)) {
		t.Fatalf("the hub's copy: %+v %v", b, err)
	}
	// The hub counts eve out before B hears of it: B's upload is refused,
	// and B keeps no copy of something nobody can reference.
	room, _ := p.a.store.ChatRoom(roomID)
	p.a.chat.MarkLeft(room, []string{eve.addr}, chat.SystemBanned, "test")
	other := append([]byte("different "), big[:1024]...)
	if w := do(t, p.bH, "POST", "/api/chat/blobs?room="+roomID, string(other), withCookie(eve.cookie)); w.Code == http.StatusOK {
		t.Fatalf("the hub took an upload from someone it counts out: %s", w.Body)
	}
	sum := sha256.Sum256(other)
	if _, err := p.b.store.ChatBlob(hex.EncodeToString(sum[:])); err == nil {
		t.Error("B kept an orphaned copy of a refused upload")
	}
	_ = ctx
}
