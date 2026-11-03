package wan

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveProbes talks to the real network — the public-address sources,
// the default gateway, and whatever router protocols answer — without
// changing anything (no mapping is requested). Opt in with FOG_WAN_LIVE=1;
// it is a way to see what a given house looks like to fog, not a CI test.
func TestLiveProbes(t *testing.T) {
	if os.Getenv("FOG_WAN_LIVE") == "" {
		t.Skip("set FOG_WAN_LIVE=1 to probe the real network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ip, err := PublicIP(ctx)
	t.Logf("public address: %q (%v) → %s", ip, err, Classify(ip))
	gw, err := Gateway()
	t.Logf("gateway: %v (%v)", gw, err)
	if gw == nil {
		return
	}
	ext, err := natpmpExternalIP(ctx, gw)
	t.Logf("NAT-PMP external address: %q (%v)", ext, err)
	locs, err := upnpDiscover(ctx, 2500*time.Millisecond)
	t.Logf("UPnP descriptions: %v (%v)", locs, err)
	for _, loc := range locs {
		g, err := upnpResolve(ctx, (&Mapper{}).httpClient(), loc)
		if err != nil {
			t.Logf("  %s: %v", loc, err)
			continue
		}
		ext, err := g.externalIP(ctx)
		t.Logf("  %s → %s external %q (%v)", loc, g.ServiceType, ext, err)
	}
}
