package vpn

import (
	"encoding/json"
	"strings"
)

// tsStatus is the slice of `tailscale status --json` fogd reads. Decoding
// into a narrow struct rather than a map keeps an added field upstream from
// changing what this reports.
type tsStatus struct {
	BackendState   string   `json:"BackendState"`
	AuthURL        string   `json:"AuthURL"`
	HaveNodeKey    bool     `json:"HaveNodeKey"`
	Health         []string `json:"Health"`
	CurrentTailnet *struct {
		Name string `json:"Name"`
	} `json:"CurrentTailnet"`
	Self *struct {
		DNSName       string   `json:"DNSName"`
		Online        bool     `json:"Online"`
		TailscaleIPs  []string `json:"TailscaleIPs"`
		PrimaryRoutes []string `json:"PrimaryRoutes"`
	} `json:"Self"`
}

// Snapshot is what fogd learned from the daemon on its last look. Every
// field is a fact read from tailscaled; nothing here is inferred, and a
// Snapshot only ever reaches the API when the read that produced it
// succeeded.
type Snapshot struct {
	BackendState string   `json:"backendState"`
	AuthURL      string   `json:"authUrl,omitempty"`
	HaveNodeKey  bool     `json:"haveNodeKey"`
	Tailnet      string   `json:"tailnet,omitempty"`
	TailnetIP    string   `json:"tailnetIp,omitempty"`
	DNSName      string   `json:"dnsName,omitempty"`
	Online       bool     `json:"online"`
	Health       []string `json:"health,omitempty"`

	// Routes this node is actually serving, as the coordination server sees
	// it. Empty while the operator hasn't approved the subnet route yet —
	// which is the single most common reason a working setup doesn't work.
	PrimaryRoutes []string `json:"primaryRoutes,omitempty"`
	RouteApproved bool     `json:"routeApproved"`
}

// parseStatus reads the daemon's JSON and judges the advertised route
// against the routes the tailnet says this node is primary for.
func parseStatus(b []byte, advertised string) (Snapshot, error) {
	var raw tsStatus
	if err := json.Unmarshal(b, &raw); err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{
		BackendState: raw.BackendState,
		AuthURL:      raw.AuthURL,
		HaveNodeKey:  raw.HaveNodeKey,
		Health:       raw.Health,
	}
	if raw.CurrentTailnet != nil {
		s.Tailnet = raw.CurrentTailnet.Name
	}
	if raw.Self != nil {
		s.Online = raw.Self.Online
		s.DNSName = strings.TrimSuffix(raw.Self.DNSName, ".")
		s.PrimaryRoutes = raw.Self.PrimaryRoutes
		for _, ip := range raw.Self.TailscaleIPs {
			// The v4 address is the one worth showing: it's the one an
			// operator recognises and the one they'd type.
			if !strings.Contains(ip, ":") {
				s.TailnetIP = ip
				break
			}
		}
		if s.TailnetIP == "" && len(raw.Self.TailscaleIPs) > 0 {
			s.TailnetIP = raw.Self.TailscaleIPs[0]
		}
	}
	s.RouteApproved = routesApproved(advertised, s.PrimaryRoutes)
	return s, nil
}

// routesApproved reports whether every route fog advertises is one the
// tailnet has approved. All of them, not any: a partially approved set
// means some name still won't connect, and reporting that as approved is
// the lie the page exists to avoid.
func routesApproved(advertised string, primary []string) bool {
	want := strings.Split(advertised, ",")
	have := map[string]bool{}
	for _, r := range primary {
		have[strings.TrimSpace(r)] = true
	}
	n := 0
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if !have[w] {
			return false
		}
		n++
	}
	return n > 0
}
