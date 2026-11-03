// Package ipguard answers one question for every place fog dials an address
// it was handed by someone else — a link preview, a push endpoint, the
// pre-setup cloud-recovery door: is this a public unicast address, reachable
// on the open internet, and not a host inside the box, the LAN, a carrier
// NAT, or a special-purpose range? One check, shared, so the guards cannot
// drift apart.
//
// IPv6 forms that carry an IPv4 address inside (IPv4-mapped, IPv4-compatible,
// NAT64, 6to4, Teredo) are judged by the IPv4 address they lead to, so a
// private IPv4 cannot be reached by spelling it as IPv6.
package ipguard

import (
	"net"
	"net/netip"
)

// nonPublic lists the special-purpose ranges (IANA IPv4 and IPv6
// Special-Purpose Address Registries) that are not globally reachable.
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		// IPv4
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // carrier-grade NAT
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local (incl. cloud metadata)
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation (TEST-NET-1)
		"192.31.196.0/24", // AS112-v4
		"192.52.193.0/24", // AMT
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"192.168.0.0/16",  // private
		"192.175.48.0/24", // direct delegation AS112
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation (TEST-NET-2)
		"203.0.113.0/24",  // documentation (TEST-NET-3)
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, incl. broadcast
		// IPv6
		"::/128",        // unspecified
		"::1/128",       // loopback
		"100::/64",      // discard-only
		"2001:2::/48",   // benchmarking
		"2001:db8::/32", // documentation
		"2001:10::/28",  // ORCHID
		"2001:20::/28",  // ORCHIDv2
		"3fff::/20",     // documentation
		"5f00::/16",     // SRv6 SIDs
		"fc00::/7",      // unique local
		"fe80::/10",     // link-local
		"fec0::/10",     // deprecated site-local
		"ff00::/8",      // multicast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
	teredo     = netip.MustParsePrefix("2001::/32")
	v4compat   = netip.MustParsePrefix("::/96")
)

// PublicUnicast reports whether ip is a globally reachable unicast address.
func PublicUnicast(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return publicAddr(a.Unmap(), 0)
}

func publicAddr(a netip.Addr, depth int) bool {
	if !a.IsValid() || depth > 2 {
		return false
	}
	if a.Zone() != "" {
		return false // a scoped address is local by definition
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	if a.Is4() {
		return true
	}
	b := a.As16()
	v4 := func(off int) netip.Addr { return netip.AddrFrom4([4]byte{b[off], b[off+1], b[off+2], b[off+3]}) }
	switch {
	case nat64.Contains(a):
		return publicAddr(v4(12), depth+1)
	case nat64Local.Contains(a):
		return false // local-use NAT64: a translator inside some network
	case sixToFour.Contains(a):
		return publicAddr(v4(2), depth+1)
	case teredo.Contains(a):
		// Teredo carries the server's address in clear and the client's
		// obfuscated (bitwise not); both must be public.
		client := netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]})
		return publicAddr(v4(4), depth+1) && publicAddr(client, depth+1)
	case v4compat.Contains(a):
		return false // deprecated IPv4-compatible form (::a.b.c.d)
	}
	return true
}
