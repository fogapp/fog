package files

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// The verify sample streams the manifest: it reads both the compact form
// written now and the indented one earlier fogd wrote, counts every file,
// returns distinct real entries, and refuses a foreign format.
func TestSampleManifest(t *testing.T) {
	mf := &snapManifest{Format: snapshotFormat, Mode: "links", Excluded: []string{"x"}}
	for i := range 1000 {
		mf.Files = append(mf.Files, snapFile{Path: fmt.Sprintf("f%04d", i), Size: int64(i), SHA256: "s"})
	}
	compact := t.TempDir()
	if err := writeManifest(compact, mf); err != nil {
		t.Fatal(err)
	}
	indented := t.TempDir()
	b, _ := json.MarshalIndent(mf, "", " ")
	os.WriteFile(filepath.Join(indented, snapshotManifest), b, 0o600)

	for _, dir := range []string{compact, indented} {
		got, total, err := sampleManifest(dir, rand.New(rand.NewSource(1)), func(n int) int { return n / 100 }, 200)
		if err != nil {
			t.Fatal(err)
		}
		if total != 1000 || len(got) != 10 {
			t.Fatalf("total %d, sample %d", total, len(got))
		}
		seen := map[string]bool{}
		for _, f := range got {
			if seen[f.Path] || f.SHA256 != "s" {
				t.Fatalf("bad sample entry %+v", f)
			}
			seen[f.Path] = true
		}
	}
	if back, err := readManifest(compact); err != nil || len(back.Files) != 1000 {
		t.Fatalf("readManifest of the compact form: %v", err)
	}

	foreign := t.TempDir()
	os.WriteFile(filepath.Join(foreign, snapshotManifest), []byte(`{"format":"other","files":[]}`), 0o600)
	if _, _, err := sampleManifest(foreign, rand.New(rand.NewSource(1)), func(int) int { return 5 }, 200); err == nil {
		t.Fatal("a foreign format was sampled")
	}
}
