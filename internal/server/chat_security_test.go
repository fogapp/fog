package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	mls "github.com/thomas-vilte/mls-go"
	"github.com/thomas-vilte/mls-go/ciphersuite"
	"github.com/thomas-vilte/mls-go/framing"
	"github.com/thomas-vilte/mls-go/group"
	"github.com/thomas-vilte/mls-go/keypackages"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/chat/mlsview"
	"gitea.lan/matthew/fog/internal/people"
)

// Each test here performs an attack on a room and checks it fails.

// newGroupRoom creates a room of kind on the hub with op's device at leaf 0.
func newGroupRoom(t *testing.T, h http.Handler, op *testDevice, kind, label string) ([]byte, string) {
	t.Helper()
	gid, err := op.c.CreateGroup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"kind": kind, "label": label, "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("create room: %d %s", w.Code, w.Body)
	}
	return gid, decode(t, w)["room"].(map[string]any)["id"].(string)
}

// addToRoom commits an Add of d's device by by, and joins d from the Welcome.
// Every member in also processes the commit.
func addToRoom(t *testing.T, h http.Handler, by *testDevice, gid []byte, roomID string, d *testDevice, also ...*testDevice) {
	t.Helper()
	ctx := context.Background()
	commit, welcome, err := by.c.InviteMember(ctx, gid, freshKeys(t, h, d, by.cookie))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, by, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("add %s: %d %s", d.handle, w.Code, w.Body)
	}
	if _, err := d.c.JoinGroup(ctx, welcome); err != nil {
		t.Fatalf("%s joins: %v", d.handle, err)
	}
	for _, m := range also {
		if err := m.c.ProcessCommit(ctx, gid, commit); err != nil {
			t.Fatalf("%s processes the add: %v", m.handle, err)
		}
	}
}

// deviceKeys fetches one device's KeyPackage, as the SDK's sibling sync
// does: the person's key material narrowed to that device.
func deviceKeys(t *testing.T, h http.Handler, as *http.Cookie, d *testDevice) []byte {
	t.Helper()
	km := decode(t, do(t, h, "GET", "/api/chat/people/"+d.handle+"/keys?device="+itoa(int(d.id)), "", withCookie(as)))
	devs := km["devices"].([]any)
	if len(devs) != 1 {
		t.Fatalf("%s's device %d: %v", d.handle, d.id, km)
	}
	kpS, ok := devs[0].(map[string]any)["keyPackage"].(string)
	if !ok {
		t.Fatalf("%s's device %d has no key package left", d.handle, d.id)
	}
	kp, _ := people.DecodeKey(kpS)
	return kp
}

// freshKeys publishes another KeyPackage for d and fetches it as as.
func freshKeys(t *testing.T, h http.Handler, d *testDevice, as *http.Cookie) []byte {
	t.Helper()
	kp, err := d.c.FreshKeyPackageBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"keyPackages": []string{people.EncodeKey(kp)}})
	if w := do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(d.id))+"/keypackages", string(body), withCookie(d.cookie)); w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	return deviceKeys(t, h, as, d)
}

// craftCommit frames a commit by hand, as a member crafting MLS without a
// client's checks would: proposals inline, from senderLeaf. The hub parses,
// it doesn't verify signatures, so none are made.
func craftCommit(gid []byte, epoch uint64, senderLeaf uint32, props ...*group.Proposal) []byte {
	c := &group.Commit{}
	for _, p := range props {
		c.Proposals = append(c.Proposals, group.ProposalOrRef{Proposal: p})
	}
	pm := &framing.PublicMessage{
		Content: framing.FramedContent{
			GroupID: gid, Epoch: epoch,
			Sender: framing.Sender{Type: framing.SenderTypeMember, LeafIndex: senderLeaf},
			Body:   framing.CommitBody{Data: c.Marshal()},
		},
		Auth:          framing.FramedContentAuthData{ConfirmationTag: make([]byte, 32)},
		MembershipTag: make([]byte, 32),
	}
	return framing.NewMLSMessagePublic(pm).Marshal()
}

func roomEpoch(t *testing.T, h http.Handler, d *testDevice, roomID string) uint64 {
	t.Helper()
	return uint64(decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(d.cookie)))["room"].(map[string]any)["epoch"].(float64))
}

func leafKeys(t *testing.T, h http.Handler, d *testDevice, roomID string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, x := range decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(d.cookie)))["leaves"].([]any) {
		l := x.(map[string]any)
		out[l["identity"].(string)] = l["signatureKey"].(string)
	}
	return out
}

func leafIndexOf(t *testing.T, h http.Handler, d *testDevice, roomID, addr string) uint32 {
	t.Helper()
	for _, x := range decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(d.cookie)))["leaves"].([]any) {
		if l := x.(map[string]any); l["identity"] == addr {
			return uint32(l["index"].(float64))
		}
	}
	t.Fatalf("no leaf for %s", addr)
	return 0
}

func personID(t *testing.T, h http.Handler, admin *http.Cookie, handle string) string {
	t.Helper()
	for _, x := range decode(t, do(t, h, "GET", "/api/people", "", withCookie(admin)))["people"].([]any) {
		p := x.(map[string]any)
		if p["handle"] == handle {
			return itoa(int(p["id"].(float64)))
		}
	}
	t.Fatalf("no person %s", handle)
	return ""
}

func systemKinds(t *testing.T, h http.Handler, d *testDevice, roomID string) []string {
	t.Helper()
	var out []string
	for _, x := range decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events?limit=500", "", withCookie(d.cookie)))["events"].([]any) {
		e := x.(map[string]any)
		if e["kind"] != "system" {
			continue
		}
		raw, _ := people.DecodeKey(e["payload"].(string))
		var sys struct {
			Kind string `json:"kind"`
		}
		json.Unmarshal(raw, &sys)
		out = append(out, sys.Kind)
	}
	return out
}

