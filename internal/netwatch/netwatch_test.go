package netwatch

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A change in the fingerprint reaches the subscriber once, on the poll;
// an unchanged network never does.
func TestPollerReportsOnlyChanges(t *testing.T) {
	var mu sync.Mutex
	fp := Fingerprint{LocalIP: "192.168.1.200", Gateway: "192.168.1.1"}
	w := &Watcher{
		Poll:      20 * time.Millisecond,
		NoNetlink: true,
		Snapshot: func() Fingerprint {
			mu.Lock()
			defer mu.Unlock()
			return fp
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Fingerprint, 8)
	go w.Watch(ctx, func(f Fingerprint) { got <- f })

	select {
	case f := <-got:
		t.Fatalf("reported a change on an unchanged network: %+v", f)
	case <-time.After(100 * time.Millisecond):
	}

	mu.Lock()
	fp = Fingerprint{LocalIP: "10.0.0.7", Gateway: "10.0.0.1"}
	mu.Unlock()
	select {
	case f := <-got:
		if f.LocalIP != "10.0.0.7" || f.Gateway != "10.0.0.1" {
			t.Fatalf("wrong fingerprint: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("move not reported")
	}
	select {
	case f := <-got:
		t.Fatalf("reported the same move twice: %+v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCurrentIsCheap(t *testing.T) {
	start := time.Now()
	Current()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("fingerprinting took %s — must not send packets", d)
	}
}
