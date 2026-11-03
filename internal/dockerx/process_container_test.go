package dockerx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestProcessContainer(t *testing.T) {
	byApp := map[string][]ContainerSummary{
		// Newest first, as Docker lists: a stale exited replica ahead of
		// the running main container, then a worker.
		"web": {
			summary("old", "", "", "exited"),
			summary("main", "", "", "running"),
			summary("worker", "worker", "", "running"),
		},
	}
	c := engineClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var filters map[string][]string
		json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		app := ""
		for _, l := range filters["label"] {
			app = strings.TrimPrefix(l, "fog.app=")
		}
		json.NewEncoder(w).Encode(byApp[app])
	}))
	ctx := context.Background()

	if got, err := c.ProcessContainer(ctx, "web", ""); err != nil || got.ID != "main" {
		t.Errorf("main: %v %v, want the running one", got.ID, err)
	}
	if got, err := c.ProcessContainer(ctx, "web", "worker"); err != nil || got.ID != "worker" {
		t.Errorf("worker: %v %v", got.ID, err)
	}
	if _, err := c.ProcessContainer(ctx, "web", "cron"); !errors.Is(err, ErrNoProcess) {
		t.Errorf("unknown process: %v, want ErrNoProcess", err)
	}
	if _, err := c.ProcessContainer(ctx, "gone", ""); !errors.Is(err, ErrNoContainer) {
		t.Errorf("no containers: %v, want ErrNoContainer", err)
	}
}
