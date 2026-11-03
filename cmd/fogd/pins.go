package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/hostpkg"
)

// pinFuncs are the materializer's pin and unpin (internal/apps/pin.go): on
// Linux as root, `fogd pin-bind` run in the host's mount namespace — fogd's
// own is private, and a mount made there would be invisible to Docker. A
// development fogd that can't mount (macOS, or not root) gets nil, and fog
// binds paths as they are and refuses nested writable volumes.
func pinFuncs(dataDir string, dev bool) (apps.PinFunc, apps.UnpinFunc) {
	if runtime.GOOS != "linux" || (dev && os.Geteuid() != 0) {
		return nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil
	}
	m := &apps.Materializer{DataDir: dataDir}
	binds := m.BindsDir()
	run := func(ctx context.Context, args []string) error {
		out, err := hostpkg.Run(ctx, nil, exe, args...)
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "pin-bind:"))
		var exit *exec.ExitError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &exit) && exit.ExitCode() == apps.PinExitWarning:
			return &apps.PinWarning{Msg: strings.TrimSpace(strings.TrimPrefix(msg, "warning:"))}
		case errors.As(err, &exit) && exit.ExitCode() == apps.PinExitNotFound:
			// Not there yet (a share still mounting): the app waits for
			// it, where any other failure refuses it.
			return fmt.Errorf("%s: %w", msg, fs.ErrNotExist)
		case msg != "":
			return errors.New(msg)
		}
		return fmt.Errorf("pin-bind: %w", err)
	}
	pin := func(ctx context.Context, root, rel, target, owner string) error {
		return run(ctx, apps.PinArgs(binds, root, rel, target, owner))
	}
	unpin := func(ctx context.Context, target string, keep ...string) error {
		return run(ctx, apps.UnpinArgs(binds, target, keep))
	}
	return pin, unpin
}

// newMaterializer is the engine's materializer, pinning where it can.
func newMaterializer(dataDir string, dev bool) *apps.Materializer {
	m := &apps.Materializer{DataDir: dataDir}
	m.Pin, m.Unpin = pinFuncs(dataDir, dev)
	return m
}
