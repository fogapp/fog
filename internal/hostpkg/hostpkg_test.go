package hostpkg

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The transient unit name must be stable for one command line (so a retry
// can't stack a second copy behind a still-running first) and distinct
// across different ones.
func TestUnitName(t *testing.T) {
	a := unitName("mount", []string{"-t", "nfs", "nas.lan:/volume1/media", "/mnt/nas/media"})
	b := unitName("mount", []string{"-t", "nfs", "nas.lan:/volume1/media", "/mnt/nas/media"})
	c := unitName("mount", []string{"-t", "nfs", "nas.lan:/volume1/backups", "/mnt/nas/backups"})
	if a != b {
		t.Errorf("same operation produced different unit names: %s vs %s", a, b)
	}
	if a == c {
		t.Errorf("different operations share a unit name: %s", a)
	}
	if !strings.HasPrefix(a, "fog-mount-") {
		t.Errorf("unit name %q lacks the fog-mount- prefix", a)
	}
	if strings.ContainsAny(a, "/ \t") {
		t.Errorf("unit name %q contains characters systemd rejects", a)
	}
}

func needApt(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("apt-get"); err != nil {
		t.Skip("no apt-get on this host; Install refuses before running anything")
	}
}

// A first "Unable to locate package" means stale lists: refresh once and
// retry, and the second attempt's success is the answer.
func TestInstallRefreshesStaleLists(t *testing.T) {
	needApt(t)
	var calls []string
	run := func(_ context.Context, _ map[string]string, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "apt-get" && args[0] == "install" && len(calls) == 1 {
			return []byte("E: Unable to locate package ffmpeg"), errors.New("exit 100")
		}
		return nil, nil
	}
	if err := Install(context.Background(), run, "ffmpeg"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{
		"apt-get install -y --no-install-recommends ffmpeg",
		"apt-get update",
		"apt-get install -y --no-install-recommends ffmpeg",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// No mirror reachable is reported as being offline, not as an apt mystery.
func TestInstallOffline(t *testing.T) {
	needApt(t)
	run := func(_ context.Context, _ map[string]string, _ string, _ ...string) ([]byte, error) {
		return []byte("Err:1 http://deb.debian.org/debian trixie InRelease\n  Temporary failure resolving 'deb.debian.org'"), errors.New("exit 100")
	}
	err := Install(context.Background(), run, "ffmpeg")
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("got %v, want ErrOffline", err)
	}
}

// Any other failure carries apt's own words, so the operator sees what apt
// saw rather than "install failed".
func TestInstallReportsAptOutput(t *testing.T) {
	needApt(t)
	run := func(_ context.Context, _ map[string]string, _ string, _ ...string) ([]byte, error) {
		return []byte("E: You don't have enough free space in /var/cache/apt/archives/."), errors.New("exit 100")
	}
	err := Install(context.Background(), run, "ffmpeg")
	if err == nil || !strings.Contains(err.Error(), "enough free space") {
		t.Fatalf("got %v, want apt's message", err)
	}
}

func TestInstallWithoutAptFailsFast(t *testing.T) {
	if _, err := exec.LookPath("apt-get"); err == nil {
		t.Skip("host has apt-get")
	}
	ran := false
	run := func(_ context.Context, _ map[string]string, _ string, _ ...string) ([]byte, error) {
		ran = true
		return nil, nil
	}
	if err := Install(context.Background(), run, "ffmpeg"); !errors.Is(err, ErrNoApt) {
		t.Fatalf("got %v, want ErrNoApt", err)
	}
	if ran {
		t.Error("ran a command on a host with no apt-get")
	}
}

// Outside systemd, a cancelled command takes its whole process group down:
// an installer's package manager must not outlive the deadline.
func TestCmdFallbackKillsProcessGroup(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := Cmd(ctx, nil, "sh", "-c", "sleep 30")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("fallback command does not lead its own process group")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("cancelled command exited cleanly")
	}
}

// The attack: a Drive folder named "Apps${X}/Glass" handed to a host
// command. systemd expands ${VAR} in a unit's command line, so it reached
// the pin helper as "Apps/Glass" — another app's data. Every argument goes
// to systemd-run with "$" doubled, which systemd reads back as written.
func TestCmdEscapesDollarForSystemd(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemd-run"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INVOCATION_ID", "test")
	cmd := Cmd(context.Background(), nil, "/usr/local/bin/fogd", "pin-bind", "--rel", "Apps${X}/Glass", "$HOME")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-- /usr/local/bin/fogd pin-bind --rel Apps$${X}/Glass $$HOME") {
		t.Fatalf("systemd-run args: %s", args)
	}
}
