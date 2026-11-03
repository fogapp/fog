package files

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"
)

// A batch lands the same rows the one-at-a-time calls do, once Flushed:
// folders, listed-only files and materialized bytes with their digests.
func TestBatchPlacesThenIndexesOnFlush(t *testing.T) {
	m := newManager(t)
	b, err := m.NewBatch()
	if err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := b.EnsureDir("Peers/x/deep/er"); err != nil {
		t.Fatal(err)
	}
	if err := b.PlaceDehydrated("Peers/x/deep/cloud.jpg", 1234, mt, "abc", "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Materialize("Peers/x/deep/er/"+name, strings.NewReader(name), mt, ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}

	n, err := m.Node("Peers/x/deep/cloud.jpg")
	if err != nil || !n.Dehydrated || n.Size != 1234 {
		t.Fatalf("listed-only row: %+v %v", n, err)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		n, err := m.Node("Peers/x/deep/er/" + name)
		want := sha256.Sum256([]byte(name))
		if err != nil || n.SHA256 != hex.EncodeToString(want[:]) {
			t.Fatalf("%s: %+v %v", name, n, err)
		}
	}
	if d, err := m.Node("Peers/x/deep/er"); err != nil || d.Entries != 3 {
		t.Fatalf("folder row: %+v %v", d, err)
	}
}
