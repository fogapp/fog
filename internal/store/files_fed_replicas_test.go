package store

import (
	"fmt"
	"testing"
)

// FileFedReplicasFor returns just the asked-for rows of one subscription,
// across the chunk boundary, and nothing from another subscription.
func TestFileFedReplicasFor(t *testing.T) {
	s := testStore(t)
	peer, err := s.CreatePairedPeer("inst-1", "peer", "", "pk")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateFileFedSubscription(&FileFedSubscription{PeerID: peer.ID, RemoteShareID: 1, Folder: "A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateFileFedSubscription(&FileFedSubscription{PeerID: peer.ID, RemoteShareID: 2, Folder: "B"})
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := range 700 {
		rel := fmt.Sprintf("f%03d", i)
		if err := s.UpsertFileFedReplica(FileFedReplica{SubscriptionID: a.ID, Rel: rel, Kind: "file", Size: int64(i)}); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			want = append(want, rel)
		}
	}
	s.UpsertFileFedReplica(FileFedReplica{SubscriptionID: b.ID, Rel: "f000", Kind: "file"})

	got, err := s.FileFedReplicasFor(a.ID, append(want, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	if r := got["f698"]; r.Size != 698 || r.SubscriptionID != a.ID {
		t.Errorf("f698 = %+v", r)
	}
	if _, ok := got["f001"]; ok {
		t.Error("returned a row that wasn't asked for")
	}
}

// UpsertFileFedReplicas inserts and updates in one go, like the single form.
func TestUpsertFileFedReplicas(t *testing.T) {
	s := testStore(t)
	peer, err := s.CreatePairedPeer("inst-1", "peer", "", "pk")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateFileFedSubscription(&FileFedSubscription{PeerID: peer.ID, RemoteShareID: 1, Folder: "A"})
	if err != nil {
		t.Fatal(err)
	}
	s.UpsertFileFedReplica(FileFedReplica{SubscriptionID: sub.ID, Rel: "old", Kind: "file", Size: 1})
	err = s.UpsertFileFedReplicas([]FileFedReplica{
		{SubscriptionID: sub.ID, Rel: "old", Kind: "file", Size: 2, Hydrated: true},
		{SubscriptionID: sub.ID, Rel: "dir", Kind: "dir", Hydrated: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListFileFedReplicas(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows["old"].Size != 2 || !rows["old"].Hydrated || rows["dir"].Kind != "dir" {
		t.Fatalf("rows = %+v", rows)
	}
}
