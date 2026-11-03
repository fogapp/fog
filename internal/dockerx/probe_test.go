package dockerx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A health path that redirects is answered by the container itself; the
// probe must not chase the redirect out to wherever it points.
func TestProbeHTTPDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer elsewhere.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer app.Close()

	if !ProbeHTTP(context.Background(), app.URL+"/health") {
		t.Fatal("a 302 from the container should count as healthy")
	}
	if followed.Load() {
		t.Fatal("probe followed the redirect")
	}
}
