package apps

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// Pruning keeps the newest tags of the app's own repository, newest by
// image creation time whatever order Docker lists them in, and removes the
// rest through the API.
func TestPruneBuiltImagesKeepsNewest(t *testing.T) {
	var mu sync.Mutex
	var removed []string
	var filter string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/json"):
			mu.Lock()
			filter = r.URL.Query().Get("filters")
			mu.Unlock()
			io.WriteString(w, `[
				{"Id":"b","RepoTags":["fog/hub:bbb"],"Created":200},
				{"Id":"a","RepoTags":["fog/hub:aaa"],"Created":100},
				{"Id":"d","RepoTags":["fog/hub:ddd","other/hub:x"],"Created":400},
				{"Id":"c","RepoTags":["fog/hub:ccc"],"Created":300}
			]`)
		case r.Method == http.MethodDelete:
			mu.Lock()
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/v1.44/images/"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	sockDir, err := os.MkdirTemp("", "fog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, h)
	t.Setenv("DOCKER_SOCK", sock)

	e := &Engine{Docker: dockerx.New(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.pruneBuiltImages(context.Background(), "hub", 2)

	mu.Lock()
	defer mu.Unlock()
	if filter != `{"reference":["fog/hub"]}` {
		t.Errorf("filters = %q", filter)
	}
	slices.Sort(removed)
	if want := []string{"fog/hub:aaa", "fog/hub:bbb"}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
}
