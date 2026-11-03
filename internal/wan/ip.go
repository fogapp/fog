// Package wan is what fog knows about the far side of the router: the
// address the internet sees this house at, the gateway that hides it, and
// the two protocols (NAT-PMP, UPnP IGD) most home routers speak for opening
// a port without anyone logging in to the admin page. It exists so the
// domain wizard can point DNS at the house from outside and keep it
// pointed there when the ISP hands out a new address — a dynamic-DNS client
// and a port-forwarding hand, in one place.
package wan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Source is one way of asking "what address do you see me from?".
type Source struct {
	URL   string
	Parse func(body string) string
}

// DefaultSources is consulted in order; the first that answers with an IPv4
// address wins. Cloudflare first — fog already depends on it, and the trace
// endpoint is tiny and has no rate limit worth mentioning — with ipify as
// the second opinion for the day 1.1.1.1 is blocked on some network.
var DefaultSources = []Source{
	{URL: "https://1.1.1.1/cdn-cgi/trace", Parse: parseTrace},
	{URL: "https://www.cloudflare.com/cdn-cgi/trace", Parse: parseTrace},
	{URL: "https://api.ipify.org", Parse: strings.TrimSpace},
}

func parseTrace(body string) string {
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ip="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ipv4Client dials over IPv4 only. The wildcard is an A record, so the
// address we assert must be the one IPv4 traffic arrives from — a
// dual-stack host would otherwise learn its IPv6 address from a source
// that prefers v6 and publish something an A record cannot hold.
var ipv4Client = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", addr)
		},
		DisableKeepAlives: true,
	},
}

// PublicIP asks the sources for this house's public IPv4 address. The
// answer is the address the internet sees — which, behind carrier-grade
// NAT, is not an address any router of ours can forward a port on; see
// Classify.
func PublicIP(ctx context.Context) (string, error) {
	return PublicIPFrom(ctx, ipv4Client, DefaultSources)
}

// PublicIPFrom is PublicIP with the client and sources injected (tests).
func PublicIPFrom(ctx context.Context, client *http.Client, sources []Source) (string, error) {
	var errs []string
	for _, src := range sources {
		ip, err := askSource(ctx, client, src)
		if err == nil {
			return ip, nil
		}
		errs = append(errs, err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 0 {
		return "", errors.New("no public-address sources configured")
	}
	return "", fmt.Errorf("couldn't learn the public address: %s", strings.Join(errs, "; "))
}

func askSource(ctx context.Context, client *http.Client, src Source) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", src.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", src.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("%s: %w", src.URL, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%s: HTTP %d", src.URL, resp.StatusCode)
	}
	ip := net.ParseIP(src.Parse(string(body)))
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("%s: no IPv4 address in the answer", src.URL)
	}
	return ip.To4().String(), nil
}

// Kind classifies an address by who can route to it.
type Kind string

const (
	KindPublic  Kind = "public"
	KindPrivate Kind = "private" // RFC 1918, link-local, loopback
	KindCGNAT   Kind = "cgnat"   // RFC 6598 100.64.0.0/10: the ISP's own NAT
	KindInvalid Kind = "invalid"
)

var cgnatNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// Classify says whether an address is one the internet can reach. A
// router whose WAN side sits in the CGNAT range — or in private space, a
// second router upstream — cannot be port-forwarded to from outside no
// matter what its admin page says.
func Classify(s string) Kind {
	ip := net.ParseIP(s)
	if ip == nil {
		return KindInvalid
	}
	if cgnatNet.Contains(ip) {
		return KindCGNAT
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return KindPrivate
	}
	return KindPublic
}
