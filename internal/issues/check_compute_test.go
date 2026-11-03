package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
	"gitea.lan/matthew/fog/internal/store"
)

func pairNode(t *testing.T, st *store.Store, id, name, os string) {
	t.Helper()
	if err := st.CreateComputeJoinCode("code-"+id, "admin", time.Hour); err != nil {
		t.Fatal(err)
	}
	n := &store.ComputeNode{ID: id, Name: name, Inventory: `{"os":"` + os + `"}`}
	if err := st.PairComputeNode("code-"+id, n, "digest-"+id); err != nil {
		t.Fatal(err)
	}
}

func openIssues(t *testing.T, st *store.Store) []store.Issue {
	t.Helper()
	list, err := st.ListIssues([]string{"open"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// A machine paired but never heard from is an issue saying how to find out
// why on the machine; it goes once the machine connects.
func TestComputeNeverConnected(t *testing.T) {
	sc, st, _ := testScanner(t, nil)
	pairNode(t, st, "m1", "Mini", "darwin")
	pairNode(t, st, "l1", "Box", "linux")

	// Within the grace, nothing yet.
	sc.SweepCompute(context.Background())
	if got := openIssues(t, st); len(got) != 0 {
		t.Fatalf("issues within the grace: %+v", got)
	}

	defer func(g time.Duration) { computeConnectGrace = g }(computeConnectGrace)
	computeConnectGrace = 0
	sc.SweepCompute(context.Background())
	got := openIssues(t, st)
	if len(got) != 2 {
		t.Fatalf("issues: %+v", got)
	}
	byTitle := map[string]store.Issue{}
	for _, i := range got {
		byTitle[i.Title] = i
	}
	mac := byTitle["Mini was paired but has never connected"]
	if !strings.Contains(mac.Fix, "fog-node/fog-node.log") || !strings.Contains(mac.Fix, "Local Network") || mac.FixURL != "/settings/server/compute" {
		t.Fatalf("mac fix: %+v", mac)
	}
	box := byTitle["Box was paired but has never connected"]
	if !strings.Contains(box.Fix, "journalctl -u fog-node") || strings.Contains(box.Fix, "Local Network") {
		t.Fatalf("linux fix: %q", box.Fix)
	}

	// The Mac connects; the Linux box is removed. Both resolve.
	if err := st.MarkComputeNodeConnected("m1", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteComputeNode("l1"); err != nil {
		t.Fatal(err)
	}
	sc.SweepCompute(context.Background())
	if got := openIssues(t, st); len(got) != 0 {
		t.Fatalf("still open: %+v", got)
	}
}

// A machine stuck behind fog's release of fog-node is an issue saying why,
// and what to do about that why; it goes once the machine catches up.
func TestComputeOutOfDate(t *testing.T) {
	sc, st, _ := testScanner(t, nil)
	stuck := []compute.Machine{
		{ID: "m1", Name: "Mini", Inventory: compute.Inventory{OS: "darwin", Version: "v1"},
			Behind: &compute.Behind{Want: "v2", Stuck: true, Error: "open /usr/local/bin/.fog-node-update-1: permission denied"}},
		{ID: "l1", Name: "Box", Inventory: compute.Inventory{OS: "linux", Version: "v1"}, Behind: &compute.Behind{Want: "v2", Stuck: true}},
	}
	sc.ComputeStuck = func() []compute.Machine { return stuck }
	sc.SweepCompute(context.Background())
	byTitle := map[string]store.Issue{}
	for _, i := range openIssues(t, st) {
		byTitle[i.Title] = i
	}
	mac, ok := byTitle["Mini runs fog-node v1, not fog's v2"]
	if !ok || !strings.Contains(mac.Detail, "permission denied") || !strings.Contains(mac.Fix, "Update now") ||
		!strings.Contains(mac.Fix, "writable") || !strings.Contains(mac.Fix, "fog-node/fog-node.log") || mac.FixURL != "/settings/server/compute" {
		t.Fatalf("mac: %+v", mac)
	}
	box, ok := byTitle["Box runs fog-node v1, not fog's v2"]
	if !ok || !strings.Contains(box.Detail, "hasn't updated") || !strings.Contains(box.Fix, "journalctl") {
		t.Fatalf("box: %+v", box)
	}

	stuck = nil
	sc.SweepCompute(context.Background())
	if got := openIssues(t, st); len(got) != 0 {
		t.Fatalf("still open: %+v", got)
	}
}
