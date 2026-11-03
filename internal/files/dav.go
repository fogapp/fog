package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"syscall"

	"golang.org/x/net/webdav"
)

// WebDAV: the same root, mounted in Finder or Explorer. The filesystem the
// handler sees is *os.Root with fog's rules on top — the reserved
// directory is invisible, a delete is a trash (so it can be undone from
// the page), and every write brings the index in line for the folder it
// touched, so a file saved from a desktop app shows up in the browser on
// the next listing.

// DAVHandler serves the root at prefix (e.g. "/dav"). Auth is the HTTP
// layer's business (an app password from files_access_tokens); by the
// time a request reaches this handler it is the operator.
func (m *Manager) DAVHandler(prefix string) http.Handler {
	return &webdav.Handler{
		Prefix:     prefix,
		FileSystem: &davFS{m: m},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				m.Log.Debug("webdav", "method", r.Method, "path", r.URL.Path, "err", err)
			}
		},
	}
}

type davFS struct {
	m *Manager
}

// rel turns a WebDAV name ("/Photos/2024") into a root-relative path,
// refusing the reserved directory as if it did not exist.
func (d *davFS) rel(name string) (string, error) {
	rel, err := CleanRel(name)
	if err != nil {
		if errors.Is(err, ErrReserved) {
			return "", fs.ErrNotExist
		}
		return "", fs.ErrPermission
	}
	return rel, nil
}

// toOSError maps package errors to the ones the webdav package keys on:
// it checks os.IsNotExist / os.IsPermission, which do not unwrap.
func toOSError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return fs.ErrNotExist
	case errors.Is(err, ErrExists):
		return fs.ErrExist
	case errors.Is(err, ErrReserved), errors.Is(err, ErrEscapes), errors.Is(err, ErrBadName), errors.Is(err, ErrNameTooLong), errors.Is(err, ErrMounted),
		errors.Is(err, ErrSymlink), errors.Is(err, ErrReadOnly):
		return fs.ErrPermission
	case errors.Is(err, ErrNotDirectory):
		return syscall.ENOTDIR
	case errors.Is(err, ErrIsDirectory):
		return syscall.EISDIR
	}
	return err
}

func (d *davFS) Mkdir(_ context.Context, name string, _ os.FileMode) error {
	rel, err := d.rel(name)
	if err != nil {
		return err
	}
	if rel == "" {
		return fs.ErrExist
	}
	_, err = d.m.Mkdir(rel)
	return toOSError(err)
}

func (d *davFS) OpenFile(_ context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	rel, err := d.rel(name)
	if err != nil {
		return nil, err
	}
	root, ix, err := d.m.rootAndIndex()
	if err != nil {
		return nil, fs.ErrPermission
	}
	if d.m.errPlacementOffline(rel) != nil {
		return nil, fs.ErrPermission
	}
	writing := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if writing {
		if rel == "" {
			return nil, fs.ErrPermission
		}
		parent, base := Split(rel)
		if err := ValidateName(parent, base); err != nil {
			return nil, fs.ErrPermission
		}
		if d.m.guardWrite(rel) != nil {
			return nil, fs.ErrPermission
		}
	}
	existed := exists(root, rel)
	if perm == 0 {
		perm = 0o644
	}
	// No link followed on the way or at the end, and nothing opened but a
	// file or a folder (nofollow.go).
	f, err := openNF(root, rel, flag, perm)
	if err != nil {
		return nil, toOSError(err)
	}
	if writing && !existed {
		d.m.chown(root, rel)
	}
	return &davFile{File: f, fs: d, rel: rel, root: root, ix: ix, writing: writing}, nil
}

func (d *davFS) RemoveAll(_ context.Context, name string) error {
	rel, err := d.rel(name)
	if err != nil {
		return err
	}
	if rel == "" {
		return fs.ErrPermission
	}
	root, ix, err := d.m.rootAndIndex()
	if err != nil {
		return fs.ErrPermission
	}
	if !exists(root, rel) {
		return fs.ErrNotExist
	}
	// A delete from a desktop is a trash, like a delete from the page.
	_, err = d.m.trashOne(root, ix, rel, nil)
	return toOSError(err)
}

