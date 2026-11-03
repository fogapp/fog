// Network checks: collisions and blind spots the per-app spec validation
// cannot see, because each only exists across apps.
package issues

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// The host probes the network checks read, as seams so tests can make them
// fail.
var (
	hostname      = os.Hostname
	lookupHost    = net.DefaultResolver.LookupHost
	netInterfaces = net.Interfaces
)

// checkRouteHostCollision: two apps claiming one hostname. The proxy
// appends routes silently and the first matcher wins, so the loser is
// unreachable with no error anywhere.
func checkRouteHostCollision(_ context.Context, sn *snapshot) report {
	// The proxy's own rule (apps.RouteConflicts): fog's hostname is fog's,
	// and otherwise the oldest app to claim a name keeps it. Each route that
	// loses is one issue, naming who keeps the name — the proxy has left it
	// out, so the app is unreachable there.
	var claimants []apps.RouteClaimant
	for _, a := range sn.apps {
		if a.spec != nil {
			claimants = append(claimants, apps.RouteClaimant{ID: a.app.ID, Name: a.app.Name, Spec: a.spec})
		}
	}
	rep := report{evaluated: []int64{0}}
	for _, c := range apps.RouteConflicts(claimants, sn.baseDomain) {
		owner := c.Owner
		if c.Fog {
			owner = "fog itself"
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "route-host-collision", AppID: c.AppID, AppName: c.App, Subject: c.Host, Severity: "warning",
			Title:  fmt.Sprintf("%s can't have %s: it is %s's", c.App, c.Host, owner),
			Detail: fmt.Sprintf("%s declares a route for %s, which %s already answers on. The proxy keeps serving %s there and leaves %s's route out, so %s is unreachable at that hostname.", c.App, c.Host, owner, owner, c.App, c.App),
			Fix:    "Give " + c.App + " a hostname of its own.",
			FixURL: "/apps/" + c.App + "/config",
		})
	}
	// A route whose name is no hostname (a spec stored before the check
	// existed) is left out of the proxy too.
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		for _, r := range a.spec.Routes {
			h := apps.RouteHost(a.app.Name, r, sn.baseDomain)
			if h == "" || apps.ValidRouteHost(h) {
				continue
			}
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "route-host-collision", AppID: a.app.ID, AppName: a.app.Name, Subject: h, Severity: "warning",
				Title:  fmt.Sprintf("%s's route %q is not a hostname", a.app.Name, h),
				Detail: "The proxy leaves the route out rather than write it into its config.",
				Fix:    "Give the route a lowercase hostname.",
				FixURL: "/apps/" + a.app.Name + "/config",
			})
		}
	}
	return rep
}

// checkSpecUnreadable: an app whose stored configuration can't be read is
// left out of the proxy (one bad spec must not cost every app its routes),
// so it goes dark — said here, where someone will see it.
func checkSpecUnreadable(_ context.Context, sn *snapshot) report {
	rep := report{evaluated: []int64{0}}
	for _, a := range sn.apps {
		if !a.specUnreadable {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "spec-unreadable", AppID: a.app.ID, AppName: a.app.Name, Subject: a.app.Name, Severity: "critical",
			Title:  fmt.Sprintf("%s's configuration can't be read", a.app.Name),
			Detail: fmt.Sprintf("fog can't decode %s's newest revision, so the proxy has left its routes out and it can't be deployed.", a.app.Name),
			Fix:    "Roll the app back to an earlier revision from History, or save its configuration again.",
			FixURL: "/apps/" + a.app.Name + "/history",
		})
	}
	return rep
}

// checkWireUnsupported: an app whose env names another app in a URL fog-wire
// can't carry — https:// to an app (fog-wire speaks plain HTTP inside the
// machine), postgres://, redis:// — has no way to it: apps share no
// network, and fog-wire carries HTTP and websockets only.
func checkWireUnsupported(_ context.Context, sn *snapshot) report {
	specs := map[string]*apps.AppSpec{}
	ids := map[string]int64{}
	for _, a := range sn.apps {
		if a.spec != nil {
			specs[a.app.Name] = a.spec
			ids[a.app.Name] = a.app.ID
		}
	}
	rep := report{evaluated: []int64{0}}
	for _, ref := range apps.UnwireableRefs(specs) {
		consumer, target, _ := strings.Cut(ref, " → ")
		provider, _, _ := strings.Cut(target, " ")
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "wire-unsupported", AppID: ids[consumer], AppName: consumer, Subject: target, Severity: "warning",
			Title:  fmt.Sprintf("%s can't reach %s", consumer, target),
			Detail: fmt.Sprintf("%s's settings name %s, but apps reach each other only over plain HTTP through fog's wire proxy, so that connection can't be made.", consumer, target),
			Fix:    fmt.Sprintf("Point it at http://%s if %s speaks HTTP there; otherwise it can't be wired.", provider, provider),
			FixURL: "/apps/" + consumer + "/config",
		})
	}
	return rep
}

