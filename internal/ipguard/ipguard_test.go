package ipguard

import (
	"net"
	"testing"
)

// Every special-purpose range, and every IPv6 spelling of a private IPv4,
// must read as not public — otherwise a guard that relies on this lets a
// caller aim fog at a host it has no business reaching.
func TestPublicUnicast(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "100.127.255.254", // carrier-grade NAT
		"198.18.0.1", "198.19.255.254", // benchmarking
		"0.0.0.0", "0.1.2.3", // "this network"
		"192.0.0.9", "192.0.2.1", "198.51.100.7", "203.0.113.9", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fe80::1", "fc00::1", "fd12::1", "ff02::1", "2001:db8::1", "100::1",
		"::ffff:10.0.0.1",             // IPv4-mapped private
		"::ffff:127.0.0.1",            // IPv4-mapped loopback
		"::10.0.0.1",                  // IPv4-compatible
		"64:ff9b::10.0.0.1",           // NAT64 to a private IPv4
		"64:ff9b::a9fe:a9fe",          // NAT64 to the metadata address
		"64:ff9b:1::1",                // local-use NAT64
		"2002:0a00:0001::1",           // 6to4 of 10.0.0.1
		"2002:a9fe:a9fe::1",           // 6to4 of 169.254.169.254
		"2001:0:0a00:0001::ffff:ffff", // Teredo, server 10.0.0.1
		"2001:0:0808:0808::f5ff:fefe", // Teredo, client ^ -> 10.0.1.1
	} {
		if PublicUnicast(net.ParseIP(s)) {
			t.Errorf("%s judged public", s)
		}
	}
	for _, s := range []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111",
		"::ffff:8.8.8.8", "64:ff9b::808:808", "2002:0808:0808::1",
	} {
		if !PublicUnicast(net.ParseIP(s)) {
			t.Errorf("%s judged not public", s)
		}
	}
	if PublicUnicast(nil) {
		t.Error("nil judged public")
	}
}
