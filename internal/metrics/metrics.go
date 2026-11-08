// Package metrics samples host utilization (CPU, memory, disk, load) into a
// fixed-size in-memory ring — enough history for the dashboard's last-hour
// ribbon without a database, an agent, or a scrape target. History does not
// survive a restart; the UI draws only the samples that exist.
package metrics

import (
	"context"
	"time"

	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/ring"
)

// Sample is one reading of host utilization. Byte fields carry their totals so
// the UI can say "used of total" from a single payload.
type Sample struct {
	T         int64   `json:"t"` // unix seconds
	CPUPct    float64 `json:"cpuPct"`
	MemUsed   uint64  `json:"memUsed"`
	MemTotal  uint64  `json:"memTotal"`
	DiskUsed  uint64  `json:"diskUsed"`
	DiskTotal uint64  `json:"diskTotal"`
	Load1     float64 `json:"load1"`
}

type Sampler struct {
	dataDir  string
	interval time.Duration

	ring *ring.Ring[Sample]

	cpu cpuState

	// OnSample, when set, hears each sample as it is taken — Glass's host
	// gauges. It runs on the sampling goroutine and must not block.
	OnSample func(Sample)
}

// New sizes the ring to keep/interval samples. dataDir picks the filesystem
// the disk figures describe — the same one /api/system reports.
func New(dataDir string, interval, keep time.Duration) *Sampler {
	n := int(keep / interval)
	if n < 2 {
		n = 2
	}
	return &Sampler{dataDir: dataDir, interval: interval, ring: ring.New[Sample](n)}
}

func (s *Sampler) Interval() time.Duration { return s.interval }

// Loop samples until ctx ends. CPU percent is a delta between readings, so the
// counters are primed first and the initial sample lands ~1s later — a fresh
// dashboard gets its first point quickly without ever seeing a made-up one.
func (s *Sampler) Loop(ctx context.Context) {
	s.cpu.prime()
	loop.Every(ctx, time.Second, s.interval, func(context.Context) { s.take() })
}

func (s *Sampler) take() {
	smp := Sample{T: time.Now().Unix(), CPUPct: s.cpu.percent(), Load1: load1()}
	smp.MemUsed, smp.MemTotal = memory()
	smp.DiskUsed, smp.DiskTotal = disk(s.dataDir)

	s.ring.Add(smp)
	if s.OnSample != nil {
		s.OnSample(smp)
	}
}

// Series returns the retained samples, oldest first.
func (s *Sampler) Series() []Sample { return s.ring.All() }

// Latest returns the newest sample, if one has been taken.
func (s *Sampler) Latest() (Sample, bool) { return s.ring.Latest() }
