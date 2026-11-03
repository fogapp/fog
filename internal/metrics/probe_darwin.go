//go:build darwin

package metrics

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Darwin is the dev environment, not the deploy target. Close-enough numbers
// from the stock CLI tools beat a cgo mach dependency here; the Linux probes
// are the exact ones.

type cpuState struct{}

func (c *cpuState) prime() {}

// percent sums ps's per-process %cpu (a decaying average) across all
// processes and normalizes by core count.
func (c *cpuState) percent() float64 {
	out, err := exec.Command("ps", "-A", "-o", "%cpu=").Output()
	if err != nil {
		return 0
	}
	var sum float64
	for _, line := range strings.Split(string(out), "\n") {
		v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err == nil {
			sum += v
		}
	}
	pct := sum / float64(runtime.NumCPU())
	return min(pct, 100)
}

func memory() (used, total uint64) {
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		total, _ = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	}
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, total
	}
	pageSize := uint64(4096)
	var pages uint64
	for _, line := range strings.Split(string(out), "\n") {
		if _, rest, ok := strings.Cut(line, "page size of "); ok {
			if n, _, ok := strings.Cut(rest, " "); ok {
				if v, err := strconv.ParseUint(n, 10, 64); err == nil {
					pageSize = v
				}
			}
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), "."), 10, 64)
		if err != nil {
			continue
		}
		// Active + wired + compressor ≈ what Activity Monitor calls "used".
		switch strings.TrimSpace(name) {
		case "Pages active", "Pages wired down", "Pages occupied by compressor":
			pages += v
		}
	}
	used = pages * pageSize
	if total > 0 && used > total {
		used = total
	}
	return used, total
}

func load1() float64 {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0
	}
	// Prints "{ 1.62 1.85 2.01 }".
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}
