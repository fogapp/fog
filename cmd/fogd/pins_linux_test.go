//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fogd pins bind sources from inside its systemd sandbox, whose mount
// namespace is private: a mount made there would be invisible to Docker.
// pinFuncs runs the helper through systemd-run instead. This runs that
// path for real — under a unit sandboxed as fogd.service is, pinning a
// folder, and Docker (in the host's namespace) binding the pin.
//
// Run as root on a systemd host with Docker, inside such a unit:
//
//	GOOS=linux go test -c -o /usr/local/bin/fogd.test ./cmd/fogd
//	systemd-run --wait --pipe -p ProtectSystem=full -p ProtectHome=read-only \
//	  -p PrivateTmp=yes -p ReadWritePaths=/var/lib \
//	  /usr/local/bin/fogd.test -test.run PinThroughSystemd -test.v
func TestPinThroughSystemd(t *testing.T) {
	if os.Getenv("INVOCATION_ID") == "" || os.Geteuid() != 0 {
		t.Skip("run as root inside a systemd unit (see the comment)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// /var/lib, as fogd's own data: PrivateTmp makes /var/tmp the unit's alone.
	base, err := os.MkdirTemp("/var/lib", "fogpinsd")
	if err != nil {
		t.Fatal(err)
	}
	drive := filepath.Join(base, "drive")
	os.MkdirAll(filepath.Join(drive, "Content", "Movies"), 0o755)
	os.WriteFile(filepath.Join(drive, "Content", "Movies", "movie.mkv"), []byte("m"), 0o644)

	pin, unpin := pinFuncs(filepath.Join(base, "data"), false)
	if pin == nil {
		t.Fatal("no pinning on Linux as root")
	}
	binds := filepath.Join(base, "data", "binds")
	target := filepath.Join(binds, "stream", "movies")
	t.Cleanup(func() {
		unpin(context.Background(), filepath.Join(binds, "stream"))
		exec.Command("systemd-run", "--quiet", "--wait", "--collect", "--", "umount", "-l", binds).Run()
		os.RemoveAll(base)
	})
	if err := pin(ctx, drive, "Content/Movies", target, ""); err != nil {
		t.Fatal(err)
	}
	// Docker runs in the host's namespace: it binds the pin only if the
	// mount was made there.
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--mount", "type=bind,source="+target+",target=/data",
		"caddy:2.10-alpine", "ls", "/data").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "movie.mkv") {
		t.Fatalf("docker binds %q (%v): the pin isn't visible in the host's namespace", out, err)
	}
	// The attack: a Drive folder literally named "Apps${X}", beside Lucid's
	// own Apps/Lucid. systemd used to expand ${X} on the helper's command
	// line and pin Apps/Lucid — Lucid's data — for the app that named the
	// other folder. The pin must be the folder named.
	os.MkdirAll(filepath.Join(drive, "Apps", "Lucid"), 0o755)
	os.WriteFile(filepath.Join(drive, "Apps", "Lucid", "lucid-secret"), []byte("s"), 0o600)
	os.MkdirAll(filepath.Join(drive, "Apps${X}", "Lucid"), 0o755)
	os.WriteFile(filepath.Join(drive, "Apps${X}", "Lucid", "own-file"), []byte("o"), 0o644)
	evil := filepath.Join(binds, "evil", "data")
	if err := pin(ctx, drive, "Apps${X}/Lucid", evil, ""); err != nil {
		t.Fatal(err)
	}
	out, err = exec.CommandContext(ctx, "docker", "run", "--rm", "--mount", "type=bind,source="+evil+",target=/data",
		"caddy:2.10-alpine", "ls", "/data").CombinedOutput()
	if err != nil || strings.Contains(string(out), "lucid-secret") || !strings.Contains(string(out), "own-file") {
		t.Fatalf("pinned %q for Apps${X}/Lucid (%v)", out, err)
	}
	unpin(ctx, filepath.Join(binds, "evil"))

	// And the helper refuses a link the same way through systemd-run.
	os.Rename(filepath.Join(drive, "Content", "Movies"), filepath.Join(drive, "Content", "Movies.old"))
	os.Symlink("/", filepath.Join(drive, "Content", "Movies"))
	if err := pin(ctx, drive, "Content/Movies", filepath.Join(binds, "stream", "again"), ""); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("pin through a link: %v", err)
	}
	// Unpinning goes the same way.
	if err := unpin(ctx, target); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--mount", "type=bind,source="+target+",target=/data",
		"caddy:2.10-alpine", "ls", "/data").CombinedOutput(); err == nil {
		t.Fatalf("the pin is still there after unpinning: %q", out)
	}
}
