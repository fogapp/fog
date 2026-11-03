package chat

import (
	"errors"
	"gitea.lan/matthew/fog/internal/ipguard"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// parsePublicURL accepts an http(s) URL whose host resolves only to public
// addresses. Anything else — loopback, RFC 1918, link-local, ULA, IPv4-mapped
// tricks — is refused before a single byte is fetched, so a message cannot
// make fog probe its own network.
func parsePublicURL(raw string, resolve func(host string) ([]string, error)) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", previewDenied
	}
	if u.User != nil {
		return "", previewDenied
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") || strings.HasSuffix(strings.ToLower(host), ".lan") {
		return "", previewDenied
	}
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return "", previewDenied
		}
		return u.String(), nil
	}
	if resolve == nil {
		resolve = net.LookupHost
	}
	addrs, err := resolve(host)
	if err != nil || len(addrs) == 0 {
		return "", errors.New("that host does not resolve")
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !publicIP(ip) {
			return "", previewDenied
		}
	}
	return u.String(), nil
}

// previewClient fetches only from public addresses, checked on the address
// actually dialed — after resolution, so DNS rebinding can't slip a private
// one past the URL check — and never through a proxy, which would dial on
// our behalf where this check can't see.
func previewClient() *http.Client {
	d := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return previewDenied
			}
			if ip := net.ParseIP(host); ip == nil || !publicIP(ip) {
				return previewDenied
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   6 * time.Second,
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, TLSHandshakeTimeout: 5 * time.Second},
	}
}

// publicIP is the shared public-unicast rule (internal/ipguard): every
// special-purpose range and every IPv6 spelling of a private IPv4 is refused,
// the same rule the push-endpoint and cloud-recovery guards apply.
func publicIP(ip net.IP) bool { return ipguard.PublicUnicast(ip) }

// CheckPublicURL is the preview fetcher's guard for any URL fog will dial
// on someone's say-so — a push subscription's endpoint, say: https only
// when https is asked, public addresses only.
//
// A host that doesn't resolve now is let through here: the dial is checked
// again, on the address actually dialed, when fog sends (previewClient).
func CheckPublicURL(raw string, httpsOnly bool) error {
	u, err := parsePublicURL(raw, func(host string) ([]string, error) {
		addrs, err := net.LookupHost(host)
		if err != nil {
			return []string{"8.8.8.8"}, nil // unresolvable: judged at dial time
		}
		return addrs, nil
	})
	if err != nil {
		return err
	}
	if httpsOnly && !strings.HasPrefix(u, "https://") {
		return previewDenied
	}
	return nil
}