// checkSealedWireRefused: a sealed app whose spec names an app that is
// neither sealed nor a sealed peer. fog doesn't wire them — the wire would
// be the sealed app's way to something with a route out — so the sealed
// app can't reach it.
func checkSealedWireRefused(_ context.Context, sn *snapshot) report {
	specs := map[string]*apps.AppSpec{}
	ids := map[string]int64{}
	for _, a := range sn.apps {
		if a.spec != nil {
			specs[a.app.Name] = a.spec
			ids[a.app.Name] = a.app.ID
		}
	}
	rep := report{evaluated: []int64{0}}
	for _, w := range apps.RefusedWires(specs) {
		consumer, provider, _ := strings.Cut(w, " → ")
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "sealed-wire-refused", AppID: ids[consumer], AppName: consumer, Subject: provider, Severity: "warning",
			Title:  fmt.Sprintf("%s is sealed and can't reach %s", consumer, provider),
			Detail: fmt.Sprintf("%s names %s, which has a route out of the machine and isn't a sealed peer, so fog doesn't wire them together.", consumer, provider),
			Fix:    fmt.Sprintf("Make %s a sealed peer if %s may trust it, or remove the reference.", provider, consumer),
			FixURL: "/apps/" + provider + "/config",
		})
	}
	return rep
}

// checkHostPortCollision: two apps publishing the same host port. Spec
// validation only catches duplicates within one app; across apps the
// second deploy fails at bind time with a compose error.
func checkHostPortCollision(_ context.Context, sn *snapshot) report {
	byPort := map[string][]string{}
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		for _, p := range a.spec.Ports {
			key := fmt.Sprintf("%d/%s", p.EffectiveHostPort(), p.EffectiveProtocol())
			byPort[key] = append(byPort[key], a.app.Name)
		}
	}
	rep := report{evaluated: []int64{0}}
	for key, names := range byPort {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "host-port-collision", Subject: key, Severity: "warning",
			Title:  fmt.Sprintf("host port %s is published by %d apps", key, len(names)),
			Detail: fmt.Sprintf("%s all publish host port %s. Only one can bind it; whichever deploys second fails with a bind error.", strings.Join(names, ", "), key),
			Fix:    "Change hostPort on all but one of these apps.",
		})
	}
	return rep
}

// checkHealthUnmonitored: an app that serves traffic but declares no
// health check is invisible to the monitor loop — it can die and nothing
// notices until a human does.
func checkHealthUnmonitored(_ context.Context, sn *snapshot) report {
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		if a.spec.Health != nil || (len(a.spec.Routes) == 0 && len(a.spec.Ports) == 0) {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "health-unmonitored", AppID: a.app.ID, AppName: a.app.Name, Severity: "info",
			Title:  a.app.Name + " has no health check",
			Detail: "The app serves traffic but declares no health endpoint, so fog never monitors it — an outage is invisible until someone visits.",
			Fix:    "Add a health: {path: /up} block to the app's spec.",
		})
	}
	return rep
}

