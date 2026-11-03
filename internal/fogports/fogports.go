// Package fogports is the one list of the host ports fog itself holds:
// those its own containers publish, fogd's own listener, and the window of
// direct ports fogd forwards to apps. fog's compose files publish from this
// list (internal/proxy), the direct-port allocator hands out from its window
// (internal/store), and an app is refused every port it names
// (internal/apps). A fog service that comes to publish a port adds it here,
// and apps are refused it from then on without anyone listing it twice.
package fogports

import (
	"fmt"
	"slices"
)

// Port is one host port a fog container publishes.
type Port struct {
	Service   string // the fog container publishing it
	Bind      string // host address it binds
	Host      int
	Container int
	Proto     string // tcp | udp
}

// Caddy's: plain HTTP, HTTPS and HTTP/3. IPv4 only, so a LAN-only route's
// remote_ip check never meets an IPv6 client it can't place.
var caddy = []Port{
	{Service: "caddy", Bind: "0.0.0.0", Host: 80, Container: 80, Proto: "tcp"},
	{Service: "caddy", Bind: "0.0.0.0", Host: 443, Container: 443, Proto: "tcp"},
	{Service: "caddy", Bind: "0.0.0.0", Host: 443, Container: 443, Proto: "udp"},
}

// The direct ports: fogd listens on each routed app's (internal/portfwd)
// and dials the app's container, so a tunnel route can target
// localhost:<port>.
const (
	DirectMin = 20000
	DirectMax = 20999
)

// Published is every host port fog's own containers publish, for whichever
// fog service. fog-wire and fog-postgres publish none; fog's tunnel reaches
// Caddy over a unix socket, not a port.
func Published() []Port { return slices.Clone(caddy) }

// Of is the ports one fog service publishes.
func Of(service string) []Port {
	var out []Port
	for _, p := range Published() {
		if p.Service == service {
			out = append(out, p)
		}
	}
	return out
}

// Compose renders ports in compose's short syntax, "bind:host:container"
// with "/udp" where it isn't TCP.
func Compose(ps []Port) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		s := fmt.Sprintf("%s:%d:%d", p.Bind, p.Host, p.Container)
		if p.Proto != "" && p.Proto != "tcp" {
			s += "/" + p.Proto
		}
		out = append(out, s)
	}
	return out
}

// Owner names what of fog holds host port p, whichever protocol, or "" when
// nothing does. fogd is fogd's own listening port, 0 when unknown.
func Owner(p, fogd int) string {
	for _, q := range Published() {
		if q.Host == p {
			return "fog's " + q.Service
		}
	}
	switch {
	case fogd != 0 && p == fogd:
		return "fogd"
	case p >= DirectMin && p <= DirectMax:
		return "fog's direct ports"
	}
	return ""
}
