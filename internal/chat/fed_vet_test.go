package chat

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// A hub can't place a room into our people's lists that none of them was
// invited to — not by pushing its events, not by listing one of our people
// in a snapshot — and a participant's household is the one their address
// names, whatever the snapshot claims.
func TestHubCantPlaceRoomsUninvited(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	fd := testFederation(st, nil)
	ctx := context.Background()

	// An event for a room nobody here was invited to.
	fd.Receive(ctx, hub, []item{{Type: "event", RoomID: "uninvited", Hub: hub.InstanceID, Event: &store.ChatEvent{RoomID: "uninvited", Seq: 1, ID: "x", Kind: EventSystem}}})
	if _, err := st.ChatRoom("uninvited"); err == nil {
		t.Fatal("an uninvited room was mirrored")
	}

	// A replica eve was invited to; the hub's snapshot also lists frank.
	replicaOf(t, st, hub, "room", 3)
	if err := st.AddChatInvite(&store.ChatInvite{RoomID: "room", PersonAddr: "fog:us/eve", InvitedBy: "fog:hub/op", Seq: 2, Welcome: []byte("w"), CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	room, _ := st.ChatRoom("room")
	snap := &RoomSnapshot{Room: *room, Policy: room.Policy, Participants: []store.ChatParticipant{
		{PersonAddr: "fog:hub/op", HomeInstance: "hub", RoleIndex: RoleOwner, JoinedSeq: 1},
		{PersonAddr: "fog:us/eve", HomeInstance: "us", RoleIndex: RoleMember, JoinedSeq: 2},
		{PersonAddr: "fog:us/frank", HomeInstance: "us", RoleIndex: RoleMember, JoinedSeq: 2},
		// A third household's person passed off as the hub's.
		{PersonAddr: "fog:elsewhere/zed", HomeInstance: "hub", RoleIndex: RoleMember, JoinedSeq: 2},
	}}
	if err := fd.Svc.refreshReplicaMeta(room, snap); err != nil {
		t.Fatal(err)
	}
	if p, err := st.ChatParticipant("room", "fog:us/frank"); err == nil && p.LeftSeq == 0 {
		t.Error("an uninvited person of ours was placed in the room")
	}
	if p, err := st.ChatParticipant("room", "fog:us/eve"); err != nil || p.LeftSeq != 0 {
		t.Errorf("the invited person is missing: %+v %v", p, err)
	}
	if p, _ := st.ChatParticipant("room", "fog:elsewhere/zed"); p == nil || p.HomeInstance != "elsewhere" {
		t.Errorf("a participant's household came from the snapshot's word: %+v", p)
	}
	if err := st.SetFedCapability(hub.ID, CapChat, store.DirGrant, `{"acceptThirdHouseholds":false}`); err != nil {
		t.Fatal(err)
	}
	if code, _ := fd.thirdHouseholdCheck(hub, snap); code != "third_household" {
		t.Error("a third household passed off as the hub's got through")
	}
}

// A household is the one its ID's key says. A row paired by a fog older
// than key-bound IDs, claiming the key-derived ID of a household under
// another key, doesn't make that household's people "paired": a room
// holding them is still a room with a third household, and their
// addresses name nobody here.
func TestThirdHouseholdIDIsCheckedAgainstItsKey(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	fd := testFederation(st, nil)
	// The derived ID of one key, held under another.
	_, pubA := keyPair(t)
	_, pubB := keyPair(t)
	third := fed.DeriveInstanceID(pubA)
	forged, err := st.CreatePairedPeer(third, "forged", "http://forged.invalid", base64.StdEncoding.EncodeToString(pubB))
	if err != nil {
		t.Fatal(err)
	}
	st.SetFedCapability(forged.ID, CapChat, store.DirGrant, `{}`)
	if err := st.SetFedCapability(hub.ID, CapChat, store.DirGrant, `{"acceptThirdHouseholds":false}`); err != nil {
		t.Fatal(err)
	}
	snap := &RoomSnapshot{Participants: []store.ChatParticipant{
		{PersonAddr: "fog:hub/op", HomeInstance: "hub", RoleIndex: RoleOwner, JoinedSeq: 1},
		{PersonAddr: "fog:" + third + "/zed", HomeInstance: third, RoleIndex: RoleMember, JoinedSeq: 2},
	}}
	if code, _ := fd.thirdHouseholdCheck(hub, snap); code != "third_household" {
		t.Fatal("a forged row made a third household count as paired")
	}
	if _, _, err := fd.RemoteDevice("fog:"+third+"/zed", []byte("k")); err == nil {
		t.Fatal("a forged row answered for its household's people")
	}
	// Held under its own key, the household is paired.
	if err := st.DeleteFedPeer(forged.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePairedPeer(third, "real", "http://real.invalid", base64.StdEncoding.EncodeToString(pubA)); err != nil {
		t.Fatal(err)
	}
	if code, _ := fd.thirdHouseholdCheck(hub, snap); code != "" {
		t.Fatalf("a genuine paired household: %q", code)
	}
}

func keyPair(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}
