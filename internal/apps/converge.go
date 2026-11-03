// Converge: the container swap itself, and the health gate it waits on.
package apps

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
)

// converge brings containers to the new spec. Strategy "swap" (default, when
// a health check exists): scale the main service to 2 with the new image,
// health-check the new replica, then drain and remove the old — near-zero
// downtime. Otherwise plain compose recreate.
func (e *Engine) converge(ctx context.Context, name string, spec *AppSpec, logBuf io.Writer) error {
	// Compose output streams into the live log as it happens (same as
	// build.go) — a long image pull shows progress, not a frozen screen. The
	// live buffer is what error handling shows the operator, so nothing is
	// captured separately.
	composeStep := func(what string, args ...string) error {
		cmd := e.Mat.ComposeCmd(ctx, name, args...)
		cmd.Stdout = logBuf
		cmd.Stderr = logBuf
		return execx.Step(ctx, convergeTimeout, what, cmd)
	}

	// Published host ports rule out a swap: the second replica would fail to
	// bind ports the old one still holds.
	canSwap := spec.DeployStrategy != "restart" && spec.Health != nil && len(spec.Ports) == 0
	if !canSwap {
		return composeStep("starting containers", "up", "-d", "--remove-orphans", "--pull", "missing")
	}

	// Identify the currently running main replica (no process/aux label), and
	// capture every main-container ID that already exists. The new replica is
	// defined by absence from this set — ListByLabel sees exited containers
	// too (a crashed replica, an old one whose rm failed), and any of those
	// matched a mere "ID != old" test. Lowest ID wins the old slot so the
	// choice doesn't ride on Docker's listing order.
	old := ""
	var oldNetworks []string
	preSwap := map[string]bool{}
	if cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name); err == nil {
		for _, c := range cs {
			if !dockerx.IsMainContainer(c) {
				continue
			}
			preSwap[c.ID] = true
			if c.State == "running" && (old == "" || c.ID < old) {
				old, oldNetworks = c.ID, servingNetworks(c)
			}
		}
	}
	if old == "" {
		// Nothing to swap; first deploy or app stopped.
		return composeStep("starting containers", "up", "-d", "--remove-orphans", "--pull", "missing")
	}

	io.WriteString(logBuf, "blue-green: starting new replica\n")
	// --no-deps: sidecars are converged after the swap (below); recreating a
	// changed sidecar here would take it down while both replicas need it.
	if err := composeStep("starting the new replica", "up", "-d", "--no-recreate", "--no-deps", "--pull", "missing",
		"--scale", name+"=2", name); err != nil {
		return err
	}

	// The new replica: the one running main container that didn't exist
	// before the scale-up. Anything other than exactly one is a fault worth
	// naming — health-checking a guess risks gating the deploy on a corpse
	// and "rolling back" the healthy replica.
	var candidates, appeared []string
	if cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name); err == nil {
		for _, c := range cs {
			if !dockerx.IsMainContainer(c) || preSwap[c.ID] {
				continue
			}
			appeared = append(appeared, fmt.Sprintf("%s (%s)", c.ID, c.State))
			if c.State == "running" {
				candidates = append(candidates, c.ID)
			}
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("expected exactly one new running replica, found %d (containers appeared since scale-up: %v)", len(candidates), appeared)
	}
	newID := candidates[0]

	if healthy := e.waitHealthy(ctx, newID, spec, 120*time.Second); !healthy {
		io.WriteString(logBuf, "new replica failed health check; rolling back\n")
		// The rollback must survive a cancelled deploy — otherwise a timeout
		// strands two replicas of the app, one of them known-bad.
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if err := e.Docker.Remove(rmCtx, newID, true); err != nil {
			e.Log.Warn("blue-green rollback: could not remove the unhealthy replica", "app", name, "container", newID, "err", err)
		}
		cancel()
		return fmt.Errorf("new replica never became healthy — old version kept running")
	}
	io.WriteString(logBuf, "new replica healthy; draining old\n")
	// Drop the old replica out of DNS first, then stop it gracefully. The
	// drain must survive a cancelled deploy — a deadline mid-sequence would
	// strand the old replica invisible-but-running, or leave a stopped corpse
	// for the next swap to trip over.
	drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancelDrain()
	for _, network := range oldNetworks {
		e.Docker.NetworkDisconnect(drainCtx, network, old, false)
	}
	if err := e.Docker.Stop(drainCtx, old, 30); err != nil {
		e.Log.Warn("blue-green drain: stop failed, old replica may still be running", "app", name, "container", old, "err", err)
	}
	if err := e.Docker.Remove(drainCtx, old, false); err != nil {
		e.Log.Warn("blue-green drain: rm failed, old replica left behind", "app", name, "container", old, "err", err)
	}

	// Recreate/refresh worker processes and prune leftovers.
	return composeStep("converging worker processes", "up", "-d", "--remove-orphans")
}

