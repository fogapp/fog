package auth

import (
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// HostAllowed says whether host — a request's Host, with or without a port —
// names this box: one of its own addresses, a loopback name, its hostname or
// hostname.local, or one of names (fog.<domain> and the household's other
// hosts, an advertised or tunnel address). A browser tricked by DNS
// rebinding sends an attacker's name here, so a request from a browser that
// names anything else is not for us.
func HostAllowed(host string, names ...string) bool {
	h := strings.ToLower(strings.TrimSuffix(stripHostPort(host), "."))
	if h == "" {
		return false
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback() || localAddrs().has(ip.String())
	}
	if name, err := os.Hostname(); err == nil {
		name = strings.ToLower(name)
		short, _, _ := strings.Cut(name, ".")
		if h == name || h == short || h == short+".local" || h == name+".local" {
			return true
		}
	}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSuffix(stripHostPort(n), "."))
		if n == "" {
			continue
		}
		if strings.HasPrefix(n, "*.") {
			if strings.HasSuffix(h, n[1:]) {
				return true
			}
			continue
		}
		if h == n {
			return true
		}
	}
	return false
}

func stripHostPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

type addrSet struct {
	mu  sync.Mutex
	set map[string]bool
	at  time.Time
}

var local addrSet

func localAddrs() *addrSet { return &local }

// has reports whether ip is one of this host's interface addresses, read
// again at most once a minute (a DHCP lease moves them).
func (a *addrSet) has(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.set == nil || time.Since(a.at) > time.Minute {
		a.set = map[string]bool{}
		if list, err := net.InterfaceAddrs(); err == nil {
			for _, x := range list {
				if n, ok := x.(*net.IPNet); ok {
					a.set[n.IP.String()] = true
				}
			}
		}
		a.at = time.Now()
	}
	return a.set[ip]
}
