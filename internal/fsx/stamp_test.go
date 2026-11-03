package fsx

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseStamp(t *testing.T) {
	want := time.Date(2026, 8, 27, 1, 3, 14, 0, time.UTC)
	for s, ok := range map[string]bool{
		"20260827-010314":    true,
		"20260827-010314-2":  true,
		"20260827-010314-17": true,
		"20260827-010314-":   false,
		"20260827-010314--2": false,
		"20260827-010314-2x": false,
		"20260827-010314.x":  false,
		"2026082-010314":     false,
	} {
		got, gotOK := ParseStamp(s)
		if gotOK != ok || (ok && !got.Equal(want)) {
			t.Errorf("ParseStamp(%q) = %v, %v", s, got, gotOK)
		}
	}
	if Stamp(want.In(time.FixedZone("x", 3600))) != "20260827-010314" {
		t.Error("Stamp is not UTC")
	}
}

// Same-second snapshots must each get a directory of their own: the second
// is never handed the first one's.
func TestMkdirStampedSuffixesACollision(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "app")
	at := time.Date(2026, 8, 27, 1, 3, 14, 0, time.UTC)
	var got []string
	for range 3 {
		dir, err := MkdirStamped(parent, at, 0o700)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.Base(dir))
	}
	if got[0] != "20260827-010314" || got[1] != "20260827-010314-2" || got[2] != "20260827-010314-3" {
		t.Fatalf("names %q", got)
	}
	if FreeStamp(at, func(s string) bool {
		_, err := os.Stat(filepath.Join(parent, s))
		return err == nil
	}) != "20260827-010314-4" {
		t.Fatal("FreeStamp did not skip the taken names")
	}
}
