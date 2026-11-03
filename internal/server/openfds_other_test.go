//go:build !(darwin || linux)

package server

import "testing"

func openFDsOn(t *testing.T, path string) int {
	t.Skip("descriptor counting needs /dev/fd")
	return 0
}