func participantRow(t *testing.T, h http.Handler, d *testDevice, roomID, addr string) map[string]any {
	t.Helper()
	for _, x := range decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(d.cookie)))["participants"].([]any) {
		if p := x.(map[string]any); p["addr"] == addr {
			return p
		}
	}
	return nil
}

func patchRoom(t *testing.T, h http.Handler, d *testDevice, roomID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "PATCH", "/api/chat/rooms/"+roomID, body, withCookie(d.cookie))
}

// Finding 1: a member wraps a commit (or a proposal) in a private message,
// where the hub can't see it, to change who is in the room behind the hub's
// back. The hub refuses anything but application data on the private path.
func TestChatPrivateHandshakeRefused(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)

	epoch := roomEpoch(t, h, op, roomID)
	for _, ct := range []framing.ContentType{framing.ContentTypeCommit, framing.ContentTypeProposal} {
		pm := &framing.PrivateMessage{GroupID: gid, Epoch: epoch, ContentType: ct, EncryptedSenderData: make([]byte, 32), Ciphertext: make([]byte, 64)}
		w := submit(t, h, dave, roomID, "mls_private", framing.NewMLSMessagePrivate(pm).Marshal(), nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "application data") {
			t.Errorf("private message of content type %d: %d %s", ct, w.Code, w.Body)
		}
	}
	if got := roomEpoch(t, h, op, roomID); got != epoch {
		t.Errorf("epoch moved to %d", got)
	}
	// Application data still flows.
	ct, _ := dave.c.SendMessage(context.Background(), gid, mustContent(t, "hi"))
	if w := submit(t, h, dave, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatalf("application message: %d %s", w.Code, w.Body)
	}
}

// Finding 2: a member commits an inline Update carrying another member's
// identity and a key of their own, to swap the victim's leaf in the hub's
// view and lock them out. An inline Update is always the committer's own,
// which RFC 9420 forbids: the hub refuses it, and nothing moves.
func TestChatLeafTakeoverRefused(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)
	ctx := context.Background()
	before := leafKeys(t, h, op, roomID)
	epoch := roomEpoch(t, h, op, roomID)

	// A leaf node naming the operator, with a key dave holds.
	forger, _ := mls.NewClient([]byte(op.addr), ciphersuite.MLS128DHKEMX25519)
	raw, _ := forger.FreshKeyPackageBytes(ctx)
	kp, err := keypackages.UnmarshalKeyPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	daveLeaf, _ := dave.c.OwnLeafIndex(ctx, gid)
	update := &group.Proposal{Type: group.ProposalTypeUpdate, Update: &group.UpdateProposal{LeafNode: kp.LeafNode}}
	if w := submit(t, h, dave, roomID, "mls_commit", craftCommit(gid, epoch, daveLeaf, update), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("inline update of another's leaf: %d %s", w.Code, w.Body)
	}
	// Nor does a real client's own inline Update land (mls-go's SelfUpdate
	// makes one): every RFC 9420 member would refuse it.
	if upd, err := dave.c.SelfUpdate(ctx, gid); err == nil {
		if w := submit(t, h, dave, roomID, "mls_commit", upd, nil); w.Code != http.StatusBadRequest {
			t.Errorf("committer's own inline update: %d %s", w.Code, w.Body)
		}
	}
	// A commit removing its own committer, which members also refuse.
	rm := &group.Proposal{Type: group.ProposalTypeRemove, Remove: &group.RemoveProposal{Removed: group.LeafNodeIndex(daveLeaf)}}
	if w := submit(t, h, dave, roomID, "mls_commit", craftCommit(gid, epoch, daveLeaf, rm), nil); w.Code != http.StatusBadRequest {
		t.Errorf("committer removing itself: %d %s", w.Code, w.Body)
	}
	after := leafKeys(t, h, op, roomID)
	if after[op.addr] != before[op.addr] || after[dave.addr] != before[dave.addr] || roomEpoch(t, h, op, roomID) != epoch {
		t.Fatalf("the hub's view moved: before %v after %v", before, after)
	}
	// The operator still speaks from their leaf.
	ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, "still me"))
	if w := submit(t, h, op, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatalf("victim locked out: %d %s", w.Code, w.Body)
	}
}