// checkMdnsCollision: another device on this network answers our .local
// name. Avahi resolves the conflict by silently renaming this box (fog →
// fog-2.local), but every banner and doc still prints the original name —
// which now leads to the other device. Common after moving the server to a
// network that already has something called "fog".
func checkMdnsCollision(ctx context.Context, _ *snapshot) report {
	host, err := hostname()
	if host == "" || err != nil {
		return report{} // can't tell, so the host stays unevaluated and keeps its issue
	}
	name := host + ".local"
	resolved, err := lookupHost(ctx, name)
	var dnsErr *net.DNSError
	if err != nil && !(errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
		// A timeout or resolver failure says nothing about who owns the
		// name; resolving on it would re-file and re-notify next sweep.
		return report{}
	}
	rep := report{evaluated: []int64{0}}
	if err != nil || len(resolved) == 0 {
		return rep // nobody answers the name at all: not a collision
	}
	local := localAddrs()
	for _, r := range resolved {
		if ip := net.ParseIP(r); ip != nil && (local[ip.String()] || ip.IsLoopback()) {
			return rep // at least one answer is us — the name is ours
		}
	}
	rep.findings = append(rep.findings, store.IssueFinding{
		Kind: "mdns-collision", Subject: name, Severity: "warning",
		Title:  name + " answers from a different device",
		Detail: fmt.Sprintf("Resolving %s returns %s, which is not one of this server's addresses. Another device on this network already owns the name, so anything printed as %s — the console banner, bookmarks — reaches that device instead.", name, strings.Join(resolved, ", "), name),
		Fix:    "Rename or remove the other device, or reach this server by the IP shown on its console/dashboard instead.",
	})
	return rep
}

// checkDockerSubnetOverlap: Docker's bridge networks default to 172.17+
// with nothing reserving them against the LAN. A house whose network lives
// inside 172.16/12 breaks container↔LAN routing in ways that look like app
// bugs. Cheap to detect from the interface table alone.
func checkDockerSubnetOverlap(_ context.Context, _ *snapshot) report {
	ifaces, err := netInterfaces()
	if err != nil {
		return report{} // can't tell; a critical issue must not resolve on a failed read
	}
	rep := report{evaluated: []int64{0}}
	type ifNet struct {
		name string
		net  *net.IPNet
	}
	var lan, docker []ifNet
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		isDocker := ifc.Name == "docker0" || strings.HasPrefix(ifc.Name, "br-")
		if strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			if isDocker {
				docker = append(docker, ifNet{ifc.Name, ipn})
			} else {
				lan = append(lan, ifNet{ifc.Name, ipn})
			}
		}
	}
	for _, l := range lan {
		for _, d := range docker {
			if l.net.Contains(d.net.IP) || d.net.Contains(l.net.IP) {
				rep.findings = append(rep.findings, store.IssueFinding{
					Kind: "docker-subnet-overlap", Subject: d.name, Severity: "critical",
					Title: "Docker's network overlaps this LAN",
					Detail: fmt.Sprintf("The Docker bridge %s (%s) overlaps the LAN on %s (%s). Containers and LAN hosts in the overlap can't reach each other — apps half-work in ways that look like app bugs. This usually surfaces after moving to a network numbered inside 172.16/12.",
						d.name, d.net.String(), l.name, l.net.String()),
					Fix: `Reserve a non-conflicting range: add {"default-address-pools":[{"base":"10.213.0.0/16","size":24}]} to /etc/docker/daemon.json, restart Docker, and redeploy apps.`,
				})
			}
		}
	}
	return rep
}

// localAddrs is the set of this host's interface IPs, normalized.
func localAddrs() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

// checkWireUnapproved: an app whose settings name another installed app in
// a URL it isn't approved to reach. fog wires only what the operator
// approved (reach, uses); an app installed after another mentions it gets
// no wire on its own — this asks.
func checkWireUnapproved(_ context.Context, sn *snapshot) report {
	specs := map[string]*apps.AppSpec{}
	ids := map[string]int64{}
	for _, a := range sn.apps {
		if a.spec != nil {
			specs[a.app.Name] = a.spec
			ids[a.app.Name] = a.app.ID
		}
	}
	rep := report{evaluated: []int64{0}}
	for _, ref := range apps.UnapprovedRefs(specs) {
		consumer, provider, _ := strings.Cut(ref, " → ")
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "wire-unapproved", AppID: ids[consumer], AppName: consumer, Subject: provider, Severity: "warning",
			Title:  fmt.Sprintf("Should %s reach %s?", consumer, provider),
			Detail: fmt.Sprintf("%s's settings name %s, but it isn't approved to reach it, so fog doesn't connect them. Approve it only if %s should be able to call %s.", consumer, provider, consumer, provider),
			Fix:    fmt.Sprintf("Add %s to %s's reach list.", provider, consumer),
			FixURL: "/apps/" + consumer + "/config",
		})
	}
	return rep
}
