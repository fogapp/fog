package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestReadPoolRefusesWrites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.rdb.Exec(`INSERT INTO settings (key, value) VALUES ('x', 'y')`); err == nil {
		t.Fatal("read pool accepted a write")
	}
}

// A read routed to the pool right after a write returns must see it: the
// writer has committed, and WAL readers see every committed transaction.
func TestReadPoolSeesCommittedWrites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 50 {
		v := string(rune('a' + i%26))
		if err := s.SetSetting("k", v); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSetting("k")
		if err != nil || got != v {
			t.Fatalf("round %d: got %q, %v; want %q", i, got, err, v)
		}
	}
}

// The read pool keeps answering while the writer is held mid-transaction,
// and a snapshot doesn't need the writer at all.
func TestReadsAndSnapshotDontQueueBehindTheWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSetting("k", "committed"); err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.withTx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE settings SET value = 'pending' WHERE key = 'k'`); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := s.GetSetting("k")
	if err != nil || got != "committed" {
		t.Fatalf("read during write: %q, %v", got, err)
	}
	if err := s.SnapshotTo(ctx, filepath.Join(dir, "snap.db")); err != nil {
		t.Fatalf("snapshot during write: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	snap, err := Open(filepath.Join(dir, "snap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if v, _ := snap.GetSetting("k"); v != "committed" {
		t.Fatalf("snapshot holds %q, want the committed value", v)
	}
}
