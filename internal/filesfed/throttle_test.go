package filesfed

import (
	"context"
	"testing"
	"time"
)

// A throttled chunk far bigger than a second of the cap waits in pieces,
// and the wait ends with its context rather than sleeping out the chunk;
// meanwhile the bucket is not locked against the peer's other transfers.
func TestThrottleWaitIsInterruptible(t *testing.T) {
	th := &throttle{}
	th.set(100 << 10)                      // 100KB/s: a 4MB chunk is 40 seconds
	th.wait(context.Background(), 100<<10) // spend the initial burst

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- th.wait(ctx, 4<<20) }()

	// Another transfer can still reach the bucket while that one waits.
	other := make(chan struct{})
	go func() {
		th.set(100 << 10)
		close(other)
	}()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("a waiting transfer held the bucket's lock")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a 40-second wait finished inside 100ms")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait ignored its context")
	}

	uncapped := &throttle{}
	uncapped.set(0)
	if err := uncapped.wait(context.Background(), 1<<30); err != nil {
		t.Fatalf("uncapped wait: %v", err)
	}
}
