package jobs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// Many callers racing to start the same kind: exactly one gets in, and a
// conflicting kind named in also blocks too. Once the job ends, the kind
// can start again.
func TestStartExclusive(t *testing.T) {
	j := New()
	release := make(chan struct{})
	block := func(ctx context.Context, p *Progress) error { <-release; return nil }

	var started atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := j.StartExclusive("sync", "s", block); ok {
				started.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := started.Load(); n != 1 {
		t.Fatalf("%d jobs of one kind started, want 1", n)
	}
	if _, ok := j.StartExclusive("copy", "c", block, "sync"); ok {
		t.Fatal("copy started while a conflicting sync runs")
	}
	if _, ok := j.StartExclusive("other", "o", func(context.Context, *Progress) error { return nil }); !ok {
		t.Fatal("an unrelated kind was refused")
	}

	cur, _ := j.Current("sync")
	close(release)
	j.Wait(context.Background(), cur.ID)
	job, ok := j.StartExclusive("sync", "s", func(context.Context, *Progress) error { return nil })
	if !ok || job.Kind != "sync" {
		t.Fatalf("kind not startable after its job ended: %+v %v", job, ok)
	}
}
