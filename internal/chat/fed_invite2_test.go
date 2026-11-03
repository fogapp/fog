package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

func inviteFor(hub *store.FedPeer, roomID, person string) item {
	snap := &RoomSnapshot{GroupID: []byte("g-" + roomID), Policy: DefaultPolicy(KindGroup).JSON()}
	snap.Room.ID, snap.Room.Kind, snap.Room.CreatedBy = roomID, KindGroup, "fog:"+hub.InstanceID+"/op"
	return item{Type: "invite", RoomID: roomID, Hub: hub.InstanceID, Invite: &inviteItem{Person: person, InvitedBy: "fog:" + hub.InstanceID + "/op", Seq: 2, Welcome: []byte("w"), Room: snap}}
}

// A room id is bound to the household that made it: no other paired
// household can set up a replica under it first. Once a replica is held,
// only its current hub — after a hand-over this fog followed, the new one —
// may invite under its id; anyone else, the old hub included, is refused,
// and no invite ever deletes a replica.
func TestRoomIDsCantBeSquatted(t *testing.T) {
	st, _ := testFedStore(t)
	real, _ := st.CreatePairedPeer("real-hub", "real", "http://real.invalid", "")
	squat, _ := st.CreatePairedPeer("squatter", "squat", "http://squat.invalid", "")
	next, _ := st.CreatePairedPeer("next-hub", "next", "http://next.invalid", "")
	if _, err := st.CreatePerson("eve", "Eve", store.PersonRoleMember); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*store.FedPeer{real, squat, next} {
		st.SetFedCapability(p.ID, store.CapPeopleDirectory, store.DirGrant, `{"people":"members"}`)
		st.SetFedCapability(p.ID, CapChat, store.DirGrant, "")
	}
	fd := testFederation(st, nil)
	fd.syncRoom = func(context.Context, *store.FedPeer, string) error { return nil }
	ctx := context.Background()
	id, _ := BoundRoomID(real.InstanceID)
	if !RoomIDBoundTo(id, real.InstanceID) || RoomIDBoundTo(id, squat.InstanceID) {
		t.Fatal("binding")
	}
	// A squatter first: refused, nothing set up.
	refused := fd.Receive(ctx, squat, []item{inviteFor(squat, id, "fog:us/eve")})
	fd.WaitInvites()
	if len(refused) != 1 || refused[0]["code"] != "room_id_taken" {
		t.Fatalf("a squat on a paired household's room id: %v", refused)
	}
	if _, err := st.ChatRoom(id); err == nil {
		t.Fatal("the squatter set up a replica under another household's room id")
	}
	// The real hub's invite lands.
	if refused := fd.Receive(ctx, real, []item{inviteFor(real, id, "fog:us/eve")}); len(refused) != 0 {
		t.Fatalf("the real hub's invite was refused: %v", refused)
	}
	fd.WaitInvites()
	if r, err := st.ChatRoom(id); err != nil || r.HubInstance != real.InstanceID {
		t.Fatalf("the real hub's room: %+v %v", r, err)
	}
	// The room moves on to next; the replica follows.
	fd.Receive(ctx, real, []item{{Type: "moved", RoomID: id, Hub: real.InstanceID, NewHub: next.InstanceID}})
	if r, _ := st.ChatRoom(id); r.HubInstance != next.InstanceID {
		t.Fatalf("the replica didn't follow the move: %+v", r)
	}
	// Now the old hub — whose id it is — is a stranger to it.
	refused = fd.Receive(ctx, real, []item{inviteFor(real, id, "fog:us/eve")})
	fd.WaitInvites()
	if len(refused) != 1 || refused[0]["code"] != "room_id_taken" {
		t.Fatalf("the old hub invited under a room that moved on: %v", refused)
	}
	if r, err := st.ChatRoom(id); err != nil || r.HubInstance != next.InstanceID {
		t.Fatalf("an invite displaced the replica: %+v %v", r, err)
	}
	// The current hub may.
	if refused := fd.Receive(ctx, next, []item{inviteFor(next, id, "fog:us/eve")}); len(refused) != 0 {
		t.Fatalf("the current hub's invite was refused: %v", refused)
	}
	fd.WaitInvites()
}

