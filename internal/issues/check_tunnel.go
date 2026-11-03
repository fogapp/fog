package issues

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/tunnel"
)

// checkTunnelService: a Cloudflare Tunnel whose service still names
// https://localhost:443. Caddy's tunnel listener (proxy.TunnelPort) is the
// only way fog can tell one tunnel visitor from another; on :443 every
// visitor arrives from the host's own address, and fog signs none of them
// in. fog can't change the service itself — the tunnel is the operator's,
// made in Cloudflare's dashboard, and fog holds only its connector token
// and a DNS-only API token — so it says exactly what to change, until fogd
// sees tunnel traffic arrive through the tunnel listener after any it saw
// arrive on :443 (internal/server, noteTunnelPath).
//
// A tunnel serving only hostnames that never reach fogd (open apps, no
// gate) shows fogd nothing either way; it clears by acknowledgement.
func (sc *Scanner) checkTunnelService(_ context.Context, _ *snapshot) report {
	if _, err := sc.Store.GetSecret(0, tunnel.SecretToken); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return report{evaluated: []int64{0}} // no tunnel
		}
		return report{}
	}
	rep := report{evaluated: []int64{0}}
	listener := seenAt(sc.Store, proxy.SettingTunnelSeenListener)
	edge := seenAt(sc.Store, proxy.SettingTunnelSeenEdge)
	if !listener.IsZero() && !listener.Before(edge) {
		return rep
	}
	service := proxy.TunnelServiceIn(sc.DataDir)
	detail := "Visitors through the tunnel reach fog's proxy on port 443, where every one of them arrives from this machine's own address and fog can't tell them apart. They can open apps that don't ask who they are, but fog signs none of them in, and no gated app lets them through."
	if !edge.IsZero() {
		detail = fmt.Sprintf("fog last saw a tunnel visitor arrive on port 443 at %s UTC. ", edge.UTC().Format("2006-01-02 15:04")) + detail
	} else {
		detail = "fog hasn't seen a visitor arrive through its tunnel listener yet. " + detail
	}
	rep.findings = append(rep.findings, store.IssueFinding{
		Kind: "tunnel-service", Subject: "tunnel", Severity: "critical",
		Title:  "Point your Cloudflare Tunnel at " + service,
		Detail: detail,
		Fix: "In the Cloudflare Zero Trust dashboard, open Networks → Tunnels → your tunnel → Public Hostnames, and for every hostname change the service from https://localhost:443 to type UNIX, path " +
			strings.TrimPrefix(service, "unix:") + " (that is, " + service + "). This clears once a visitor arrives through the new service; for a tunnel with no hostname fog itself answers, acknowledge it once the change is made.",
		FixURL: "/settings/network/tunnel",
	})
	return rep
}

// seenAt reads a sighting time, zero when there is none.
func seenAt(st *store.Store, key string) time.Time {
	v, _ := st.GetSetting(key)
	t, _ := time.Parse(time.RFC3339, v)
	return t
}
