package chunkstore

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func writeArtifact(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"manifest.json", "db/primary.dump", "files/storage.tar.zst"} {
		b, ok := files[name]
		if !ok {
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func zst(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, _ := zstd.NewWriter(nil)
	return enc.EncodeAll(b, nil)
}

// textBytes is compressible: words from a small vocabulary, so zstd
// entropy-codes it and a one-spot edit reshuffles the compressed stream.
func textBytes(seed uint64, n int) []byte {
	words := [][]byte{[]byte("alpha "), []byte("beta "), []byte("gamma "), []byte("delta\n"), []byte("epsilon "), []byte("zeta ")}
	r := randBytes(seed, n)
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		b.Write(words[int(r[i%len(r)])%len(words)])
	}
	return b.Bytes()[:n]
}

func TestAnalyzeSeesNightlyChange(t *testing.T) {
	root := t.TempDir()
	storage := textBytes(9, 4<<20)
	dump := randBytes(10, 1<<20)
	for i, stamp := range []string{"20260101-030000", "20260102-030000", "20260103-030000"} {
		s := append([]byte{}, storage...)
		copy(s[len(s)/8:], randBytes(uint64(100+i), 4096)) // a small edit each night
		writeArtifact(t, filepath.Join(root, "hub", stamp, "artifact.tar"), map[string][]byte{
			"manifest.json":         []byte(`{"format":"fog-backup/1","app":"hub"}`),
			"db/primary.dump":       dump,
			"files/storage.tar.zst": zst(t, s),
		})
	}
	os.MkdirAll(filepath.Join(root, "fog-recovery"), 0o755)
	rep, err := Analyze(context.Background(), root, func(n string) bool { return n == "fog-recovery" }, AnalyzeOptions{Params: smallParams})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Series) != 1 || rep.Series[0].Series != "hub" || rep.Series[0].Snapshots != 3 {
		t.Fatalf("series = %+v", rep.Series)
	}
	s := rep.Series[0]
	if s.Expanded.Median() > 200<<10 {
		t.Fatalf("expanded nightly cost %d, want a small edit's worth", s.Expanded.Median())
	}
	if s.Expanded.Logical <= s.AsIs.Logical {
		t.Fatalf("expanded logical %d not past as-is %d: the inner archive was not opened", s.Expanded.Logical, s.AsIs.Logical)
	}
	if s.Expanded.DBStored == 0 {
		t.Fatal("dump bytes not attributed to the database")
	}
	if tot := rep.Totals(); tot.OnDisk != s.OnDisk {
		t.Fatalf("totals %+v", tot)
	}
}
