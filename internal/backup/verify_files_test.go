package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// filesArtifact is writeArtifact with every component a file archive, as an
// app with no database (newtab) produces, and the manifest and digest a
// stored backup would carry.
func filesArtifact(t *testing.T, tweak func(m *Manifest)) (path, manifest, sum string) {
	t.Helper()
	var m *Manifest
	path = writeArtifact(t, map[string]string{
		"files/state.tar.zst":  "state bytes",
		"files/config.tar.zst": "config bytes",
	}, func(mm *Manifest) {
		for i := range mm.Components {
			mm.Components[i].Kind = "files"
		}
		if tweak != nil {
			tweak(mm)
		}
		m = mm
	})
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return path, string(raw), hex.EncodeToString(h[:])
}

// An artifact with no database verifies on its components' checksums: it
// has no dump for the scratch database, and no Postgres is needed.
func TestTestRestoreFilesOnly(t *testing.T) {
	m := &Manager{DataDir: t.TempDir()}
	path, manifest, sum := filesArtifact(t, nil)
	if err := m.testRestore(context.Background(), "newtab", path, manifest, sum); err != nil {
		t.Fatalf("files-only artifact: %v", err)
	}
	if err := m.TestRestore(context.Background(), "newtab", path, manifest); err != nil {
		t.Fatalf("files-only artifact, no recorded digest: %v", err)
	}
}

func TestTestRestoreFilesOnlyRefuses(t *testing.T) {
	m := &Manager{DataDir: t.TempDir()}
	t.Run("changed on disk", func(t *testing.T) {
		path, manifest, _ := filesArtifact(t, nil)
		err := m.testRestore(context.Background(), "newtab", path, manifest, strings.Repeat("0", 64))
		if err == nil || !strings.Contains(err.Error(), "changed on disk") {
			t.Fatalf("want changed on disk, got %v", err)
		}
	})
	t.Run("component corrupt", func(t *testing.T) {
		path, manifest, sum := filesArtifact(t, func(m *Manifest) { m.Components[0].SHA256 = strings.Repeat("0", 64) })
		err := m.testRestore(context.Background(), "newtab", path, manifest, sum)
		if err == nil || !strings.Contains(err.Error(), "fails checksum") {
			t.Fatalf("want fails checksum, got %v", err)
		}
	})
	t.Run("component missing", func(t *testing.T) {
		path, manifest, sum := filesArtifact(t, func(m *Manifest) {
			m.Components = append(m.Components, ManifestComponent{Name: "extra", Path: "files/extra.tar.zst", Kind: "files"})
		})
		err := m.testRestore(context.Background(), "newtab", path, manifest, sum)
		if err == nil || !strings.Contains(err.Error(), "missing from the artifact") {
			t.Fatalf("want missing, got %v", err)
		}
	})
}

// A database anywhere in the manifest keeps the test-restore: a dump is
// only proven by loading it.
func TestFilesOnly(t *testing.T) {
	for _, tc := range []struct {
		manifest string
		want     bool
	}{
		{`{"components":[{"kind":"files"},{"kind":"files"}]}`, true},
		{`{"components":[{"kind":"db"},{"kind":"files"}]}`, false},
		{`{"components":[]}`, false},
		{``, false},
		{`not json`, false},
	} {
		if got := filesOnly(tc.manifest); got != tc.want {
			t.Errorf("filesOnly(%q) = %v, want %v", tc.manifest, got, tc.want)
		}
	}
}
