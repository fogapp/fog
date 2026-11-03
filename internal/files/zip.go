package files

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
)

// WriteZip streams an archive of the given entries to w with no temporary
// file: a folder download of any size starts immediately and costs no
// disk. Already-compressed content is stored, not deflated. Symlinks and
// special files are skipped; the reserved directory never appears.
func (m *Manager) WriteZip(ctx context.Context, w io.Writer, rels []string) error {
	root, err := m.openRoot()
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := lstatNF(root, rel)
		if err != nil {
			return err
		}
		// Entries are named relative to the selection's parent, so zipping
		// "Photos/2024" yields "2024/…", and zipping the root yields the
		// entries themselves.
		base := ""
		if rel != "" {
			base, _ = Split(rel)
		}
		err = walkTree(root, rel, info, func(p string, fi fs.FileInfo) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := p
			if base != "" {
				name = p[len(base)+1:]
			}
			if name == "" {
				return nil
			}
			return addZipEntry(zw, root, p, name, fi)
		})
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

func addZipEntry(zw *zip.Writer, root *os.Root, rel, name string, fi fs.FileInfo) error {
	if fi.IsDir() {
		h := &zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: fi.ModTime()}
		h.SetMode(fi.Mode())
		_, err := zw.CreateHeader(h)
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	h, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	h.Name = name
	h.Method = zip.Deflate
	if storeInZip(MimeFor(path.Base(name))) {
		h.Method = zip.Store
	}
	out, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	f, _, err := openFileNF(root, rel)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(out, f)
	return err
}

// walkTree visits rel (with its lstat info) and, if it is a directory,
// everything beneath it, pre-order, never following symlinks.
func walkTree(root *os.Root, rel string, info fs.FileInfo, fn func(rel string, fi fs.FileInfo) error) error {
	return walkTreeOpts(context.Background(), root, rel, info, walkOpts{}, fn)
}

// walkOpts tunes walkTreeOpts.
type walkOpts struct {
	// vanishedOK passes over a directory that disappears between being
	// listed and being read, instead of failing the walk.
	vanishedOK bool
	// tooDeep, when set, is told of each folder more than MaxDepth levels
	// below the root, which is then left out; unset, such a folder fails
	// the walk with ErrTooDeep.
	tooDeep func(rel string)
}

// walkTreeOpts is walkTree with cancellation and options. fn returning
// fs.SkipDir prunes a directory (and is ignored on a file). Each folder is
// read through its parent's descriptor, a bounded number of them held at
// once (deepwalk.go), and the walk goes no deeper than MaxDepth.
func walkTreeOpts(ctx context.Context, root *os.Root, rel string, info fs.FileInfo, o walkOpts, fn func(rel string, fi fs.FileInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(rel, info); err != nil {
		if errors.Is(err, fs.SkipDir) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if depthOf(rel) > MaxDepth {
		if o.tooDeep != nil {
			o.tooDeep(rel)
			return nil
		}
		return tooDeep(rel)
	}
	fd, err := openDirFD(root, rel)
	if err != nil {
		if o.vanishedOK && errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	s, err := newDirStack(fd, MaxDepth-depthOf(rel))
	if err != nil {
		return err
	}
	defer s.close()
	return walkTreeAt(ctx, s, 0, rel, o, fn)
}

// walkTreeAt walks the children of the folder at level of s (rel).
func walkTreeAt(ctx context.Context, s *dirStack, level int, rel string, o walkOpts, fn func(rel string, fi fs.FileInfo) error) error {
	fd, err := s.fd(level)
	var ents []fs.FileInfo
	if err == nil {
		ents, err = readDirAt(fd, rel)
	}
	if err != nil {
		if o.vanishedOK && errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	for _, fi := range ents {
		if err := ctx.Err(); err != nil {
			return err
		}
		child := Join(rel, fi.Name())
		if err := fn(child, fi); err != nil {
			if errors.Is(err, fs.SkipDir) {
				continue
			}
			return err
		}
		if !fi.IsDir() {
			continue
		}
		if _, err := s.push(fi.Name(), rawStat(fi)); err != nil {
			switch {
			case errors.Is(err, ErrTooDeep) && o.tooDeep != nil:
				o.tooDeep(child)
				continue
			case errors.Is(err, ErrTooDeep):
				return tooDeep(child)
			case o.vanishedOK && errors.Is(err, ErrNotFound):
				continue
			}
			return fmt.Errorf("%s: %w", child, err)
		}
		err := walkTreeAt(ctx, s, level+1, child, o, fn)
		s.pop()
		if err != nil {
			return err
		}
	}
	return nil
}
