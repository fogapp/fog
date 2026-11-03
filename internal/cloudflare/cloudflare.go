// Package cloudflare is the thin API client behind Fog's domain wizard:
// verify the operator's token, find their zone, and manage the one wildcard
// record Fog needs. Cloudflare is a deliberate hard dependency — DNS-01 cert
// issuance already rides on it.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.cloudflare.com/client/v4"

type Client struct {
	Token string
	// HTTP is swappable for tests; nil gets the shared default with a
	// bounded timeout.
	HTTP *http.Client
	// Base overrides the API base URL (tests); empty means the real API.
	Base string
}

func New(token string) *Client {
	return &Client{Token: token}
}

// defaultHTTP is the one client every Client without an injected transport
// shares — built once, always with a timeout, so no code path can issue an
// unbounded API call.
var (
	defaultHTTPOnce sync.Once
	defaultHTTP     *http.Client
)

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	defaultHTTPOnce.Do(func() {
		defaultHTTP = &http.Client{Timeout: 20 * time.Second}
	})
	return defaultHTTP
}

func (c *Client) apiURL(path string) string {
	if c.Base != "" {
		return c.Base + path
	}
	return apiBase + path
}

type apiEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL(path), rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare unreachable: %w", err)
	}
	defer resp.Body.Close()
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare returned a non-API response (HTTP %d)", resp.StatusCode)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		if len(msgs) == 0 {
			msgs = append(msgs, fmt.Sprintf("HTTP %d", resp.StatusCode))
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// VerifyToken confirms the token is valid and active.
func (c *Client) VerifyToken(ctx context.Context) error {
	var res struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, "GET", "/user/tokens/verify", nil, &res); err != nil {
		return err
	}
	if res.Status != "active" {
		return fmt.Errorf("token status is %q", res.Status)
	}
	return nil
}

type Zone struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"` // "active" once nameservers point here
	NameServers []string `json:"name_servers"`
}

// FindZone locates the zone for a domain. The token must have access to it.
func (c *Client) FindZone(ctx context.Context, domain string) (*Zone, error) {
	var zones []Zone
	if err := c.do(ctx, "GET", "/zones?name="+domain, nil, &zones); err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		return nil, fmt.Errorf("no zone named %s is visible to this token — is the domain added to this Cloudflare account, and does the token cover it?", domain)
	}
	return &zones[0], nil
}

type Record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"` // seconds; 1 means "auto" on Cloudflare
}

// TTLs for the wildcard. A LAN address changes when the box moves house;
// a public one whenever the ISP feels like it — so the record that is
// asserted from outside carries the shortest TTL Cloudflare allows on an
// unproxied record, and a phone on the road recovers within a minute of
// the change instead of five.
const (
	TTLLAN = 300
	TTLWAN = 60
)

// Records lists the zone's address-ish records (A/AAAA/CNAME).
func (c *Client) Records(ctx context.Context, zoneID string) ([]Record, error) {
	var out []Record
	for _, typ := range []string{"A", "AAAA", "CNAME"} {
		var page []Record
		if err := c.do(ctx, "GET", "/zones/"+zoneID+"/dns_records?per_page=500&type="+typ, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	return out, nil
}

func addressType(t string) bool { return t == "A" || t == "AAAA" || t == "CNAME" }

// EnsureWildcard converges the zone on the single unproxied wildcard A
// record (*.domain → ip). One A record is kept and corrected; every other
// record on the wildcard name — a stray AAAA or CNAME, a surplus A — is
// deleted, because a stray pointing elsewhere shadows resolution
// nondeterministically, which is exactly the drift this package exists to
// prevent. Returns what it did: "created", "updated", or "ok".
func (c *Client) EnsureWildcard(ctx context.Context, zoneID, domain, ip string, ttl int) (string, error) {
	if ttl <= 0 {
		ttl = TTLLAN
	}
	wildcard := "*." + domain
	// Only the wildcard's own records: one request, however big the zone.
	var records []Record
	if err := c.do(ctx, "GET", "/zones/"+zoneID+"/dns_records?per_page=100&name="+url.QueryEscape(wildcard), nil, &records); err != nil {
		return "", err
	}
	var target *Record
	deleted := false
	for i := range records {
		r := &records[i]
		if r.Name != wildcard || !addressType(r.Type) {
			// A TXT or MX on the wildcard is someone's deliberate record,
			// not an address that could shadow ours.
			continue
		}
		if r.Type == "A" && target == nil {
			target = r
			continue
		}
		if err := c.do(ctx, "DELETE", "/zones/"+zoneID+"/dns_records/"+r.ID, nil, nil); err != nil {
			return "", err
		}
		deleted = true
	}
	body := map[string]any{"type": "A", "name": wildcard, "content": ip, "proxied": false, "ttl": ttl}
	switch {
	case target == nil:
		if err := c.do(ctx, "POST", "/zones/"+zoneID+"/dns_records", body, nil); err != nil {
			return "", err
		}
		return "created", nil
	case target.Content != ip || target.Proxied || target.TTL != ttl:
		// Wrong IP, proxied (private IPs can't be), or the wrong TTL for
		// how the record is being used — fix it.
		if err := c.do(ctx, "PUT", "/zones/"+zoneID+"/dns_records/"+target.ID, body, nil); err != nil {
			return "", err
		}
		return "updated", nil
	case deleted:
		return "updated", nil
	}
	return "ok", nil
}
