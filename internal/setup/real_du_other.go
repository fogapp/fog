//go:build !linux

package setup

func duBytes(string) int64 { return 0 }
