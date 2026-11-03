package wan

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mapping is what a router agreed to after Map: which method it spoke,
// which ports now reach this host, and for how long.
type Mapping struct {
	Method     string         `json:"method"` // "natpmp" or "upnp"
	Gateway    string         `json:"gateway"`
	ExternalIP string         `json:"externalIp,omitempty"` // the router's own WAN address
	Opened     []int          `json:"opened"`
	Failed     map[int]string `json:"failed,omitempty"`
	Lease      time.Duration  `json:"-"`
	// Permanent is true when the router only keeps mappings until it is
	// asked to drop them (or reboots) — nothing to renew.
	Permanent bool `json:"permanent"`
}

// Mapper opens ports on the default gateway. The zero value is the real
// thing; fields exist for tests.
type Mapper struct {
	Gateway func() (net.IP, error) // nil → Gateway()
	// Locations short-circuits SSDP discovery with known description URLs.
	Locations []string
	HTTP      *http.Client
	// NoNATPMP / NoUPnP skip a protocol (tests, and the day one turns out
	// to misbehave on some router).
	NoNATPMP, NoUPnP bool

	mu     sync.Mutex
	client *http.Client
	// The router's UPnP service, remembered per gateway so a lease renewal
	// (every 45 minutes) or an unmap doesn't repeat a 2.5s SSDP search and
	// the description fetch. Dropped when a call against it fails.
	igd   *igd
	igdGW string
}

func (m *Mapper) gateway() (net.IP, error) {
	if m.Gateway != nil {
		return m.Gateway()
	}
	return Gateway()
}

func (m *Mapper) httpClient() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client == nil {
		m.client = &http.Client{Timeout: 5 * time.Second}
	}
	return m.client
}

// findIGD returns the gateway's UPnP service: the remembered one when it
// belongs to gw, else discovered and resolved afresh.
func (m *Mapper) findIGD(ctx context.Context, gw net.IP) (*igd, error) {
	m.mu.Lock()
	if m.igd != nil && m.igdGW == gw.String() {
		g := m.igd
		m.mu.Unlock()
		return g, nil
	}
	m.mu.Unlock()
	locs := m.Locations
	if len(locs) == 0 {
		var err error
		locs, err = upnpDiscover(ctx, 2500*time.Millisecond)
		if err != nil {
			return nil, err
		}
	}
	lastErr := fmt.Errorf("no UPnP gateway found")
	for _, loc := range locs {
		g, err := upnpResolve(ctx, m.httpClient(), loc)
		if err != nil {
			lastErr = err
			continue
		}
		m.mu.Lock()
		m.igd, m.igdGW = g, gw.String()
		m.mu.Unlock()
		return g, nil
	}
	return nil, lastErr
}

// forgetIGD drops a remembered service that just failed, so the next call
// looks for the router again (it may have rebooted onto a new port).
func (m *Mapper) forgetIGD(g *igd) {
	m.mu.Lock()
	if m.igd == g {
		m.igd = nil
	}
	m.mu.Unlock()
}

// Map asks the router to send TCP ports to lanIP for lease. NAT-PMP goes
// first — one UDP round-trip, no XML — and UPnP is the fallback. Success
// means at least one port opened; the rest are reported in Failed so the
// caller can say which. Both protocols failing returns an error that
// names what each one said, since "port mapping failed" alone sends the
// operator to the router's admin page with no idea what to look for.
func (m *Mapper) Map(ctx context.Context, lanIP string, ports []int, lease time.Duration) (*Mapping, error) {
	gw, err := m.gateway()
	if err != nil {
		return nil, fmt.Errorf("finding the router: %w", err)
	}
	var reasons []string
	if !m.NoNATPMP {
		mp, err := m.mapNATPMP(ctx, gw, ports, lease)
		if err == nil {
			return mp, nil
		}
		reasons = append(reasons, "NAT-PMP: "+err.Error())
	}
	if !m.NoUPnP {
		mp, err := m.mapUPnP(ctx, gw, lanIP, ports, lease)
		if err == nil {
			return mp, nil
		}
		reasons = append(reasons, "UPnP: "+err.Error())
	}
	return nil, fmt.Errorf("%s", strings.Join(reasons, "; "))
}

func (m *Mapper) mapNATPMP(ctx context.Context, gw net.IP, ports []int, lease time.Duration) (*Mapping, error) {
	ext, err := natpmpExternalIP(ctx, gw)
	if err != nil {
		return nil, err
	}
	mp := &Mapping{Method: "natpmp", Gateway: gw.String(), ExternalIP: ext, Failed: map[int]string{}, Lease: lease}
	for _, p := range ports {
		granted, err := natpmpMapTCP(ctx, gw, p, uint32(lease/time.Second))
		if err != nil {
			mp.Failed[p] = err.Error()
			continue
		}
		mp.Opened = append(mp.Opened, p)
		if g := time.Duration(granted) * time.Second; g > 0 && g < mp.Lease {
			mp.Lease = g
		}
	}
	return finish(mp)
}

func (m *Mapper) mapUPnP(ctx context.Context, gw net.IP, lanIP string, ports []int, lease time.Duration) (*Mapping, error) {
	g, err := m.findIGD(ctx, gw)
	if err != nil {
		return nil, err
	}
	mp := &Mapping{Method: "upnp", Gateway: gw.String(), Failed: map[int]string{}, Lease: lease}
	if ext, err := g.externalIP(ctx); err == nil {
		mp.ExternalIP = ext
	}
	for _, p := range ports {
		granted, err := g.addMapping(ctx, lanIP, p, uint32(lease/time.Second))
		if err != nil {
			mp.Failed[p] = err.Error()
			continue
		}
		mp.Opened = append(mp.Opened, p)
		if granted == 0 {
			mp.Permanent = true
		}
	}
	if len(mp.Opened) == 0 {
		m.forgetIGD(g)
	}
	return finish(mp)
}

func finish(mp *Mapping) (*Mapping, error) {
	if len(mp.Opened) == 0 {
		var parts []string
		for p, why := range mp.Failed {
			parts = append(parts, fmt.Sprintf("port %d: %s", p, why))
		}
		sort.Strings(parts)
		return nil, fmt.Errorf("%s", strings.Join(parts, "; "))
	}
	sort.Ints(mp.Opened)
	if len(mp.Failed) == 0 {
		mp.Failed = nil
	}
	return mp, nil
}

// Unmap releases ports opened earlier by the same method. Best effort: a
// router that has forgotten the mapping (rebooted, lease expired) says so
// and that is fine.
func (m *Mapper) Unmap(ctx context.Context, method string, ports []int) error {
	gw, err := m.gateway()
	if err != nil {
		return err
	}
	switch method {
	case "natpmp":
		for _, p := range ports {
			natpmpMapTCP(ctx, gw, p, 0)
		}
		return nil
	case "upnp":
		g, err := m.findIGD(ctx, gw)
		if err != nil {
			return fmt.Errorf("no UPnP gateway to release the ports on: %w", err)
		}
		for _, p := range ports {
			g.deleteMapping(ctx, p)
		}
		return nil
	}
	return fmt.Errorf("unknown mapping method %q", method)
}
