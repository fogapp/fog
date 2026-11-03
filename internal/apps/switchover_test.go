package apps

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Two Postgres-backed apps must never be in their switchover at once, and
// the one that waits must be told who it is waiting for.
func TestSwitchGateAdmitsOneAndNamesTheHolder(t *testing.T) {
	var g switchGate
	release, err := g.acquire(context.Background(), "feed", nil)
	if err != nil {
		t.Fatal(err)
	}

	waitedOn := make(chan string, 1)
	admitted := make(chan struct{})
	go func() {
		rel, err := g.acquire(context.Background(), "stream", func(h string) { waitedOn <- h })
		if err != nil {
			t.Error(err)
			return
		}
		close(admitted)
		rel()
	}()

	select {
	case h := <-waitedOn:
		if h != "feed" {
			t.Fatalf("waiting on %q, want feed", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire was never told it is waiting")
	}
	select {
	case <-admitted:
		t.Fatal("second app admitted while the first held the gate")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("second app not admitted after release")
	}
	if h := g.current(); h != "" {
		t.Fatalf("holder %q after both released", h)
	}
}

// A deploy cancelled while it waits gives up its place instead of
// switching over later, unasked.
func TestSwitchGateWaitHonoursContext(t *testing.T) {
	var g switchGate
	release, err := g.acquire(context.Background(), "feed", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(ctx, "hook", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if h := g.current(); h != "feed" {
		t.Fatalf("holder %q, want feed still", h)
	}
}
