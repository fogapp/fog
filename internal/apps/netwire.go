package apps

// Network isolation (docs/sealed-network.md, "Isolation"). Every app used to
// sit on one shared bridge, where any container could reach any other app
// directly — around the proxy, and so around the sign-in gate: one app could
// send a gated app "Remote-User: <anyone>" and be believed.
//
// Now each app has a network of its own, which only its own containers and
// fog's own containers join: the proxy, Postgres (for an app that needs
// it), and the wire proxy. No two apps share a network.
//
// An app reaches another only over a wire, and only through fog-wire, a
// Caddy of fog's own with no published port. It answers to the provider's
// name on the consumer's network, accepts a request only from that
// consumer's subnet, strips every spelling of the identity headers, and
// forwards to the provider. So a wire runs one way: the consumer reaches the
// provider, and the provider — which never has a path into the consumer's
// network — reaches nothing of the consumer's. A provider can't send a
// consumer forged identity headers, and a contract a repo declares only
// ever makes that app reachable, never gives it reach.
//
// An app is wired to another (it "reaches" it) when its spec says so: the
// other app is in its uses list, or one of its env values is an http URL
// naming the other app ("http://glass", "http://gotenberg:3000" — which is
// also how a connected integration's {{url}} lands); an app the operator
// granted "home" reaches the apps that serve widgets, for their pictures. A
// sealed app is wired only to sealed apps and sealed peers, as before.
//
// fog reaches each app by "<app>.fog-app", an alias only fog sets: a name
// with a dot, which no service, container or app name can be, so no app can
// answer in another's place (a worker "foo-bar" of app foo shadowed app
// foo-bar's own name).
//
// Compose declares an app's own network; ReconcileNetworks keeps running
// containers — the proxy, the database, the wire proxy — in line with what
// the specs say, and takes the apps off the old shared networks and the old
// per-wire ones.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	appNetPrefix    = "fog-app-"
	sealedNetPrefix = "fog-sealed-"
	linkNetPrefix   = "fog-link-"
	// An app's sidecar network: its main and workers and its sidecars, and
	// nothing else. Two prefixes, not a suffix, so no app name can make one
	// read as the other.
	auxNetPrefix       = "fog-aux-"
	sealedAuxNetPrefix = "fog-auxsealed-"
	// netLabel marks the networks fog made for isolation: "app" or "link".
	netLabel = "fog.net"
)

// OwnNetwork is the app's own network: internal for a sealed app.
func OwnNetwork(name string, spec *AppSpec) string {
	if spec != nil && spec.Sealed() {
		return sealedNetPrefix + name
	}
	return appNetPrefix + name
}

// AuxNetwork is the app's sidecar network: internal for a sealed app.
func AuxNetwork(name string, spec *AppSpec) string {
	if spec != nil && spec.Sealed() {
		return sealedAuxNetPrefix + name
	}
	return auxNetPrefix + name
}

// AppAliasSuffix makes an app's fog-only name: "<app>.fog-app" on its own
// network. fog's proxies reach an app by it and by nothing else.
const AppAliasSuffix = ".fog-app"

// AppUpstream is the name fog's proxies reach app by.
func AppUpstream(app string) string { return app + AppAliasSuffix }

// isolationNetwork reports whether a network is one of fog's per-app ones
// (or one of the per-wire networks fog used to make).
func isolationNetwork(n string) bool {
	return strings.HasPrefix(n, appNetPrefix) || strings.HasPrefix(n, sealedNetPrefix) || strings.HasPrefix(n, linkNetPrefix)
}

// legacyShared reports whether a network is one of the old shared ones apps
// used to sit on together.
func legacyShared(n string) bool { return n == NetworkName || n == SealedNetworkName }

// envURLRe finds each URL in an env value: its scheme, then (after any
// userinfo) a single-label host — an app name has no dots — and its port.
var envURLRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://(?:[^@/\s]*@)?([a-z0-9-]+)(?::([0-9]{1,5}))?(?:[/?#\s"']|$)`)

