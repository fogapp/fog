package supervise

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Process supervises one long-running child: it installs the binary first
// when it has to, starts it, restarts it on the backoff ladder when it
// exits, and stops it cleanly. Everything that differs between the
// processes fogd runs — which binary, which arguments, what to do once it
// is up — comes in through the fields; the bookkeeping, which is where the
// subtle bugs live, is written once here.
//
// The fields are read by the supervision loop, so set them before the first
// Start and leave them alone after.
type Process struct {
	// Name is the binary's name, for logs and error messages.
	Name string
	Log  *slog.Logger

	// Installed reports whether the binary is in place. When it says no,
	// the loop runs Install before the first start. Both may be nil for a
	// binary that is always present. Install's context is cancelled by
	// Stop, so a download doesn't outlive the operator changing their mind.
	Installed func() bool
	Install   func(ctx context.Context) error

	// Command builds the command for one run. It is called before every
	// start, so it can read state that may have changed since the last one
	// (a token, a route). An error stops supervision and becomes LastError.
	// Stdout and stderr are taken over for the error tail.
	Command func() (*exec.Cmd, error)

	// Started, when set, runs on the loop's goroutine right after each
	// start.
	Started func(pid int)
	// Exited, when set, runs after each exit, once the state says the
	// process is gone.
	Exited func()
	// Stopping, when set, runs in Stop while the process is still alive,
	// before it is signalled.
	Stopping func()

	mu         sync.Mutex
	desired    bool // the operator wants it running
	looping    bool // a supervision loop is alive (not the same as desired)
	running    bool
	pid        int
	proc       *os.Process
	restarts   int
	lastErr    string
	installing bool
	installErr string
	wake       chan struct{} // cuts the current loop's backoff short
	ctx        context.Context
	cancel     context.CancelFunc
}

// State is a point-in-time reading of a Process.
type State struct {
	Desired      bool
	Running      bool
	Pid          int
	Restarts     int
	LastError    string
	Installing   bool
	InstallError string
}

// Status is the part of a supervised daemon's status every manager reports
// the same way. Managers embed it in their own status so its fields sit at
// the top level of their JSON.
type Status struct {
	Installed    bool   `json:"installed"`
	Installing   bool   `json:"installing"`
	InstallError string `json:"installError,omitempty"`
	Running      bool   `json:"running"`
	Pid          int    `json:"pid,omitempty"`
	Restarts     int    `json:"restarts"`
	LastError    string `json:"lastError,omitempty"`
}

// Status reports the supervisor's state; installed is the manager's own
// answer, since only it knows which binaries make a complete install.
func (p *Process) Status(installed bool) Status {
	st := p.State()
	return Status{
		Installed:    installed,
		Installing:   st.Installing,
		InstallError: st.InstallError,
		Running:      st.Running,
		Pid:          st.Pid,
		Restarts:     st.Restarts,
		LastError:    st.LastError,
	}
}

// Executable reports whether path is a regular file someone may execute.
func Executable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

func (p *Process) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return State{
		Desired:      p.desired,
		Running:      p.running,
		Pid:          p.pid,
		Restarts:     p.restarts,
		LastError:    p.lastErr,
		Installing:   p.installing,
		InstallError: p.installErr,
	}
}

// StartResult says what Start found.
type StartResult int

const (
	// AlreadyStarted: supervision was already wanted; nothing changed.
	AlreadyStarted StartResult = iota
	// Resumed: a loop was still alive (asleep in a backoff, or finishing
	// after a Stop) and carries on supervising.
	Resumed
	// Launched: a new supervision loop was started.
	Launched
)

// Start begins supervision and returns at once; progress shows in State.
func (p *Process) Start() StartResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.desired {
		return AlreadyStarted
	}
	p.desired = true
	p.lastErr = ""
	// Gate the second loop on whether one is alive, not on whether the
	// operator wants it running. Stopping during a restart backoff leaves
	// the loop asleep; starting again before it wakes would otherwise
	// supervise a second process alongside the first. The live loop sees
	// desired again and carries on.
	if p.looping {
		if p.ctx.Err() != nil {
			// Stop cancelled the loop's context; an install it was running
			// gives up and goes again under this one.
			p.ctx, p.cancel = context.WithCancel(context.Background())
		}
		return Resumed
	}
	p.looping = true
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.wake = make(chan struct{}, 1)
	go p.loop(p.wake)
	return Launched
}

