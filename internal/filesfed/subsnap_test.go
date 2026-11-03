package filesfed

import (
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// The hot paths read subscriptions from memory, and every write through the
// service drops that copy: a subscription added, changed or removed is seen
// by the very next BackupSkip, Guard or lookup.
func TestSubscriptionSnapshotFollowsWrites(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	peer, err := st.CreatePairedPeer("peer-1", "north", "", "key")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: st}

	if s.BackupSkip("Peers/north/Album/a.jpg") {
		t.Fatal("skipped with no subscriptions")
	}
	sub, err := s.createSubscription(&store.FileFedSubscription{
		PeerID: peer.ID, RemoteShareID: 1, Title: "Album", Folder: "Peers/north/Album",
		Mode: ModeRead, DetailOffered: DetailFull, DetailWanted: DetailFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.BackupSkip("Peers/north/Album/a.jpg") {
		t.Error("a new copy left out of backups was backed up")
	}
	if err := s.Guard("Peers/north/Album/a.jpg"); err == nil {
		t.Error("guard let a write into a new copy")
	}

	sub.Backup = true
	if err := s.updateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	if s.BackupSkip("Peers/north/Album/a.jpg") {
		t.Error("a copy chosen for backup is still skipped")
	}

	if err := s.deleteSubscription(sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.subscriptionFor("Peers/north/Album/a.jpg"); ok {
		t.Error("a removed subscription is still found")
	}
	if err := s.Guard("Peers/north/Album/a.jpg"); err != nil {
		t.Errorf("guard still refuses a removed copy: %v", err)
	}
}