// Finding 3: an operator resets a person's identity and signs in as them.
// The new identity is out of every room, with a warning line in each, can't
// let itself back in, can't ride a Welcome meant for the old devices, and
// is back only when a member adds it again. A hub that missed the reset (here:
// a reset made straight in the store) catches it at the self-join.
func TestChatIdentityResetIsADeparture(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	eve := registerDevice(t, h, eveS, "eve")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)
	// A Welcome still waiting for dave's old device, in another room.
	gid2, room2 := newGroupRoom(t, h, op, "group", "Pending")
	c2, w2, _ := op.c.InviteMember(ctx, gid2, freshKeys(t, h, dave, op.cookie))
	if w := submit(t, h, op, room2, "mls_commit", c2, w2); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}

	if w := do(t, h, "POST", "/api/people/"+personID(t, h, admin, "dave")+"/reset-identity", "", withCookie(admin)); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if p := participantRow(t, h, op, roomID, dave.addr); p == nil || p["leftSeq"] == nil {
		t.Fatalf("dave still in the room after the reset: %v", p)
	}
	if kinds := systemKinds(t, h, op, roomID); len(kinds) == 0 || kinds[len(kinds)-1] != chat.SystemIdentityReset {
		t.Fatalf("no identity-reset line: %v", kinds)
	}
	// Whoever holds dave's sign-in now makes a new identity and a device it
	// attests: verified, and still not let in by itself.
	ghost := registerDevice(t, h, daveS, "dave")
	if w := do(t, h, "GET", "/api/chat/rooms/"+roomID+"/groupinfo", "", withCookie(ghost.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("new identity fetching group info: %d %s", w.Code, w.Body)
	}
	gi, _ := op.c.GroupInfo(ctx, gid)
	_, ext, err := ghost.c.ExternalJoin(ctx, gi)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, ghost, roomID, "mls_commit", ext, nil); w.Code != http.StatusForbidden {
		t.Errorf("new identity self-joining: %d %s", w.Code, w.Body)
	}
	if invs := decode(t, do(t, h, "GET", "/api/chat/invites", "", withCookie(ghost.cookie)))["invites"].([]any); len(invs) != 0 {
		t.Errorf("Welcomes for the old devices survived the reset: %v", invs)
	}
	gi2, _ := op.c.GroupInfo(ctx, gid2)
	if _, ext2, err := ghost.c.ExternalJoin(ctx, gi2); err == nil {
		if w := submit(t, h, ghost, room2, "mls_commit", ext2, nil); w.Code != http.StatusForbidden {
			t.Errorf("new identity self-joining on the old devices' invite: %d %s", w.Code, w.Body)
		}
	}
	// Any member may now remove dave's old leaf: the hub counts him out.
	addToRoom(t, h, op, gid, roomID, eve)
	rmc, err := eve.c.RemoveMember(ctx, gid, []byte(dave.addr))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, eve, roomID, "mls_commit", rmc, nil); w.Code != http.StatusOK {
		t.Fatalf("a member removing a departed person's leaf: %d %s", w.Code, w.Body)
	}
	op.c.ProcessCommit(ctx, gid, rmc)
	// A member adding the new identity back is how it returns.
	addToRoom(t, h, op, gid, roomID, ghost, eve)
	if p := participantRow(t, h, op, roomID, dave.addr); p == nil || p["leftSeq"] != nil || p["roleIndex"].(float64) != chat.RoleMember {
		t.Fatalf("dave not back as a member: %v", p)
	}

	// The same reset made behind chat's back: the hub still holds the key
	// dave joined with, refuses the next one, and says so in the room.
	gid3, room3 := newGroupRoom(t, h, op, "group", "Later")
	addToRoom(t, h, op, gid3, room3, ghost)
	id := personID(t, h, admin, "dave")
	var pid int64
	json.Unmarshal([]byte(id), &pid)
	if err := srv.store.ResetPersonIdentity(pid); err != nil {
		t.Fatal(err)
	}
	ghost2 := registerDevice(t, h, daveS, "dave")
	if w := do(t, h, "GET", "/api/chat/rooms/"+room3+"/groupinfo", "", withCookie(ghost2.cookie)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "identity key changed") {
		t.Fatalf("self-join with a replaced identity: %d %s", w.Code, w.Body)
	}
	if kinds := systemKinds(t, h, op, room3); len(kinds) == 0 || kinds[len(kinds)-1] != chat.SystemIdentityReset {
		t.Fatalf("no identity-reset line when the hub caught it: %v", kinds)
	}
}

// Finding 5, hub side: a member's client adding a device its person's
// identity key never attested — the ghost device an operator holding the
// person's sign-in can register — is refused, however the client was built.
func TestChatAddRefusesUnattestedDevice(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	registerDevice(t, h, daveS, "dave") // dave's real, attested device
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	ghost := registerUnattested(t, h, daveS, "dave")
	ctx := context.Background()
	km := decode(t, do(t, h, "GET", "/api/chat/people/dave/keys?device="+itoa(int(ghost.id)), "", withCookie(op.cookie)))
	kpS := km["devices"].([]any)[0].(map[string]any)["keyPackage"].(string)
	kp, _ := people.DecodeKey(kpS)
	commit, welcome, err := op.c.InviteMember(ctx, gid, kp)
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not verified") {
		t.Fatalf("adding an unattested device: %d %s", w.Code, w.Body)
	}
}

// registerUnattested registers a device for the person with no attestation:
// what someone holding their sign-in but not their identity key can make.
func registerUnattested(t *testing.T, h http.Handler, session *http.Cookie, handle string) *testDevice {
	t.Helper()
	me := decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(session)))
	addr := "fog:" + me["household"].(map[string]any)["instanceId"].(string) + "/" + handle
	c, _ := mls.NewClient([]byte(addr), ciphersuite.MLS128DHKEMX25519)
	kp, _ := c.FreshKeyPackageBytes(context.Background())
	pkg, _ := keypackages.UnmarshalKeyPackage(kp)
	body, _ := json.Marshal(map[string]any{"name": "ghost", "deviceKey": people.EncodeKey(pkg.LeafNode.SignatureKeyBytes), "keyPackages": []string{people.EncodeKey(kp)}})
	w := do(t, h, "POST", "/api/chat/me/devices", string(body), withCookie(session))
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	d := decode(t, w)["device"].(map[string]any)
	if d["verified"] == true {
		t.Fatal("an unattested device registered as verified")
	}
	return &testDevice{handle: handle, addr: addr, c: c, cookie: personCookie(t, w), id: int64(d["id"].(float64))}
}

