package spa

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(h http.Handler, host string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = host
	SecurityHeaders(h).ServeHTTP(rec, r)
	return rec
}

func directives(policy string) map[string]string {
	out := map[string]string{}
	for _, d := range strings.Split(policy, ";") {
		f := strings.Fields(d)
		if len(f) > 0 {
			out[f[0]] = strings.Join(f[1:], " ")
		}
	}
	return out
}

// The policy itself: what an injected <script> or <base> or framing sibling
// app would need is exactly what it refuses.
func TestSecurityHeadersPolicy(t *testing.T) {
	rec := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html></html>"))
	}), "fog.example.com")
	h := rec.Header()
	csp := directives(h.Get("Content-Security-Policy"))
	want := map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self'",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
		"img-src":         "'self' blob: data:",
		"connect-src":     "'self' ws://fog.example.com wss://fog.example.com",
	}
	for k, v := range want {
		if csp[k] != v {
			t.Errorf("%s = %q, want %q", k, csp[k], v)
		}
	}
	if strings.Contains(csp["script-src"], "unsafe") || strings.Contains(csp["default-src"], "unsafe") {
		t.Errorf("script policy allows unsafe sources: %v", csp)
	}
	for k, v := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
		"X-Frame-Options":        "DENY",
	} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if h.Get("Permissions-Policy") == "" {
		t.Error("no Permissions-Policy")
	}
}

// A Host header is the client's to write; one that could smuggle a directive
// into the policy is left out of it.
func TestSecurityHeadersIgnoresHostileHost(t *testing.T) {
	rec := serve(http.NotFoundHandler(), "evil; script-src *")
	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "evil") || directives(csp)["script-src"] != "'self'" {
		t.Errorf("hostile Host reached the policy: %q", csp)
	}
}

// Routes that serve app or user bytes set a sandbox CSP of their own: it must
// survive untouched, and they stay frameable by fog itself (Drive's PDF
// preview) but by nobody else.
func TestSecurityHeadersKeepsSandbox(t *testing.T) {
	rec := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "sandbox")
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<script>alert(1)</script>"))
	}), "fog.example.com")
	got := rec.Header().Values("Content-Security-Policy")
	if len(got) != 2 || got[0] != "sandbox" || got[1] != "frame-ancestors 'self'" {
		t.Errorf("CSP = %q, want sandbox kept plus frame-ancestors 'self'", got)
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", xfo)
	}
}

// A page with its own policy that forgets frame-ancestors (the Drive share
// page, a sign-in page) still cannot be framed: a second policy says so.
func TestSecurityHeadersAddsFramingToOwnPolicy(t *testing.T) {
	rec := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.WriteHeader(http.StatusForbidden)
	}), "fog.example.com")
	got := rec.Header().Values("Content-Security-Policy")
	if len(got) != 2 || got[0] != "default-src 'none'; style-src 'unsafe-inline'" || got[1] != "frame-ancestors 'none'" {
		t.Errorf("CSP = %q", got)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options = %q", rec.Header().Get("X-Frame-Options"))
	}
	// One that names frame-ancestors itself is left alone.
	rec = serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	}), "fog.example.com")
	if got := rec.Header().Values("Content-Security-Policy"); len(got) != 1 {
		t.Errorf("CSP = %q, want the handler's alone", got)
	}
	// And a handler's own Referrer-Policy wins.
	rec = serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
	}), "fog.example.com")
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

// The terminal hijacks and the log stream flushes; the wrapper must not hide
// either from them.
func TestSecurityHeadersForwardsHijackAndFlush(t *testing.T) {
	inner := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("not a Flusher")
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("not a Hijacker")
		}
		hj.Hijack()
	})).ServeHTTP(inner, httptest.NewRequest("GET", "/", nil))
	if !inner.hijacked {
		t.Error("Hijack not forwarded")
	}
}
