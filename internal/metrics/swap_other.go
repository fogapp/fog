//go:build !linux

package metrics

// Swap is only read on Linux; elsewhere nothing is claimed.
func Swap() (used, total uint64, ok bool) { return 0, 0, false }