// Finding 7: roles move only below one's own rank. An admin can't demote
// the owner, touch another admin, or make someone an admin; nobody changes
// their own role; a role the policy lacks is refused. The owner can.
func TestChatRolesMoveOnlyDownward(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	carolS := inviteAndJoin(t, h, admin, "carol", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	eve := registerDevice(t, h, eveS, "eve")
	carol := registerDevice(t, h, carolS, "carol")
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)
	addToRoom(t, h, op, gid, roomID, eve, dave)
	addToRoom(t, h, op, gid, roomID, carol, dave, eve)
	role := func(addr string, r int) string { return `{"role":{"addr":"` + addr + `","role":` + itoa(r) + `}}` }
	for _, who := range []*testDevice{dave, eve} {
		if w := patchRoom(t, h, op, roomID, role(who.addr, chat.RoleAdmin)); w.Code != http.StatusOK {
			t.Fatalf("owner makes %s admin: %d %s", who.handle, w.Code, w.Body)
		}
	}
	for _, c := range []struct {
		why  string
		body string
		want int
	}{
		{"admin demotes the owner", role(op.addr, chat.RoleMember), http.StatusForbidden},
		{"admin bans the owner", role(op.addr, chat.RoleBanned), http.StatusForbidden},
		{"admin demotes another admin", role(eve.addr, chat.RoleMember), http.StatusForbidden},
		{"admin makes a member admin", role(carol.addr, chat.RoleAdmin), http.StatusForbidden},
		{"admin makes a member owner", role(carol.addr, chat.RoleOwner), http.StatusForbidden},
		{"admin makes themselves owner", role(dave.addr, chat.RoleOwner), http.StatusForbidden},
		{"a role the room doesn't have", role(carol.addr, 9), http.StatusBadRequest},
	} {
		if w := patchRoom(t, h, dave, roomID, c.body); w.Code != c.want {
			t.Errorf("%s: %d %s", c.why, w.Code, w.Body)
		}
	}
	if w := patchRoom(t, h, op, roomID, role(op.addr, chat.RoleMember)); w.Code != http.StatusForbidden {
		t.Errorf("owner changing their own role: %d %s", w.Code, w.Body)
	}
	if p := participantRow(t, h, op, roomID, op.addr); p["roleIndex"].(float64) != chat.RoleOwner {
		t.Fatalf("the owner lost their role: %v", p)
	}
	// Nor may an admin remove the owner, or another admin, by a commit.
	// (Crafted, as a client that applies its own commits at once would be
	// out of step after the first refusal.)
	daveLeaf, _ := dave.c.OwnLeafIndex(context.Background(), gid)
	for _, victim := range []*testDevice{op, eve} {
		rm := &group.Proposal{Type: group.ProposalTypeRemove, Remove: &group.RemoveProposal{Removed: group.LeafNodeIndex(leafIndexOf(t, h, op, roomID, victim.addr))}}
		rmc := craftCommit(gid, roomEpoch(t, h, op, roomID), daveLeaf, rm)
		if w := submit(t, h, dave, roomID, "mls_commit", rmc, nil); w.Code != http.StatusForbidden {
			t.Errorf("admin removing %s: %d %s", victim.handle, w.Code, w.Body)
		}
	}
	// Below their own rank an admin may act.
	if w := patchRoom(t, h, dave, roomID, role(carol.addr, chat.RoleMember)); w.Code != http.StatusOK {
		t.Errorf("admin setting a member's role: %d %s", w.Code, w.Body)
	}
	if w := patchRoom(t, h, op, roomID, role(eve.addr, chat.RoleMember)); w.Code != http.StatusOK {
		t.Errorf("owner demoting an admin: %d %s", w.Code, w.Body)
	}
}

// Finding 8: a ban takes the person out of the room and keeps them out: no
// member can add them back, they can't let themselves back in, and any
// member may remove their leaves. Lifting the ban is a role change.
func TestChatBanSticks(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	eve := registerDevice(t, h, eveS, "eve")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "channel", "general")
	addToRoom(t, h, op, gid, roomID, dave)
	addToRoom(t, h, op, gid, roomID, eve, dave)

	// A webhook dave made while he could goes with him.
	if _, err := srv.store.CreateChatWebhook(roomID, "dave's", "digest-of-daves-hook", dave.addr); err != nil {
		t.Fatal(err)
	}
	if w := patchRoom(t, h, op, roomID, `{"role":{"addr":"`+dave.addr+`","role":1}}`); w.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", w.Code, w.Body)
	}
	if hooks, _ := srv.store.ChatWebhooks(roomID); len(hooks) != 0 {
		t.Errorf("a banned person's webhook survived: %+v", hooks)
	}
	if p := participantRow(t, h, op, roomID, dave.addr); p["leftSeq"] == nil {
		t.Fatalf("a banned person is still in the room: %v", p)
	}
	if kinds := systemKinds(t, h, op, roomID); kinds[len(kinds)-1] != chat.SystemBanned {
		t.Fatalf("no ban line: %v", kinds)
	}
	ct, _ := dave.c.SendMessage(ctx, gid, mustContent(t, "still here"))
	if w := submit(t, h, dave, roomID, "mls_private", ct, nil); w.Code != http.StatusForbidden {
		t.Errorf("banned person posting: %d %s", w.Code, w.Body)
	}
	// Eve, a plain member, removes his leaf; then tries to add him back.
	rmc, _ := eve.c.RemoveMember(ctx, gid, []byte(dave.addr))
	if w := submit(t, h, eve, roomID, "mls_commit", rmc, nil); w.Code != http.StatusOK {
		t.Fatalf("member removing a banned person's leaf: %d %s", w.Code, w.Body)
	}
	op.c.ProcessCommit(ctx, gid, rmc)
	daveNew := registerDevice(t, h, daveS, "dave")
	// (eve's own client now holds a commit the hub refused; she is out of
	// step from here, as a client whose commit bounced is until it resyncs.)
	add, welcome, _ := eve.c.InviteMember(ctx, gid, deviceKeys(t, h, eve.cookie, daveNew))
	if w := submit(t, h, eve, roomID, "mls_commit", add, welcome); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "banned") {
		t.Fatalf("member re-adding a banned person: %d %s", w.Code, w.Body)
	}
	// Nor in by himself, though it's an open channel.
	gi, _ := op.c.GroupInfo(ctx, gid)
	if _, ext, err := daveNew.c.ExternalJoin(ctx, gi); err == nil {
		if w := submit(t, h, daveNew, roomID, "mls_commit", ext, nil); w.Code != http.StatusForbidden {
			t.Errorf("banned person self-joining: %d %s", w.Code, w.Body)
		}
	}
	// The owner lifts the ban; then he may be added again.
	if w := patchRoom(t, h, op, roomID, `{"role":{"addr":"`+dave.addr+`","role":2}}`); w.Code != http.StatusOK {
		t.Fatalf("unban: %d %s", w.Code, w.Body)
	}
	add2, welcome2, err := op.c.InviteMember(ctx, gid, freshKeys(t, h, daveNew, op.cookie))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", add2, welcome2); w.Code != http.StatusOK {
		t.Fatalf("re-adding after the ban was lifted: %d %s", w.Code, w.Body)
	}
}

