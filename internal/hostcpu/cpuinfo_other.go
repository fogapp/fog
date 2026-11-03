//go:build !linux

package hostcpu

// Only Linux exposes the flag list as text; elsewhere (a developer's Mac)
// nothing is read and nothing is claimed.
func readCPUInfo() (string, bool) { return "", false }
