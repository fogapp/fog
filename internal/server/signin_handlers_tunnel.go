// Which way Cloudflare Tunnel visitors come in (docs/signin-contract.md,
// "Tunnel visitors are each their own"). fog can't change a tunnel's
// service itself: the tunnel is the operator's, made in the Zero Trust
// dashboard, and fog holds only its connector token and a DNS-only API
// token. So fogd notes where tunnel traffic arrives — Caddy's tunnel
// listener, or :443 from the host's own address with Cf-Connecting-Ip, a
// tunnel still pointed at localhost:443 — and the issues sweep tells the
// operator until the tunnel listener is the one in use
// (internal/issues, checkTunnelService).
package server

import (
	"net"
	"net/http"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/proxy"
)

// tunnelService is the service a tunnel's public hostnames must name.
func (s *Server) tunnelService() string {
	if s.proxy != nil {
		return s.proxy.TunnelService()
	}
	return proxy.TunnelServiceIn(s.cfg.DataDir)
}

// tunnelNoteEvery bounds how often a sighting is written: the sweep reads
// minutes, not requests.
const tunnelNoteEvery = time.Minute

// tunnelSightings throttles noteTunnelPath's writes, per Server.
type tunnelSightings struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// noteTunnelPath records a tunnel visitor's request by the listener it came
// in on. Caddy says which (proxy.HeaderVia), and only with the proxy
// secret: dropUnprovenForwarding removed it from anyone else.
func (s *Server) noteTunnelPath(r *http.Request) {
	var key string
	switch r.Header.Get(proxy.HeaderVia) {
	case proxy.ViaTunnel:
		key = proxy.SettingTunnelSeenListener
	case proxy.ViaEdge:
		if r.Header.Get("Cf-Connecting-Ip") == "" {
			return
		}
		// Only from one of this host's own addresses — the bridge gateway
		// a tunnel on :443 arrives from. Never from a container's address
		// (an app's, or Caddy's own), never from the LAN: those sending
		// Cf-Connecting-Ip are someone's word, not a tunnel.
		a, ok := parseClientAddr(s.clientIP(r))
		if !ok || !s.signinLimits().isHostAddr(net.IP(a.AsSlice())) {
			return
		}
		key = proxy.SettingTunnelSeenEdge
	default:
		return
	}
	now := time.Now().UTC()
	seen := &s.signinLimits().tunnelSeen
	seen.mu.Lock()
	if seen.last == nil {
		seen.last = map[string]time.Time{}
	}
	if now.Sub(seen.last[key]) < tunnelNoteEvery {
		seen.mu.Unlock()
		return
	}
	seen.last[key] = now
	seen.mu.Unlock()
	if err := s.store.SetSetting(key, now.Format(time.RFC3339)); err != nil {
		s.log.Warn("recording where tunnel traffic arrives", "err", err)
	}
}
