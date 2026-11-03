package spa

import (
	"bufio"
	"net"
	"net/http"
	"strings"
)

// ContentSecurityPolicy is the policy every fog page is served under unless
// its handler sets a stricter one of its own: the dashboard SPA, the setup
// wizard, and the plain server-rendered pages (sign-in dead ends, the reach
// beacon, the gate's refusal).
//
// Scripts come from fog's own origin and nowhere else — no inline script,
// no eval — so markup that slips past escaping (a chat message, an app's
// name) has nothing to run. The theme bootstrap that used to sit inline in
// index.html is /theme-boot.js for that reason.
//
// Styles allow 'unsafe-inline' and nothing else does: xterm.js builds its
// theme in a <style> element at runtime, and the server-rendered pages and
// placeholders carry style attributes. CSS cannot run script.
//
// Images allow blob: (chat attachments decrypted in the browser, Drive
// previews) and data: (inlined icons); media allows blob: for decrypted chat
// video and audio; fonts allow data: for any Vite inlines. connect-src names
// the page's own ws:/wss: origin as well as 'self', for browsers that do not
// yet count same-origin websockets as 'self'. frame-src stays 'self' — Drive's
// PDF preview frames fog's own sandboxed file route.
const ContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' blob: data:; " +
	"media-src 'self' blob:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'%s; " +
	"worker-src 'self'; " +
	"frame-src 'self'; " +
	"manifest-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// PermissionsPolicy turns off the powerful features fog's UI never uses, so
// nothing running in a fog page (or framed by one) can ask for them. The
// camera, microphone and the rest stay available to nothing; WebAuthn and
// clipboard writes are not gated by this list.
const PermissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), hid=(), bluetooth=(), midi=()"

// policyFor renders ContentSecurityPolicy for a request, adding the page's
// own websocket origin when the Host header is a plain host[:port].
func policyFor(r *http.Request) string {
	ws := ""
	if h := r.Host; h != "" && plainHost(h) {
		ws = " ws://" + h + " wss://" + h
	}
	return strings.Replace(ContentSecurityPolicy, "%s", ws, 1)
}

func plainHost(h string) bool {
	if len(h) > 255 {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']':
		default:
			return false
		}
	}
	return true
}

// SecurityHeaders wraps a handler so every response carries fog's baseline
// security headers:
//
//   - X-Content-Type-Options: nosniff, Referrer-Policy: same-origin and
//     Permissions-Policy, on everything. A handler that sets its own
//     Referrer-Policy (no-referrer on the sign-in redirects) keeps it.
//   - Content-Security-Policy: the policy above, unless the handler sets its
//     own. Routes that serve app or user bytes set "sandbox", and a
//     sign-in page sets a tighter default-src 'none'; neither is replaced.
//   - Framing: no fog response can be framed by another origin — a sibling
//     app is same-site, so it could otherwise frame the console or a sign-in
//     prompt with the household's cookies attached. A response whose own CSP
//     names no frame-ancestors gets a second policy header saying so (a
//     browser enforces every policy it is sent), plus X-Frame-Options for
//     browsers that predate frame-ancestors. A sandboxed file route is the
//     one thing fog frames itself (Drive's PDF preview), so it gets 'self'
//     and SAMEORIGIN; everything else gets 'none' and DENY.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", PermissionsPolicy)
		h.Set("Content-Security-Policy", policyFor(r))
		next.ServeHTTP(&headerWriter{ResponseWriter: w}, r)
	})
}

// finishFraming settles the framing headers once the handler has had its say
// on the CSP, just before the header block goes out.
func finishFraming(h http.Header) {
	if h.Get("X-Frame-Options") != "" {
		return
	}
	csps := h.Values("Content-Security-Policy")
	sandboxed := false
	for _, p := range csps {
		if strings.Contains(p, "frame-ancestors") {
			h.Set("X-Frame-Options", xfoFor(p))
			return
		}
		for _, d := range strings.Split(p, ";") {
			if f := strings.Fields(d); len(f) > 0 && strings.EqualFold(f[0], "sandbox") {
				sandboxed = true
			}
		}
	}
	if sandboxed {
		h.Add("Content-Security-Policy", "frame-ancestors 'self'")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		return
	}
	h.Add("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
}

func xfoFor(policy string) string {
	if strings.Contains(policy, "frame-ancestors 'none'") {
		return "DENY"
	}
	return "SAMEORIGIN"
}

// headerWriter applies finishFraming at the last moment headers can change.
// Flush and Hijack are forwarded explicitly, as fog's other recorders do:
// the log stream type-asserts http.Flusher and the terminal http.Hijacker.
type headerWriter struct {
	http.ResponseWriter
	done bool
}

func (w *headerWriter) finish() {
	if !w.done {
		w.done = true
		finishFraming(w.ResponseWriter.Header())
	}
}

func (w *headerWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.finish()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *headerWriter) Write(b []byte) (int, error) {
	w.finish()
	return w.ResponseWriter.Write(b)
}

func (w *headerWriter) Flush() {
	w.finish()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *headerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}

func (w *headerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