// servingNetworks are the networks of fog's own that c answers on. A swap asks
// the replica what it's on rather than the spec: the deploy that seals an app
// swaps out a replica still on the fog network.
func servingNetworks(c dockerx.ContainerSummary) []string {
	names := make([]string, 0, len(c.NetworkSettings.Networks))
	for n := range c.NetworkSettings.Networks {
		names = append(names, n)
	}
	return ownNetworks(names)
}

// checkAuxNames rejects a sidecar named like the app itself — the two would
// collide as compose service keys.
func checkAuxNames(name string, spec *AppSpec) error {
	for _, a := range spec.Aux {
		if a.Name == name {
			return fmt.Errorf("aux service %q collides with the app name", a.Name)
		}
	}
	return nil
}

// MainIP is where fog reaches app's running main container among containers
// (a /containers/json listing, which carries addresses): its fog-network
// address, else its sealed-network one; "" when it has neither.
func MainIP(containers []dockerx.ContainerSummary, app string) string {
	return mainIPIn(dockerx.MainContainers(containers), app)
}

// mainIPIn is MainIP over an already-built MainContainers map, for a caller
// asking about every app in one listing.
func mainIPIn(mains map[string]dockerx.ContainerSummary, app string) string {
	c, ok := mains[app]
	if !ok || c.State != "running" {
		return ""
	}
	return ownNetworkIP(c.NetworkSettings.Networks, app)
}

// sealedBreaches names the running containers of a sealed app that could
// reach out regardless of what compose was asked for: on the fog network, or
// on any network that isn't internal. A failed read of a network counts, since
// the boundary is only as good as what fog actually saw.
func sealedBreaches(containers []dockerx.ContainerSummary, internal func(network string) (bool, error)) []string {
	var out []string
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		for network := range c.NetworkSettings.Networks {
			ok, err := internal(network)
			switch {
			case err != nil:
				out = append(out, fmt.Sprintf("%s on %s: %v", c.ID, network, err))
			case !ok:
				out = append(out, fmt.Sprintf("%s on %s, which has a route out", c.ID, network))
			}
		}
	}
	sort.Strings(out)
	return out
}

// verifySealed checks a sealed app's containers landed where its spec says.
func (e *Engine) verifySealed(ctx context.Context, name string) error {
	cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name)
	if err != nil {
		return fmt.Errorf("checking the sealed network: %w", err)
	}
	breaches := sealedBreaches(cs, func(network string) (bool, error) { return e.Docker.NetworkInternal(ctx, network) })
	if len(breaches) > 0 {
		return fmt.Errorf("%s is sealed but isn't: %s", name, strings.Join(breaches, "; "))
	}
	return nil
}

// waitHealthy polls the container's health endpoint via its address on the
// network fog reaches it by.
// A cancelled deploy ends the wait at once rather than at the next poll.
func (e *Engine) waitHealthy(ctx context.Context, id string, spec *AppSpec, timeout time.Duration) bool {
	port := spec.HealthPort()
	if port == 0 {
		return false
	}
	// sleep waits d, and reports false when ctx ended first.
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		d, err := e.Docker.Inspect(ctx, id)
		if err == nil && d.State.Running {
			eps := map[string]dockerx.NetworkEndpoint{}
			for n, ep := range d.NetworkSettings.Networks {
				eps[n] = dockerx.NetworkEndpoint{IPAddress: ep.IPAddress}
			}
			ip := ownNetworkIP(eps, "")
			if ip != "" {
				if dockerx.ProbeHTTP(ctx, fmt.Sprintf("http://%s:%d%s", ip, port, spec.Health.Path)) {
					return true
				}
				if e.DevPermissiveProbe {
					// macOS dev: container IPs aren't host-routable. Accept a
					// running container after it has had a moment to boot.
					if !sleep(3 * time.Second) {
						return false
					}
					if d2, err := e.Docker.Inspect(ctx, id); err == nil && d2.State.Running {
						return true
					}
				}
			}
		}
		if !sleep(2 * time.Second) {
			return false
		}
	}
	return false
}
