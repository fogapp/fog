package vpn

import (
	"fmt"
	"net/netip"
	"strings"
)

// AutoRoute is the route fog advertises when the operator hasn't overridden
// it: a single host route to fog's own LAN address.
//
// A /32 rather than the whole LAN is deliberate. Advertising 192.168.1.0/24
// collides with every café, hotel and parents' house that numbers its
// network the same way, and the colliding route wins on the travelling
// laptop — so the one setup that must work from anywhere breaks in exactly
// the places you're away from home. A single host route is the smallest
// thing that makes `*.<domain>` resolve and connect, and it is the least
// likely address on the LAN to collide with anything.
func AutoRoute(lanIP string) (string, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(lanIP))
	if err != nil {
		return "", fmt.Errorf("no usable LAN address yet")
	}
	if addr.Is6() {
		return addr.String() + "/128", nil
	}
	return addr.String() + "/32", nil
}

// ParseRoutes normalizes an operator's override — comma- or
// space-separated CIDRs — into the form `tailscale up --advertise-routes`
// wants, rejecting anything that isn't a prefix. A bare address is read as
// a host route, since that's what someone typing one means.
func ParseRoutes(s string) (string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	var out []string
	for _, f := range fields {
		if !strings.Contains(f, "/") {
			r, err := AutoRoute(f)
			if err != nil {
				return "", fmt.Errorf("%q is not an address or a CIDR range", f)
			}
			out = append(out, r)
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return "", fmt.Errorf("%q is not a CIDR range", f)
		}
		// Masked() so 192.168.1.200/24 becomes 192.168.1.0/24 rather than
		// being refused by tailscaled after fogd has already reported success.
		out = append(out, p.Masked().String())
	}
	if len(out) == 0 {
		return "", fmt.Errorf("no routes given")
	}
	return strings.Join(out, ","), nil
}

// Hostname is the name this node takes in the tailnet: the server's display
// name if it makes a legal DNS label, else "fog". MagicDNS would mangle
// anything else, and a mangled name is one the operator can't predict.
func Hostname(serverName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(serverName)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == ' ' || r == '.' || r == '_':
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	if name == "" {
		return "fog"
	}
	return name
}