// Finding 10: whoever leaves a room — removed, banned, gone by choice —
// sees nothing sequenced after: not in the log, not on a stream opened
// before, not among the attachments, not in the room's details. What they
// were sent before they left stays theirs.
func TestChatFormerMemberAccessEnds(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)
	putBlob := func(body string) string {
		r := httptest.NewRequest("POST", "/api/chat/blobs?room="+roomID, strings.NewReader(body))
		r.AddCookie(op.cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("put blob: %d %s", w.Code, w.Body)
		}
		return decode(t, w)["sha256"].(string)
	}
	before := putBlob("before-bytes")

	// Dave's stream is open when he leaves.
	ts := httptest.NewServer(sameTestClient(h)) // one client in-process and over the socket
	defer ts.Close()
	hdr := http.Header{}
	hdr.Add("Cookie", dave.cookie.Name+"="+dave.cookie.Value)
	wsctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/chat/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.Write(wsctx, websocket.MessageText, []byte(`{"since":{}}`))
	for {
		_, data, err := conn.Read(wsctx)
		if err != nil {
			t.Fatal(err)
		}
		var f streamFrame
		json.Unmarshal(data, &f)
		if f.Kind == "synced" {
			break
		}
	}

	if w := patchRoom(t, h, dave, roomID, `{"leave":true}`); w.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", w.Code, w.Body)
	}
	left := int64(participantRow(t, h, op, roomID, dave.addr)["leftSeq"].(float64))
	ct, _ := op.c.SendMessage(ctx, gid, mustContent(t, "after you left"))
	if w := submit(t, h, op, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	after := putBlob("after-bytes")

	// The stream carried his departure and nothing after it.
	rctx, rcancel := context.WithTimeout(ctx, 700*time.Millisecond)
	for {
		_, data, err := conn.Read(rctx)
		if err != nil {
			break
		}
		var f streamFrame
		json.Unmarshal(data, &f)
		if f.Kind == "event" && f.Event != nil && f.Event.Seq > left {
			t.Errorf("stream sent seq %d after dave left at %d", f.Event.Seq, left)
		}
	}
	rcancel()
	evs := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID+"/events", "", withCookie(dave.cookie)))["events"].([]any)
	for _, x := range evs {
		if s := int64(x.(map[string]any)["seq"].(float64)); s > left {
			t.Errorf("log served seq %d after dave left at %d", s, left)
		}
	}
	if len(evs) == 0 {
		t.Error("dave lost the history he was sent")
	}
	if w := do(t, h, "GET", "/api/chat/blobs/"+before, "", withCookie(dave.cookie)); w.Code != http.StatusOK {
		t.Errorf("an attachment from before he left: %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/chat/blobs/"+after, "", withCookie(dave.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("an attachment from after he left: %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(dave.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("room details after leaving: %d", w.Code)
	}
	// The Welcome that once added him, never marked accepted, is gone: a
	// pending invite would let his device back in by itself.
	if invs := decode(t, do(t, h, "GET", "/api/chat/invites", "", withCookie(dave.cookie)))["invites"].([]any); len(invs) != 0 {
		t.Errorf("an invite outlived his departure: %v", invs)
	}
	if w := do(t, h, "GET", "/api/chat/rooms/"+roomID+"/groupinfo", "", withCookie(dave.cookie)); w.Code != http.StatusForbidden {
		t.Errorf("his device may still join by itself after leaving: %d %s", w.Code, w.Body)
	}
}

// The other moderation-adjacent hub writes: archive, labels and policy.
// A member without the capability changes nothing, and a refused patch
// writes no part of itself.
func TestChatRoomPatchIsAllOrNothing(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	gid, roomID := newGroupRoom(t, h, op, "channel", "general")
	addToRoom(t, h, op, gid, roomID, dave)
	// A member may not change the name; the topic with it must not land.
	if w := patchRoom(t, h, dave, roomID, `{"topic":"hijacked","label":"mine"}`); w.Code != http.StatusForbidden {
		t.Fatalf("member renaming: %d %s", w.Code, w.Body)
	}
	if w := patchRoom(t, h, op, roomID, `{"topic":"ours"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	// An owner's label lands, but not when the same patch asks for what
	// they can't: an admin hiding the channel without canChangePolicy.
	if w := patchRoom(t, h, dave, roomID, `{"archive":true}`); w.Code != http.StatusForbidden {
		t.Errorf("member archiving: %d %s", w.Code, w.Body)
	}
	detail := decode(t, do(t, h, "GET", "/api/chat/rooms/"+roomID, "", withCookie(op.cookie)))["room"].(map[string]any)
	if detail["topicLabel"] != "ours" || detail["label"] != "general" || detail["archivedAt"] != nil {
		t.Fatalf("room after refused patches: %v", detail)
	}
	// An admin rewriting the roles by a policy commit (to make admins
	// owners) is refused; only an owner may change roles that way.
	if w := patchRoom(t, h, op, roomID, `{"role":{"addr":"`+dave.addr+`","role":3}}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	pol := chat.DefaultPolicy(chat.KindChannel)
	pol.Roles[2].Capabilities = []string{chat.CapAll}
	ext := group.Extension{Type: chat.PolicyExtensionType, Data: []byte(pol.JSON())}
	gce := &group.Proposal{Type: group.ProposalTypeGroupContextExtensions, GroupContextExtensions: &group.ContextExtensionsProposal{Extensions: []group.Extension{ext}}}
	daveLeaf, _ := dave.c.OwnLeafIndex(context.Background(), gid)
	if w := submit(t, h, dave, roomID, "mls_commit", craftCommit(gid, roomEpoch(t, h, op, roomID), daveLeaf, gce), nil); w.Code != http.StatusForbidden {
		t.Fatalf("admin rewriting the roles: %d %s", w.Code, w.Body)
	}
}

// Revoking a device ends what it still had: its open stream closes, and the
// room says which leaf to drop, which any member may then remove.
func TestChatRevokedDeviceIsCutOff(t *testing.T) {
	old := streamRecheck
	streamRecheck = 50 * time.Millisecond
	defer func() { streamRecheck = old }()
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	eve := registerDevice(t, h, eveS, "eve")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)
	addToRoom(t, h, op, gid, roomID, eve, dave)

	ts := httptest.NewServer(sameTestClient(h)) // one client in-process and over the socket
	defer ts.Close()
	hdr := http.Header{}
	hdr.Add("Cookie", dave.cookie.Name+"="+dave.cookie.Value)
	wsctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/chat/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.Write(wsctx, websocket.MessageText, []byte(`{"since":{}}`))

	// The phone is stolen; dave revokes it from another session.
	if w := do(t, h, "DELETE", "/api/chat/me/devices/"+itoa(int(dave.id)), "", withCookie(daveS)); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	closed := false
	for !closed {
		if _, _, err := conn.Read(wsctx); err != nil {
			closed = wsctx.Err() == nil
			break
		}
	}
	if !closed {
		t.Error("the revoked device's stream stayed open")
	}
	if kinds := systemKinds(t, h, op, roomID); len(kinds) == 0 || kinds[len(kinds)-1] != chat.SystemDeviceRevoked {
		t.Fatalf("no device-revoked line: %v", kinds)
	}
	// Eve, a plain member, removes the revoked leaf; dave stays a participant.
	rmc, err := eve.c.RemoveMember(ctx, gid, []byte(dave.addr))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, eve, roomID, "mls_commit", rmc, nil); w.Code != http.StatusOK {
		t.Fatalf("member removing a revoked device's leaf: %d %s", w.Code, w.Body)
	}
}

// Draining a person's one-use KeyPackages is bounded: one asker consumes at
// most chat.KeysPerAskerPerson an hour, and past that — or once the device
// has none left — gets the device's last-resort package, never used up, so
// adds still work.
func TestChatKeyPackagesCantBeDrained(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	malS := inviteAndJoin(t, h, admin, "mallory", "correct horse battery")
	dave := registerDevice(t, h, daveS, "dave")
	var pkgs []string
	for range chat.KeysPerAskerPerson + 5 {
		kp, _ := dave.c.FreshKeyPackageBytes(context.Background())
		pkgs = append(pkgs, people.EncodeKey(kp))
	}
	lr, _ := dave.c.FreshKeyPackageBytes(context.Background())
	body, _ := json.Marshal(map[string]any{"keyPackages": pkgs, "lastResort": people.EncodeKey(lr)})
	if w := do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(dave.id))+"/keypackages", string(body), withCookie(dave.cookie)); w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	fetch := func(as *http.Cookie) (string, bool) {
		dev := decode(t, do(t, h, "GET", "/api/chat/people/dave/keys", "", withCookie(as)))["devices"].([]any)[0].(map[string]any)
		kp, _ := dev["keyPackage"].(string)
		last, _ := dev["lastResort"].(bool)
		return kp, last
	}
	for i := range chat.KeysPerAskerPerson {
		if kp, last := fetch(malS); kp == "" || last {
			t.Fatalf("fetch %d within budget: package %v last-resort %v", i, kp != "", last)
		}
	}
	for range 5 {
		if kp, last := fetch(malS); kp != people.EncodeKey(lr) || !last {
			t.Fatalf("past the budget: got last-resort %v", last)
		}
	}
	left := 0
	for _, x := range decode(t, do(t, h, "GET", "/api/chat/me", "", withCookie(dave.cookie)))["devices"].([]any) {
		if d := x.(map[string]any); int64(d["id"].(float64)) == dave.id {
			left = int(d["keyPackages"].(float64))
		}
	}
	if want := 1 + 5; left != want { // registerDevice's one, plus the five past mallory's budget
		t.Errorf("dave has %d one-use packages left, want %d", left, want)
	}
	// Someone else is on their own budget: a real add still gets a fresh one.
	if kp, last := fetch(admin); kp == "" || last {
		t.Errorf("another asker after the drain: last-resort %v", last)
	}
}

