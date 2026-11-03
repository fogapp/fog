package wan

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// A lease renewal must not repeat discovery and the description fetch: the
// router's service is remembered per gateway, and forgotten when it stops
// answering.
func TestMapperRemembersTheGatewayService(t *testing.T) {
	srv, _ := startIGD(t, false)
	var descs atomic.Int32
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/desc.xml" {
			descs.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.Close)
	m := &Mapper{
		Gateway:   func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil },
		Locations: []string{counting.URL + "/desc.xml"},
		NoNATPMP:  true,
	}
	ctx := context.Background()
	for range 3 {
		if _, err := m.Map(ctx, "192.168.1.200", []int{443}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Unmap(ctx, "upnp", []int{443}); err != nil {
		t.Fatal(err)
	}
	if n := descs.Load(); n != 1 {
		t.Fatalf("description fetched %d times, want 1", n)
	}
	// Every port refused (here: an address the fake router won't map) —
	// the remembered service is dropped and the next call resolves again.
	if _, err := m.Map(ctx, "192.168.1.9", []int{443}, time.Hour); err == nil {
		t.Fatal("want a refusal")
	}
	if _, err := m.Map(ctx, "192.168.1.200", []int{443}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := descs.Load(); n != 2 {
		t.Fatalf("description fetched %d times after a failure, want 2", n)
	}
}
