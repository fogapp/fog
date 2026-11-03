package supervise

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRuns hands a Process its commands and counts what it did with them:
// how many commands were built, and the most that were ever alive at once.
type fakeRuns struct {
	mu      sync.Mutex
	built   int
	live    int
	maxLive int
	next    func(n int) *exec.Cmd // n counts from 1
}

func (f *fakeRuns) process() *Process {
	return &Process{
		Name: "fake",
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Command: func() (*exec.Cmd, error) {
			f.mu.Lock()
			f.built++
			n := f.built
			f.mu.Unlock()
			return f.next(n), nil
		},
		Started: func(int) {
			f.mu.Lock()
			f.live++
			f.maxLive = max(f.maxLive, f.live)
			f.mu.Unlock()
		},
		Exited: func() {
			f.mu.Lock()
			f.live--
			f.mu.Unlock()
		},
	}
}

func (f *fakeRuns) counts() (built, maxLive int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.built, f.maxLive
}

func crash() *exec.Cmd                   { return exec.Command("sh", "-c", "echo boom >&2; exit 3") }
func longRun() *exec.Cmd                 { return exec.Command("sleep", "30") }
func quiet() *slog.Logger                { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func stopAtEnd(t *testing.T, p *Process) { t.Cleanup(p.Stop) }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Stop and Start while the loop sleeps in its restart backoff must leave
// one supervisor, not two: the sleeping loop carries on, and no second loop
// starts a second process beside it.
func TestStopDuringBackoffThenStartRunsOneProcess(t *testing.T) {
	f := &fakeRuns{next: func(n int) *exec.Cmd {
		if n == 1 {
			return crash()
		}
		return longRun()
	}}
	p := f.process()
	stopAtEnd(t, p)

	if p.Start() != Launched {
		t.Fatal("first Start did not launch a loop")
	}
	waitUntil(t, "the first run to crash", func() bool { return p.State().Restarts == 1 })
	p.Stop()
	p.Start()
	waitUntil(t, "the second run", func() bool { return p.State().Running })
	// Past the whole base backoff: a second loop would have built its own run.
	time.Sleep(BaseDelay + 500*time.Millisecond)
	if built, maxLive := f.counts(); built != 2 || maxLive != 1 {
		t.Fatalf("built %d runs with up to %d alive at once, want 2 and 1", built, maxLive)
	}
}

// Every exit the operator didn't ask for is a restart, and the last one's
// output is what LastError says.
func TestRestartsAndLastError(t *testing.T) {
	f := &fakeRuns{next: func(int) *exec.Cmd { return crash() }}
	p := f.process()
	stopAtEnd(t, p)
	p.Start()
	waitUntil(t, "two restarts", func() bool { return p.State().Restarts == 2 })
	st := p.State()
	if !st.Desired {
		t.Error("a crash turned supervision off")
	}
	if !strings.Contains(st.LastError, "exit status 3") || !strings.Contains(st.LastError, "boom") {
		t.Errorf("LastError = %q, want the exit status and the output tail", st.LastError)
	}

	// Stop cuts the backoff short and the loop ends: nothing more is built.
	p.Stop()
	built, _ := f.counts()
	time.Sleep(200 * time.Millisecond)
	if again, _ := f.counts(); again != built {
		t.Errorf("built %d more runs after Stop", again-built)
	}
	if st := p.State(); st.Restarts != 2 || st.Desired {
		t.Errorf("after Stop: %+v", st)
	}

	// A new Start clears the old error but keeps counting.
	p.Start()
	if st := p.State(); st.LastError != "" {
		t.Errorf("Start kept the previous LastError %q", st.LastError)
	}
}

// An operator-requested stop is not a restart and not an error.
func TestStopIsNotARestart(t *testing.T) {
	stopping := 0
	p := &Process{Name: "fake", Log: quiet(), Command: func() (*exec.Cmd, error) { return longRun(), nil }}
	p.Stopping = func() { stopping++ }
	p.Start()
	waitUntil(t, "the run", func() bool { return p.State().Running })
	p.Stop()
	st := p.State()
	if st.Running || st.Pid != 0 || st.Restarts != 0 || st.LastError != "" {
		t.Errorf("after Stop: %+v", st)
	}
	if stopping != 1 {
		t.Errorf("Stopping ran %d times, want 1", stopping)
	}
	// Stopping with nothing running doesn't call the hook.
	p.Stop()
	if stopping != 1 {
		t.Errorf("Stopping ran with no process")
	}
}

func TestCommandErrorEndsSupervision(t *testing.T) {
	fail := true
	var mu sync.Mutex
	p := &Process{Name: "fake", Log: quiet(), Command: func() (*exec.Cmd, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("no token")
		}
		return longRun(), nil
	}}
	stopAtEnd(t, p)
	p.Start()
	waitUntil(t, "the error", func() bool { return p.State().LastError == "no token" })
	if p.State().Desired {
		t.Error("still desired after a fatal error")
	}
	// The loop is gone, so the next Start launches a new one.
	mu.Lock()
	fail = false
	mu.Unlock()
	waitUntil(t, "the loop to end", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !p.looping
	})
	if r := p.Start(); r != Launched {
		t.Fatalf("Start after a fatal error = %v, want Launched", r)
	}
	waitUntil(t, "the run", func() bool { return p.State().Running })
}

func TestInstallFailureIsReported(t *testing.T) {
	p := &Process{
		Name:      "fake",
		Log:       quiet(),
		Installed: func() bool { return false },
		Install:   func(context.Context) error { return errors.New("HTTP 404") },
		Command:   func() (*exec.Cmd, error) { t.Error("ran without an install"); return longRun(), nil },
	}
	p.Start()
	waitUntil(t, "the install error", func() bool { return p.State().InstallError == "HTTP 404" })
	if st := p.State(); st.LastError != "HTTP 404" || st.Desired || st.Installing {
		t.Errorf("after a failed install: %+v", st)
	}
}

// Stop cancels an install in flight without calling it a failure, and a
// Start before the loop has noticed goes again with a live context rather
// than inheriting the cancelled one.
func TestStopCancelsInstallAndStartRetries(t *testing.T) {
	var mu sync.Mutex
	installed, attempts := false, 0
	entered := make(chan struct{}, 2)
	p := &Process{
		Name: "fake",
		Log:  quiet(),
		Installed: func() bool {
			mu.Lock()
			defer mu.Unlock()
			return installed
		},
		Install: func(ctx context.Context) error {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			entered <- struct{}{}
			if n == 1 {
				<-ctx.Done() // a download that only ends when cancelled
				// Hold on past the cancel so the next Start lands while
				// this loop is still alive.
				time.Sleep(100 * time.Millisecond)
				return ctx.Err()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			mu.Lock()
			installed = true
			mu.Unlock()
			return nil
		},
		Command: func() (*exec.Cmd, error) { return longRun(), nil },
	}
	stopAtEnd(t, p)
	p.Start()
	<-entered
	if !p.State().Installing {
		t.Error("Installing not reported during the install")
	}
	p.Stop()
	if st := p.State(); st.InstallError != "" || st.LastError != "" {
		t.Errorf("a cancelled install reported an error: %+v", st)
	}
	if r := p.Start(); r != Resumed {
		t.Fatalf("Start during the cancelled install = %v, want Resumed", r)
	}
	waitUntil(t, "the run after the retried install", func() bool { return p.State().Running })
}
