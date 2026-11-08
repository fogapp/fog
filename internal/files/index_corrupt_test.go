package files

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// jen's index came back from an fsck torn: it opened, said its schema
// version, and failed every listing with "database disk image is
// malformed" until someone moved it aside by hand. The index is a cache of
// the tree, so fog now does that itself: the damaged file is kept aside, a
// fresh one opened, and a walk refills it.
func TestADamagedIndexIsReplacedAndRebuilt(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	for i := range 200 {
		write(t, m, filepath.Join("docs", "note-"+strconv.Itoa(i)+".txt"), "hello")
	}
	if err := m.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	ix, err := m.Index()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	m.imu.Lock()
	m.idx = nil
	m.imu.Unlock()

	// Every page past the first (whose header and schema still read) is
	// overwritten: the file opens, and the tables under it are garbage.
	raw, err := os.ReadFile(m.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 4096 {
		t.Fatalf("index too small to damage: %d bytes", len(raw))
	}
	copy(raw[4096:], bytes.Repeat([]byte{0xA5}, len(raw)-4096))
	if err := os.WriteFile(m.IndexPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.List("docs", ListOpts{}); err == nil {
		t.Fatal("a listing over a damaged index succeeded; the test did not damage it")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := m.List("docs", ListOpts{})
		if err == nil && len(l.Entries) == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("index never came back: %d entries, %v", len(l.Entries), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	aside, _ := filepath.Glob(m.IndexPath() + ".corrupt-*")
	if len(aside) == 0 {
		t.Fatal("the damaged index was not kept aside")
	}
}
