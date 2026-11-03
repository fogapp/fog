//go:build linux

package hostcpu

import "os"

func readCPUInfo() (string, bool) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", false
	}
	return string(data), true
}
