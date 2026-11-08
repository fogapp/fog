package store

import "testing"

func TestFedPeersByInstanceIDsAndFanoutEnqueue(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	a, _ := s.CreatePairedPeer("inst-a", "a", "", "pk-a")
	b, _ := s.CreatePairedPeer("inst-b", "b", "", "pk-b")

	peers, err := s.FedPeersByInstanceIDs([]string{"inst-a", "inst-b", "inst-gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("peers: %+v", peers)
	}

	if err := s.EnqueueFedOutboxAll([]int64{a.ID, b.ID}, "chat", "room", `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a.ID, b.ID} {
		rows, err := s.DueFedOutbox(id, "chat", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Payload != `{"x":1}` {
			t.Fatalf("peer %d rows: %+v", id, rows)
		}
	}
}
