// Package proxy manages the Caddy reverse proxy: a Fog-managed container
// whose Caddyfile is generated from app routes and reloaded gracefully.
// Like every app, its artifacts are plain files under <datadir>/caddy/ that
// keep working if fogd stops.
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fogports"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/tunnel"
	"gitea.lan/matthew/fog/internal/vault"
)

// Settings keys owned by the proxy subsystem.
const (
	SettingCaddyImage = "caddy_image"
	SettingAcmeEmail  = "acme_email"
	SettingDNSToken   = "dns_api_token" // system secret key, not a setting

	// secretForward is the system secret Caddy presents on requests it
	// forwards to fogd, so fogd can trust the client address it names.
	secretForward = "_proxy_forward_secret"
)

// Headers Caddy sets on every request it forwards to fogd. Both overwrite
// whatever the client sent: the address is Caddy's own view of the TCP peer,
// and the secret proves the request really passed through Caddy rather than
// arriving from some other LAN host with made-up headers.
const (
	HeaderClientIP = "X-Fog-Client-Ip"
	HeaderSecret   = "X-Fog-Proxy-Secret"
	// HeaderVia says which listener a request came in on: ViaTunnel for
	// the tunnel socket (TunnelSocket), ViaEdge for :80/:443. fogd uses it to tell an operator
	// whose Cloudflare Tunnel still points at :443.
	HeaderVia = "X-Fog-Via"
)

// The listeners HeaderVia names.
const (
	ViaEdge   = "edge"
	ViaTunnel = "tunnel"
)

// DefaultCaddyImage includes the Cloudflare DNS module for DNS-01 wildcard
// certs on a LAN. Overridable via settings for other DNS providers.
const DefaultCaddyImage = "ghcr.io/caddybuilds/caddy-cloudflare:latest"

// applyTimeout bounds one converge of the proxy container. Apply sits on the
// synchronous path of every app create/deploy/delete, so an unresponsive
// Docker must surface as an error rather than as a deploy that never returns.
const applyTimeout = 5 * time.Minute

type Proxy struct {
	DataDir string
	Store   *store.Store
	Vault   *vault.Vault
	Log     *slog.Logger
	// Docker is the shared Engine API client. Nil makes one on first use.
	Docker *dockerx.Client
	// FogUpstream is where Caddy reaches fogd itself (the host), e.g.
	// host.docker.internal:8080. Main derives it from the listen address;
	// there is no default, because a guessed port sends fog's own route
	// and every gate check to nothing. Apply refuses to run without it.
	FogUpstream string

	// applyMu serializes Apply. It is reachable from app create/deploy/delete,
	// the settings and system-secret writes, the domain wizard, and boot —
	// concurrent callers would otherwise rewrite the same Caddyfile while
	// running `docker compose up` in the same directory.
	//
	// Deliberately a plain mutex rather than coalescing: two of the callers
	// (the Apply button, the domain wizard) report the result to the operator,
	// and collapsing runs would let one of them be told "applied" about a
	// converge that started before its change was written.
	applyMu sync.Mutex
	// applied is the digest of the files the last successful converge
	// loaded. Most deploys don't touch routing, so a rendered config equal
	// to it — with Caddy still running — needs no compose up, validate or
	// reload. Guarded by applyMu.
	applied string

	// wireApplied is applied's twin for fog-wire (wire.go).
	wireApplied string

	// Test seams: the docker-touching halves of Apply.
	converge     func(ctx context.Context, dir string) error
	wireConverge func(ctx context.Context, dir string) error
	caddyRunning func(ctx context.Context) bool
	subnets      func(ctx context.Context) (map[string]string, error)

	// tunnelOn is whether a Cloudflare Tunnel is set up (its connector
	// token is saved), and so whether Caddy serves the tunnel socket; set
	// by applyCaddy before it renders. Saving the token applies the proxy.
	tunnelOn bool

	dockerOnce sync.Once
	secretMu   sync.Mutex
	secret     string // cached forward secret, once read or minted
}

// ForwardSecret is the secret Caddy sends fogd with each forwarded request,
// minted and stored in the vault on first use.
func (p *Proxy) ForwardSecret() (string, error) {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.secret != "" {
		return p.secret, nil
	}
	v, err := vault.Secrets{Store: p.Store, Vault: p.Vault}.GetOrMint(0, secretForward,
		func() (string, error) { return randx.Hex(32), nil })
	if err != nil {
		return "", err
	}
	p.secret = v
	return p.secret, nil
}

