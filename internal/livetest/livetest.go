// Package livetest serialises tests that use a real Docker daemon's
// fog-wide names — the "fog" network, a container named "caddy" — across
// the test binaries go test runs side by side.
package livetest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Lock holds the machine-wide lock for the rest of t.
func Lock(t testing.TB) {
	t.Helper()
	path := filepath.Join(os.TempDir(), "fog-live-docker.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		// Made by root's run, and not writable: a lock needs only reading.
		f, err = os.Open(path)
	} else {
		f.Chmod(0o666) // so a later run as another user can open it too
	}
	if err != nil {
		t.Skipf("live docker lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Skipf("live docker lock: %v", err)
	}
	t.Cleanup(func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() })
}
