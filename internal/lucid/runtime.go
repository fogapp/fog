package lucid

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// gcState is what the last runtime sample saw of the collector, so the next
// reports only what happened since.
type gcState struct {
	numGC  uint32
	primed bool
}

// sampleRuntime reports the Go runtime once a window: goroutines, heap,
// resident memory, and each collection's pause since the last sample.
func (c *Client) sampleRuntime() {
	if !c.active() {
		return
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	c.Gauge("go.goroutines", "", float64(runtime.NumGoroutine()), nil)
	c.Gauge("go.heap_alloc_bytes", "bytes", float64(m.HeapAlloc), nil)
	c.Gauge("go.heap_sys_bytes", "bytes", float64(m.HeapSys), nil)
	c.Gauge("go.sys_bytes", "bytes", float64(m.Sys), nil)
	if rss, ok := residentBytes(); ok {
		c.Gauge("process.rss_bytes", "bytes", float64(rss), nil)
	}

	c.mu.Lock()
	last, primed := c.gc.numGC, c.gc.primed
	c.gc.numGC, c.gc.primed = m.NumGC, true
	c.mu.Unlock()
	if !primed {
		return
	}
	n := m.NumGC - last
	c.Count("go.gc.count", float64(n), nil)
	// PauseNs is a ring of the last 256 pauses; older ones are gone.
	for i := uint32(0); i < min(n, 256); i++ {
		c.Dist("go.gc.pause", "ms", float64(m.PauseNs[(m.NumGC-i+255)%256])/1e6, nil)
	}
}

// residentBytes reads the process's resident set from /proc; elsewhere (a
// developer's Mac) nothing is claimed.
func residentBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}
