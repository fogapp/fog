package apps

// Route hostnames are one namespace across every app and fog itself. Two
// claims on one name used to reach the Caddyfile as two blocks for it, which
// Caddy refuses — and a refused Caddyfile is every app's routes failing to
// apply, not just the two that clashed. A route could also claim fog's own
// hostname and stand in front of the console.
//
// The rule, everywhere it is asked: fog's own hostname is fog's; otherwise
// the first app to claim a name (the oldest, by id) keeps it. A save that
// would make a new claim on a taken name is refused; a deploy of the app
// that would lose is refused; and the proxy, meeting a clash some older
// state left behind, routes the name to its owner and leaves the loser out,
// where the issues sweep says so.

import (
	"fmt"
	"sort"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// FogRouteLabel is the label of fog's own hostname, fog.<base domain>.
const FogRouteLabel = "fog"

// RouteClaimant is one app's say in the namespace.
type RouteClaimant struct {
	ID   int64
	Name string
	Spec *AppSpec
}

// RouteConflict is one route that loses its hostname: to fog itself (Owner
// "" and Fog true) or to an older app.
type RouteConflict struct {
	AppID   int64
	App     string
	Host    string // the name as it resolves, or as written when no domain is set
	Owner   string // the app that keeps it, "" when it is fog's
	OwnerID int64
	Fog     bool
}

func (c RouteConflict) Error() string {
	if c.Fog {
		return fmt.Sprintf("the hostname %s is fog's own — pick another for %s", c.Host, c.App)
	}
	return fmt.Sprintf("the hostname %s is already %s's — pick another for %s", c.Host, c.Owner, c.App)
}

// routeHostKey is the name a route answers on, in a form comparable whether
// or not a base domain is set yet: a label (or the app-name default) is
// <label>.<domain>, and with no domain it is keyed as the label under a
// placeholder no hostname can equal.
func routeHostKey(app string, r Route, domain string) string {
	if r.Host != "" && strings.Contains(r.Host, ".") {
		return strings.ToLower(strings.TrimSuffix(r.Host, "."))
	}
	label := r.Host
	if label == "" {
		label = app
	}
	if domain == "" {
		return strings.ToLower(label) + ".<base domain>"
	}
	return strings.ToLower(label + "." + domain)
}

func fogHostKey(domain string) string {
	return routeHostKey("", Route{Host: FogRouteLabel}, domain)
}

// RouteConflicts lists every route that loses its hostname, fog first, then
// by the oldest claimant. A dotted host equal to a label's resolution clashes
// once the domain makes them equal. Deterministic: the same inputs give the
// same owners, so the proxy, deploys and the sweep never disagree.
func RouteConflicts(claimants []RouteClaimant, domain string) []RouteConflict {
	sorted := append([]RouteClaimant(nil), claimants...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	type owner struct {
		id   int64
		name string
	}
	owners := map[string]owner{fogHostKey(domain): {}}
	var out []RouteConflict
	for _, c := range sorted {
		if c.Spec == nil {
			continue
		}
		mine := map[string]bool{}
		for _, r := range c.Spec.Routes {
			key := routeHostKey(c.Name, r, domain)
			if mine[key] {
				continue // two routes of one app on one name: Validate's to refuse
			}
			o, taken := owners[key]
			switch {
			case taken && o.name == "":
				out = append(out, RouteConflict{AppID: c.ID, App: c.Name, Host: key, Fog: true})
			case taken:
				out = append(out, RouteConflict{AppID: c.ID, App: c.Name, Host: key, Owner: o.name, OwnerID: o.id})
			default:
				owners[key] = owner{c.ID, c.Name}
				mine[key] = true
			}
		}
	}
	return out
}

// LosesRoute reports whether app id's route r lost its hostname, given the
// conflicts RouteConflicts found.
func LosesRoute(conflicts []RouteConflict, id int64, app string, r Route, domain string) bool {
	key := routeHostKey(app, r, domain)
	for _, c := range conflicts {
		if c.AppID == id && c.Host == key {
			return true
		}
	}
	return false
}

// checkRouteClaims answers whether spec may be saved (or deployed) as app
// name's: none of its routes may claim fog's hostname, or a name another app
// holds. At save, a name this app already held in its newest spec is not a
// new claim, so an old clash never blocks an unrelated edit of the app that
// owns the name; at deploy (prior nil) only losing to an older app or to fog
// fails.
func (e *Engine) checkRouteClaims(name string, spec *AppSpec, atSave bool) error {
	if e.Store == nil || len(spec.Routes) == 0 {
		return nil
	}
	domain, _ := e.Store.GetSetting(store.SettingBaseDomain)
	list, err := WithSpecs(e.Store)
	if err != nil {
		return err
	}
	var self int64 = -1
	held := map[string]bool{}
	others := make([]RouteClaimant, 0, len(list))
	for _, a := range list {
		if a.Name == name {
			self = a.ID
			if a.Spec != nil {
				for _, r := range a.Spec.Routes {
					held[routeHostKey(name, r, domain)] = true
				}
			}
			continue
		}
		others = append(others, RouteClaimant{ID: a.ID, Name: a.Name, Spec: a.Spec})
	}
	fog := fogHostKey(domain)
	for _, r := range spec.Routes {
		key := routeHostKey(name, r, domain)
		if key == fog {
			return RouteConflict{App: name, Host: key, Fog: true}
		}
		for _, o := range others {
			if o.Spec == nil {
				continue
			}
			for _, or := range o.Spec.Routes {
				if routeHostKey(o.Name, or, domain) != key {
					continue
				}
				olderOwner := self < 0 || o.ID < self
				if atSave && (olderOwner || !held[key]) {
					return RouteConflict{App: name, Host: key, Owner: o.Name, OwnerID: o.ID}
				}
				if !atSave && olderOwner {
					return RouteConflict{App: name, Host: key, Owner: o.Name, OwnerID: o.ID}
				}
			}
		}
	}
	return nil
}

// ValidRouteHost reports whether host is a lowercase DNS hostname, the only
// thing a route's name may be when it reaches the Caddyfile.
func ValidRouteHost(host string) bool {
	return len(host) <= 253 && hostRe.MatchString(host)
}

// checkServiceNames refuses names that would let one app answer as another
// on a network: a worker's compose service "<app>-<process>" is a DNS name
// wherever the worker is, and fog's own proxies used to reach an app by its
// bare name — app foo's worker "bar" captured app foo-bar's traffic, gate
// headers and all. The proxies now use a name no app can take
// (AppUpstream), and no app may make a service name that is another app's
// name or one of fog's own, nor be named like another app's service.
func (e *Engine) checkServiceNames(name string, spec *AppSpec) error {
	if e.Store == nil {
		return nil
	}
	list, err := WithSpecs(e.Store)
	if err != nil {
		return err
	}
	apps := map[string]bool{}
	for _, a := range list {
		if a.Name != name {
			apps[a.Name] = true
		}
	}
	for _, p := range spec.Processes {
		svc := name + "-" + p.Name
		if apps[svc] {
			return fmt.Errorf("process %s would be named %s on the network, which is another app's name", p.Name, svc)
		}
		if reserved[svc] || SurfaceGrant(svc) {
			return fmt.Errorf("process %s would be named %s on the network, which is fog's", p.Name, svc)
		}
	}
	for _, a := range list {
		if a.Name == name || a.Spec == nil {
			continue
		}
		for _, p := range a.Spec.Processes {
			if a.Name+"-"+p.Name == name {
				return fmt.Errorf("%s's process %s is already named %s on the network", a.Name, p.Name, name)
			}
		}
	}
	return nil
}
