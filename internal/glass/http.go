package glass

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Middleware makes each request a transaction named for the route pattern
// that served it ("GET /api/apps/{name}"), continuing the caller's
// traceparent. It goes directly around the ServeMux, which writes Pattern
// on the request it is handed. Upgrades, hijacked connections and responses
// that flush (the log streams) are not reported: how long one stays open is
// how long someone watched, not how slow fog was.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Default().active() || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, t := Start(WithTraceparent(r.Context(), r.Header.Get("traceparent")), "request", "")
		r = r.WithContext(ctx)
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		returned := false
		defer func() {
			if rec.streamed {
				t.Discard()
				return
			}
			status := rec.status
			if !returned {
				// A panic on its way to the recovery above us, which answers 500.
				status = http.StatusInternalServerError
				t.Fail("panic")
			} else if status >= 500 {
				t.Fail(fmt.Sprintf("http %d", status))
			}
			name, route := routeName(r.Pattern, status)
			t.SetName(name)
			// The route template stands in for the path: a path's segments
			// may be secrets (an invite, a share link, a webhook token), and
			// a trace keeps them. Unrouted paths are not recorded at all.
			t.HTTP(r.Method, route, route, status)
			t.End(nil)
		}()
		next.ServeHTTP(rec, r)
		returned = true
	})
}

// routeName is a request's name and route template from the pattern the
// mux matched. The catch-all "/" is the bundled UI.
func routeName(pattern string, status int) (name, route string) {
	route = pattern
	if _, path, ok := strings.Cut(pattern, " "); ok {
		route = path
	}
	switch {
	case pattern == "" && status == http.StatusNotFound:
		return "(404)", ""
	case pattern == "":
		return "(unrouted)", ""
	case route == "/":
		return "(static)", route
	}
	return pattern, route
}

type recorder struct {
	http.ResponseWriter
	status   int
	wrote    bool
	streamed bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush and Hijack are forwarded explicitly, as fog's own recorder does:
// the log stream type-asserts http.Flusher and the terminal http.Hijacker.
func (r *recorder) Flush() {
	r.streamed = true
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("connection does not support hijacking")
	}
	r.streamed = true
	return hj.Hijack()
}

// Transport makes each outbound call made inside a transaction an http
// span, and counts it toward the transaction's ext_ms. It passes
// traceparent only to private addresses and bare hostnames — the apps on
// this box and its LAN — never to the public internet.
type Transport struct {
	Base http.RoundTripper
}

func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	host := req.URL.Hostname()
	ctx, sp := StartSpan(req.Context(), "http", req.Method+" "+host, req.Method+" "+host+req.URL.Path)
	if sp == nil {
		return base.RoundTrip(req)
	}
	if private(host) {
		req = req.Clone(ctx)
		req.Header.Set("traceparent", Traceparent(ctx))
	}
	resp, err := base.RoundTrip(req)
	switch {
	case err != nil:
		sp.End(err)
	default:
		sp.Attr("status", resp.StatusCode)
		if resp.StatusCode >= 500 {
			sp.End(fmt.Errorf("http %d", resp.StatusCode))
		} else {
			sp.End(nil)
		}
	}
	return resp, err
}

func private(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast()
	}
	return !strings.Contains(host, ".") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal")
}