func (p *Proxy) dir() string { return filepath.Join(p.DataDir, "caddy") }

func (p *Proxy) docker() *dockerx.Client {
	p.dockerOnce.Do(func() {
		if p.Docker == nil {
			p.Docker = dockerx.New()
		}
	})
	return p.Docker
}

// Apply regenerates the Caddyfile + compose file and converges. Called after
// any app/settings change that affects routing. No-op when no base domain is
// configured yet, and when the rendered files are exactly what the running
// Caddy already loaded.
func (p *Proxy) Apply(ctx context.Context) error {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()

	// Wires between apps run whether or not fog has a domain (wire.go).
	wireErr := p.applyWires(ctx)
	domain, _ := p.Store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		return wireErr
	}
	if err := p.applyCaddy(ctx, domain); err != nil {
		return err
	}
	return wireErr
}

// applyCaddy is Apply's half for the public proxy. Caller holds applyMu.
func (p *Proxy) applyCaddy(ctx context.Context, domain string) error {
	if p.FogUpstream == "" {
		return errors.New("proxy: no upstream for fogd (FogUpstream unset)")
	}
	// Both reach the Caddyfile as written: a value that is not a plain name
	// would be config of its own.
	if !apps.ValidRouteHost(domain) {
		return fmt.Errorf("proxy: the base domain %q is not a hostname", domain)
	}
	routes, err := p.collectRoutes(domain)
	if err != nil {
		return err
	}
	// One vault decrypt serves both the Caddyfile's shape and the .env.
	dnsToken := p.dnsToken()
	secret, err := p.ForwardSecret()
	if err != nil {
		return fmt.Errorf("proxy forward secret: %w", err)
	}
	email, _ := p.Store.GetSetting(SettingAcmeEmail)
	if email != "" && !acmeEmailRe.MatchString(email) {
		p.warn("proxy: ignoring the ACME email: not a plain address", "email", email)
		email = ""
	}
	_, tokErr := p.Store.GetSecret(0, tunnel.SecretToken)
	p.tunnelOn = tokErr == nil
	caddyfile := []byte(p.caddyfile(domain, routes, dnsToken != "", email))
	env := envFile(secret, dnsToken)
	compose, err := p.composeFile()
	if err != nil {
		return err
	}
	sum := digest(caddyfile, env, compose)
	if sum == p.applied && p.isCaddyRunning(ctx) {
		return nil
	}
	// Whatever happens next, the running Caddy no longer provably matches
	// the last success: a failure below must not let the next call skip.
	p.applied = ""

	dir := p.dir()
	for _, sub := range []string{"data", "config"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	// The tunnel socket's directory: fog's own user (and root) only, so
	// only cloudflared — fogd's child — can reach the socket in it.
	if err := os.MkdirAll(filepath.Join(dir, tunnelDir), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, tunnelDir), 0o700); err != nil {
		return err
	}
	// Staged, never written over the live file: a config Caddy rejects
	// must not be where the next restart would load it (stage.go).
	if err := stageConfig(dir, caddyfile); err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, ".env"), env, 0o600); err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, "compose.yaml"), compose, 0o644); err != nil {
		return err
	}
	conv := p.converge
	if conv == nil {
		conv = p.composeConverge
	}
	if err := conv(ctx, dir); err != nil {
		discardStaged(dir)
		return err
	}
	if err := promoteStaged(dir); err != nil {
		return err
	}
	p.applied = sum
	return nil
}

