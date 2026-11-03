//go:build !darwin && !(linux && (amd64 || arm64))

package computenode

func fakeHandOver(*jail) {}

func lookAt(string) (bool, bool) { return false, false }

// meddle is Linux's test.
func meddle(string) string { return "" }
