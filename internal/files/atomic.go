package files

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/jobs"
)

// writeFS is where atomicWrite stages and renames: an *os.Root, or osFS for
// absolute paths outside one (a snapshot target, a root being moved).
type writeFS interface {
	OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error)
	Remove(name string) error
	Rename(oldname, newname string) error
	Chtimes(name string, atime, mtime time.Time) error
}

type osFS struct{}

func (osFS) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}
func (osFS) Remove(name string) error                  { return os.Remove(name) }
func (osFS) Rename(oldname, newname string) error      { return os.Rename(oldname, newname) }
func (osFS) Chtimes(name string, a, m time.Time) error { return os.Chtimes(name, a, m) }

// writeOpts shapes one atomicWrite.
type writeOpts struct {
	// tmp is the staging name; empty means a fixed-length random name beside
	// it (stagingName).
	tmp  string
	perm fs.FileMode // used as given, 0 included
	// mtime, when set, is stamped on the file before the rename.
	mtime time.Time
	// hash digests the bytes on the way; wantSHA, when set, must match or
	// nothing is placed (ErrDigestMismatch).
	hash    bool
	wantSHA string
	p       *jobs.Progress
	// beforeRename runs on the staged file last; an error abandons it.
	beforeRename func(tmp string) error
	// place, when set, puts the staged file at its name instead of a plain
	// rename (placeOwned).
	place func(tmp, name string) error
}

// atomicWrite copies src to name through a staging file: written, fsynced,
// stamped, then renamed, so a crash or a failed check never leaves a
// partial file under the final name. Returns the hex sha256 when hashing.
// OS errors come back unmapped; root callers map them.
func atomicWrite(dst writeFS, name string, src io.Reader, o writeOpts) (string, error) {
	tmp := o.tmp
	if tmp == "" {
		tmp = stagingName(name)
	}
	// A fresh file, never whatever already holds the staging name: beside
	// the destination that name is in a folder an app may write, and a link
	// planted there would otherwise have these bytes written through it into
	// a file somewhere else. A leftover from a crash is removed first (a
	// link is removed as itself); one that reappears in between fails the
	// write.
	dst.Remove(tmp)
	f, err := dst.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, o.perm)
	if err != nil {
		return "", err
	}
	var h hash.Hash
	var w io.Writer = f
	if o.hash || o.wantSHA != "" {
		h = sha256.New()
		w = io.MultiWriter(f, h)
	}
	if o.p != nil {
		w = &progressWriter{w: w, p: o.p}
	}
	_, err = io.Copy(w, src)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		dst.Remove(tmp)
		return "", err
	}
	var sum string
	if h != nil {
		sum = hex.EncodeToString(h.Sum(nil))
		if o.wantSHA != "" && !strings.EqualFold(sum, o.wantSHA) {
			dst.Remove(tmp)
			return "", ErrDigestMismatch
		}
	}
	if !o.mtime.IsZero() {
		dst.Chtimes(tmp, o.mtime, o.mtime)
	}
	if o.beforeRename != nil {
		if err := o.beforeRename(tmp); err != nil {
			dst.Remove(tmp)
			return "", err
		}
	}
	rename := dst.Rename
	if o.place != nil {
		rename = o.place
	}
	if err := rename(tmp, name); err != nil {
		dst.Remove(tmp)
		return "", err
	}
	return sum, nil
}

// stagingName is a fresh name beside name to write it under first. Fixed
// length and random, never name plus a suffix: a name already at the
// 255-byte limit (an app's, or 82 CJK characters) plus a suffix is too
// long to create, and that one file then failed every backup and copy.
func stagingName(name string) string {
	dir := ""
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		dir = name[:i+1]
	}
	return dir + ".fog-staging-" + randHex(8)
}
