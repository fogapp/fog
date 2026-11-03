package offsite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
)

// RestoreFiles writes a Files snapshot from the locker into dir: every
// file in the manifest is fetched from its blob, hashed as it lands, and
// refused if the digest disagrees with the manifest. Paths are confined
// to dir. onFile, when non-nil, is told each path as it completes.
//
// Files are written whole to a temp name and renamed into place, so an
// interrupted restore leaves complete files and missing ones, never a
// half-written one. Re-running restores only what is missing or differs.
func (r *Reader) RestoreFiles(ctx context.Context, stamp, dir string, onFile func(path string)) (count int64, bytes int64, err error) {
	var snap FilesSnapshot
	if err := r.Document(ctx, r.Layout.FilesSnapshot(stamp), &snap); err != nil {
		return 0, 0, err
	}
	if snap.Format != Format {
		return 0, 0, fmt.Errorf("snapshot %s declares format %q", stamp, snap.Format)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, 0, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	// One unreadable or unsafe file must not strand the rest of the restore:
	// the loop reports it and carries on, so a single bad blob in the bucket
	// costs that one file, not the whole Drive. A cancellation still stops at
	// once; an unsafe path is still refused for that entry.
	var failed []string
	for _, f := range snap.Files {
		if err := ctx.Err(); err != nil {
			return count, bytes, err
		}
		rel, ok := safeRel(f.Path)
		if !ok {
			failed = append(failed, fmt.Sprintf("%s (unsafe path)", f.Path))
			continue
		}
		if same, _ := fileMatches(root, rel, f); same {
			count++
			bytes += f.Size
			continue
		}
		if err := r.restoreOne(ctx, root, rel, f); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", f.Path, err))
			continue
		}
		count++
		bytes += f.Size
		if onFile != nil {
			onFile(f.Path)
		}
	}
	if len(failed) > 0 {
		show := failed
		if len(show) > 10 {
			show = append(show[:10:10], fmt.Sprintf("… and %d more", len(failed)-10))
		}
		return count, bytes, fmt.Errorf("%d of %d files could not be restored: %s", len(failed), len(snap.Files), strings.Join(show, "; "))
	}
	return count, bytes, nil
}

// safeRel accepts a slash path that stays inside its root.
func safeRel(p string) (string, bool) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", false
	}
	return clean, true
}

// fileMatches reports whether the file at rel already has the manifest's
// size and digest — the re-run shortcut.
func fileMatches(root *os.Root, rel string, f FileEntry) (bool, error) {
	info, err := root.Stat(rel)
	if err != nil || !info.Mode().IsRegular() || info.Size() != f.Size {
		return false, err
	}
	fh, err := root.Open(rel)
	if err != nil {
		return false, err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == f.SHA256, nil
}

func (r *Reader) restoreOne(ctx context.Context, root *os.Root, rel string, f FileEntry) error {
	if len(f.Blob) < 4 {
		return errors.New("manifest entry has no blob")
	}
	if d := filepath.Dir(rel); d != "." {
		if err := mkdirAllRoot(root, d); err != nil {
			return err
		}
	}
	src, err := r.Open(ctx, r.Layout.Blob(f.Blob))
	if err != nil {
		return err
	}
	defer src.Close()
	// A fixed-length name beside it, not rel plus a suffix: a name at the
	// 255-byte limit plus a suffix could not be created.
	tmpName := filepath.Join(filepath.Dir(rel), ".fog-restore-"+randx.Hex(8))
	tmp, err := root.OpenFile(tmpName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		root.Remove(tmpName)
		return err
	}
	if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		root.Remove(tmpName)
		return fmt.Errorf("blob content does not match the manifest (got %d bytes, want %d)", n, f.Size)
	}
	if err := root.Rename(tmpName, rel); err != nil {
		root.Remove(tmpName)
		return err
	}
	if f.Mtime > 0 {
		mt := time.Unix(0, f.Mtime)
		root.Chtimes(rel, mt, mt)
	}
	return nil
}

func mkdirAllRoot(root *os.Root, rel string) error {
	parts := strings.Split(rel, string(filepath.Separator))
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = filepath.Join(cur, p)
		}
		if err := root.Mkdir(cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}
