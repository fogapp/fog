package chat

import (
	"context"
	"testing"
)

// A hub's invite answers say nothing about which handles exist here: an
// invite for no one is answered as one filed would be.
func TestInviteAnswersDontRevealHandles(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	fd := testFederation(st, nil)
	snap := &RoomSnapshot{GroupID: []byte("g")}
	snap.Room.ID, snap.Room.Kind = "room", KindGroup
	refused := fd.Receive(context.Background(), hub, []item{{
		Type: "invite", RoomID: "room", Hub: hub.InstanceID,
		Invite: &inviteItem{Person: "fog:us/nobody-by-this-name", Room: snap},
	}})
	if len(refused) != 0 {
		t.Fatalf("an invite for an unknown handle was answered differently: %v", refused)
	}
}
