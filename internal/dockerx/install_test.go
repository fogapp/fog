package dockerx

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/execx"
)

// A line longer than the scanner's 1MB cap makes bufio.Scanner stop with
// ErrTooLong while the child keeps writing. Without the post-scan drain the
// 64KB pipe fills, the child blocks on write, and cmd.Wait never returns —
// the install wedges as "running" forever. This pins the drain: the run
// finishes, succeeds, and says in its log that capture stopped.
func TestRunDrainsAfterScannerOverflow(t *testing.T) {
	in := NewInstaller()
	in.newCmd = func(ctx context.Context) *exec.Cmd {
		// One >1MB line, then plenty more output than a pipe buffer holds.
		return exec.CommandContext(ctx, "sh", "-c",
			`dd if=/dev/zero bs=1024 count=2048 2>/dev/null | tr "\\0" "a"; echo; dd if=/dev/zero bs=1024 count=256 2>/dev/null | tr "\\0" "b"; echo; echo after-overflow`)
	}
	done := make(chan error, 1)
	go func() { done <- in.run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run never returned — the pipe was not drained after scanner overflow")
	}
	joined := strings.Join(in.lines, "\n")
	if !strings.Contains(joined, "output capture stopped") {
		t.Fatalf("no capture-stopped note in the log:\n%s", joined)
	}
}

// The install context carries a deadline (Start owns it); a hung script must
// come back as an error instead of holding running=true until a fogd
// restart.
func TestRunStopsOnContextDeadline(t *testing.T) {
	in := NewInstaller()
	// execx, as the real fallback uses: dash (Debian's sh) forks the sleep
	// rather than exec'ing it, and a plain CommandContext kill would leave
	// that grandchild holding the output pipe for the full minute.
	in.newCmd = func(ctx context.Context) *exec.Cmd {
		return execx.Command(ctx, "sh", "-c", "sleep 60")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- in.run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a deadline-killed install reported success")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run outlived its context deadline")
	}
}
