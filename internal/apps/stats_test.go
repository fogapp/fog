package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// The sampler asks Docker for one-shot snapshots — no one-second CPU wait
// per container — and turns the cumulative CPU counters into a percentage
// between its own readings.
func TestSampleStatsComputesCPUBetweenSnapshots(t *testing.T) {
	var (
		mu      sync.Mutex
		calls   int
		queries []string
	)
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerx.ContainerSummary{
				{ID: "c1", State: "running", Labels: map[string]string{"fog.app": "web"}},
			})
		case strings.HasSuffix(r.URL.Path, "/stats"):
			mu.Lock()
			calls++
			n := uint64(calls)
			queries = append(queries, r.URL.RawQuery)
			mu.Unlock()
			// Each reading: the container used 0.5s of CPU while the host
			// clocked 4s across its 2 CPUs — 25% of one core.
			fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":2},
				"memory_stats":{"usage":1000,"limit":2000}}`, n*500_000_000, n*4_000_000_000)
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
	go http.Serve(ln, api)
	t.Setenv("DOCKER_SOCK", sock)
	e := &Engine{Docker: dockerx.New(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	e.sampleStats(context.Background())
	st, _, ok := e.containerStats("c1")
	if !ok {
		t.Fatal("no reading after the first pass")
	}
	if st.MemUsage != 1000 {
		t.Errorf("memory = %d, want 1000 on the first pass", st.MemUsage)
	}
	if st.CPUPercent != 0 {
		t.Errorf("first pass CPU = %v, want 0: one reading can't give a rate", st.CPUPercent)
	}

	e.sampleStats(context.Background())
	st, _, _ = e.containerStats("c1")
	if math.Abs(st.CPUPercent-25) > 0.001 {
		t.Errorf("second pass CPU = %v, want 25", st.CPUPercent)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, q := range queries {
		if !strings.Contains(q, "one-shot=true") {
			t.Errorf("stats query %q is not one-shot", q)
		}
	}
}

// An app's figures are all of its containers together, CPU as a share of
// the whole host: Stream's transcoder pinning three of four cores while its
// web container idles is Stream at about 78%, not the web container's 0.3%.
func TestAppUsagesSumsContainersOnTheHostScale(t *testing.T) {
	const gb = 1 << 30
	now := time.Now()
	e := &Engine{hostMem: 8 * gb, stats: map[string]statsEntry{
		"web":   {at: now, stats: dockerx.Stats{CPUPercent: 1.2, MemUsage: 1 * gb, MemLimit: 8 * gb, OnlineCPUs: 4}},
		"trans": {at: now, stats: dockerx.Stats{CPUPercent: 308, MemUsage: 2 * gb, MemLimit: 3 * gb, OnlineCPUs: 4}, rates: &Rates{NetIn: 10, DiskWrite: 5}},
		"stale": {at: now.Add(-2 * statsMaxAge), stats: dockerx.Stats{CPUPercent: 400, MemUsage: gb, OnlineCPUs: 4}},
		"solo":  {at: now, stats: dockerx.Stats{CPUPercent: 50, MemUsage: gb, MemLimit: 2 * gb, OnlineCPUs: 4}},
	}}
	label := func(app, process string) map[string]string {
		l := map[string]string{"fog.app": app}
		if process != "" {
			l["fog.process"] = process
		}
		return l
	}
	containers := []dockerx.ContainerSummary{
		{ID: "web", State: "running", Labels: label("stream", "")},
		{ID: "trans", State: "running", Labels: label("stream", "transcoder")},
		{ID: "stale", State: "running", Labels: label("stream", "worker")},
		{ID: "solo", State: "running", Labels: label("feed", "")},
		{ID: "gone", State: "exited", Labels: label("hud", "")},
	}

	got := e.appUsages(containers)
	s := got["stream"]
	if !s.Running || s.Containers != 3 || !s.Measured {
		t.Fatalf("stream = %+v, want 3 running containers, measured", s)
	}
	if math.Abs(s.CPUPercent-77.3) > 0.001 {
		t.Errorf("stream CPU = %v, want 77.3 (309.2%% of one core over 4)", s.CPUPercent)
	}
	if s.MemUsage != 3*gb {
		t.Errorf("stream memory = %d, want 3 GB: the stale reading counts for nothing", s.MemUsage)
	}
	if s.MemLimit != 8*gb {
		t.Errorf("stream limit = %d, want the host's 8 GB, not 11", s.MemLimit)
	}
	if s.Rates == nil || s.Rates.NetIn != 10 || s.Rates.DiskWrite != 5 {
		t.Errorf("stream rates = %+v, want the transcoder's", s.Rates)
	}
	if f := got["feed"]; f.CPUPercent != 12.5 || f.MemLimit != 2*gb || f.Rates != nil {
		t.Errorf("feed = %+v, want 12.5%% of the host, its own 2 GB limit, no rates yet", f)
	}
	if h := got["hud"]; h.Running || h.Measured {
		t.Errorf("hud = %+v, want not running and unmeasured", h)
	}
}
