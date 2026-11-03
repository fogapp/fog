package execx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// The whole point of the package: a cancelled command's grandchildren die too.
// The shell backgrounds a long sleep and exits; with plain CommandContext the
// sleep would survive, because only the shell gets signalled.
func TestCommandKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Print the grandchild's pid, then block so the group stays alive.
	cmd := Command(ctx, "sh", "-c", "sleep 30 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatalf("reading grandchild pid: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("bogus grandchild pid %d", pid)
	}

	cancel()
	_ = cmd.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return // dead — the group died with its leader
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL) // don't leak it out of the test
	t.Fatalf("grandchild %d survived cancellation", pid)
}

// alive reports whether pid is still running. A killed grandchild is
// reparented to PID 1, and when PID 1 is not an init that reaps orphans — the
// CI job container, where act's `sleep` entrypoint is PID 1 — it lingers as a
// zombie that kill(pid, 0) still reports as present. On Linux /proc tells the
// two apart; elsewhere the signal probe is all there is.
func alive(pid int) bool {
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// The state field follows the parenthesised comm, which may itself
		// contain spaces or parens — so anchor on the last ')'.
		if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) {
			return stat[i+2] != 'Z' && stat[i+2] != 'X'
		}
	}
	// Signal 0 probes for existence without delivering anything.
	return syscall.Kill(pid, 0) == nil
}

// A command that finishes normally must not be disturbed by the machinery.
func TestCommandRunsToCompletion(t *testing.T) {
	out, err := Command(context.Background(), "echo", "hello").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello\n" {
		t.Errorf("got %q, want %q", out, "hello\n")
	}
}

func TestCommandContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Command(ctx, "sleep", "10").Run()
	if err == nil {
		t.Fatal("expected sleep to be killed by the deadline")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v — the deadline did not kill the child", elapsed)
	}
	var ee *exec.ExitError
	_ = ee // exit status shape is platform noise; the timing is the assertion
}