func (d *davFS) Rename(_ context.Context, oldName, newName string) error {
	from, err := d.rel(oldName)
	if err != nil {
		return err
	}
	to, err := d.rel(newName)
	if err != nil {
		return err
	}
	if from == "" || to == "" {
		return fs.ErrPermission
	}
	return toOSError(d.m.renameAny(from, to))
}

func (d *davFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	rel, err := d.rel(name)
	if err != nil {
		return nil, err
	}
	root, err := d.m.openRoot()
	if err != nil {
		return nil, fs.ErrPermission
	}
	info, err := lstatNF(root, rel)
	if err != nil {
		return nil, toOSError(err)
	}
	if rel == "" {
		return rootInfo{davInfo{info}}, nil
	}
	return davInfo{info}, nil
}

// davInfo declares a file's type from its name, as every other route that
// serves Drive's bytes does. Without it the webdav package sniffs the
// first 512 bytes, and a file named anything at all that starts with
// "<html>" goes out as text/html.
type davInfo struct{ os.FileInfo }

func (i davInfo) ContentType(context.Context) (string, error) { return MimeFor(i.Name()), nil }

// rootInfo names the root "/" rather than "." for the client's benefit.
type rootInfo struct{ os.FileInfo }

func (r rootInfo) Name() string { return "/" }

// davFile is *os.File with two fog behaviours: the reserved directory is
// filtered from a root listing, and a write refreshes the index on close.
type davFile struct {
	*os.File
	fs      *davFS
	rel     string
	root    *os.Root
	ix      *Index
	writing bool
}

func (f *davFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	if f.rel == "" {
		return rootInfo{davInfo{info}}, nil
	}
	_, name := Split(f.rel)
	return davInfo{namedInfo{info, name}}, nil
}

// namedInfo carries the entry's own name: a descriptor's Stat names the
// file after the path it was opened by.
type namedInfo struct {
	os.FileInfo
	name string
}

func (n namedInfo) Name() string { return n.name }

// Readdir lists through the folder's own descriptor: names by getdents,
// each lstat'ed with fstatat on it (statNames) — never os.File.Readdir,
// which on some Go versions stats a path made from the file's name.
func (f *davFile) Readdir(count int) ([]fs.FileInfo, error) {
	names, err := f.File.Readdirnames(count)
	fd, ferr := fileFD(f.File)
	if ferr != nil {
		return nil, ferr
	}
	ents := statNames(fd, names)
	out := make([]fs.FileInfo, 0, len(ents))
	for _, e := range ents {
		if !reservedAt(f.rel, e.Name()) {
			out = append(out, davInfo{e})
		}
	}
	return out, err
}

func (f *davFile) Close() error {
	err := f.File.Close()
	if f.writing {
		parent, _ := Split(f.rel)
		f.fs.m.refreshDir(f.root, f.ix, parent, false)
		f.fs.m.kickHasher()
	}
	return err
}

// renameAny renames or moves one entry to a full new path, refusing to
// overwrite (WebDAV's handler removes the target first when the client
// asked to overwrite), and follows with the index, exclusions and shares.
func (m *Manager) renameAny(from, to string) error {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if IsUnder(to, from) {
		return ErrBadName
	}
	// The target too: a rename lands on top of whatever holds that name.
	om := &opMounts{}
	if err := m.guardRemove(om, from); err != nil {
		return err
	}
	if err := m.guardRemove(om, to); err != nil {
		return err
	}
	toParent, toName := Split(to)
	if err := ValidateName(toParent, toName); err != nil {
		return err
	}
	// Never onto another file: a case-only rename replaces only when both
	// names are the one entry (caseRename).
	rename, taken := caseRename(root, from, to)
	if taken {
		n, _ := ix.Get(to)
		return &ConflictError{Path: to, Existing: n}
	}
	if err := rename(from, to); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("%s and %s are on different disks: copy, then delete the original", from, toParent)
		}
		return mapOSError(err)
	}
	if err := ix.moveSubtree(from, to); err != nil {
		return err
	}
	m.followRename(from, to)
	fromParent, _ := Split(from)
	m.refreshDir(root, ix, fromParent, false)
	if toParent != fromParent {
		m.refreshDir(root, ix, toParent, false)
	}
	m.renamed(fromParent, toParent)
	return nil
}
