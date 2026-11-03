package main

import (
	"os"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

// TestMain lets this test binary stand in for fogd as its own pin helper:
// pinFuncs runs os.Executable() with "pin-bind", exactly as fogd does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "pin-bind" {
		os.Exit(apps.PinMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}
