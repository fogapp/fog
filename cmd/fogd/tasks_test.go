package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/server"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/tunnel"
	"gitea.lan/matthew/fog/internal/vault"
	"gitea.lan/matthew/fog/internal/vpn"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// wait returns once every task has seen its context end and finished.
func TestTasksWaitForEveryTask(t *testing.T) {
	ts := &tasks{log: quietLog()}
	ctx, cancel := context.WithCancel(context.Background())
	var finished atomic.Int32
	for _, name := range []string{"a", "b", "c"} {
		ts.spawn(ctx, name, func(ctx context.Context) {
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond) // the pass in hand
			finished.Add(1)
		})
	}
	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if left := ts.wait(wctx); left != nil {
		t.Errorf("still running: %v", left)
	}
	if n := finished.Load(); n != 3 {
		t.Errorf("%d tasks finished before wait returned, want 3", n)
	}
}

// A task that won't stop is named when the budget runs out, and tracked
// hook work (run) counts the same as a spawned goroutine.
func TestTasksNameWhatDidNotFinish(t *testing.T) {
	ts := &tasks{log: quietLog()}
	stuck := make(chan struct{})
	defer close(stuck)
	ts.spawn(context.Background(), "stuck loop", func(context.Context) { <-stuck })
	go ts.run(context.Background(), "stuck hook", func(context.Context) { <-stuck })
	ts.spawn(context.Background(), "quick", func(context.Context) {})
	time.Sleep(20 * time.Millisecond)
	wctx, wcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer wcancel()
	if left := ts.wait(wctx); !slices.Equal(left, []string{"stuck hook", "stuck loop"}) {
		t.Errorf("still running = %v, want the two stuck tasks", left)
	}
}

// Once wait has begun nothing new starts, and a panicking task is raised
// again (so systemd restarts fogd) after it is counted out.
func TestTasksAfterWaitAndPanics(t *testing.T) {
	raised := make(chan any, 1)
	ts := &tasks{log: quietLog(), repanic: func(v any) { raised <- v }}
	ts.spawn(context.Background(), "panics", func(context.Context) { panic("boom") })
	select {
	case v := <-raised:
		if v != "boom" {
			t.Errorf("raised %v, want boom", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a task's panic was swallowed")
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if left := ts.wait(wctx); left != nil {
		t.Errorf("a panicked task kept the wait open: %v", left)
	}
	ran := false
	ts.spawn(context.Background(), "late", func(context.Context) { ran = true })
	ts.run(context.Background(), "late hook", func(context.Context) { ran = true })
	time.Sleep(20 * time.Millisecond)
	if ran {
		t.Error("a task started after shutdown began")
	}
}

// shutdown cancels the root context and waits for fogd's own tasks before
// it closes the store: a loop finishing its pass after the signal still
// has a database to write to.
func TestShutdownWaitsForTasksBeforeClosingTheStore(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := quietLog()
	cfg := config.Config{DataDir: dir}
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	d := &daemon{
		cfg: cfg, log: log, ctx: rootCtx, st: st, vlt: vlt,
		vpnMgr: &vpn.Manager{Store: st, Vault: vlt, Log: log, DataDir: dir},
		tun:    &tunnel.Manager{Store: st, Vault: vlt, Log: log},
		api:    server.New(cfg, st, server.Deps{Vault: vlt}, log),
		srv:    &http.Server{},
	}
	d.tasks.log = log

	writeErr := make(chan error, 1)
	d.spawn("loop", func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // the pass in hand when the signal came
		writeErr <- st.SetSetting("shutdown-test", "written")
	})
	d.shutdown(cancelRoot)
	select {
	case err := <-writeErr:
		if err != nil {
			t.Errorf("the task's last write failed: %v — the store closed under it", err)
		}
	default:
		t.Error("shutdown returned before the task finished")
	}
}
