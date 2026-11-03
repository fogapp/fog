package wan

import "net"

// LANIP is this machine's primary LAN address: the source address the
// kernel would pick to reach the internet. No packets are sent; a UDP
// "dial" only selects the route. "" when there is no route out.
//
// Everything that says or asserts where fog lives on the LAN — the DNS
// keeper's lan target, the domain wizard, the VPN's advertised route, the
// network watcher — reads it here, so none of them can disagree.
func LANIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
