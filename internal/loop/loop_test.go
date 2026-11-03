package loop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestEveryRunsFirstThenOnCadence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var n atomic.Int32
	done := make(chan struct{})
	go func() {
		Every(ctx, 0, 5*time.Millisecond, func(context.Context) {
			if n.Add(1) == 3 {
				cancel()
			}
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop never reached three passes")
	}
	if got := n.Load(); got != 3 {
		t.Errorf("passes = %d, want 3 (none after cancel)", got)
	}
}

func TestEveryWaitsOutFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	Every(ctx, time.Hour, time.Millisecond, func(context.Context) { n.Add(1) })
	if n.Load() != 0 {
		t.Error("ran before the first delay elapsed")
	}
}

func TestRunKickSkipsTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kick := make(chan struct{}, 1)
	ran := make(chan struct{}, 4)
	go Run(ctx, time.Hour, kick, func(context.Context) time.Duration {
		ran <- struct{}{}
		return time.Hour
	})
	kick <- struct{}{}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("kick did not run a pass")
	}
}

func TestRunUsesReturnedWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var n atomic.Int32
	done := make(chan struct{})
	go func() {
		Run(ctx, 0, nil, func(context.Context) time.Duration {
			if n.Add(1) == 2 {
				cancel()
			}
			return time.Millisecond // a long return would hang the test
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("returned wait was not honoured")
	}
}