// The hub verifies each commit against the group's public state before it
// orders it: a member's commit whose signature doesn't verify is refused,
// so is an honest commit carrying a GroupInfo for the wrong epoch, and so
// is a GroupInfo published that isn't this epoch's. The room carries on.
// (Commits a member signs over a bad tree are refused in mlsview's
// TestPublicGroupRefusesSignedForgeries, by the same check.)
func TestChatHubVerifiesCommits(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	eveS := inviteAndJoin(t, h, admin, "eve", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	eve := registerDevice(t, h, eveS, "eve")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)
	epoch := roomEpoch(t, h, op, roomID)
	staleGI := groupInfoOf(t, op, gid)

	// Dave's key refresh with its signature broken.
	refresh, _, err := dave.c.CommitPendingProposals(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	mm, _ := framing.UnmarshalMLSMessage(refresh)
	pm, _ := mm.AsPublic()
	sig := append([]byte(nil), pm.Auth.Signature.AsSlice()...)
	sig[0] ^= 1
	pm.Auth.Signature = ciphersuite.NewSignature(sig)
	if w := submit(t, h, dave, roomID, "mls_commit", framing.NewMLSMessagePublic(pm).Marshal(), nil); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "signature") {
		t.Fatalf("a commit with a broken signature: %d %s", w.Code, w.Body)
	}
	// A GroupInfo published for this epoch that isn't this group's state.
	other, _ := mls.NewClient([]byte(op.addr), ciphersuite.MLS128DHKEMX25519)
	ogid, _ := other.CreateGroup(ctx)
	ogi, _ := other.GroupInfo(ctx, ogid)
	body, _ := json.Marshal(map[string]any{"epoch": epoch, "groupInfo": people.EncodeKey(mlsview.WrapGroupInfo(ogi))})
	if w := do(t, h, "PUT", "/api/chat/rooms/"+roomID+"/groupinfo", string(body), withCookie(op.cookie)); w.Code != http.StatusBadRequest {
		t.Errorf("another group's group info: %d %s", w.Code, w.Body)
	}
	// The operator's honest add of eve, sent with the old epoch's GroupInfo.
	add, welcome, err := op.c.InviteMember(ctx, gid, freshKeys(t, h, eve, op.cookie))
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"id": ulid(t), "kind": "mls_commit", "payload": people.EncodeKey(add), "welcome": people.EncodeKey(welcome), "groupInfo": staleGI}
	b, _ := json.Marshal(in)
	if w := do(t, h, "POST", "/api/chat/rooms/"+roomID+"/events", string(b), withCookie(op.cookie)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "groupInfo") {
		t.Fatalf("a commit with a wrong-epoch group info: %d %s", w.Code, w.Body)
	}
	if got := roomEpoch(t, h, op, roomID); got != epoch {
		t.Fatalf("a refused commit moved the room to epoch %d", got)
	}
	// The same add with the right GroupInfo lands, and everyone carries on.
	in["id"], in["groupInfo"] = ulid(t), groupInfoOf(t, op, gid)
	b, _ = json.Marshal(in)
	if w := do(t, h, "POST", "/api/chat/rooms/"+roomID+"/events", string(b), withCookie(op.cookie)); w.Code != http.StatusOK {
		t.Fatalf("the honest add: %d %s", w.Code, w.Body)
	}
	if _, err := eve.c.JoinGroup(ctx, welcome); err != nil {
		t.Fatal(err)
	}
	if err := dave.c.ProcessCommit(ctx, gid, add); err != nil {
		// dave's client applied its own refresh locally; a real client
		// whose commit bounced resyncs. Here, dave just stops.
		t.Logf("dave is out of step after his refused commit: %v", err)
	}
	ct, _ := eve.c.SendMessage(ctx, gid, mustContent(t, "hello"))
	if w := submit(t, h, eve, roomID, "mls_private", ct, nil); w.Code != http.StatusOK {
		t.Fatalf("an honest member after the forgeries: %d %s", w.Code, w.Body)
	}
	if _, err := op.c.ReceiveMessage(ctx, gid, ct); err != nil {
		t.Fatalf("the operator reading eve: %v", err)
	}
}

