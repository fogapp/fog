package chat

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// A follower's answer to a notify batch may refuse the invites it was sent,
// for its own people. A refusal naming anyone else — the hub's owner, say —
// or a room it was sent no invite to is ignored: otherwise any paired
// household could take anyone out of any room hubbed here.
func TestSettleHonoursOnlyTheFollowersOwnInvites(t *testing.T) {
	st, _ := testFedStore(t)
	peer, err := st.CreatePairedPeer("them", "them", "http://them.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	fd := testFederation(st, nil)
	if err := st.CreateChatRoom(&store.ChatRoom{ID: "room", GroupID: []byte("group-room"), Kind: KindGroup, Policy: DefaultPolicy(KindGroup).JSON(), ReplicaStatus: "live"}); err != nil {
		t.Fatal(err)
	}
	owner, theirs := "fog:us/owner", "fog:them/eve"
	if err := st.AppendChatEvent(&store.ChatCommit{
		Event: &store.ChatEvent{RoomID: "room", ID: "created", Kind: EventSystem, Payload: []byte("{}")},
		Added: []store.ChatParticipant{{PersonAddr: owner, HomeInstance: "us", RoleIndex: RoleOwner}, {PersonAddr: theirs, HomeInstance: "them", RoleIndex: RoleMember}},
	}); err != nil {
		t.Fatal(err)
	}
	inv, _ := json.Marshal(item{Type: "invite", RoomID: "room", Hub: "us", Invite: &inviteItem{Person: theirs}})
	batch := []fed.OutboxRow[string]{{Payload: string(inv)}}
	answer, _ := json.Marshal(map[string]any{"refused": []map[string]string{
		{"roomId": "room", "person": owner, "code": "x"},      // not theirs to speak for
		{"roomId": "other", "person": theirs, "code": "x"},    // no invite sent there
		{"roomId": "room", "person": theirs, "code": "third"}, // theirs, invited: honoured
	}})
	src := notifySource{fd, fed.OutboxQueue{Store: st, Kind: OutboxKind}}
	if err := src.Settle(peer, batch, answer); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.ChatParticipant("room", owner); p == nil || p.LeftSeq != 0 {
		t.Fatalf("a paired household took the owner out: %+v", p)
	}
	if p, _ := st.ChatParticipant("room", theirs); p == nil || p.LeftSeq == 0 {
		t.Fatalf("the follower's own refusal was not honoured: %+v", p)
	}
}

// A paired household's key fetches are bounded per household, per person it
// asks about, and for each person across every asker.
func TestKeyBudgetBoundsAPeer(t *testing.T) {
	var b KeyBudget
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i := range KeysPerAskerPerson {
		if !b.Take(now, PeerRules(7, "dave")...) {
			t.Fatalf("take %d refused within the budget", i)
		}
	}
	if b.Take(now, PeerRules(7, "dave")...) {
		t.Fatal("a peer drained past its budget for one person")
	}
	n := KeysPerAskerPerson
	for h := 0; n < KeysPerPeer; h++ {
		if b.Take(now, PeerRules(7, "p"+strconv.Itoa(h))...) {
			n++
		}
	}
	if b.Take(now, PeerRules(7, "someone-else")...) {
		t.Fatal("a peer drained past its hourly budget")
	}
	if !b.Take(now, PeerRules(8, "erin")...) {
		t.Fatal("one peer's spending blocked another's")
	}
	if !b.Take(now.Add(keyWindow), PeerRules(7, "dave")...) {
		t.Fatal("the budget never refills")
	}
}
