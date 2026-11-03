package chat

import (
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// What an old hub hands over is held to what this fog can see: a DM set
// that doesn't cover the people in the room, a creator who was never in
// it, a DM set on a room that isn't a DM, or a role the policy lacks —
// each sinks the adoption.
func TestAdoptionVetsWhatItIsHanded(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	fd := testFederation(st, nil)
	mk := func(id, kind, dm, createdBy string, parts ...store.ChatParticipant) *store.ChatRoom {
		if err := st.CreateChatRoom(&store.ChatRoom{ID: id, GroupID: []byte("g-" + id), HubInstance: hub.InstanceID, Kind: kind, Policy: DefaultPolicy(kind).JSON(), ReplicaStatus: "live", CreatedBy: createdBy, DMKey: dm}); err != nil {
			t.Fatal(err)
		}
		st.AppendChatEvent(&store.ChatCommit{Event: &store.ChatEvent{RoomID: id, ID: "c-" + id, Kind: EventSystem, Payload: []byte("{}")}, Added: parts})
		r, _ := st.ChatRoom(id)
		return r
	}
	op := store.ChatParticipant{PersonAddr: "fog:hub/op", HomeInstance: "hub", RoleIndex: RoleOwner}
	eve := store.ChatParticipant{PersonAddr: "fog:us/eve", HomeInstance: "us", RoleIndex: RoleMember}
	mallory := store.ChatParticipant{PersonAddr: "fog:hub/mallory", HomeInstance: "hub", RoleIndex: RoleMember}
	odd := store.ChatParticipant{PersonAddr: "fog:hub/odd", HomeInstance: "hub", RoleIndex: 9}
	for name, room := range map[string]*store.ChatRoom{
		"a DM set that leaves someone in it out": mk("dm1", KindDM, "fog:hub/op,fog:us/eve", "fog:hub/op", op, eve, mallory),
		"a creator who was never in it":          mk("g1", KindGroup, "", "fog:us/someone-local", op, eve),
		"a DM set on a group":                    mk("g2", KindGroup, "fog:hub/op,fog:us/eve", "fog:hub/op", op, eve),
		"a role the policy lacks":                mk("g3", KindGroup, "", "fog:hub/op", op, odd),
	} {
		if err := fd.Svc.vetAdoption(room); err == nil {
			t.Errorf("%s: adopted", name)
		}
	}
	// Without a member's agreement, no adoption at all.
	if fd.hasAcceptance("g1", "hub") {
		t.Error("an adoption nobody agreed to went ahead")
	}
	// An agreement is to a move from the hub that hosted the room when it
	// was made, and outlives a failed attempt.
	fd.moves = map[string]moveConsent{"g1": {at: fd.Now(), from: "hub"}}
	if fd.hasAcceptance("g1", "elsewhere") {
		t.Error("an agreement to a move from one hub covered another's offer")
	}
	if !fd.hasAcceptance("g1", "hub") || !fd.hasAcceptance("g1", "hub") {
		t.Error("checking an agreement used it up")
	}
}
