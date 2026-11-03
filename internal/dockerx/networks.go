package dockerx

// Per-app networks (docs/sealed-network.md, "Isolation"): fog gives every app
// a network of its own and every wire between two apps a network of its own,
// so an app reaches fog's proxy, its database and the apps it is wired to —
// not every container on the box. That is several networks per app, which
// Docker's default address pools (about thirty networks, a /16 or /20 each)
// would run out of, so fog hands each one a small subnet of its own choosing.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
)

// NetworkInfo is the part of a network listing fog reads.
type NetworkInfo struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Internal bool              `json:"Internal"`
	Labels   map[string]string `json:"Labels"`
	IPAM     struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// ListNetworks lists every network Docker has.
func (c *Client) ListNetworks(ctx context.Context) ([]NetworkInfo, error) {
	var out []NetworkInfo
	err := c.get(ctx, "/networks", &out)
	return out, err
}

// IsolatedSubnetBits is the size of each network fog allocates: a /27, room
// for 29 containers — an app's main replica and its swap twin, its workers,
// the proxy and the database, or the two ends of a wire.
const IsolatedSubnetBits = 27

// isolatedBases are where fog carves its subnets from, in order. They sit in
// Docker's own default range, which Docker skips once fog holds a piece of
// it, and anything a host interface already uses is skipped too.
var isolatedBases = []netip.Prefix{
	netip.MustParsePrefix("172.30.0.0/16"),
	netip.MustParsePrefix("172.31.0.0/16"),
	netip.MustParsePrefix("10.213.0.0/16"),
}

// allocMu serialises allocation within this process; Docker's own overlap
// check settles a race with anything else.
var allocMu sync.Mutex

// hostPrefixes are the subnets the host's interfaces sit on; overridable in
// tests.
var hostPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if p, err := netip.ParsePrefix(n.String()); err == nil && p.Addr().Is4() {
				out = append(out, p.Masked())
			}
		}
	}
	return out
}

// freeSubnet picks the first /IsolatedSubnetBits in isolatedBases that
// overlaps neither a network in taken nor a host interface.
func freeSubnet(taken []netip.Prefix) (netip.Prefix, error) {
	used := append(append([]netip.Prefix{}, taken...), hostPrefixes()...)
	for _, base := range isolatedBases {
		step := uint32(1) << (32 - IsolatedSubnetBits)
		start := base.Addr().As4()
		first := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
		count := uint32(1) << (IsolatedSubnetBits - base.Bits())
	next:
		for i := uint32(0); i < count; i++ {
			v := first + i*step
			cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), IsolatedSubnetBits)
			for _, u := range used {
				if u.Overlaps(cand) {
					continue next
				}
			}
			return cand, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("no free /%d subnet left for an app network", IsolatedSubnetBits)
}

func takenSubnets(nets []NetworkInfo) []netip.Prefix {
	var out []netip.Prefix
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if p, err := netip.ParsePrefix(c.Subnet); err == nil {
				out = append(out, p.Masked())
			}
		}
	}
	return out
}

// EnsureIsolatedNetwork creates a bridge network of fog's on a subnet fog
// chose, if it doesn't exist, and answers its IPv4 gateway. internal means
// no route out (Docker's flag); a network of that name whose flag differs is
// refused, never reused — Docker can't change it on a network that exists,
// and a sealed app's boundary rests on it.
func (c *Client) EnsureIsolatedNetwork(ctx context.Context, name string, internal bool, labels map[string]string) (gateway string, err error) {
	allocMu.Lock()
	defer allocMu.Unlock()
	for attempt := 0; attempt < 5; attempt++ {
		var n networkDetail
		err := c.get(ctx, "/networks/"+url.PathEscape(name), &n)
		if err == nil {
			if n.Internal != internal {
				if internal {
					return "", fmt.Errorf("network %s exists with a route out; remove it and fog will recreate it as internal", name)
				}
				return "", fmt.Errorf("network %s exists without a route out; remove it and fog will recreate it", name)
			}
			if gw := n.gateway(); gw != "" {
				return gw, nil
			}
			return "", fmt.Errorf("network %s has no IPv4 gateway", name)
		}
		if !hasStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("checking network %s: %w", name, err)
		}
		all, err := c.ListNetworks(ctx)
		if err != nil {
			return "", fmt.Errorf("listing networks: %w", err)
		}
		subnet, err := freeSubnet(takenSubnets(all))
		if err != nil {
			return "", err
		}
		body := map[string]any{
			"Name": name, "Driver": "bridge", "Internal": internal, "CheckDuplicate": true,
			"Labels": labels,
			"IPAM":   map[string]any{"Driver": "default", "Config": []map[string]string{{"Subnet": subnet.String()}}},
		}
		err = c.do(ctx, c.http, http.MethodPost, "/networks/create", body, nil)
		if err == nil || hasStatus(err, http.StatusConflict) {
			continue // read it back (or the one someone else just made)
		}
		// Docker refuses a subnet something else took since the listing:
		// list again and take the next.
		if strings.Contains(err.Error(), "overlap") {
			continue
		}
		return "", fmt.Errorf("creating network %s: %w", name, err)
	}
	return "", fmt.Errorf("could not create network %s", name)
}

// NetworkConnect attaches a container to a network, answering to aliases
// there. Already attached is success.
func (c *Client) NetworkConnect(ctx context.Context, network, container string, aliases []string) error {
	body := map[string]any{"Container": container}
	if len(aliases) > 0 {
		body["EndpointConfig"] = map[string]any{"Aliases": aliases}
	}
	err := c.do(ctx, c.http, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", body, nil)
	if err != nil && (hasStatus(err, http.StatusForbidden) || hasStatus(err, http.StatusConflict)) &&
		strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return err
}

// RemoveNetwork deletes a network; one that is already gone is success.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	err := c.do(ctx, c.http, http.MethodDelete, "/networks/"+url.PathEscape(name), nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// NetworksByLabel lists the networks carrying label ("key" or "key=value").
func (c *Client) NetworksByLabel(ctx context.Context, label string) ([]NetworkInfo, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []NetworkInfo
	err := c.get(ctx, "/networks?filters="+url.QueryEscape(string(filters)), &out)
	return out, err
}
