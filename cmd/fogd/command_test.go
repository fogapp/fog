package main

import (
	"bytes"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/config"
)

// fogd with no arguments is the daemon; --version answers and exits; and an
// argument it does not know is refused — never a second daemon started
// beside the running one.
func TestCommandArguments(t *testing.T) {
	var out, errb bytes.Buffer
	if _, done := command(nil, &out, &errb); done {
		t.Fatal("no arguments must start the daemon")
	}
	code, done := command([]string{"--version"}, &out, &errb)
	if !done || code != 0 || strings.TrimSpace(out.String()) != "fogd "+config.Version {
		t.Fatalf("--version: done=%v code=%d out=%q", done, code, out.String())
	}
	out.Reset()
	code, done = command([]string{"--frobnicate"}, &out, &errb)
	if !done || code != 2 || !strings.Contains(errb.String(), `unknown argument "--frobnicate"`) || out.Len() != 0 {
		t.Fatalf("unknown argument: done=%v code=%d err=%q", done, code, errb.String())
	}
}
