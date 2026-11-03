//go:build linux

package metrics

import (
	"os"
	"strconv"
	"strings"
)

// cpuState holds the previous /proc/stat counters; percent is the busy share
// of the delta since the last reading.
type cpuState struct {
	busy, total uint64
}

func (c *cpuState) prime() { c.busy, c.total = readCPU() }

func (c *cpuState) percent() float64 {
	busy, total := readCPU()
	db, dt := busy-c.busy, total-c.total
	c.busy, c.total = busy, total
	if dt == 0 || db > dt {
		return 0
	}
	return 100 * float64(db) / float64(dt)
}

// readCPU parses the aggregate "cpu" line of /proc/stat. Idle counts idle +
// iowait; everything else is busy.
func readCPU() (busy, total uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var idle uint64
	for i, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, 0
		}
		total += v
		if i == 3 || i == 4 { // idle, iowait
			idle += v
		}
	}
	return total - idle, total
}

func memory() (used, total uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var avail uint64
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, _ := strings.Cut(line, ":")
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	if avail > total {
		return 0, total
	}
	return total - avail, total
}

func load1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	first, _, _ := strings.Cut(string(data), " ")
	v, _ := strconv.ParseFloat(first, 64)
	return v
}

// Swap reads swap use from /proc/meminfo. Not part of Sample, which the
// dashboard draws; Lucid's host gauges report it.
func Swap() (used, total uint64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var free uint64
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, _ := strings.Cut(line, ":")
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "SwapTotal":
			total, ok = kb*1024, true
		case "SwapFree":
			free = kb * 1024
		}
	}
	if free > total {
		return 0, total, ok
	}
	return total - free, total, ok
}