// A room from before the hub kept verified state seeds it from a GroupInfo
// for its epoch whose tree holds exactly the members the hub knows; until
// then it takes no commit, and a GroupInfo that doesn't match seeds nothing.
func TestChatLegacyRoomSeedsVerifiedState(t *testing.T) {
	srv, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "group", "Old")
	if _, err := srv.store.DB().Exec(`UPDATE chat_rooms SET public_state = '', group_info = NULL, group_info_epoch = -1 WHERE id = ?`, roomID); err != nil {
		t.Fatal(err)
	}
	gi0 := groupInfoOf(t, op, gid)
	commit, welcome, err := op.c.InviteMember(ctx, gid, freshKeys(t, h, dave, op.cookie))
	if err != nil {
		t.Fatal(err)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "need_group_info") {
		t.Fatalf("a commit before the room has verified state: %d %s", w.Code, w.Body)
	}
	other, _ := mls.NewClient([]byte(op.addr), ciphersuite.MLS128DHKEMX25519)
	ogid, _ := other.CreateGroup(ctx)
	ogi, _ := other.GroupInfo(ctx, ogid)
	body, _ := json.Marshal(map[string]any{"epoch": 0, "groupInfo": people.EncodeKey(mlsview.WrapGroupInfo(ogi))})
	if w := do(t, h, "PUT", "/api/chat/rooms/"+roomID+"/groupinfo", string(body), withCookie(op.cookie)); w.Code != http.StatusBadRequest {
		t.Errorf("seeding from another group's group info: %d %s", w.Code, w.Body)
	}
	if r, _ := srv.store.ChatRoom(roomID); r.PublicState != "" {
		t.Fatal("a mismatched group info seeded state")
	}
	body, _ = json.Marshal(map[string]any{"epoch": 0, "groupInfo": gi0})
	if w := do(t, h, "PUT", "/api/chat/rooms/"+roomID+"/groupinfo", string(body), withCookie(op.cookie)); w.Code != http.StatusOK {
		t.Fatalf("seeding from the room's own group info: %d %s", w.Code, w.Body)
	}
	if w := submit(t, h, op, roomID, "mls_commit", commit, welcome); w.Code != http.StatusOK {
		t.Fatalf("the commit once the room is seeded: %d %s", w.Code, w.Body)
	}
}

