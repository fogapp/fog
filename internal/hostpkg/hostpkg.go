// Package hostpkg is how fogd does boring host setup itself: a command run
// in the host's own namespace (outside the unit's sandbox), and a Debian
// package installed on demand. The NAS mount helpers, ffmpeg for video
// thumbnails — anything a feature needs from the host that a bare install
// may not have — go through here, with the same retries and the same
// plain-English failures.
package hostpkg

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/execx"
)

// Runner executes one host-namespace command and returns its combined
// output. Run is the production one; tests substitute a fake.
type Runner func(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error)

var (
	// ErrNoApt: the host has no apt-get, so the package must be installed
	// by hand. Callers name the package and the binary in their own words.
	ErrNoApt = errors.New("no apt-get on this host")
	// ErrOffline: apt could not resolve the mirror — the server has no
	// internet yet. Callers say what to retry once it does.
	ErrOffline = errors.New("no internet access")
)

// InstallTimeout bounds one package install. Generous — a slow mirror on
// a slow box is fine — but finite: a wedged apt must not pin a caller.
const InstallTimeout = 10 * time.Minute

// Install puts pkg on the host with apt-get, refreshing stale package
// lists and unwedging dpkg once each if that is what stands in the way.
// A nil error means the package is installed; the caller decides whether
// its binary is on PATH.
func Install(ctx context.Context, run Runner, pkg string) error {
	if _, err := exec.LookPath("apt-get"); err != nil {
		return ErrNoApt
	}
	ctx, cancel := context.WithTimeout(ctx, InstallTimeout)
	defer cancel()
	env := map[string]string{"DEBIAN_FRONTEND": "noninteractive"}
	install := func() (string, error) {
		out, err := run(ctx, env, "apt-get", "install", "-y", "--no-install-recommends", pkg)
		return string(out), err
	}
	out, err := install()
	if err != nil && strings.Contains(out, "Unable to locate package") {
		// Stale/absent package lists (fresh minimal install) — refresh once.
		run(ctx, env, "apt-get", "update")
		out, err = install()
	}
	if err != nil && strings.Contains(out, "dpkg --configure -a") {
		// A previously interrupted install (e.g. one that died against the
		// systemd sandbox) leaves dpkg wedged — unwedge and retry once.
		run(ctx, env, "dpkg", "--configure", "-a")
		out, err = install()
	}
	if err == nil {
		return nil
	}
	// Name the actual problem when apt is failing for want of a network —
	// otherwise an install attempted before the uplink is live reads as a
	// package-manager mystery instead of "no internet yet".
	if strings.Contains(out, "Temporary failure resolving") || strings.Contains(out, "Could not resolve") {
		return fmt.Errorf("installing %s: %w", pkg, ErrOffline)
	}
	tail := out
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	return fmt.Errorf("installing %s failed: %s", pkg, strings.TrimSpace(tail))
}

// Run executes one command in the host's namespace and returns its
// combined output.
func Run(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error) {
	return Cmd(ctx, env, name, args...).CombinedOutput()
}

// Cmd builds a command that runs OUTSIDE fogd's systemd sandbox. The
// unit's ProtectSystem/ProtectHome hardening gives fogd a private mount
// namespace with a read-only /etc — inside it, package installs fail (dpkg
// can't write /etc) and, worse, mounts would succeed invisibly: they'd live
// in fogd's namespace only, and Docker would bind an empty host directory.
// systemd-run asks PID 1 to spawn the command in the host's namespace.
// Host mounts still propagate INTO the sandbox (slave propagation), so
// status checks via /proc/mounts and statfs remain accurate.
func Cmd(ctx context.Context, env map[string]string, name string, args ...string) *exec.Cmd {
	// INVOCATION_ID is set by systemd for services — the sandbox is only a
	// concern when we're actually running under the unit.
	if os.Getenv("INVOCATION_ID") != "" {
		if _, err := exec.LookPath("systemd-run"); err == nil {
			// The transient unit gets a deterministic name. Killing the
			// systemd-run client (all exec.CommandContext does on ctx expiry)
			// leaves the unit itself — the mount, the apt-get — running in
			// the host's namespace, and every retry would stack another
			// behind it. With a fixed per-operation name, a retry while the
			// old unit lives is refused loudly instead of stacked, and
			// cancellation can stop the unit by name.
			unit := unitName(name, args)
			full := []string{"--quiet", "--wait", "--collect", "--pipe", "--unit=" + unit}
			// RuntimeMaxSec is the backstop's backstop: even if fogd dies
			// before issuing the stop, PID 1 reaps the unit at the deadline
			// the caller intended (plus a little grace).
			if dl, ok := ctx.Deadline(); ok {
				if secs := int64(time.Until(dl).Seconds()) + 5; secs > 0 {
					full = append(full, fmt.Sprintf("--property=RuntimeMaxSec=%d", secs))
				}
			}
			for k, v := range env {
				full = append(full, "--setenv="+k+"="+v)
			}
			// systemd expands ${VAR} in a unit's command line, as in
			// ExecStart=: a Drive folder "Apps${X}/Glass" reached the pin
			// helper as "Apps/Glass", another app's data. "$$" is a literal
			// "$" on every systemd (--expand-environment=no needs 254, and
			// Debian 12 has 252), so every argument goes over escaped.
			full = append(full, "--", noExpand(name))
			for _, a := range args {
				full = append(full, noExpand(a))
			}
			cmd := exec.CommandContext(ctx, "systemd-run", full...)
			// The unit holds the write end of --pipe; without a WaitDelay a
			// cancelled client would sit in Wait until the unit dies.
			cmd.WaitDelay = 10 * time.Second
			cmd.Cancel = func() error {
				// Best-effort and detached: Cancel runs on ctx expiry and
				// must not block, and the expired ctx can't govern its own
				// cleanup. Stop the transient unit on its own clock, then
				// kill the client as CommandContext would have.
				go func() {
					sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					exec.CommandContext(sctx, "systemctl", "stop", unit+".service").Run()
				}()
				if cmd.Process == nil {
					return os.ErrProcessDone
				}
				return cmd.Process.Kill()
			}
			return cmd
		}
	}
	// Not under systemd: kill the whole process group on cancel, since
	// installers fork package managers that outlive a signalled parent.
	cmd := execx.Command(ctx, name, args...)
	if len(env) > 0 {
		e := os.Environ()
		for k, v := range env {
			e = append(e, k+"="+v)
		}
		cmd.Env = e
	}
	return cmd
}

// unitName names the transient unit for one host operation: stable across
// retries of the same command line, distinct across different ones. Only
// the command's base name and a short hash go in — mount points and share
// paths aren't valid unit-name characters.
func unitName(name string, args []string) string {
	h := sha256.Sum256([]byte(name + "\x00" + strings.Join(args, "\x00")))
	return fmt.Sprintf("fog-%s-%x", filepath.Base(name), h[:4])
}

// noExpand escapes s so systemd passes it to the command as written.
func noExpand(s string) string { return strings.ReplaceAll(s, "$", "$$") }
