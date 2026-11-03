// ExecInApp: short read-only probes inside an app's main container, for the
// issues scanner. Deliberately not a general exec facility — the terminal
// handler owns interactive sessions; this exists so scans can ask questions
// like "what pg_dump does this image carry" without a contract endpoint.
package apps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
)

// ExecInApp runs a command in the app's main container and returns combined
// output. It takes no app lock — callers must check Deploying(name) first
// and skip; a probe must never queue behind a deploy, and racing one would
// probe a half-swapped container anyway.
func (e *Engine) ExecInApp(ctx context.Context, name string, timeout time.Duration, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("no command")
	}
	c, err := e.Docker.ProcessContainer(ctx, name, "")
	if err != nil && !errors.Is(err, dockerx.ErrNoContainer) && !errors.Is(err, dockerx.ErrNoProcess) {
		return "", err
	}
	if err != nil || c.State != "running" {
		return "", fmt.Errorf("%s has no running main container", name)
	}
	id := c.ID
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := execx.Output(ctx, "docker", append([]string{"exec", id}, argv...)...)
	if err != nil {
		return "", fmt.Errorf("exec in %s: %w", name, err)
	}
	return out, nil
}
