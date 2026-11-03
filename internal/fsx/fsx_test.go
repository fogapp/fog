package fsx

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestWriteFileAtomicReplacesWholeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	fi, _ := os.Stat(path)
	if string(got) != "new" || fi.Mode().Perm() != 0o600 {
		t.Errorf("got %q mode %v", got, fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// A writer that fails part-way must leave the old file exactly as it was.
func TestWriteAtomicFailureKeepsTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	os.WriteFile(path, []byte("old"), 0o644)
	boom := errors.New("boom")
	err := WriteAtomic(path, 0o644, func(w io.Writer) error {
		w.Write([]byte("half"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Errorf("old file became %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestWriteFileExclusiveNeverReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := WriteFileExclusive(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileExclusive(path, []byte("second"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second write: %v, want ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "first" {
		t.Errorf("key became %q", got)
	}
}

func TestDirSizeCountsRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 23), 0o644)
	os.Symlink("/a/rather/long/target/that/is/not/counted", filepath.Join(dir, "link"))
	if n := DirSize(context.Background(), dir); n != 123 {
		t.Errorf("DirSize = %d, want 123", n)
	}
	if n := DirSize(context.Background(), filepath.Join(dir, "a")); n != 100 {
		t.Errorf("DirSize of a file = %d, want 100", n)
	}
	if n := DirSize(context.Background(), filepath.Join(dir, "missing")); n != 0 {
		t.Errorf("DirSize of a missing dir = %d", n)
	}
}

// A cancelled walk stops at the next directory instead of finishing the
// tree: the disk sizer's pass over a NAS share has to end when fogd does.
func TestDirSizeStopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	for i := range 50 {
		sub := filepath.Join(dir, strconv.Itoa(i))
		os.MkdirAll(sub, 0o755)
		os.WriteFile(filepath.Join(sub, "f"), make([]byte, 10), 0o644)
	}
	if n := DirSize(context.Background(), dir); n != 500 {
		t.Fatalf("uncancelled DirSize = %d, want 500", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := DirSize(ctx, dir); n != 0 {
		t.Errorf("cancelled DirSize = %d, want 0: the walk went on past a cancelled context", n)
	}
}

func TestFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("abc"), 0o644)
	got, err := FileSHA256(path)
	if err != nil || got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("FileSHA256 = %q, %v", got, err)
	}
}
