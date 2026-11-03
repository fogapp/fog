package files

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func swapInstaller(t *testing.T, f func(context.Context) error) {
	t.Helper()
	old := installFFmpeg
	installFFmpeg = f
	t.Cleanup(func() { installFFmpeg = old })
}

// The install is opt-in: a test manager, a dev machine, must never reach
// apt-get.
func TestEnsureFFmpegOffByDefault(t *testing.T) {
	m := newManager(t)
	swapInstaller(t, func(context.Context) error {
		t.Error("installer ran with InstallTools off")
		return nil
	})
	m.ensureFFmpeg(context.Background())
}

// On a Linux box without ffmpeg, the install runs once, and the lookup
// forgets its miss so the new binary is seen straight away.
func TestEnsureFFmpegInstallsWhenMissing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host installs are Linux-only")
	}
	if haveFFmpeg() != "" {
		t.Skip("host already has ffmpeg")
	}
	m := newManager(t)
	m.InstallTools = true
	calls := 0
	swapInstaller(t, func(context.Context) error {
		calls++
		return nil
	})
	m.ensureFFmpeg(context.Background())
	if calls != 1 {
		t.Fatalf("installer ran %d times, want 1", calls)
	}
	ffmpegMu.Lock()
	checked := ffmpegChecked
	ffmpegMu.Unlock()
	if time.Since(checked) > time.Second {
		t.Errorf("lookup was not retried after the install")
	}
}
