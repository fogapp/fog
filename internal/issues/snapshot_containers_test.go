package issues

import (
	"context"
	"errors"
	"testing"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// With AppContainers wired, the snapshot reads the engine's shared listing
// instead of asking Docker itself; a failed listing still reads as "no
// evidence", not "no containers".
func TestSnapshotUsesTheSharedContainerListing(t *testing.T) {
	sc, st, _ := testScanner(t, nil)
	if _, err := st.CreateApp("manor"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	sc.AppContainers = func(context.Context) ([]dockerx.ContainerSummary, error) {
		calls++
		return []dockerx.ContainerSummary{{
			ID: "c1", State: "running",
			Labels: map[string]string{"fog.app": "manor", "fog.role": "main"},
		}}, nil
	}
	sn, err := sc.buildSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !sn.dockerOK || len(sn.apps) != 1 || sn.apps[0].main == nil || sn.apps[0].main.ID != "c1" {
		t.Fatalf("calls=%d dockerOK=%v apps=%+v", calls, sn.dockerOK, sn.apps)
	}

	sc.AppContainers = func(context.Context) ([]dockerx.ContainerSummary, error) {
		return nil, errors.New("daemon hiccup")
	}
	if sn, err = sc.buildSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sn.dockerOK {
		t.Fatal("a failed listing read as an empty one")
	}
}
