// Package fsx holds the few file operations fogd needs done one careful
// way everywhere: whole-file replacement that a crash can't tear, the size
// of a tree, a file's digest, how a size reads to a person, and how a
// snapshot is named after the second it was taken.
//
// Each of these used to be written out beside its one caller, five times
// over for some, with small differences nobody chose. A torn write in
// particular is the kind of bug that only shows up after a power cut, so
// there is one implementation to get right.
package fsx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// WriteFileAtomic replaces path with data: a temp file in the same
// directory, written, given perm, fsynced, then renamed over path. A crash
// at any point leaves either the old file or the new one, never a torn mix
// — truncate-in-place would leave whatever had been written so far.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return WriteAtomic(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteAtomic is WriteFileAtomic for content produced as a stream: write
// fills the temp file, and path is replaced only if it returns nil.
func WriteAtomic(path string, perm os.FileMode, write func(io.Writer) error) error {
	tmp, err := stage(path, perm, write)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

// WriteFileExclusive is WriteFileAtomic for a file that must never be
// replaced once it exists: the temp file is linked into place rather than
// renamed, so of two racing writers exactly one wins and the loser gets an
// error satisfying errors.Is(err, fs.ErrExist).
func WriteFileExclusive(path string, data []byte, perm os.FileMode) error {
	tmp, err := stage(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Link(tmp, path)
}

// stage writes a durable sibling temp file for path and returns its name.
func stage(path string, perm os.FileMode, write func(io.Writer) error) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	err = write(f)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// DirSize is the bytes of every regular file under root, symlinks not
// followed; root may itself be a file. Unreadable entries and a missing
// root count as empty — it answers "how much is here", not "is it all
// readable".
//
// A tree can be a media library on a NAS — terabytes over NFS, minutes of
// walking — so ctx is checked at every directory: once it ends the walk
// stops and the bytes counted so far come back. A caller that might be
// cancelled checks ctx.Err() before trusting the figure.
func DirSize(ctx context.Context, root string) int64 {
	return DirSizeExcept(ctx, root, nil)
}

// DirSizeExcept is DirSize leaving out the directories in skip (below root,
// never root itself) — the mount points inside a tree, whose bytes are on
// another disk and are counted there.
func DirSizeExcept(ctx context.Context, root string, skip map[string]bool) int64 {
	var n int64
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if path != root && skip[path] {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// FileSHA256 is the lowercase hex SHA-256 of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HumanBytes renders a size for a person: "512 B", "1.5 KB", "3.2 GB".
// Units step by 1024 and are written KB/MB/GB like the UI's formatBytes,
// so a message from the server and the page beside it name a size alike.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "B"
}