// approved is the providers the operator approved this app reaching: its
// reach list and the apps in its uses.
func (s *AppSpec) approved() map[string]bool {
	out := map[string]bool{}
	for _, r := range s.Reach {
		out[strings.ToLower(r)] = true
	}
	for _, u := range s.Uses {
		if !SurfaceGrant(u) {
			out[strings.ToLower(u)] = true
		}
	}
	return out
}

// wiresOf answers the installed apps this spec reaches and the container
// ports it reaches each on. Only an approved provider (reach, uses) is
// ever reached — an env URL naming any other app wires nothing, so an app
// installed later is never wired to one that merely mentions its name. The
// ports are those its http:// (or ws://) env URLs name, else its connection
// port, else its route ports. installed answers an installed app's spec.
func (s *AppSpec) wiresOf(self string, installed func(string) *AppSpec) map[string][]int {
	ok := s.approved()
	out := map[string][]int{}
	add := func(n string, port int) {
		n = strings.ToLower(n)
		if !ok[n] || n == self || installed(n) == nil || port < 1 || port > 65535 {
			return
		}
		if !slices.Contains(out[n], port) {
			out[n] = append(out[n], port)
		}
	}
	for _, v := range s.Env {
		for _, m := range envURLRe.FindAllStringSubmatch(v, -1) {
			scheme := strings.ToLower(m[1])
			if scheme != "http" && scheme != "ws" {
				continue
			}
			port := 80
			if m[3] != "" {
				port, _ = strconv.Atoi(m[3])
			}
			add(m[2], port)
		}
	}
	for n := range ok {
		if _, has := out[n]; has {
			continue
		}
		p := installed(n)
		if p == nil {
			continue
		}
		if p.Connection != nil && p.Connection.Port > 0 {
			add(n, p.Connection.Port)
			continue
		}
		for _, r := range p.Routes {
			add(n, r.Port)
		}
	}
	for n := range out {
		slices.Sort(out[n])
	}
	return out
}

// unapproved lists the installed apps an http(s) env URL names that the
// operator hasn't approved this app reaching — for the issues sweep, which
// asks rather than wires.
func (s *AppSpec) unapproved(self string, installed func(string) *AppSpec) []string {
	ok := s.approved()
	seen := map[string]bool{}
	for _, v := range s.Env {
		for _, m := range envURLRe.FindAllStringSubmatch(v, -1) {
			n := strings.ToLower(m[2])
			if n != self && !ok[n] && !SurfaceGrant(n) && installed(n) != nil {
				seen[n] = true
			}
		}
	}
	return sortedKeys(seen)
}

// UnapprovedRefs lists, per app, the installed apps its env names that it
// isn't approved to reach, as "consumer → provider".
func UnapprovedRefs(specs map[string]*AppSpec) []string {
	var out []string
	for _, n := range sortedKeys(specs) {
		if s := specs[n]; s != nil {
			for _, p := range s.unapproved(n, func(x string) *AppSpec { return specs[x] }) {
				out = append(out, n+" → "+p)
			}
		}
	}
	return out
}

// GrandfatherReach is the one-time step for specs saved before reach
// existed: every installed app an http(s) env URL names now — which the
// operator's own configuration has been wiring all along — is approved.
// Answers whether it added anything. Apps installed later are never
// approved this way.
func (s *AppSpec) GrandfatherReach(self string, installed func(string) bool) bool {
	inst := func(n string) *AppSpec {
		if installed(n) {
			return &AppSpec{}
		}
		return nil
	}
	added := false
	for _, n := range s.unapproved(self, inst) {
		s.Reach = append(s.Reach, n)
		added = true
	}
	return added
}

