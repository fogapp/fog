package apps

import (
	"context"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// Read paths share one listing for a moment; an app lock released (a deploy,
// start, stop or delete finishing) drops it, so the next read is fresh.
func TestAppContainersSharedUntilAnAppChanges(t *testing.T) {
	api := &fakeDockerAPI{list: func(call int) []dockerx.ContainerSummary {
		return []dockerx.ContainerSummary{mainC("c1", "running")}
	}}
	e := swapEngine(t, api, "app")
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cs, err := e.AppContainers(ctx); err != nil || len(cs) != 1 {
				t.Errorf("AppContainers = %v, %v", cs, err)
			}
		}()
	}
	wg.Wait()
	if _, err := e.AppContainers(ctx); err != nil {
		t.Fatal(err)
	}
	if api.listCalls != 1 {
		t.Fatalf("listed %d times, want 1", api.listCalls)
	}

	e.lock("app")()
	if _, err := e.AppContainers(ctx); err != nil {
		t.Fatal(err)
	}
	if api.listCalls != 2 {
		t.Fatalf("after an app lock: listed %d times, want 2", api.listCalls)
	}
}
