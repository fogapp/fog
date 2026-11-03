package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// StampLayout is how fogd names a snapshot after the moment it was taken:
// UTC, to the second, sorting as a string the way it sorts in time. App
// backups, Files snapshots, recovery kits and the offsite locker all use it,
// and the offsite side parses it back, so there is one spelling.
const StampLayout = "20060102-150405"

// Stamp is t as a snapshot stamp.
func Stamp(t time.Time) string { return t.UTC().Format(StampLayout) }

// ParseStamp reads a stamp back to its time. A "-N" suffix — what
// FreeStamp adds when two snapshots land in the same second — is allowed
// and ignored: both snapshots were taken in that second.
func ParseStamp(s string) (time.Time, bool) {
	if i := len(StampLayout); len(s) > i+1 && s[i] == '-' {
		for _, c := range s[i+1:] {
			if c < '0' || c > '9' {
				return time.Time{}, false
			}
		}
		s = s[:i]
	}
	t, err := time.Parse(StampLayout, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// FreeStamp is t's stamp, or the stamp with "-2", "-3"... appended when
// taken reports the plain one in use. Two snapshots in one second (a
// double-click on Snapshot now) must not share a name.
func FreeStamp(t time.Time, taken func(stamp string) bool) string {
	base := Stamp(t)
	stamp := base
	for i := 2; taken(stamp); i++ {
		stamp = base + "-" + strconv.Itoa(i)
	}
	return stamp
}

// MkdirStamped creates parent/<stamp> for t — parent itself made if need
// be — and returns its path. os.Mkdir, not MkdirAll, so a directory that
// already exists is never adopted: of two snapshots in the same second the
// second gets "<stamp>-2" instead of writing into the first one's files.
func MkdirStamped(parent string, t time.Time, perm os.FileMode) (string, error) {
	if err := os.MkdirAll(parent, perm); err != nil {
		return "", err
	}
	base := Stamp(t)
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name += "-" + strconv.Itoa(i)
		}
		dir := filepath.Join(parent, name)
		err := os.Mkdir(dir, perm)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) || i >= 1000 {
			return "", err
		}
	}
}
