package main

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
)

// tasks tracks fogd's own background goroutines — the loops the start steps
// launch and the work the wired hooks set off — so shutdown can wait for
// them before the store closes beneath them. Without it a loop mid-pass
// when the signal came wrote into a closed database, or was killed halfway
// through a write when the process exited.
//
// A panic is logged with the task's name and stack and then raised again:
// a daemon loop that died would otherwise leave fogd up but quietly without
// it, where a crash has systemd restart the whole daemon.
type tasks struct {
	log *slog.Logger
	// repanic raises a task's panic again once it is logged; nil means
	// panic. Tests swap it to watch for one without dying.
	repanic func(v any)

	mu      sync.Mutex
	wg      sync.WaitGroup
	closed  bool
	running map[string]int // live tasks by name, for the shutdown log
}

// spawn runs fn on its own goroutine under ctx. A task spawned once wait
// has begun is dropped: fogd is on its way out.
func (t *tasks) spawn(ctx context.Context, name string, fn func(context.Context)) {
	if !t.add(name) {
		return
	}
	go func() {
		defer t.done(name)
		defer func() {
			if v := recover(); v != nil {
				t.log.Error("panic in background task", "task", name, "panic", v, "stack", string(debug.Stack()))
				if t.repanic != nil {
					t.repanic(v)
					return
				}
				panic(v)
			}
		}()
		fn(ctx)
	}()
}

// run is spawn for work that already has a goroutine of its own — a hook
// called from a component's loop or job — and only needs counting: it runs
// fn here, under ctx, and shutdown waits for it like any task. A panic
// stays the caller's, as it was before the hook was counted.
func (t *tasks) run(ctx context.Context, name string, fn func(context.Context)) {
	if !t.add(name) {
		return
	}
	defer t.done(name)
	fn(ctx)
}

func (t *tasks) add(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		t.log.Warn("background task after shutdown dropped", "task", name)
		return false
	}
	if t.running == nil {
		t.running = map[string]int{}
	}
	t.running[name]++
	t.wg.Add(1)
	return true
}

func (t *tasks) done(name string) {
	t.mu.Lock()
	if t.running[name]--; t.running[name] <= 0 {
		delete(t.running, name)
	}
	t.mu.Unlock()
	t.wg.Done()
}

// wait stops new tasks and waits for the running ones until ctx ends. The
// caller cancels their context first; wait only waits. It returns the names
// of the tasks still running when ctx ended, sorted, nil when all finished.
func (t *tasks) wait(ctx context.Context) []string {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	finished := make(chan struct{})
	go func() { t.wg.Wait(); close(finished) }()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	names := make([]string, 0, len(t.running))
	for name := range t.running {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
