//go:build !darwin && !(linux && (amd64 || arm64))

package computenode

import (
	"errors"
	"os/exec"
	"runtime"
)

// Elsewhere fog-node has no way to confine a run: it lends no video unless
// its owner turns confinement off.

const (
	helperListens = false
	helperPort    = 0
)

func confinementKind() (string, error) {
	return "", errors.New("fog-node can't confine ffmpeg on " + runtime.GOOS + "/" + runtime.GOARCH)
}

func prepareScratch(string) error { return nil }

func (r *Runner) startHelper(cmd *exec.Cmd, _ *jail, _ *proc) error { return cmd.Start() }

func confine(j *jail) error {
	if j.Confine {
		return errors.New("fog-node can't confine ffmpeg here")
	}
	return errors.New("fog-node can't run ffmpeg here")
}

// runAs is Linux's: elsewhere ffmpeg runs as fog-node's own user.
type runAs struct{}

func sandboxUser() *runAs { return nil }

func restrictedUserns() bool { return false }