// An invite answers, and calls back, the same whether it names one of our
// people the hub may see, someone hidden from it, or no one at all: a hub
// can't probe which handles exist here, and files nothing for someone our
// people.directory grant doesn't show it.
func TestInvitesDontRevealWhoLivesHere(t *testing.T) {
	st, _ := testFedStore(t)
	hub, _ := st.CreatePairedPeer("hub", "hub", "http://hub.invalid", "")
	st.SetFedCapability(hub.ID, CapChat, store.DirGrant, "")
	st.SetFedCapability(hub.ID, store.CapPeopleDirectory, store.DirGrant, `{"people":["eve"]}`)
	st.CreatePerson("eve", "Eve", store.PersonRoleMember)
	st.CreatePerson("zed", "Zed", store.PersonRoleMember) // not shown to the hub
	fd := testFederation(st, nil)
	var mu sync.Mutex
	calls := map[string][]string{}
	fd.getRoom = func(_ context.Context, _ *store.FedPeer, path, etag string, out any) (string, error) {
		parts := strings.SplitN(strings.TrimPrefix(path, "/fed/v1/chat/rooms/"), "/", 2)
		kind := "snapshot"
		if len(parts) > 1 {
			kind = parts[1]
		}
		mu.Lock()
		calls[parts[0]] = append(calls[parts[0]], kind+" etag="+etag)
		mu.Unlock()
		body := `{"events":[]}`
		if kind == "snapshot" {
			body = `{"room":{"id":"` + parts[0] + `","kind":"group"},"groupId":"Zy0=","policy":"","participants":[],"tree":""}`
		}
		return "", json.Unmarshal([]byte(body), out)
	}
	ctx := context.Background()
	answers := map[string]int{}
	for person, room := range map[string]string{"eve": "R-EVE", "zed": "R-ZED", "nobody": "R-NOBODY"} {
		refused := fd.Receive(ctx, hub, []item{inviteFor(hub, room, "fog:us/"+person)})
		answers[person] = len(refused)
	}
	fd.WaitInvites()
	if answers["eve"] != answers["zed"] || answers["eve"] != answers["nobody"] {
		t.Errorf("the answers differ: %v", answers)
	}
	want := strings.Join(calls["R-EVE"], ",")
	for _, r := range []string{"R-ZED", "R-NOBODY"} {
		if got := strings.Join(calls[r], ","); got != want {
			t.Errorf("calls back for %s: %q, for a real invite %q", r, got, want)
		}
	}
	if want == "" {
		t.Error("no calls at all: the comparison proves nothing")
	}
	if invs, _ := st.ChatInvitesFor("fog:us/zed"); len(invs) != 0 {
		t.Error("an invite was filed for someone the directory grant hides from the hub")
	}
	if invs, _ := st.ChatInvitesFor("fog:us/eve"); len(invs) != 1 {
		t.Errorf("the visible person's invite: %v", invs)
	}

	// Afterwards too: every poll pass calls back about each room alike,
	// for the inviteWatch a pending invite is watched…
	now := time.Now()
	fd.Now = func() time.Time { return now }
	poll := func() map[string]string {
		mu.Lock()
		clear(calls)
		mu.Unlock()
		fd.SyncAll(ctx)
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for r, c := range calls {
			out[r] = strings.Join(c, ",")
		}
		return out
	}
	got := poll()
	if got["R-EVE"] == "" || got["R-ZED"] != got["R-EVE"] || got["R-NOBODY"] != got["R-EVE"] {
		t.Errorf("a poll pass tells the rooms apart: %v", got)
	}
	// …and then for none of them: the decoys go, the real invite waits for
	// its person.
	now = now.Add(inviteWatch + time.Hour)
	if got := poll(); len(got) != 0 {
		t.Errorf("polling after the watch tells the rooms apart: %v", got)
	}
	for _, id := range []string{"R-ZED", "R-NOBODY"} {
		if _, err := st.ChatRoom(id); err == nil {
			t.Errorf("decoy %s outlived its watch", id)
		}
	}
	if _, err := st.ChatRoom("R-EVE"); err != nil {
		t.Error("the real invite's room went with the decoys")
	}
}
