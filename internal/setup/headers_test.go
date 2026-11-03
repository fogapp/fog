package setup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The installer wizard is a page too: it carries fog's CSP and cannot be
// framed, like the dashboard (internal/spa/headers.go).
func TestHandlerSecurityHeaders(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><title>Fog Setup</title>"))
	})
	for _, p := range []string{"/", "/api/setup/state"} {
		rec := httptest.NewRecorder()
		(&Agent{}).Handler(ui).ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		h := rec.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"script-src 'self';", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %s", p, csp, want)
			}
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") == "" {
			t.Errorf("%s: headers = %v", p, h)
		}
	}
}
