package dockerx

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeEngine answers lifecycle requests with a canned status per
// "METHOD path" and records what it was asked, bodies included.
type fakeEngine struct {
	mu     sync.Mutex
	status map[string]int
	calls  []string
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/"+apiVersion)
	body, _ := io.ReadAll(r.Body)
	call := r.Method + " " + path
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	if len(body) > 0 {
		call += " " + string(body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	code, ok := f.status[r.Method+" "+path]
	f.mu.Unlock()
	if !ok {
		code = http.StatusNoContent
	}
	w.WriteHeader(code)
	if code >= 400 {
		io.WriteString(w, `{"message":"conflict: unable to remove repository reference"}`)
	}
}

func engineClient(t *testing.T, f http.Handler) *Client {
	t.Helper()
	// os.MkdirTemp keeps the socket path under the ~104-byte sun_path limit.
	dir, err := os.MkdirTemp("", "fogdx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	t.Setenv("DOCKER_SOCK", sock)
	return New()
}

func TestLifecycleRequests(t *testing.T) {
	f := &fakeEngine{}
	c := engineClient(t, f)
	ctx := context.Background()
	for _, err := range []error{
		c.Remove(ctx, "abc", true),
		c.Remove(ctx, "def", false),
		c.Stop(ctx, "abc", 30),
		c.NetworkDisconnect(ctx, "fog", "abc", false),
		c.RemoveImage(ctx, "lscr.io/linuxserver/jellyfin:10.9"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"DELETE /containers/abc?force=true",
		"DELETE /containers/def",
		"POST /containers/abc/stop?t=30",
		`POST /networks/fog/disconnect {"Container":"abc","Force":false}`,
		"DELETE /images/lscr.io/linuxserver/jellyfin:10.9",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

// Gone is done: the callers want the thing absent, which a 404 (or Stop's
// 304) says it already is. A real refusal is an error carrying the
// daemon's own words.
func TestLifecycleStatuses(t *testing.T) {
	f := &fakeEngine{status: map[string]int{
		"DELETE /containers/gone":    http.StatusNotFound,
		"POST /containers/idle/stop": http.StatusNotModified,
		"DELETE /images/in-use:1":    http.StatusConflict,
	}}
	c := engineClient(t, f)
	ctx := context.Background()
	if err := c.Remove(ctx, "gone", true); err != nil {
		t.Errorf("removing a missing container: %v", err)
	}
	if err := c.Stop(ctx, "idle", 10); err != nil {
		t.Errorf("stopping a stopped container: %v", err)
	}
	err := c.RemoveImage(ctx, "in-use:1")
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "unable to remove") {
		t.Errorf("image in use: %v", err)
	}
}
