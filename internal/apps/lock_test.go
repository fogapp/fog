package apps

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// The reason ComposeCmd takes a context at all: a wedged compose must be
// killable, or it pins the app lock to work nobody is waiting for.
//
// Docker isn't assumed present, so the wiring is exercised by pointing the
// built command at a binary that definitely blocks. What's under test is
// ComposeCmd's cancellation contract, not which binary it names.
func TestComposeCmdHonoursContext(t *testing.T) {
	m := &Materializer{DataDir: t.TempDir()}
	// ComposeCmd runs in the app's directory; without it the child can't even
	// chdir, and the command would fail long before the deadline proved
	// anything.
	if err := os.MkdirAll(m.AppDir("whatever"), 0o755); err != nil {
		t.Fatal(err)
	}
	const timeout = 200 * time.Millisecond
	// The clock starts before the deadline does: started after it, a loaded
	// machine's delay in launching the child counted against the deadline
	// but not the measurement, and the check below misfired.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := m.ComposeCmd(ctx, "whatever", "version")
	cmd.Err = nil // discard any "docker not found" lookup failure
	cmd.Path = "/bin/sleep"
	cmd.Args = []string{"sleep", "10"}

	err := cmd.Run()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled command reported success")
	}
	// Proves the child really started and really blocked: anything that failed
	// to launch would return well before the deadline.
	if elapsed < timeout {
		t.Fatalf("returned after %v, before the %v deadline — the child never ran", elapsed, timeout)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %v — the context did not kill the child", elapsed)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.Exited() {
		t.Errorf("child exited on its own rather than being signalled: %v", cmd.ProcessState)
	}
}

// WithLock gives sibling subsystems the same exclusion a deploy takes.
func TestWithLockSerializes(t *testing.T) {
	e := &Engine{}
	var mu sync.Mutex
	inside, maxInside := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.WithLock("app", func() error {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()

				time.Sleep(2 * time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("%d holders were inside the lock at once, want 1", maxInside)
	}
}

// Different apps must not block each other — the lock is per app, not global.
func TestWithLockIsPerApp(t *testing.T) {
	e := &Engine{}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		e.WithLock("first", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	done := make(chan struct{})
	go func() {
		e.WithLock("second", func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("a lock on one app blocked another")
	}
	close(release)
}

func TestWithLockPropagatesError(t *testing.T) {
	e := &Engine{}
	sentinel := errors.New("boom")
	if err := e.WithLock("app", func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("got %v, want %v", err, sentinel)
	}
	// A failing call must still release, or the app is stuck forever.
	done := make(chan struct{})
	go func() {
		e.WithLock("app", func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("the lock was not released after an error")
	}
}

// A pre-update snapshot joins its update rather than competing with it: the
// app reads as busy throughout, and the outer claim survives the inner one.
func TestJoinOpNestsUnderBeginOp(t *testing.T) {
	e := &Engine{}
	if !e.BeginOp("app", "starting update") {
		t.Fatal("BeginOp refused an idle app")
	}
	done := e.JoinOp("app", "taking a snapshot")
	if snap := e.CurrentDeploy("app"); !snap.Running || snap.Stage != "taking a snapshot" {
		t.Errorf("stage = %q running = %v, want the inner stage while running", snap.Stage, snap.Running)
	}
	done()
	// The outer operation is still in flight — the inner one must not end it.
	if !e.Deploying("app") {
		t.Error("finishing the nested operation released the enclosing claim")
	}
	e.EndOp("app")
	if e.Deploying("app") {
		t.Error("app still reads as busy after the outer operation ended")
	}
}

// Standalone, JoinOp still marks the app busy — a snapshot with no enclosing
// update must not look idle to the UI or to the update checker.
func TestJoinOpStandalone(t *testing.T) {
	e := &Engine{}
	done := e.JoinOp("app", "taking a snapshot")
	if !e.Deploying("app") {
		t.Error("a standalone JoinOp should mark the app busy")
	}
	done()
	if e.Deploying("app") {
		t.Error("app still busy after the operation finished")
	}
}

// The eviction race: prune may TryLock-and-delete a mutex a waiter has fetched
// from the map but not yet locked, and the next caller mints a replacement —
// without the held-mutex re-check, two goroutines sit inside the same app's
// critical section. Not deterministically schedulable, so hammer WithLock
// against a continuous prune (which treats every app as dead) under -race and
// assert exclusion never breaks.
func TestWithLockExcludesAcrossPrune(t *testing.T) {
	e := &Engine{}
	var inside, breaches int32
	stop := make(chan struct{})
	var pruner sync.WaitGroup
	pruner.Add(1)
	go func() {
		defer pruner.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.pruneAppState(nil)
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				e.WithLock("app", func() error {
					if atomic.AddInt32(&inside, 1) != 1 {
						atomic.AddInt32(&breaches, 1)
					}
					atomic.AddInt32(&inside, -1)
					return nil
				})
			}
		}()
	}
	wg.Wait()
	close(stop)
	pruner.Wait()
	if n := atomic.LoadInt32(&breaches); n != 0 {
		t.Errorf("%d times two holders were inside the per-app lock at once", n)
	}
}

// Per-app bookkeeping must not accumulate forever: all three maps are keyed by
// app name and were previously only ever added to.
func TestPruneAppStateDropsDeletedApps(t *testing.T) {
	e := &Engine{}
	e.healthState.Store("gone", true)
	e.healthState.Store("kept", true)
	e.liveFor("gone")
	e.liveFor("kept")
	e.WithLock("gone", func() error { return nil })
	e.WithLock("kept", func() error { return nil })

	e.pruneAppState([]store.App{{Name: "kept"}})

	for _, name := range []string{"gone", "kept"} {
		_, health := e.healthState.Load(name)
		_, live := e.liveDeploys.Load(name)
		_, lock := e.locks.Load(name)
		want := name == "kept"
		if health != want || live != want || lock != want {
			t.Errorf("%s: health=%v live=%v lock=%v, want all %v", name, health, live, lock, want)
		}
	}
}

// An app whose row is gone but whose delete is still running must keep its
// live entry — that is what the UI polls to see the work in progress.
func TestPruneAppStateSparesInFlightWork(t *testing.T) {
	e := &Engine{}
	if !e.BeginOp("deleting", "removing containers") {
		t.Fatal("BeginOp refused an idle app")
	}
	defer e.EndOp("deleting")

	e.pruneAppState(nil)

	if _, ok := e.liveDeploys.Load("deleting"); !ok {
		t.Error("pruned the live state of an operation still in flight")
	}
	if !e.Deploying("deleting") {
		t.Error("the in-flight operation stopped reporting as busy")
	}
}
