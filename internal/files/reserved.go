// Fog's reserved directory — the trash and the staging area uploads land in
// before they are named — is the one part of the Files tree fog keeps for
// itself. It is also the one part that can stop belonging to fog: on a share
// that maps identities (an NFS export with a squash setting), the uid fog
// writes as is the server's choice, not fogd's, and changing it strands a
// directory fog created under an identity it no longer has. Fog then reports
// its own homelessness as "openat .fog/trash: permission denied", which names
// the syscall and nothing else. This file makes fog take the directory back
// when it can, and say what happened when it cannot.
package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// reservedSubdirs are the directories fog keeps under ReservedDir.
var reservedSubdirs = []string{"uploads", "trash"}

// ensureReserved creates the reserved directories under root and proves fog
// can write in them.
func (m *Manager) ensureReserved(root string) error {
	res := filepath.Join(root, ReservedDir)
	err := prepareReserved(root, res)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	// Refused inside fog's own directory: on this filesystem fog is not the
	// identity that created it. Try to take it back before explaining.
	adoptReserved(root, res)
	if again := prepareReserved(root, res); again == nil {
		return nil
	}
	owner, ownerOK := pathOwner(res)
	mine, _, mineOK := fsIdentity(root)
	return reservedRefused(res, err, owner, mine, ownerOK && mineOK)
}

// prepareReserved creates the directories and writes in each of them. The
// write is the only proof worth having: a directory fog cannot enter still
// stats, still lists from its parent, and still satisfies MkdirAll.
func prepareReserved(root, res string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for _, name := range reservedSubdirs {
		if err := os.MkdirAll(filepath.Join(res, name), 0o755); err != nil {
			return err
		}
	}
	// The reserved directory is fog's; keep other uids out of staging.
	os.Chmod(res, 0o700)
	for _, name := range reservedSubdirs {
		if err := probeWrite(filepath.Join(res, name)); err != nil {
			return err
		}
	}
	return nil
}

// adoptReserved hands the reserved directories to whoever fog is on this
// filesystem. Every call is best effort: a chown fog is not entitled to make
// fails, which is exactly the case reservedRefused goes on to explain.
func adoptReserved(root, res string) {
	uid, gid, ok := fsIdentity(root)
	if !ok {
		return
	}
	for _, p := range append([]string{res}, reservedPaths(res)...) {
		os.Chown(p, uid, gid)
		os.Chmod(p, 0o700)
	}
}

func reservedPaths(res string) []string {
	out := make([]string, 0, len(reservedSubdirs))
	for _, name := range reservedSubdirs {
		out = append(out, filepath.Join(res, name))
	}
	return out
}

// reservedRefused names the mismatch when there is one to name. The bare
// refusal is still worth reporting — a read-only share reaches here too —
// but the mismatch is the case an operator cannot work out unaided.
func reservedRefused(res string, cause error, owner, mine int, known bool) error {
	base := fmt.Errorf("%w: fog cannot write in %s, the folder it keeps for the trash and for staging uploads: %v", ErrNotConfigured, res, cause)
	if !known || owner == mine {
		return base
	}
	return fmt.Errorf("%w — the folder belongs to uid %d while fog writes as uid %d here, so this share is presenting fog as a different user than the one that created it. Give uid %d ownership of %s on the server that hosts the share, or put back the identity mapping fog had when it created the folder", base, owner, mine, mine, res)
}

// retryReserved handles a write refused inside the reserved directory: it
// repairs what it can and runs the operation once more, and when it cannot,
// returns the error that says why fog no longer owns its own folder.
func (m *Manager) retryReserved(cause error, again func() error) error {
	if !errors.Is(cause, fs.ErrPermission) {
		return cause
	}
	if err := m.ensureReserved(m.Root()); err != nil {
		return err
	}
	return again()
}

// fsIdentity reports the uid and gid the filesystem records for something fog
// creates in dir. On a local disk that is fogd's own; on a share that maps
// identities it is the server's answer, and only the server's answer says
// anything about the files fog owns there.
func fsIdentity(dir string) (uid, gid int, ok bool) {
	f, err := os.CreateTemp(dir, ".fog-identity-*")
	if err != nil {
		return 0, 0, false
	}
	defer os.Remove(f.Name())
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// pathOwner reports the uid recorded for an existing path.
func pathOwner(path string) (uid int, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// probeWrite creates and removes one file of fog's own, named so that an
// operator who ever sees one knows whose it is.
func probeWrite(dir string) error {
	f, err := os.CreateTemp(dir, ".fog-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
