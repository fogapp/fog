package chat

import (
	"context"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// A hub announcing it moved a room is followed only to a household this fog
// granted chat: the old hub doesn't choose who our people's room lives with.
func TestMovedFollowsOnlyToAChatGrantedHousehold(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	stranger, _ := st.CreatePairedPeer("stranger", "stranger", "http://stranger.invalid", "")
	friend, _ := st.CreatePairedPeer("friend", "friend", "http://friend.invalid", "")
	if err := st.SetFedCapability(friend.ID, CapChat, store.DirGrant, ""); err != nil {
		t.Fatal(err)
	}
	replicaOf(t, st, hub, "r1", 3)
	replicaOf(t, st, hub, "r2", 3)
	fd := testFederation(st, nil)
	fd.syncRoom = func(context.Context, *store.FedPeer, string) error { return nil }

	fd.moved(context.Background(), hub, "r1", stranger.InstanceID)
	if r, _ := st.ChatRoom("r1"); r.HubInstance != hub.InstanceID || r.ReplicaStatus != "archived" {
		t.Fatalf("followed a move to an ungranted household: hub %q status %q", r.HubInstance, r.ReplicaStatus)
	}
	fd.moved(context.Background(), hub, "r2", friend.InstanceID)
	if r, _ := st.ChatRoom("r2"); r.HubInstance != friend.InstanceID || r.ReplicaStatus != "live" {
		t.Fatalf("didn't follow a move to a granted household: hub %q status %q", r.HubInstance, r.ReplicaStatus)
	}
}
