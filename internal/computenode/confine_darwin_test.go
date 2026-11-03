package computenode

import (
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// fakeHandOver: on a Mac fog-node listens itself; nothing to hand over.
func fakeHandOver(*jail) {}

// lookAt reports whether pid shows in the process list, and whether its
// arguments can be read.
func lookAt(pid string) (listed, args bool) {
	n, _ := strconv.Atoi(pid)
	procs, _ := unix.SysctlKinfoProcSlice("kern.proc.all")
	for _, p := range procs {
		listed = listed || int(p.Proc.P_pid) == n
	}
	raw, err := unix.SysctlRaw("kern.procargs2", n)
	return listed, err == nil && strings.Contains(string(raw), "fog-node-canary")
}

// meddle is Linux's test.
func meddle(string) string { return "" }
