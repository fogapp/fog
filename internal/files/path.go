// Package files is fog's own filesystem: an operator-chosen root of plain
// files and directories, a rebuildable SQLite index beside it for fast
// listing, sorting and search, and the operations the Drive page performs
// on it. The tree is the truth; the index is a cache. Everything that
// touches disk goes through *os.Root so a client path can never resolve
// outside the root, symlinks included.
package files

import (
	"errors"
	"path"
	"strings"
	"unicode/utf8"
)

// ReservedDir is the one name under the root that belongs to fog: upload
// staging and the trash live under it, and it is hidden from every listing.
const ReservedDir = ".fog"

// maxNameBytes is the Linux NAME_MAX; ext4, xfs, NFS and SMB all agree.
const maxNameBytes = 255

var (
	ErrEscapes       = errors.New("path escapes the root")
	ErrReserved      = errors.New(ReservedDir + " is reserved for fog")
	ErrBadName       = errors.New("invalid name")
	ErrNameTooLong   = errors.New("name is longer than 255 bytes")
	ErrNotDirectory  = errors.New("not a directory")
	ErrIsDirectory   = errors.New("is a directory")
	ErrNotFound      = errors.New("no such file or directory")
	ErrExists        = errors.New("already exists")
	ErrNotConfigured = errors.New("files root is not usable")
)

// CleanRel normalizes a client-supplied path into a root-relative slash
// path: leading and trailing slashes dropped, "." and ".." resolved. The
// empty string is the root. Anything that would climb out, name the
// reserved directory, or carry bytes a filesystem would reject is refused
// here — but the invariant the package rests on is *os.Root, not this
// function; this is what turns a bad request into a 400 instead of an
// opaque syscall error.
func CleanRel(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.ContainsRune(p, 0) {
		return "", ErrBadName
	}
	if !utf8.ValidString(p) {
		return "", ErrBadName
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	p = path.Clean(p)
	if p == "." {
		return "", nil
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", ErrEscapes
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if err := validateSegment(seg); err != nil {
			return "", err
		}
	}
	if reservedAt("", segs[0]) || (len(segs) > 1 && reservedAt(segs[0], segs[1])) {
		return "", ErrReserved
	}
	return p, nil
}

// reservedAt says name in parent is a reserved directory: the root's, or
// Apps' or Content's own — the one a folder that may live on another disk
// stages its uploads and trash in (see placement.go).
//
// Case does not matter: on a case-insensitive root (macOS, a CIFS share)
// ".FOG" is the same directory as ".fog", and "apps/.fog" is Apps' own.
// strings.EqualFold also covers a filesystem that normalises Unicode: the
// names are ASCII, which no normal form changes, and its folding is a
// superset of any filesystem's.
func reservedAt(parent, name string) bool {
	return strings.EqualFold(name, ReservedDir) &&
		(parent == "" || strings.EqualFold(parent, AppsDir) || strings.EqualFold(parent, ContentDir))
}

// ValidateName checks a single new entry name (mkdir, rename, upload) in
// the directory parent. The reserved name is only reserved at the top
// level and directly in Apps and Content; a ".fog" three levels down is
// just an odd folder.
func ValidateName(parent, name string) error {
	if err := validateSegment(name); err != nil {
		return err
	}
	if reservedAt(parent, name) {
		return ErrReserved
	}
	return nil
}

func validateSegment(seg string) error {
	if seg == "" || seg == "." || seg == ".." {
		return ErrBadName
	}
	if strings.ContainsAny(seg, "/\x00") {
		return ErrBadName
	}
	if !utf8.ValidString(seg) {
		return ErrBadName
	}
	if len(seg) > maxNameBytes {
		return ErrNameTooLong
	}
	return nil
}

// Join builds a root-relative path from a parent and a name; the root's
// children have no leading slash.
func Join(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// Split is the inverse of Join.
func Split(rel string) (parent, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

// osPath is the name *os.Root wants: "." for the root itself, otherwise
// the relative path as is.
func osPath(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// IsUnder reports whether rel is inside (or is) dir, by whole path
// segments: "photos/2024" is under "photos", "photos2" is not. An empty dir
// is the root and holds everything. It works the same on absolute paths,
// except that "/" is not special-cased: pass "" to mean "anything".
func IsUnder(rel, dir string) bool {
	if dir == "" {
		return true
	}
	return rel == dir || strings.HasPrefix(rel, dir+"/")
}

// subtree is a WHERE fragment matching rows whose col is dir or lies under
// it, with its arguments. It is a range, not a prefix LIKE: '0' is the byte
// after '/', so [dir/, dir0) is exactly "starts with dir/". A range can use
// the parent index, which LIKE can't against a BINARY column, and it is
// case-sensitive like the paths themselves, where LIKE would have folded
// "Photos/" onto "photos/".
func subtree(col, dir string) (string, []any) {
	return "(" + col + " = ? OR (" + col + " >= ? AND " + col + " < ?))", []any{dir, dir + "/", dir + "0"}
}

// escapeLike makes a path safe as a LIKE prefix with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
