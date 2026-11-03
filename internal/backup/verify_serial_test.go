package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The scheduled and offsite verifiers share fog_verify_<app>; a second
// test-restore must wait for the first rather than drop the scratch
// database out from under it.
func TestTestRestoreWaitsForTheOneRunning(t *testing.T) {
	m := &Manager{DataDir: t.TempDir()}
	m.verifyMu.Lock() // a test-restore already running

	done := make(chan error, 1)
	go func() {
		done <- m.TestRestore(context.Background(), "stream", filepath.Join(t.TempDir(), "missing.tar"), "")
	}()
	select {
	case err := <-done:
		t.Fatalf("second test-restore ran alongside the first: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	m.verifyMu.Unlock()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "artifact unreadable") {
			t.Fatalf("want artifact unreadable once it ran, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second test-restore never ran after the first finished")
	}
}