// unwireable lists env URLs naming an installed app in a scheme fog-wire
// can't carry (https to an app, postgres://, redis://), as
// "provider (scheme://)". The app won't reach it: said, not left to fail.
func (s *AppSpec) unwireable(self string, installed func(string) *AppSpec) []string {
	seen := map[string]bool{}
	for _, v := range s.Env {
		for _, m := range envURLRe.FindAllStringSubmatch(v, -1) {
			scheme, host := strings.ToLower(m[1]), strings.ToLower(m[2])
			if scheme == "http" || scheme == "ws" || host == self || SurfaceGrant(host) || installed(host) == nil {
				continue
			}
			seen[host+" ("+scheme+"://)"] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UnwireableRefs lists, per app, the references fog-wire can't carry, as
// "consumer → provider (scheme://)" — for the issues sweep.
func UnwireableRefs(specs map[string]*AppSpec) []string {
	var out []string
	for _, n := range sortedKeys(specs) {
		if s := specs[n]; s != nil {
			for _, u := range s.unwireable(n, func(x string) *AppSpec { return specs[x] }) {
				out = append(out, n+" → "+u)
			}
		}
	}
	return out
}

// Reaches lists the installed apps this spec is wired to, sorted: the
// approved ones (reach, uses) that are installed.
func (s *AppSpec) Reaches(self string, installed func(string) bool) []string {
	var out []string
	for n := range s.approved() {
		if n != self && installed(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// sealedCompatible reports whether a wire between a and b keeps a sealed
// app sealed: a sealed app is wired only to sealed apps and sealed peers.
func sealedCompatible(a, b *AppSpec) bool {
	trusted := func(s *AppSpec) bool { return s.Sealed() || s.SealedPeer }
	if a.Sealed() || b.Sealed() {
		return trusted(a) && trusted(b)
	}
	return true
}

// Wire is one consumer's reach of one provider, on the provider's container
// ports.
type Wire struct {
	Provider, Consumer string
	Ports              []int
}

// netPlan is where every app's containers belong.
type netPlan struct {
	own   map[string]string // app -> its own network
	wires []Wire
	pg    map[string]bool // apps that need fog's Postgres
	// refused lists wires a sealed app's spec asked for that would have
	// breached it, as "consumer → provider".
	refused []string
}

// planNetworks works out the plan from each app's spec (the deployed one;
// see ReconcileNetworks) and the apps that serve widgets: an app granted
// "home" fetches their pictures from them (docs/widgets-contract.md), on
// port 80, so it reaches each one — a sealed one only if it may.
// Deterministic for the same input.
func planNetworks(specs map[string]*AppSpec, widgets map[string]bool) *netPlan {
	p := &netPlan{own: map[string]string{}, pg: map[string]bool{}}
	names := make([]string, 0, len(specs))
	for n, s := range specs {
		if s != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	installed := func(n string) *AppSpec { return specs[n] }
	for _, n := range names {
		s := specs[n]
		p.own[n] = OwnNetwork(n, s)
		if s.Needs != nil && s.Needs.Postgres != nil {
			p.pg[n] = true
		}
		reach := s.wiresOf(n, installed)
		if s.UsesGrant(UsesHome) {
			for _, w := range names {
				if w != n && widgets[w] && !slices.Contains(reach[w], 80) && sealedCompatible(s, specs[w]) {
					reach[w] = append(reach[w], 80)
					slices.Sort(reach[w])
				}
			}
		}
		provs := make([]string, 0, len(reach))
		for prov := range reach {
			provs = append(provs, prov)
		}
		sort.Strings(provs)
		for _, prov := range provs {
			if !sealedCompatible(s, specs[prov]) {
				p.refused = append(p.refused, n+" → "+prov)
				continue
			}
			p.wires = append(p.wires, Wire{Provider: prov, Consumer: n, Ports: reach[prov]})
		}
	}
	return p
}

// providersOf are the providers consumer reaches, sorted.
func (p *netPlan) providersOf(consumer string) []string {
	var out []string
	for _, w := range p.wires {
		if w.Consumer == consumer {
			out = append(out, w.Provider)
		}
	}
	return out
}

// isProvider reports whether any app reaches app.
func (p *netPlan) isProvider(app string) bool {
	for _, w := range p.wires {
		if w.Provider == app {
			return true
		}
	}
	return false
}

// AppNetworks is what one app's compose file joins: its own network, and
// its sidecar network.
type AppNetworks struct {
	Own       string
	Sealed    bool
	gatewayIP string
	auxName   string // the sidecar network, when known
}

// aux is the sidecar network's name.
func (a *AppNetworks) aux() string { return a.auxName }

// defaultNetworks is an app's networks — what Render gives a caller that
// didn't prepare them.
func defaultNetworks(name string, spec *AppSpec) *AppNetworks {
	return &AppNetworks{Own: OwnNetwork(name, spec), Sealed: spec.Sealed(), auxName: AuxNetwork(name, spec)}
}

// deployedSpecs answers every app's deployed spec, with override standing in
// for the apps it names (a deploy in progress, not yet recorded), and the
// apps that declare the widgets contract. An app never deployed has no
// containers and is left out, and so is one whose spec can't be read — it
// keeps what it has rather than losing its wires on a read error.
func (e *Engine) deployedSpecs(override map[string]*AppSpec) (map[string]*AppSpec, map[string]bool, error) {
	return deployedSpecsFrom(e.Store, override)
}

func deployedSpecsFrom(st *store.Store, override map[string]*AppSpec) (map[string]*AppSpec, map[string]bool, error) {
	list, err := st.ListApps()
	if err != nil {
		return nil, nil, err
	}
	widgets := map[string]bool{}
	if ws, err := st.AppsWithContract(build.WidgetsContract); err == nil {
		for _, a := range ws {
			widgets[a.Name] = true
		}
	}
	held, _ := st.HeldApps()
	out := map[string]*AppSpec{}
	for _, a := range list {
		if held[a.Name] {
			continue // an import nobody has reviewed is wired to nothing
		}
		if s, ok := override[a.Name]; ok {
			out[a.Name] = s
			continue
		}
		if a.CurrentRev == 0 {
			continue
		}
		_, s, err := SpecAt(st, a.ID, a.CurrentRev)
		if err != nil {
			continue
		}
		// Who an app may reach is the operator's say as it stands now — the
		// newest revision's reach and uses — not as it stood at the last
		// deploy: approving or withdrawing a wire needs no redeploy.
		if latest, err := LatestSpec(st, a.ID); err == nil {
			s.Reach, s.Uses = latest.Reach, latest.Uses
		}
		out[a.Name] = s
	}
	return out, widgets, nil
}

// settingReachGrandfathered marks the one-time GrandfatherReach pass done.
const settingReachGrandfathered = "net_reach_grandfathered"

// GrandfatherReachOnce runs once, for every app saved before these rules,
// as a revision of its own: it approves the installed apps its env URLs
// already name — what fog wired it to until now; apps installed later never
// are — and acknowledges the host folders outside fog's storage, and the
// Drive folders not its own, it already binds, which the operator chose and which would otherwise stop its next
// save and deploy. A path fog refuses outright stays refused.
func (e *Engine) GrandfatherReachOnce(ctx context.Context) {
	if v, _ := e.Store.GetSetting(settingReachGrandfathered); v == "1" {
		return
	}
	list, err := WithSpecs(e.Store)
	if err != nil {
		return
	}
	installed := map[string]bool{}
	for _, a := range list {
		installed[a.Name] = true
	}
	for _, a := range list {
		if a.Spec == nil {
			continue
		}
		spec := *a.Spec
		spec.Reach = append([]string{}, a.Spec.Reach...)
		spec.AcknowledgeHostAccess = append([]string{}, a.Spec.AcknowledgeHostAccess...)
		changed := spec.GrandfatherReach(a.Name, func(n string) bool { return installed[n] })
		if e.Mat != nil {
			h := e.storageFor(a.Name, spec.Uses)
			for _, hv := range spec.hostVolumes() {
				p := cleanHostPath(hv.Host)
				if refused, ack := h.HostPathVerdict(p, hv.ReadOnly); refused == "" && ack != "" && !spec.acknowledged(p) {
					spec.AcknowledgeHostAccess = append(spec.AcknowledgeHostAccess, p)
					changed = true
				}
			}
			for _, dv := range spec.driveVolumes() {
				if rel := cleanDriveRel(dv.Files); h.driveWhy(rel) != "" && !spec.acknowledgedDrive(rel) {
					spec.AcknowledgeHostAccess = append(spec.AcknowledgeHostAccess, rel)
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		if err := e.UpdateSpec(ctx, a.Name, &spec, nil, "upgrade: approve the apps and host folders it already reaches", "system"); err != nil {
			e.Log.Warn("grandfathering an app's reach and host folders", "app", a.Name, "err", err)
		}
	}
	e.Store.SetSetting(settingReachGrandfathered, "1")
}

// settingApprovalsGrandfathered marks the one-time approvals pass done.
const settingApprovalsGrandfathered = "app_approvals_grandfathered"

// GrandfatherApprovalsOnce records, once, the acknowledgements every app
// already carries as the operator's approvals: before approvals were kept
// apart, an operator's save was the only way one got there. An import still
// held for review is skipped — it gets its approvals by being confirmed.
func (e *Engine) GrandfatherApprovalsOnce() {
	if v, _ := e.Store.GetSetting(settingApprovalsGrandfathered); v == "1" {
		return
	}
	list, err := WithSpecs(e.Store)
	if err != nil {
		return
	}
	held, _ := e.Store.HeldApps()
	for _, a := range list {
		if a.Spec == nil || held[a.Name] {
			continue
		}
		if err := e.Store.AddAppApprovals(a.ID, specApprovals(a.Spec), "upgrade"); err != nil {
			e.Log.Warn("grandfathering an app's approvals", "app", a.Name, "err", err)
			return
		}
	}
	e.Store.SetSetting(settingApprovalsGrandfathered, "1")
}

// PlanWires answers every wire the deployed specs ask for — what fog-wire
// carries (internal/proxy renders its config from it).
func PlanWires(st *store.Store) ([]Wire, error) {
	specs, widgets, err := deployedSpecsFrom(st, nil)
	if err != nil {
		return nil, err
	}
	return planNetworks(specs, widgets).wires, nil
}

// prepareNetworks makes the networks a deploy of name at spec joins, and
// puts fog's proxy and (if it needs one) Postgres on the app's own network
// before anything of the app's starts — its release command may migrate.
func (e *Engine) prepareNetworks(ctx context.Context, name string, spec *AppSpec) (*AppNetworks, error) {
	specs, widgets, err := e.deployedSpecs(map[string]*AppSpec{name: spec})
	if err != nil {
		return nil, err
	}
	plan := planNetworks(specs, widgets)
	nets := defaultNetworks(name, spec)
	gw, err := e.Docker.EnsureIsolatedNetwork(ctx, nets.Own, spec.Sealed(), map[string]string{netLabel: "app", "fog.app": name})
	if err != nil {
		return nil, err
	}
	nets.gatewayIP = gw
	if err := e.attachHubs(ctx, plan, name, nets.Own); err != nil {
		return nil, err
	}
	if len(spec.Aux) > 0 {
		if err := e.prepareAuxNetwork(ctx, name, spec, nets.aux()); err != nil {
			return nil, err
		}
	}
	return nets, nil
}

// prepareAuxNetwork makes the app's sidecar network and puts the sidecars
// already running on it, under their names. They were started on the
// network compose used to make for them; until they are recreated after the
// swap, the new replica (and the release command before it) must still find
// its database.
func (e *Engine) prepareAuxNetwork(ctx context.Context, name string, spec *AppSpec, network string) error {
	if _, err := e.Docker.EnsureIsolatedNetwork(ctx, network, spec.Sealed(), map[string]string{netLabel: "aux", "fog.app": name}); err != nil {
		return err
	}
	cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name)
	if err != nil {
		return fmt.Errorf("listing %s's sidecars: %w", name, err)
	}
	for _, c := range cs {
		aux := c.Labels["fog.aux"]
		if aux == "" || c.State != "running" {
			continue
		}
		if _, on := c.NetworkSettings.Networks[network]; on {
			continue
		}
		if err := e.Docker.NetworkConnect(ctx, network, c.ID, []string{aux}); err != nil {
			return fmt.Errorf("moving sidecar %s onto %s: %w", aux, network, err)
		}
	}
	return nil
}

// RemoveLegacyAuxNetwork deletes the sidecar network compose used to make
// for the app, once nothing is on it. Best effort: one still in use stays.
func (e *Engine) RemoveLegacyAuxNetwork(ctx context.Context, name string) {
	if e.Docker != nil {
		e.Docker.RemoveNetwork(ctx, name+"_private")
	}
}

// attachHubs puts fog's running containers on app's own network, as the
// plan says, before anything of the app's starts: the proxy, Postgres (its
// release command may migrate), and the wire proxy under the names of the
// providers the app reaches. One that isn't running yet is joined by the
// next reconcile once it is.
func (e *Engine) attachHubs(ctx context.Context, plan *netPlan, app, network string) error {
	cs, err := e.Docker.ListByLabel(ctx, "fog.managed=true")
	if err != nil {
		return fmt.Errorf("listing fog's own containers: %w", err)
	}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		want, _ := plan.desiredFor(c)
		for _, m := range want {
			if m.network != network {
				continue
			}
			if _, on := c.NetworkSettings.Networks[network]; on {
				continue // aliases are the reconcile's to correct
			}
			if err := e.Docker.NetworkConnect(ctx, network, c.ID, m.aliases); err != nil {
				return fmt.Errorf("connecting %s to %s: %w", dockerx.FogSystem(c), network, err)
			}
		}
	}
	return nil
}

// membership is one container's place on one network.
type membership struct {
	network string
	aliases []string
}

// desiredFor answers the isolation networks container c belongs on, and
// whether fog manages its networks at all (an aux sidecar, or a container
// fog doesn't know, is left alone).
func (p *netPlan) desiredFor(c dockerx.ContainerSummary) ([]membership, bool) {
	switch dockerx.FogSystem(c) {
	case "caddy":
		var out []membership
		for _, n := range sortedValues(p.own) {
			out = append(out, membership{network: n})
		}
		return out, true
	case "postgres":
		var out []membership
		for _, app := range sortedKeys(p.own) {
			if p.pg[app] {
				out = append(out, membership{network: p.own[app], aliases: []string{pg.ContainerName}})
			}
		}
		return out, true
	case "wire":
		// On each consumer's network under the names of the providers it
		// reaches, and on each provider's to reach it.
		var out []membership
		for _, app := range sortedKeys(p.own) {
			if provs := p.providersOf(app); len(provs) > 0 {
				out = append(out, membership{network: p.own[app], aliases: provs})
			} else if p.isProvider(app) {
				out = append(out, membership{network: p.own[app]})
			}
		}
		return out, true
	}
	// Not one of fog's own (FogSystem said so above): an app's container,
	// whose fog.* labels compose sets on every service (Render), overriding
	// any an image brings.
	app := c.Labels["fog.app"]
	if app == "" || c.Labels["fog.aux"] != "" {
		return nil, false
	}
	own, ok := p.own[app]
	if !ok {
		return nil, false // not deployed (or unreadable): leave it be
	}
	// A one-off (the release command, `compose run`) gets the app's network
	// but never answers for it: a migration isn't the app.
	if dockerx.IsMainContainer(c) && c.Labels["com.docker.compose.oneoff"] != "True" {
		return []membership{{network: own, aliases: []string{app, AppUpstream(app)}}}, true
	}
	return []membership{{network: own}}, true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// netOp is one change ReconcileNetworks makes.
type netOp struct {
	connect   bool
	network   string
	container string
	app       string // the container's app, "" for the proxy and Postgres
	aliases   []string
}

// touches reports whether op is about app: its container, its own network,
// or a wire it is an end of.
func (op netOp) touches(app string) bool {
	if op.app == app || op.network == appNetPrefix+app || op.network == sealedNetPrefix+app {
		return true
	}
	if rest, ok := strings.CutPrefix(op.network, linkNetPrefix); ok {
		prov, cons, _ := strings.Cut(rest, ".")
		return prov == app || cons == app
	}
	return false
}

// diffNetworks answers the changes that bring the containers to the plan:
// connections first, then endpoints whose names are wrong (taken off and put
// back with the right ones), every disconnection last, so a container is
// never between networks with nowhere to answer. An app's container leaves
// the old shared networks; fog's own keep them (they are how the proxy gets
// out and the old pg tools reach the database). A network a container is on
// whose names fog doesn't know (a listing carries none) isn't second-guessed.
func diffNetworks(p *netPlan, cs []dockerx.ContainerSummary) []netOp {
	var connects, renames, disconnects []netOp
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		want, managed := p.desiredFor(c)
		if !managed {
			continue
		}
		wanted := map[string]bool{}
		for _, m := range want {
			wanted[m.network] = true
			ep, on := c.NetworkSettings.Networks[m.network]
			op := netOp{connect: true, network: m.network, container: c.ID, app: c.Labels["fog.app"], aliases: m.aliases}
			switch {
			case !on:
				connects = append(connects, op)
			case len(m.aliases) > 0 && (ep.Aliases != nil || ep.DNSNames != nil) && !answersAll(ep, m.aliases):
				renames = append(renames, netOp{network: m.network, container: c.ID, app: op.app}, op)
			}
		}
		hub := dockerx.FogSystem(c) != ""
		for n := range c.NetworkSettings.Networks {
			if wanted[n] {
				continue
			}
			if isolationNetwork(n) || (!hub && legacyShared(n)) {
				disconnects = append(disconnects, netOp{network: n, container: c.ID, app: c.Labels["fog.app"]})
			}
		}
	}
	order := func(ops []netOp) {
		sort.SliceStable(ops, func(i, j int) bool {
			if ops[i].container != ops[j].container {
				return ops[i].container < ops[j].container
			}
			return ops[i].network < ops[j].network
		})
	}
	order(connects)
	order(disconnects)
	return append(append(connects, renames...), disconnects...)
}

func answersAll(ep dockerx.NetworkEndpoint, names []string) bool {
	for _, n := range names {
		if !ep.Answers(n) {
			return false
		}
	}
	return true
}

// ReconcileNetworks brings every running container of fog's onto the
// networks the deployed specs say, creating the networks as needed.
// override stands in for apps whose spec is being deployed right now.
//
// Reconciles run one at a time. An app with a deploy in flight that isn't
// in override is left alone — its containers, its network, its wires: the
// recorded spec is about to be replaced, and the deploy reconciles it
// itself when it lands.
func (e *Engine) ReconcileNetworks(ctx context.Context, override map[string]*AppSpec) error {
	if e.Docker == nil {
		return nil
	}
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	var busy []string
	for _, a := range e.DeployingApps() {
		if _, ok := override[a]; !ok {
			busy = append(busy, a)
		}
	}
	specs, widgets, err := e.deployedSpecs(override)
	if err != nil {
		return err
	}
	plan := planNetworks(specs, widgets)
	for app, n := range plan.own {
		if _, err := e.Docker.EnsureIsolatedNetwork(ctx, n, specs[app].Sealed(), map[string]string{netLabel: "app", "fog.app": app}); err != nil {
			return err
		}
	}
	cs, err := e.Docker.ListByLabel(ctx, "fog.managed=true")
	if err != nil {
		return err
	}
	// A listing carries no names; read them for every container that
	// should answer to some.
	for i, c := range cs {
		want, _ := plan.desiredFor(c)
		if c.State != "running" || !slices.ContainsFunc(want, func(m membership) bool { return len(m.aliases) > 0 }) {
			continue
		}
		d, err := e.Docker.Inspect(ctx, c.ID)
		if err != nil {
			continue
		}
		for n, ep := range d.NetworkSettings.Networks {
			cur := cs[i].NetworkSettings.Networks[n]
			cur.Aliases, cur.DNSNames = ep.Aliases, ep.DNSNames
			if cur.Aliases == nil && cur.DNSNames == nil {
				cur.Aliases = []string{}
			}
			cs[i].NetworkSettings.Networks[n] = cur
		}
	}
	var errs []string
ops:
	for _, op := range diffNetworks(plan, cs) {
		for _, b := range busy {
			if op.touches(b) {
				continue ops
			}
		}
		var err error
		if op.connect {
			err = e.Docker.NetworkConnect(ctx, op.network, op.container, op.aliases)
		} else {
			err = e.Docker.NetworkDisconnect(ctx, op.network, op.container, false)
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s %.12s on %s: %v", map[bool]string{true: "connecting", false: "disconnecting"}[op.connect], op.container, op.network, err))
		}
	}
	// The per-wire networks fog used to make, once nothing is on them.
	if links, err := e.Docker.NetworksByLabel(ctx, netLabel+"=link"); err == nil {
		for _, n := range links {
			e.Docker.RemoveNetwork(ctx, n.Name)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("reconciling app networks: %s", strings.Join(errs, "; "))
	}
	return nil
}

// RemoveAppNetworks deletes the networks only name used — its own, its
// sidecars', and any old per-wire network it was an end of — once its
// containers are gone.
func (e *Engine) RemoveAppNetworks(ctx context.Context, name string) {
	if e.Docker == nil {
		return
	}
	nets, err := e.Docker.NetworksByLabel(ctx, netLabel)
	if err != nil {
		e.Log.Warn("listing app networks", "app", name, "err", err)
		return
	}
	// Disconnect what is still on them first — fog's own containers: Docker
	// won't remove a network with endpoints on it.
	cs, _ := e.Docker.ListByLabel(ctx, "fog.managed=true")
	for _, n := range nets {
		mine := n.Name == appNetPrefix+name || n.Name == sealedNetPrefix+name ||
			n.Name == auxNetPrefix+name || n.Name == sealedAuxNetPrefix+name
		if strings.HasPrefix(n.Name, linkNetPrefix) {
			prov, cons, _ := strings.Cut(strings.TrimPrefix(n.Name, linkNetPrefix), ".")
			mine = prov == name || cons == name
		}
		if !mine {
			continue
		}
		for _, c := range cs {
			if _, on := c.NetworkSettings.Networks[n.Name]; on {
				e.Docker.NetworkDisconnect(ctx, n.Name, c.ID, true)
			}
		}
		if err := e.Docker.RemoveNetwork(ctx, n.Name); err != nil {
			e.Log.Warn("removing app network", "app", name, "network", n.Name, "err", err)
		}
	}
}

// ownNetworkIP is a container's address on its app's own network, else on
// one of the old shared ones (a container not yet moved).
// app "" takes whichever own network the container is on (a replica being
// health-checked knows no name to ask by).
func ownNetworkIP(networks map[string]dockerx.NetworkEndpoint, app string) string {
	if app == "" {
		for n, ep := range networks {
			if (strings.HasPrefix(n, appNetPrefix) || strings.HasPrefix(n, sealedNetPrefix)) && ep.IPAddress != "" {
				return ep.IPAddress
			}
		}
	}
	for _, n := range []string{appNetPrefix + app, sealedNetPrefix + app, NetworkName, SealedNetworkName} {
		if ip := networks[n].IPAddress; ip != "" {
			return ip
		}
	}
	return ""
}

// ownNetworks are the fog networks a container answers on, for a swap to
// take the old replica out of every one before stopping it.
func ownNetworks(names []string) []string {
	var out []string
	for _, n := range names {
		if isolationNetwork(n) || legacyShared(n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// RefusedWires lists the wires a sealed app asked for that fog left out
// because they would breach it, as "consumer → provider" — for the issues
// sweep.
func RefusedWires(specs map[string]*AppSpec) []string {
	return planNetworks(specs, nil).refused
}

var reconcileMu sync.Mutex

// NetworkInterval is how often running containers are brought back in line
// with the wiring: what heals a hub fog's own lifecycle recreated (Postgres
// after an upgrade) and anything changed by hand.
const NetworkInterval = time.Minute

// NetworkLoop reconciles app networks now and every interval after.
func (e *Engine) NetworkLoop(ctx context.Context, interval time.Duration) {
	e.grandfather(ctx)
	loop.Every(ctx, 0, interval, func(ctx context.Context) {
		// An app holding fog's port is stopped, and fog's services brought
		// back up on it (fogports.go).
		if e.ReclaimFogPorts(ctx) && e.OnChange != nil {
			e.OnChange(ctx)
		}
		if err := e.ReconcileNetworks(ctx, nil); err != nil {
			e.Log.Warn("app networks", "err", err)
		}
	})
}

// grandfather runs the one-time upgrade passes (reach and host folders,
// then approvals), once, before either the network loop or the boot pass
// judges any app by the rules they carry over to: run after, the boot pass
// would stop every app whose folders it hadn't approved yet.
func (e *Engine) grandfather(ctx context.Context) {
	e.grandfatherMu.Lock()
	defer e.grandfatherMu.Unlock()
	e.GrandfatherReachOnce(ctx)
	e.GrandfatherApprovalsOnce()
}
