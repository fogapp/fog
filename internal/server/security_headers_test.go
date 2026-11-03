package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every page fog serves carries a CSP that forbids inline script and plugins,
// plus nosniff and a referrer policy — and none of them can be framed by
// another origin. A sibling app is same-site, so without frame-ancestors it
// could frame the console, /device or a sign-in prompt with the household's
// cookies attached and clickjack the person into approving something.
func TestSecurityHeadersOnEveryPage(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	pages := []struct {
		name, path string
		signedIn   bool
	}{
		{"SPA index", "/", true},
		{"console route (deep link)", "/apps/nextcloud/settings", true},
		{"device sign-in", "/device", false},
		{"visit authorize (server-rendered)", "/visit/authorize", true},
		{"oauth2 authorize (server-rendered)", "/oauth2/authorize?client_id=x", true},
		{"reach beacon (server-rendered)", "/api/reach/beacon/not-a-token", false},
		{"API JSON", "/api/setup/status", false},
	}
	for _, p := range pages {
		var opts []func(*http.Request)
		if p.signedIn {
			opts = append(opts, withCookie(c))
		}
		w := do(t, h, "GET", p.path, "", opts...)
		hd := w.Header()
		if got := hd.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", p.name, got)
		}
		if hd.Get("Referrer-Policy") == "" {
			t.Errorf("%s: no Referrer-Policy", p.name)
		}
		if got := hd.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", p.name, got)
		}
		all := strings.Join(hd.Values("Content-Security-Policy"), " , ")
		if !strings.Contains(all, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q does not forbid framing", p.name, all)
		}
		// The page's own scripting policy: nothing inline, nothing from
		// elsewhere, no plugins, no <base> hijack.
		first := hd.Get("Content-Security-Policy")
		for _, want := range []string{"base-uri 'none'"} {
			if !strings.Contains(first, want) {
				t.Errorf("%s: CSP %q lacks %s", p.name, first, want)
			}
		}
		if strings.Contains(first, "unsafe-inline") && !strings.Contains(first, "script-src 'self'") && !strings.Contains(first, "default-src 'none'") {
			t.Errorf("%s: CSP %q allows inline script", p.name, first)
		}
		if strings.Contains(first, "default-src 'self'") {
			for _, want := range []string{"script-src 'self';", "object-src 'none'"} {
				if !strings.Contains(first, want) {
					t.Errorf("%s: CSP %q lacks %s", p.name, first, want)
				}
			}
		}
	}
}

// Drive's raw route serves the person's own bytes (an HTML file is just a
// file) under "sandbox". The site-wide policy must not replace it, and the
// route stays frameable by fog's own PDF preview but by no other origin.
func TestSecurityHeadersKeepFileSandbox(t *testing.T) {
	s, h := newTestServer(t)
	c := setup(t, h)
	os.MkdirAll(s.files.Root(), 0o755)
	os.WriteFile(filepath.Join(s.files.Root(), "page.html"), []byte("<script>alert(1)</script>"), 0o644)

	w := do(t, h, "GET", "/api/files/raw/page.html", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("raw: %d %s", w.Code, w.Body)
	}
	got := w.Header().Values("Content-Security-Policy")
	if len(got) == 0 || got[0] != "sandbox" {
		t.Fatalf("CSP = %q, want the route's own sandbox first", got)
	}
	for _, p := range got {
		if strings.Contains(p, "script-src") || strings.Contains(p, "default-src 'self'") {
			t.Errorf("site policy leaked onto the sandboxed route: %q", got)
		}
	}
	if !strings.Contains(strings.Join(got, ","), "frame-ancestors 'self'") || w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("sandboxed route framing: CSP %q, XFO %q", got, w.Header().Get("X-Frame-Options"))
	}
}
