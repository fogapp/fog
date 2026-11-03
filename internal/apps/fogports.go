package apps

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/fogports"
)

// fogPortHolders lists the app containers holding a host port fog holds
// (fogports), by app, with the ports each holds. Docker starts containers
// before fogd at boot, so an app published on one — a spec from before
// fogPortErr, a container made by hand under an app's labels — takes it
// before fog's own service can, and answers in its place.
func fogPortHolders(cs []dockerx.ContainerSummary, fogd int) map[string][]string {
	out := map[string][]string{}
	for _, c := range cs {
		if c.State != "running" || dockerx.FogSystem(c) != "" {
			continue
		}
		app := c.Labels["fog.app"]
		if app == "" {
			continue
		}
		for _, p := range c.Ports {
			if p.PublicPort == 0 || fogports.Owner(p.PublicPort, fogd) == "" {
				continue
			}
			held := fmt.Sprintf("%d/%s", p.PublicPort, p.Type)
			if !slices.Contains(out[app], held) {
				out[app] = append(out[app], held)
			}
		}
	}
	return out
}

// ReclaimFogPorts stops every app holding a port fog holds and leaves it
// stopped, with a notice saying why — before fog brings its own services
// up at boot (OnChange), and every minute after (NetworkLoop). Reports
// whether it stopped anything, so the caller can bring fog's services back
// up on the ports now free.
func (e *Engine) ReclaimFogPorts(ctx context.Context) bool {
	if e.Docker == nil {
		return false
	}
	cs, err := e.Docker.ListByLabel(ctx, "fog.app")
	if err != nil {
		e.Log.Warn("checking fog's ports", "err", err)
		return false
	}
	holders := fogPortHolders(cs, e.FogdPort())
	if len(holders) == 0 {
		return false
	}
	stopped := false
	for app, ports := range holders {
		for _, c := range cs {
			if c.Labels["fog.app"] != app || c.State != "running" || dockerx.FogSystem(c) != "" {
				continue
			}
			// Every container of the app, not only the one holding the
			// port: the app is stopped as a whole, as its desired state
			// says, and nothing of it restarts the holder.
			if err := e.Docker.Stop(ctx, c.ID, 10); err != nil {
				e.Log.Warn("stopping an app holding fog's port", "app", app, "container", c.ID, "err", err)
				continue
			}
			stopped = true
		}
		if a, err := e.Store.AppByName(app); err == nil {
			e.Store.SetAppDesiredState(a.ID, "stopped")
		}
		e.Store.Audit("fog", "app.stop", app, fmt.Sprintf(`{"reason":"held fog's port %s"}`, strings.Join(ports, ", ")))
		e.Store.CreateNotice("fog-port-taken", app,
			"fog stopped "+app+": it held "+plural(len(ports), "a port", "ports")+" of fog's own",
			fmt.Sprintf("%s was published on host %s %s, which fog's own services listen on. "+
				"Docker starts apps before fog at boot, so it could take the port first and answer in fog's place. "+
				"fog stopped it. Change its published ports, then start it again.",
				app, plural(len(ports), "port", "ports"), strings.Join(ports, ", ")))
		e.Log.Warn("stopped an app holding fog's port", "app", app, "ports", ports)
	}
	return stopped
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
