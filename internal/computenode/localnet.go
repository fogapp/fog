package computenode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// macOS's Local Network privacy (macOS 15 and later): a background process
// may not reach a device on the LAN until its owner allows it, and until
// then its connections fail as if there were no route. The answer to the
// prompt only reaches processes started after it, so fog-node, run by
// launchd, exits for launchd to start it again: a fresh process picks up a
// permission just given, without the owner having to know to restart it.

// localNetworkFile records when fog-node was first refused, so restarts
// slow down while access stays refused, and the first poll that gets
// through can say it was allowed.
const localNetworkFile = "local-network-refused"

// Restart pacing: quickly at first, while the owner is likely still
// looking at the prompt, then every half minute, so a "Don't Allow" never
// spins.
const (
	localNetworkQuick   = 3 * time.Second
	localNetworkPatient = 30 * time.Second
	localNetworkWindow  = 2 * time.Minute
)

// localNetworkRefused reports a connection macOS refused for Local Network
// privacy: no route to host, on a Mac. (A machine truly unreachable says
// the same, and restarting costs it nothing.)
func localNetworkRefused(err error) bool {
	return runtime.GOOS == "darwin" && errors.Is(err, syscall.EHOSTUNREACH)
}

// awaitLocalNetwork says what to allow and where, waits a while, and
// answers true for the caller to exit so launchd starts fog-node afresh.
func (a *Agent) awaitLocalNetwork(ctx context.Context) bool {
	if a.Exit == nil {
		// Run by hand from a terminal, fog-node has the terminal's access;
		// there's no service manager to start it again.
		return false
	}
	path := filepath.Join(a.Dir, localNetworkFile)
	first := time.Now()
	if info, err := os.Stat(path); err == nil {
		first = info.ModTime()
	} else {
		_ = os.WriteFile(path, nil, 0o600)
	}
	wait := localNetworkQuick
	if time.Since(first) > localNetworkWindow {
		wait = localNetworkPatient
	}
	a.Log.Warn("macOS hasn't allowed fog-node on the local network, so it can't reach fog. "+
		"Allow it in System Settings › Privacy & Security › Local Network (or answer the prompt): "+
		"fog-node restarts by itself to pick the permission up.", "fog", a.Config.Fog, "restarting in", wait)
	select {
	case <-ctx.Done():
		return false
	case <-a.after(wait):
	}
	return true
}

// localNetworkAllowed notes a poll that got through after a refusal.
func (a *Agent) localNetworkAllowed() {
	if err := os.Remove(filepath.Join(a.Dir, localNetworkFile)); err == nil {
		a.Log.Info("macOS allows fog-node on the local network now; connected to fog", "fog", a.Config.Fog)
	}
}

func (a *Agent) after(d time.Duration) <-chan time.Time {
	if a.wait != nil {
		return a.wait(d)
	}
	return time.After(d)
}
