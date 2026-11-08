package store

import "testing"

// The bulk reads the peer list uses agree with their per-peer forms.
func TestFedBulkReads(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	a, err := s.CreatePairedPeer("inst-a", "a", "", "pk-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreatePairedPeer("inst-b", "b", "", "pk-b")
	if err != nil {
		t.Fatal(err)
	}
	s.SetFedCapability(a.ID, CapAppsDirectory, DirUse, `{"x":1}`)
	s.SetFedCapability(a.ID, CapAppsDirectory, DirGrant, "")
	s.SetFedCapability(b.ID, CapAppsIntroduce, DirUse, "")

	caps, err := s.AllFedCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if len(caps[a.ID]) != 2 || len(caps[b.ID]) != 1 {
		t.Fatalf("caps by peer: %+v", caps)
	}

	cfg, err := s.FedCapabilityConfigs(CapAppsDirectory, DirUse)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg) != 1 || cfg[a.ID] != `{"x":1}` {
		t.Fatalf("configs: %+v", cfg)
	}

	for i, folder := range []string{"A", "B"} {
		if _, err := s.CreateFileFedSubscription(&FileFedSubscription{PeerID: a.ID, RemoteShareID: int64(i + 1), Folder: folder}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateFileFedShare(&FileFedShare{PeerID: b.ID, Path: "Photos", Title: "Photos"}); err != nil {
		t.Fatal(err)
	}
	shares, subs, err := s.FileFedCountsByPeer()
	if err != nil {
		t.Fatal(err)
	}
	if subs[a.ID] != 2 || subs[b.ID] != 0 || shares[b.ID] != 1 || shares[a.ID] != 0 {
		t.Fatalf("shares %v subs %v", shares, subs)
	}
}
