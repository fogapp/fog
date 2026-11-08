package server

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownCancelsAndWaitsForBackgroundTasks(t *testing.T) {
	t.Parallel()

	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())

	started := make(chan struct{})
	var finished atomic.Bool
	s.goSafe("long task", func() {
		close(started)
		<-s.bgContext().Done()
		time.Sleep(20 * time.Millisecond) // cleanup after cancellation
		finished.Store(true)
	})
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Shutdown returned before the task finished")
	}

	var ran atomic.Bool
	s.goSafe("late task", func() { ran.Store(true) })
	time.Sleep(20 * time.Millisecond)
	if ran.Load() {
		t.Fatal("a task started after Shutdown ran")
	}
}

func TestShutdownGivesUpAtItsDeadline(t *testing.T) {
	t.Parallel()

	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	s.goSafe("stuck task", func() { <-release })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown reported success with a task still running")
	}
}
