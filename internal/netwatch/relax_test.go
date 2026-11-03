package netwatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// While the kernel subscription is up the poll backs off to the relaxed
// interval; when the subscription drops it tightens again.
func TestPollRelaxesWhileSubscribed(t *testing.T) {
	up := make(chan func(bool), 1)
	prev := subscribe
	subscribe = func(ctx context.Context, _ func(), live func(bool)) {
		up <- live
		<-ctx.Done()
	}
	t.Cleanup(func() { subscribe = prev })

	var polls atomic.Int32
	w := &Watcher{
		Poll:     10 * time.Millisecond,
		Relaxed:  time.Hour,
		Snapshot: func() Fingerprint { polls.Add(1); return Fingerprint{} },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Watch(ctx, func(Fingerprint) {})

	live := <-up
	live(true)
	time.Sleep(30 * time.Millisecond) // let the reset land
	before := polls.Load()
	time.Sleep(100 * time.Millisecond)
	if n := polls.Load() - before; n > 0 {
		t.Fatalf("polled %d times while subscribed", n)
	}
	live(false)
	time.Sleep(100 * time.Millisecond)
	if polls.Load() == before {
		t.Fatal("poll never resumed after the subscription dropped")
	}
}
