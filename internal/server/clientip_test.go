package server

import (
	"net/http/httptest"
	"testing"

	"gitea.lan/matthew/fog/internal/proxy"
)

// withProxy gives the test server a proxy over its own store and vault and
// returns the forward secret Caddy would present.
func withProxy(t *testing.T, srv *Server) string {
	t.Helper()
	srv.proxy = &proxy.Proxy{Store: srv.store, Vault: srv.vault}
	secret, err := srv.proxy.ForwardSecret()
	if err != nil || secret == "" {
		t.Fatalf("forward secret: %q %v", secret, err)
	}
	return secret
}

// Rate limits key on clientIP. A request Caddy forwards is keyed by the
// client Caddy saw; a LAN host claiming to be a proxy, by X-Forwarded-For or
// by Caddy's own header without the secret, is keyed by its own address.
func TestClientIPTrustsOnlyCaddy(t *testing.T) {
	srv, _ := newTestServer(t)
	secret := withProxy(t, srv)
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"direct", nil, "192.168.1.50"},
		{"spoofed xff", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "192.168.1.50"},
		{"client header without secret", map[string]string{proxy.HeaderClientIP: "203.0.113.9"}, "192.168.1.50"},
		{"wrong secret", map[string]string{proxy.HeaderClientIP: "203.0.113.9", proxy.HeaderSecret: "guess"}, "192.168.1.50"},
		{"through caddy", map[string]string{proxy.HeaderClientIP: "203.0.113.9", proxy.HeaderSecret: secret}, "203.0.113.9"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.168.1.50:5000"
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := srv.clientIP(r); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}

	// A fresh proxy over the same vault reads the same secret back rather
	// than minting a new one, so a fogd restart doesn't desync from Caddy.
	again, err := (&proxy.Proxy{Store: srv.store, Vault: srv.vault}).ForwardSecret()
	if err != nil || again != secret {
		t.Errorf("secret not stable across restarts: %q vs %q (%v)", again, secret, err)
	}
}
