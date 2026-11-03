package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// locker.json's name and the index's fog name are not signed, so fog-locker
// must strip control characters before printing them — otherwise a bucket
// writer could inject ANSI escape sequences into the operator's terminal.
func TestSafeStripsControlCharacters(t *testing.T) {
	for _, in := range []string{"house\x1b[31mred", "a\x07bell", "line\nbreak", "null\x00byte"} {
		got := safe(in)
		if strings.ContainsAny(got, "\x1b\x07\n\x00") {
			t.Errorf("safe(%q) = %q still carries a control character", in, got)
		}
	}
	if got := safe("house 2"); got != "house 2" {
		t.Errorf("safe must leave ordinary text alone, got %q", got)
	}
}

// restore-kit without --out takes its filename from the bucket listing, which
// an attacker with bucket-write controls. A "../" name must never steer the
// write out of the working directory.
func TestSafeOutName(t *testing.T) {
	ok := map[string]string{
		"fog-recovery-kit-20260827-010000.enc": "fog-recovery-kit-20260827-010000.enc",
		"kit.enc":                              "kit.enc",
	}
	for in, want := range ok {
		got, err := safeOutName(in)
		if err != nil || got != want {
			t.Errorf("safeOutName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"../../etc/cron.d/evil",
		"/etc/passwd",
		"..",
		".",
		"",
		"a/b.enc",
		"x\x00y",
	} {
		if got, err := safeOutName(bad); err == nil {
			t.Errorf("safeOutName(%q) = %q, want a refusal", bad, got)
		}
		// And no traversal survives even if it were used.
		if got, _ := safeOutName(bad); filepath.IsAbs(got) || got == ".." {
			t.Errorf("safeOutName(%q) leaked a traversal: %q", bad, got)
		}
	}
}