// Stop disables restarts and terminates the process: SIGTERM, then SIGKILL
// if it is still there five seconds later. It returns once the process is
// gone, or at once when there is none.
func (p *Process) Stop() {
	p.mu.Lock()
	p.desired = false
	if p.cancel != nil {
		p.cancel()
	}
	if p.wake != nil {
		select {
		case p.wake <- struct{}{}: // cut a restart backoff short
		default:
		}
	}
	proc := p.proc
	p.mu.Unlock()
	if proc == nil {
		return
	}
	if p.Stopping != nil {
		p.Stopping()
	}
	proc.Signal(syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		p.mu.Lock()
		// Compare against this process, not "anything running": a Start
		// racing this Stop may already have a new one up.
		alive := p.proc == proc
		p.mu.Unlock()
		if !alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	proc.Kill()
}

// keepLooping reports whether the loop should run the process again. When
// it shouldn't, the loop is marked gone in the same critical section, so a
// Start racing the exit either sees it alive and is carried on by it, or
// sees it gone and starts a new one — never neither.
func (p *Process) keepLooping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.desired {
		p.looping = false
	}
	return p.desired
}

// fail records an error that ends supervision; the loop returns right after.
func (p *Process) fail(s string) {
	p.mu.Lock()
	p.lastErr = s
	p.desired = false
	p.looping = false
	p.mu.Unlock()
}

// install runs Install until the binary is in place, and reports whether
// the loop should go on.
func (p *Process) install() bool {
	for p.Installed != nil && !p.Installed() {
		p.mu.Lock()
		ctx := p.ctx
		p.installing = true
		p.installErr = ""
		p.mu.Unlock()

		err := p.Install(ctx)

		p.mu.Lock()
		p.installing = false
		if err == nil {
			p.mu.Unlock()
			return true
		}
		if ctx.Err() != nil && p.desired {
			// Stopped mid-install and started again: Start left a fresh
			// context for the retry.
			p.mu.Unlock()
			continue
		}
		if ctx.Err() == nil {
			// A cancelled install is the operator's doing, not a fault.
			p.installErr = err.Error()
			p.lastErr = err.Error()
		}
		p.desired = false
		p.looping = false
		p.mu.Unlock()
		return false
	}
	return true
}

func (p *Process) loop(wake chan struct{}) {
	if !p.install() {
		return
	}
	crashes := 0
	for p.keepLooping() {
		cmd, err := p.Command()
		if err != nil {
			p.fail(err.Error())
			return
		}
		var tail TailBuffer
		cmd.Stdout = &tail
		cmd.Stderr = &tail
		if err := cmd.Start(); err != nil {
			p.fail(p.Name + " failed to start: " + err.Error())
			return
		}
		start := time.Now() // measured from process start, so ran below is the actual run duration
		p.mu.Lock()
		p.running = true
		p.pid = cmd.Process.Pid
		p.proc = cmd.Process
		if !p.desired {
			// A Stop landed between keepLooping and here, and found no
			// process to signal. Finish its job; the exit below then ends
			// the loop as usual.
			cmd.Process.Signal(syscall.SIGTERM)
		}
		p.mu.Unlock()
		p.Log.Info(p.Name+" started", "pid", cmd.Process.Pid)
		if p.Started != nil {
			p.Started(cmd.Process.Pid)
		}

		waitErr := cmd.Wait()
		ran := time.Since(start)
		p.mu.Lock()
		p.running = false
		p.pid = 0
		p.proc = nil
		stillDesired := p.desired
		if stillDesired {
			p.restarts++
			p.lastErr = strings.TrimSpace(fmt.Sprintf("exited (%v): %s", waitErr, tail.Tail()))
		} else {
			p.looping = false
		}
		p.mu.Unlock()
		if p.Exited != nil {
			p.Exited()
		}
		if !stillDesired {
			return
		}
		// Pick the delay from how long the process actually ran, BEFORE
		// sleeping: a crash after a long healthy run restarts in seconds
		// rather than paying the crash-loop ceiling one more time.
		var delay time.Duration
		delay, crashes = restartDelay(crashes, ran)
		p.Log.Warn(p.Name+" exited; restarting", "err", waitErr, "ran", ran.Truncate(time.Second).String(), "backoff", delay.String())
		select {
		case <-time.After(delay):
		case <-wake:
		}
	}
}
