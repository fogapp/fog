package bootmark

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootMark(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "fog.db")

	// First boot ever: no db, no marker — clean.
	if BeginBoot(dir, db) {
		t.Fatal("first boot flagged dirty")
	}

	// Graceful cycle: db exists, marker written on stop.
	os.WriteFile(db, []byte("x"), 0o600)
	EndClean(dir)
	if BeginBoot(dir, db) {
		t.Fatal("boot after clean stop flagged dirty")
	}

	// BeginBoot consumed the marker; a crash now (no EndClean) is dirty.
	if !BeginBoot(dir, db) {
		t.Fatal("boot after crash not flagged dirty")
	}
}
