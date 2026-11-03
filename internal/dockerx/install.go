package dockerx

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/hostpkg"
)

// Installer runs Docker's convenience install script on Linux, streaming
// output lines to subscribers so the first-run wizard can show progress.
// Exactly one install may run at a time.
type Installer struct {
	mu      sync.Mutex
	running bool
	lines   []string
	err     error
	done    bool
	// cancel stops the in-flight install (it owns the timeout context).
	cancel context.CancelFunc
	// newCmd builds the install command; tests substitute a fake. nil gets
	// the real get.docker.com script.
	newCmd func(ctx context.Context) *exec.Cmd
}

// installTimeout bounds the whole install. Generous — a slow mirror on a
// slow box is fine — but finite: a wedged apt would otherwise hold
// running=true forever, every retry would answer "install already running",
// and only a fogd restart could recover.
const installTimeout = 30 * time.Minute

func NewInstaller() *Installer { return &Installer{} }

type InstallStatus struct {
	Supported bool     `json:"supported"`
	Running   bool     `json:"running"`
	Done      bool     `json:"done"`
	Error     string   `json:"error,omitempty"`
	Lines     []string `json:"lines"`
}

func (in *Installer) Status() InstallStatus {
	in.mu.Lock()
	defer in.mu.Unlock()
	st := InstallStatus{
		Supported: runtime.GOOS == "linux",
		Running:   in.running,
		Done:      in.done,
		// Non-nil even when empty: this crosses the wire as JSON, and the
		// frontend indexes into it — null would crash the page.
		Lines: append([]string{}, in.lines...),
	}
	if in.err != nil {
		st.Error = in.err.Error()
	}
	return st
}

// Start kicks off the install in the background. On non-Linux hosts it fails
// fast: Docker Desktop must be installed manually on macOS dev machines.
func (in *Installer) Start(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("automatic Docker install is only supported on Linux; install Docker Desktop manually")
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.running {
		return fmt.Errorf("install already running")
	}
	// The caller hands over context.Background() (the install outlives the
	// HTTP request), so the deadline and the cancel handle live here.
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	in.running, in.done, in.err, in.lines = true, false, nil, nil
	in.cancel = cancel

	go func() {
		err := in.run(ctx)
		if err != nil && ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("the install did not finish within %s and was stopped: %v", installTimeout, err)
		}
		cancel()
		in.mu.Lock()
		in.running, in.done, in.err = false, true, err
		in.cancel = nil
		in.mu.Unlock()
	}()
	return nil
}

// defaultInstallCmd is the real thing: get.docker.com is the documented
// convenience path and handles every mainstream distro; fogd runs as root
// under systemd so no sudo needed. Download-then-run rather than curl|sh: a
// download failure surfaces as curl's own error instead of the script
// choking on partial input, and the script never executes from a truncated
// stream.
func defaultInstallCmd(ctx context.Context) *exec.Cmd {
	script := `set -e; f=$(mktemp); trap 'rm -f "$f"' EXIT; curl -fsSL https://get.docker.com -o "$f"; sh "$f"`
	// fogd's unit sandbox (ProtectSystem=full) mounts /etc and /usr
	// read-only for fogd and its children — a package install needs both.
	// hostpkg.Cmd runs it in a named transient unit of its own that the
	// deadline or a cancel stops outright, with RuntimeMaxSec as a backstop
	// should fogd die first. Outside systemd it is a process-group command:
	// the tree is sh → sh "$f" → apt and friends, and a deadline that
	// signalled only the outer sh would leave the rest running with the
	// output pipe open.
	return hostpkg.Cmd(ctx, nil, "sh", "-c", script)
}

func (in *Installer) run(ctx context.Context) error {
	newCmd := in.newCmd
	if newCmd == nil {
		newCmd = defaultInstallCmd
	}
	cmd := newCmd(ctx)
	// A child that shrugs off SIGKILL (D-state on a dying disk) must not
	// hold Wait — and running=true — forever after the context fires.
	cmd.WaitDelay = 10 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		in.mu.Lock()
		in.lines = append(in.lines, sc.Text())
		if len(in.lines) > 500 {
			in.lines = in.lines[len(in.lines)-500:]
		}
		in.mu.Unlock()
	}
	if serr := sc.Err(); serr != nil {
		// The scanner quit early (a line over its 1MB cap, most likely) but
		// the child is still writing: without a drain the 64KB pipe fills,
		// the child blocks on write, and cmd.Wait below never returns. The
		// install itself is unharmed — say so in the log rather than fail a
		// working install over lost cosmetics.
		io.Copy(io.Discard, stdout)
		in.mu.Lock()
		in.lines = append(in.lines, fmt.Sprintf("(fog: output capture stopped: %v — the install itself continues)", serr))
		in.mu.Unlock()
	}
	if err := cmd.Wait(); err != nil {
		// A bare "exit status N" diagnoses nothing; carry the script's last
		// words so the UI's error line points at the actual failure.
		in.mu.Lock()
		tail := ""
		for i := len(in.lines) - 1; i >= 0 && tail == ""; i-- {
			tail = strings.TrimSpace(in.lines[i])
		}
		in.mu.Unlock()
		if tail != "" {
			return fmt.Errorf("%v — last output: %s", err, tail)
		}
		return err
	}
	return nil
}
