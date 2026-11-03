package catalog

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/catalog.golden.json")

// The catalog is data the store front and installs consume as JSON. Its
// Go source is free to change shape — builders, embedded types — but the
// JSON it produces only changes on purpose: run with -update and review
// the diff.
func TestCatalogJSONGolden(t *testing.T) {
	got, err := json.MarshalIndent(Entries(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "catalog.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/catalog -run Golden -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("catalog JSON changed; if intended, rerun with -update and review the diff")
	}
}
