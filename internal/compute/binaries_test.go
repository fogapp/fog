package compute

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBinariesFromDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fog-node-linux-amd64"), "one")
	b := &Binaries{Dir: dir, Version: "dev"}
	got, err := b.Get(context.Background(), "linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	// sha256("one")
	if got.SHA256 != "7692c3ad3540bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed" {
		t.Fatalf("sum: %s", got.SHA256)
	}
	for _, bad := range []string{"darwin-arm64", "../../etc-passwd", "windows-amd64", ""} {
		if _, err := b.Get(context.Background(), bad); !errors.Is(err, ErrNoBinary) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestBinariesDownloadOncePerRelease(t *testing.T) {
	calls := 0
	b := &Binaries{Cache: t.TempDir(), Version: "v1.2.3", Download: func(_ context.Context, key, dst string) (string, string, error) {
		calls++
		if key != "fog-node-darwin-arm64" {
			t.Fatalf("key %s", key)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", "", err
		}
		return "v1.2.3", "abc", os.WriteFile(dst, []byte("bin"), 0o755)
	}}
	for range 2 {
		got, err := b.Get(context.Background(), "darwin-arm64")
		if err != nil {
			t.Fatal(err)
		}
		if got.SHA256 != "abc" || got.Version != "v1.2.3" {
			t.Fatalf("got %+v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("downloaded %d times", calls)
	}

	// A source on another release is refused rather than served as this one.
	b2 := &Binaries{Cache: t.TempDir(), Version: "v2", Download: func(_ context.Context, _, dst string) (string, string, error) {
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return "v1", "abc", os.WriteFile(dst, []byte("old"), 0o755)
	}}
	if _, err := b2.Get(context.Background(), "darwin-arm64"); err == nil {
		t.Fatal("served another release's build")
	}
	// A development fog has nothing to download.
	if _, err := (&Binaries{Cache: t.TempDir(), Version: "dev", Download: b.Download}).Get(context.Background(), "darwin-arm64"); !errors.Is(err, ErrNoBinary) {
		t.Fatalf("dev: %v", err)
	}
}
