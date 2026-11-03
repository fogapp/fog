package server

import (
	"net/http"
	"testing"
	"time"
)

func TestPowerRequiresAuthAndFires(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		set  func(*Server, func() error)
	}{
		{"reboot", "/api/system/reboot", func(s *Server, fn func() error) { s.rebootFn = fn }},
		{"shutdown", "/api/system/shutdown", func(s *Server, fn func() error) { s.shutdownFn = fn }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, h := newTestServer(t)
			fired := make(chan struct{})
			tc.set(srv, func() error { close(fired); return nil })

			if w := do(t, h, "POST", tc.path, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s: got %d, want 401", tc.name, w.Code)
			}
			select {
			case <-fired:
				t.Fatalf("%s fired without auth", tc.name)
			case <-time.After(2500 * time.Millisecond):
			}

			c := setup(t, h)
			if w := do(t, h, "POST", tc.path, "", withCookie(c)); w.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
			}
			select {
			case <-fired:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s never fired after authenticated request", tc.name)
			}
		})
	}
}