// acmeEmailRe is an address with nothing Caddyfile syntax would act on.
var acmeEmailRe = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+$`)

// ValidAcmeEmail reports whether v may be the ACME account email: it is
// written into the Caddyfile as is.
func ValidAcmeEmail(v string) bool { return acmeEmailRe.MatchString(v) }

func digest(parts ...[]byte) string {
	h := sha256.New()
	for _, b := range parts {
		// Length-prefixed, so moving bytes between files changes the sum.
		fmt.Fprintf(h, "%d:", len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Proxy) isCaddyRunning(ctx context.Context) bool {
	if p.caddyRunning != nil {
		return p.caddyRunning(ctx)
	}
	d, err := p.docker().Inspect(ctx, "caddy")
	return err == nil && d.State.Running
}

// composeConverge brings the caddy container up on the files in dir and
// makes it serve them.
func (p *Proxy) composeConverge(ctx context.Context, dir string) error {
	// The compose file declares the network external, and on a fresh box no
	// app deploy has created it yet — the proxy is often the first container.
	if err := p.docker().EnsureNetwork(ctx, apps.NetworkName); err != nil {
		return fmt.Errorf("caddy up: %w", err)
	}
	if _, err := p.docker().EnsureInternalNetwork(ctx, apps.SealedNetworkName); err != nil {
		return fmt.Errorf("caddy up: %w", err)
	}

	up := execx.Command(ctx, "docker", "compose", "up", "-d")
	up.Dir = dir
	if err := execx.Step(ctx, applyTimeout, "bringing the proxy up", up); err != nil {
		return err
	}
	// Validate the generated config before asking Caddy to load it. A
	// rejected Caddyfile would leave the OLD config serving while every
	// caller of Apply (deploy, wizard, the Apply button) was told the change
	// took — the exact green lie the truthful-state rule forbids. The file
	// is bind-mounted, so the running container can check the new bytes
	// without applying them.
	validate := execx.Command(ctx, "docker", "exec", "caddy", "caddy", "validate", "--adapter", "caddyfile", "--config", "/etc/caddy/"+stagedName)
	if err := execx.Step(ctx, applyTimeout, "validating the proxy config", validate); err != nil {
		if execx.TimedOut(err) {
			return err
		}
		return fmt.Errorf("generated Caddyfile rejected, previous routes still serving: %w", errors.Unwrap(err))
	}
	// Graceful zero-downtime reload; harmless if the container just started
	// (it already booted from this very file, and reloading it is a no-op).
	// A failure here means the routes on disk are NOT the routes serving, so
	// it must reach the caller, not a log line.
	if err := promoteStaged(dir); err != nil {
		return err
	}
	reload := execx.Command(ctx, "docker", "exec", "caddy", "caddy", "reload", "--config", "/etc/caddy/Caddyfile")
	return execx.Step(ctx, applyTimeout, "reloading the proxy", reload)
}

func (p *Proxy) dnsToken() string {
	v, err := vault.Secrets{Store: p.Store, Vault: p.Vault}.Get(0, SettingDNSToken)
	if err != nil {
		return ""
	}
	return v
}

type route struct {
	App      string // the app it serves; "" for fogd's own
	Host     string
	Upstream string // container:port or host upstream
	NoBuffer bool
	Auth     bool // gate behind Fog's session via forward_auth
	LanOnly  bool // answer private addresses only, 403 the rest
	Fog      bool // fogd's own route
}

// collectRoutes gathers every route the Caddyfile serves. One app's bad
// state must not cost every other app its routes: a refused Caddyfile is
// all of them failing to apply. So an app whose spec cannot be read, a route
// whose hostname is not one, and a route that loses its hostname to fog or
// to an older app (apps.RouteConflicts) are each left out and logged — and
// the issues sweep, reading the same state by the same rules, files each
// one where the operator sees it.
func (p *Proxy) collectRoutes(domain string) ([]route, error) {
	routes := []route{{Host: "fog." + domain, Upstream: p.FogUpstream, Fog: true}}

	list, err := apps.WithSpecs(p.Store)
	if err != nil {
		return nil, err
	}
	// An app held for review serves nothing until it is confirmed.
	held, _ := p.Store.HeldApps()
	claimants := make([]apps.RouteClaimant, 0, len(list))
	for i, a := range list {
		if held[a.Name] {
			continue
		}
		if a.Spec == nil {
			// Not in the batch: no revision yet, or one that doesn't parse.
			// Ask for this app alone to learn which.
			one, err := apps.LatestSpec(p.Store, a.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue // just created, no revision yet — legitimately routeless
			}
			if err != nil {
				p.warn("proxy: leaving an app out: its spec cannot be read", "app", a.Name, "err", err)
				continue
			}
			list[i].Spec = one
		}
		claimants = append(claimants, apps.RouteClaimant{ID: a.ID, Name: a.Name, Spec: list[i].Spec})
	}
	conflicts := apps.RouteConflicts(claimants, domain)
	for _, c := range conflicts {
		p.warn("proxy: leaving a route out: "+c.Error(), "app", c.App, "host", c.Host)
	}
	for _, c := range claimants {
		for _, r := range c.Spec.Routes {
			host := apps.RouteHost(c.Name, r, domain)
			if host == "" {
				continue
			}
			if !apps.ValidRouteHost(host) || r.Port < 1 || r.Port > 65535 {
				p.warn("proxy: leaving a route out: not a hostname and port", "app", c.Name, "host", host, "port", r.Port)
				continue
			}
			if apps.LosesRoute(conflicts, c.ID, c.Name, r, domain) {
				continue
			}
			routes = append(routes, route{
				App:  c.Name,
				Host: host,
				// The fog-only name: no other app's service, container or
				// alias can answer to it (apps.AppUpstream).
				Upstream: fmt.Sprintf("%s:%d", apps.AppUpstream(c.Name), r.Port),
				NoBuffer: r.NoBuffer,
				Auth:     r.Auth,
				LanOnly:  r.LanOnly,
			})
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Host < routes[j].Host })
	return routes, nil
}

func (p *Proxy) warn(msg string, args ...any) {
	if p.Log != nil {
		p.Log.Warn(msg, args...)
	}
}

// caddyfile renders the config. With a DNS token: one wildcard HTTPS block
// (single cert, DNS-01, nothing exposed to the internet). Without: plain
// HTTP host routing, still useful behind local DNS.
func (p *Proxy) caddyfile(domain string, routes []route, hasDNS bool, email string) string {
	var b strings.Builder
	b.WriteString("# Generated by Fog. Edit through the Fog UI.\n")
	if email != "" {
		fmt.Fprintf(&b, "{\n\temail %s\n}\n\n", email)
	}

	if hasDNS {
		fmt.Fprintf(&b, "*.%s, %s {\n", domain, domain)
		b.WriteString("\ttls {\n\t\tdns cloudflare {env.DNS_API_TOKEN}\n\t}\n")
		for _, r := range routes {
			matcher := matcherName(r.Host)
			fmt.Fprintf(&b, "\t%s host %s\n", matcher, r.Host)
			fmt.Fprintf(&b, "\thandle %s {\n%s\t}\n", matcher, routeBody(r, p.FogUpstream, "\t\t"))
		}
		b.WriteString("\thandle {\n\t\trespond \"unknown app\" 404\n\t}\n}\n")
		b.WriteString(p.tunnelSite(domain, routes, hasDNS))
		return b.String()
	}

	for _, r := range routes {
		fmt.Fprintf(&b, "http://%s {\n%s}\n\n", r.Host, routeBody(r, p.FogUpstream, "\t"))
	}
	b.WriteString(p.tunnelSite(domain, routes, hasDNS))
	return b.String()
}

// Caddy's tunnel listener is a unix socket, in a directory on the host
// only fog's own user can open, bind-mounted into Caddy's container. fog's
// cloudflared — a child of fogd on the host — reaches Caddy there: the
// tunnel's service is "unix:<TunnelSocket>". Nothing on a network can reach
// it, so no app container — whatever address it forges, whatever raw
// sockets it holds — can speak as cloudflared. (On :443 every tunnel
// visitor arrived from the host's own bridge address, which fog could tell
// from no one; a TCP listener answering "only the host's address" was
// reachable by a container that forged that address.)
const (
	tunnelDir         = "tunnel"
	tunnelSocketName  = "caddy.sock"
	tunnelInContainer = "/run/fog-tunnel"
)

// TunnelSocket is the tunnel listener's path on the host.
func (p *Proxy) TunnelSocket() string { return tunnelSocketIn(p.DataDir) }

// TunnelService is what a tunnel's public hostnames name as their service.
func (p *Proxy) TunnelService() string { return TunnelServiceIn(p.DataDir) }

// TunnelServiceIn is TunnelService for fog's data directory dataDir.
func TunnelServiceIn(dataDir string) string { return "unix:" + tunnelSocketIn(dataDir) }

func tunnelSocketIn(dataDir string) string {
	return filepath.Join(dataDir, "caddy", tunnelDir, tunnelSocketName)
}

// Where fogd records the last time it saw Cloudflare Tunnel traffic come in
// on each listener (RFC 3339): through the tunnel socket, or through :443
// from the host's own address with Cf-Connecting-Ip — a tunnel whose service
// still names localhost:443. The issues sweep reads them.
const (
	SettingTunnelSeenListener = "tunnel_seen_listener"
	SettingTunnelSeenEdge     = "tunnel_seen_edge"
)

// tunnelSite renders the tunnel listener: the same routes, served over the
// socket as plain http (a visitor's TLS ended at Cloudflare; apps and fogd
// are told https, which is what the visitor is on). On it a request's
// client is Cf-Connecting-Ip — Cloudflare's word, which only cloudflared
// can bring — and a LAN-only route refuses everyone.
func (p *Proxy) tunnelSite(domain string, routes []route, hasDNS bool) string {
	if !p.tunnelOn {
		return ""
	}
	var b strings.Builder
	if hasDNS {
		fmt.Fprintf(&b, "http://*.%s, http://%s {\n", domain, domain)
	} else {
		hosts := make([]string, 0, len(routes))
		for _, r := range routes {
			hosts = append(hosts, "http://"+r.Host)
		}
		if len(hosts) == 0 {
			return ""
		}
		fmt.Fprintf(&b, "%s {\n", strings.Join(hosts, ", "))
	}
	fmt.Fprintf(&b, "\tbind unix/%s/%s|0666\n", tunnelInContainer, tunnelSocketName)
	for _, r := range routes {
		matcher := matcherName(r.Host)
		fmt.Fprintf(&b, "\t%s host %s\n", matcher, r.Host)
		fmt.Fprintf(&b, "\thandle %s {\n%s\t}\n", matcher, tunnelBody(r, p.FogUpstream, "\t\t"))
	}
	b.WriteString("\thandle {\n\t\trespond \"unknown app\" 404\n\t}\n}\n")
	return b.String()
}

// tunnelBody is routeBody on the tunnel listener.
func tunnelBody(r route, fogUp, indent string) string {
	if r.LanOnly {
		return fmt.Sprintf("%srespond %q 403\n", indent, lanRefusal)
	}
	return servedBodyAs(r, fogUp, indent, "{http.request.header."+cloudflareClientHeader+"}", ViaTunnel)
}

// matcherName derives a Caddy named matcher from the whole hostname, not just
// its first label: once routes can carry custom hosts, "claw.example.com" and
// "claw.apps.example.com" would otherwise share the matcher @claw and the
// second block would never match.
func matcherName(host string) string {
	// From a hash of the whole name, not a spelling of it: "-" and "." once
	// both became "_", so x-y.example.com and x.y.example.com shared a
	// matcher, and one host's route answered the other's.
	sum := sha256.Sum256([]byte(host))
	return "@h_" + hex.EncodeToString(sum[:10])
}

// The identity headers the gate sets (gate_handlers.go in internal/server).
// Caddy deletes any a client sent before the gate runs, so an app reading
// them reads the gate's answer or nothing.
var gateHeaders = []string{"Remote-User", "Remote-Name", "Remote-Groups", "Remote-Email", "Fog-Assertion"}

// forgeableHeaders is every name a client could send that an app might read
// as one of gateHeaders. Deleting "Remote-User" by name leaves
// "Remote_User", and a CGI-style server (Rack, WEBrick, Puma, wsgiref,
// Django) folds "-" and "_" alike into HTTP_REMOTE_USER — so that one walked
// through, and on a gated route the gate never set a dashed Remote-Email to
// contest it. Caddy matches a deleted name case-insensitively (Go
// canonicalises both sides), so the separators are what vary: "-", "_", and
// "." (which some servers fold too). Fog's own route forwards to fogd, a Go
// server that never confuses the forms, but strips them all the same.
var forgeableHeaders = func() []string {
	var out []string
	for _, h := range gateHeaders {
		for _, sep := range []string{"-", "_", "."} {
			out = append(out, strings.ReplaceAll(h, "-", sep))
		}
	}
	return out
}()

// appIdentityPrefixes are the header-name prefixes nothing but the gate may
// send an app: fog-client deletes every Remote* and Fog* header it is
// handed, and Caddy deletes them first.
var appIdentityPrefixes = []string{"Remote", "Fog"}

// stripFogCookies removes fog's own cookies from what an app receives: the
// gate's per-host cookie, and the domain-wide session cookie browsers still
// carry from before fog's session became host-only. An app never holds a
// credential of fog's, not even its own gate's.
const stripFogCookies = "header_up Cookie `\\b(?:__Host-)?fog_(?:gate|session|person)=[^;]*(?:;\\s*)?` \"\""

// lanRefusal is what a client outside the house is told by a lanOnly route.
const lanRefusal = "This address only answers on the home network"

// cloudflareClientHeader is on every request through a Cloudflare Tunnel.
// cloudflared runs on this host, so such a request reaches Caddy from a
// private address, and the header is all that tells it from the LAN. A LAN
// client that sends it refuses only itself.
const cloudflareClientHeader = "Cf-Connecting-Ip"

// routeBody renders what Caddy does for one host, each line at indent. A
// lanOnly route is the same body inside a branch only a private address
// enters. The body goes inside rather than a refusal going in front: Caddy
// runs handle before respond, and a handle with a path before one without,
// so a refusal in front is one a gated route's /.fog/gate/* never meets.
//
// remote_ip reads the TCP peer, never X-Forwarded-For, which a client writes.
func routeBody(r route, fogUp, indent string) string {
	if !r.LanOnly {
		return servedBody(r, fogUp, indent)
	}
	matcher := "@lan_" + strings.TrimPrefix(matcherName(r.Host), "@")
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s {\n", indent, matcher)
	fmt.Fprintf(&b, "%s\tremote_ip private_ranges\n", indent)
	fmt.Fprintf(&b, "%s\tnot header %s *\n", indent, cloudflareClientHeader)
	fmt.Fprintf(&b, "%s}\n", indent)
	fmt.Fprintf(&b, "%shandle %s {\n%s%s}\n", indent, matcher, servedBody(r, fogUp, indent+"\t"), indent)
	fmt.Fprintf(&b, "%shandle {\n%s\trespond %q 403\n%s}\n", indent, indent, lanRefusal, indent)
	return b.String()
}

// servedBody renders what Caddy does for a request it will answer.
//
// fogd's own route forwards the client address and the proxy secret. An app
// route scrubs the identity headers a client could forge and forwards to the
// app with fog's cookies stripped. A gated route (auth: true) also hands
// /.fog/gate/* to fogd for the gate's callback and puts forward_auth in
// front of the app, copying who passed onto the request.
func servedBody(r route, fogUp, indent string) string {
	return servedBodyAs(r, fogUp, indent, "{remote_host}", ViaEdge)
}

// servedBodyAs is servedBody naming the client to fogd as client — the TCP
// peer, or on the tunnel listener the visitor Cloudflare names — and the
// listener as via.
func servedBodyAs(r route, fogUp, indent, client, via string) string {
	var b strings.Builder
	line := func(depth int, format string, args ...any) {
		b.WriteString(indent + strings.Repeat("\t", depth) + fmt.Sprintf(format, args...) + "\n")
	}
	// On the tunnel socket the visitor is on https at Cloudflare, whatever
	// the socket's own scheme.
	proto := func(depth int) {
		if via == ViaTunnel {
			line(depth, "header_up X-Forwarded-Proto https")
		}
	}
	fogProxy := func(depth int) {
		line(depth, "reverse_proxy %s {", fogUp)
		line(depth+1, "header_up %s %s", HeaderClientIP, client)
		line(depth+1, "header_up %s %s", HeaderVia, via)
		line(depth+1, "header_up %s {env.FOG_PROXY_SECRET}", HeaderSecret)
		proto(depth + 1)
		line(depth, "}")
	}
	appProxy := func(depth int) {
		line(depth, "reverse_proxy %s {", r.Upstream)
		line(depth+1, "%s", stripFogCookies)
		proto(depth + 1)
		if r.NoBuffer {
			// flush_interval -1 disables response buffering — required for
			// media streaming and Range-heavy endpoints (Hub podcast audio).
			line(depth+1, "flush_interval -1")
		}
		line(depth, "}")
	}
	// Every route, gated or not: an app that once sat behind the gate, or
	// reads Remote-User by default, must never take a client's word for who
	// is asking — in any spelling a server would fold into the same name.
	// What Caddy alone tells fogd — the client, the listener, the secret —
	// is never a client's: every X-Fog-* it sent goes, on every route.
	line(0, "request_header -X-Fog-*")
	if r.Fog {
		// fogd's own route keeps Fog-Instance, Fog-Signature and the rest
		// of the federation's headers, which fogd verifies itself; nothing
		// a client sends there may claim a person.
		line(0, "request_header -Remote*")
		for _, h := range forgeableHeaders {
			if strings.HasPrefix(strings.ToLower(h), "fog") {
				line(0, "request_header -%s", h)
			}
		}
		fogProxy(0)
		return b.String()
	}
	// An app's route: every header whose name begins Remote or Fog, in
	// whatever case and separator, is the gate's to set or nobody's. Caddy
	// deletes by prefix (a trailing *) against Go's canonical form of the
	// name, so "remote_anything", "REMOTE.USER" and "fog.whatever" all go.
	for _, p := range appIdentityPrefixes {
		line(0, "request_header -%s*", p)
	}
	switch {
	case !r.Auth:
		appProxy(0)
	default:
		line(0, "handle /.fog/gate/* {")
		fogProxy(1)
		line(0, "}")
		line(0, "handle {")
		line(1, "forward_auth %s {", fogUp)
		line(2, "uri /api/authgate")
		// fogd trusts X-Forwarded-* only from a request carrying the proxy
		// secret (server.go, protect); the gate's sub-request needs them.
		line(2, "header_up %s {env.FOG_PROXY_SECRET}", HeaderSecret)
		line(2, "header_up %s %s", HeaderClientIP, client)
		line(2, "header_up %s %s", HeaderVia, via)
		proto(2)
		line(2, "copy_headers %s", strings.Join(gateHeaders, " "))
		line(1, "}")
		appProxy(1)
		line(0, "}")
	}
	return b.String()
}

// envFile is Caddy's .env, quoted as an app's is (apps.EnvFileLine): a
// token with a newline in it can't add a line of its own, compose settings
// among them.
func envFile(secret, token string) []byte {
	var b strings.Builder
	line, _ := apps.EnvFileLine("FOG_PROXY_SECRET", secret)
	b.WriteString(line)
	if token != "" {
		if line, err := apps.EnvFileLine("DNS_API_TOKEN", token); err == nil {
			b.WriteString(line)
		}
	}
	return []byte(b.String())
}

func (p *Proxy) composeFile() ([]byte, error) {
	image, _ := p.Store.GetSetting(SettingCaddyImage)
	if image == "" {
		image = DefaultCaddyImage
	}
	service := map[string]any{
		"image":          image,
		"container_name": "caddy",
		"restart":        "unless-stopped",
		// IPv4 only. Published on [::] too, an IPv6 client reached Caddy
		// through Docker's userland proxy, which hands the container the
		// bridge gateway as the peer — a private address — so a lanOnly
		// route let the IPv6 internet in. fog's DNS records are IPv4 only;
		// an IPv6 client is refused at once and its browser falls back to
		// IPv4, where the peer address is the client's own.
		// The ports come from fogports, the one list apps are refused.
		"ports":    fogports.Compose(fogports.Of("caddy")),
		"env_file": []string{".env"},
		"volumes": []string{
			// A directory, not the file: the config is staged beside the
			// live one and renamed into place once Caddy accepts it.
			"./conf:/etc/caddy:ro",
			"./data:/data",
			"./config:/config",
			// The tunnel listener's socket (TunnelSocket).
			"./" + tunnelDir + ":" + tunnelInContainer,
		},
		"extra_hosts": []string{"host.docker.internal:host-gateway"},
		// Neither proxy forges packets; without raw sockets it can't either.
		"cap_drop": []string{"MKNOD", "NET_RAW"},
		// Sealed apps get their routes through fog-sealed; fog is how Caddy
		// keeps its way out, for certificates.
		"networks": map[string]any{apps.NetworkName: map[string]any{}, apps.SealedNetworkName: map[string]any{}},
		"labels":   map[string]string{"fog.managed": "true", "fog.system": "caddy"},
	}
	doc := map[string]any{
		"name":     "fog-caddy",
		"services": map[string]any{"caddy": service},
		"networks": map[string]any{
			apps.NetworkName:       map[string]any{"external": true},
			apps.SealedNetworkName: map[string]any{"external": true},
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	header := "# Generated by Fog. Recovery without Fog: docker compose up -d\n"
	return []byte(header + string(out)), nil
}
