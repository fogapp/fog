// Package supervise holds the parts every fogd-supervised child process
// needs, whatever the process is: the supervision loop itself (Process),
// the crash-loop backoff ladder it climbs, a bounded tail of its output for
// error reporting, and the download path — bounded, fsynced, checksummed
// when there is a checksum, and shape-checked so a truncated download or an
// HTML error page never lands on a binary's real path.
//
// It exists because fogd supervises more than one downloaded binary
// (cloudflared for the Cloudflare Tunnel, tailscaled for away access), and
// a second copy of a restart loop is a second thing to get subtly wrong —
// as it was, when a Stop-then-Start fix reached one copy and not the other.
package supervise

import (
	"debug/elf"
	"fmt"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/backoff"
)

// HealthyRun is how long a process must stay up for its next crash to be
// treated as a one-off rather than a continuation of a crash loop.
const HealthyRun = 5 * time.Minute

// BaseDelay is the bottom rung of the ladder: the wait before restarting a
// process that crashed for the first time, or after a healthy run.
const BaseDelay = 2 * time.Second

// restartDelay is the wait before the restart after a process that ran for
// ran, when crashes crashes came before it without a healthy run between —
// backoff's doubling ladder from BaseDelay to a one-minute ceiling — and the
// count to carry into the next one. A healthy run drops back to the bottom,
// so a one-off crash after a week of uptime costs BaseDelay, not the
// crash-loop ceiling.
//
// Call it with the run's duration BEFORE sleeping — the reset has to be
// measured from when the process started, not from when the sleep ends.
func restartDelay(crashes int, ran time.Duration) (delay time.Duration, next int) {
	if ran > HealthyRun {
		crashes = 0
	}
	return backoff.Exp(BaseDelay, time.Minute, crashes), crashes + 1
}

// TailBuffer keeps the last ~2KB of process output for error reporting. The
// zero value is ready to use, and it is safe for a process's stdout and
// stderr to write to the same one concurrently.
type TailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailBytes = 2048

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBytes {
		t.buf = t.buf[len(t.buf)-tailBytes:]
	}
	return len(p), nil
}

func (t *TailBuffer) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// elfMachines maps GOARCH onto the ELF machine an installable binary must
// declare. Only the architectures fogd installs binaries for appear here.
var elfMachines = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

// VerifyELF confirms the file at path is an ELF binary built for goarch.
// Downloads that name no version can't be checked against a published
// checksum, so this is the shape check that stands in for one: whatever
// arrived must at least be an executable for this machine.
func VerifyELF(path, goarch string) error {
	want, ok := elfMachines[goarch]
	if !ok {
		return fmt.Errorf("unsupported architecture %s", goarch)
	}
	ef, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("not an ELF binary: %w", err)
	}
	defer ef.Close()
	if ef.Machine != want {
		return fmt.Errorf("binary is for %v, this machine needs %v (%s)", ef.Machine, want, goarch)
	}
	return nil
}