// Each message carries the role its sender held when the hub sequenced it,
// which clients judge moderation acts by.
func TestChatEventsRecordTheSendersRole(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()
	gid, roomID := newGroupRoom(t, h, op, "group", "Us")
	addToRoom(t, h, op, gid, roomID, dave)
	post := func() float64 {
		ct, _ := dave.c.SendMessage(ctx, gid, mustContent(t, "hi"))
		w := submit(t, h, dave, roomID, "mls_private", ct, nil)
		if w.Code != http.StatusOK {
			t.Fatal(w.Body)
		}
		r, _ := decode(t, w)["event"].(map[string]any)["senderRole"].(float64)
		return r
	}
	if r := post(); r != chat.RoleMember {
		t.Errorf("as a member: %v", r)
	}
	patchRoom(t, h, op, roomID, `{"role":{"addr":"`+dave.addr+`","role":3}}`)
	if r := post(); r != chat.RoleAdmin {
		t.Errorf("as an admin: %v", r)
	}
}

// Someone who left a DM gets nothing of it by asking to create it again.
func TestChatDepartedDMMemberSeesNothing(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	gid, _ := op.c.CreateGroup(context.Background())
	body, _ := json.Marshal(map[string]any{"kind": "dm", "participants": []string{dave.addr}, "groupId": people.EncodeKey(gid), "groupInfo": groupInfoOf(t, op, gid)})
	w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	roomID := decode(t, w)["room"].(map[string]any)["id"].(string)
	addToRoom(t, h, op, gid, roomID, dave)
	if w := patchRoom(t, h, dave, roomID, `{"leave":true}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	dgid, _ := dave.c.CreateGroup(context.Background())
	again, _ := json.Marshal(map[string]any{"kind": "dm", "participants": []string{op.addr}, "groupId": people.EncodeKey(dgid), "groupInfo": groupInfoOf(t, dave, dgid)})
	if w := do(t, h, "POST", "/api/chat/rooms", string(again), withCookie(dave.cookie)); w.Code != http.StatusForbidden {
		t.Fatalf("a departed member re-creating the DM: %d %s", w.Code, w.Body)
	}
}

// A push endpoint is one fog will dial: never on its own network.
func TestChatPushEndpointMustBePublic(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	op := registerDevice(t, h, admin, "admin")
	for _, ep := range []string{"https://127.0.0.1/push", "https://10.0.0.5/push", "https://nas.local/push", "https://[::1]/p"} {
		body := `{"endpoint":"` + ep + `","keys":{"p256dh":"BP","auth":"AA"}}`
		if w := do(t, h, "POST", "/api/chat/me/devices/"+itoa(int(op.id))+"/push", body, withCookie(op.cookie)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", ep, w.Code, w.Body)
		}
	}
}

// An identity reset is a departure from every kind of room, and the new
// identity can't let itself back into any of them — not even an open
// channel anyone in the household may join. Only a member's Add does.
func TestChatResetIdentityCantRejoinAnyRoom(t *testing.T) {
	_, h := newTestServer(t)
	admin := setup(t, h)
	daveS := inviteAndJoin(t, h, admin, "dave", "correct horse battery")
	op := registerDevice(t, h, admin, "admin")
	dave := registerDevice(t, h, daveS, "dave")
	ctx := context.Background()
	type room struct {
		kind    string
		gid     []byte
		id      string
		creator *testDevice
	}
	var rooms []room
	for _, kind := range []string{"channel", "group"} {
		gid, id := newGroupRoom(t, h, op, kind, kind+" room")
		addToRoom(t, h, op, gid, id, dave)
		rooms = append(rooms, room{kind, gid, id, op})
	}
	dgid, _ := op.c.CreateGroup(ctx)
	body, _ := json.Marshal(map[string]any{"kind": "dm", "participants": []string{dave.addr}, "groupId": people.EncodeKey(dgid), "groupInfo": groupInfoOf(t, op, dgid)})
	w := do(t, h, "POST", "/api/chat/rooms", string(body), withCookie(op.cookie))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	did := decode(t, w)["room"].(map[string]any)["id"].(string)
	addToRoom(t, h, op, dgid, did, dave)
	rooms = append(rooms, room{"dm", dgid, did, op})

	if w := do(t, h, "POST", "/api/people/"+personID(t, h, admin, "dave")+"/reset-identity", "", withCookie(admin)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	ghost := registerDevice(t, h, daveS, "dave") // the new identity, self-attested
	for _, r := range rooms {
		gi, err := op.c.GroupInfo(ctx, r.gid)
		if err != nil {
			t.Fatal(err)
		}
		if w := do(t, h, "GET", "/api/chat/rooms/"+r.id+"/groupinfo", "", withCookie(ghost.cookie)); w.Code != http.StatusForbidden {
			t.Errorf("%s: the new identity fetching group info: %d %s", r.kind, w.Code, w.Body)
		}
		_, ext, err := ghost.c.ExternalJoin(ctx, gi)
		if err != nil {
			t.Fatal(err)
		}
		if w := submit(t, h, ghost, r.id, "mls_commit", ext, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s: the new identity let itself back in: %d %s", r.kind, w.Code, w.Body)
		}
	}
	// Nor a brand-new open channel it was never in: members there may have
	// pinned the old key, and only a member's Add vouches for the new one.
	gid, id := newGroupRoom(t, h, op, "channel", "fresh")
	gi, _ := op.c.GroupInfo(ctx, gid)
	_, ext, _ := ghost.c.ExternalJoin(ctx, gi)
	if w := submit(t, h, ghost, id, "mls_commit", ext, nil); w.Code != http.StatusForbidden {
		t.Errorf("an open channel it was never in: %d %s", w.Code, w.Body)
	}
}
