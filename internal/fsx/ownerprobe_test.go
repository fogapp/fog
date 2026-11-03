package fsx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLastLine(t *testing.T) {
	cases := map[string]string{
		"sh: line 1: touch: /x/.probe: Permission denied": "Permission denied",
		"touch: /x/.probe: Read-only file system":         "Read-only file system",
		"the write was refused":                           "the write was refused",
	}
	for in, want := range cases {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// The contract that matters: a probe that cannot run is never a denial. A
// wrong answer here files a critical issue about a share that is fine.
func TestCanWriteAsSeparatesVerdictFromFailure(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if os.Geteuid() != 0 {
		// Switching uid needs root; unprivileged, the fork fails outright.
		ok, why, err := CanWriteAs(ctx, dir, 65534, 65534)
		if err == nil {
			t.Fatal("an unprivileged probe cannot have a verdict")
		}
		if ok || why != "" {
			t.Errorf("failure reported as a verdict: ok=%v why=%q", ok, why)
		}
		return
	}
	if ok, why, err := CanWriteAs(ctx, dir, 0, 0); err != nil || !ok {
		t.Fatalf("root in its own temp dir: ok=%v why=%q err=%v", ok, why, err)
	}
	closed := filepath.Join(dir, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	ok, why, err := CanWriteAs(ctx, closed, 65534, 65534)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if ok {
		t.Error("nobody should not be able to write a root-only directory")
	}
	if why == "" {
		t.Error("a denial must carry the reason it was denied")
	}
}
