package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/store"
)

// Share links: one token names one file or folder, optionally until a
// date and optionally behind a password. The token is shown once and
// stored as a digest; the page served at /s/<token> is fog's, without a
// session, so the link works for someone who has never seen fog.
//
// A link is of an item, not of a name. It records the item's path and its
// inode, and it leads somewhere only while that path holds that inode:
//
//   - renamed or moved by fog, the item takes its links along (RenamePath);
//     moved to another disk by a copy, it takes them along and they are
//     bound to the copy (rebindShares);
//   - trashed, its links are suspended with it (trashed_with): restored,
//     they come back wherever it is restored to; purged, they are deleted;
//   - deleted for good by fog (RemovePermanently), its links are deleted;
//   - replaced or deleted behind fog's back, the path holds another inode
//     or nothing, and the link leads nowhere. It shows as broken in the
//     list of links rather than ever serving the newcomer.

var ErrShareExpired = errors.New("this link has expired")

// CreateShare mints a link for rel. ttl of zero means never expires.
func (m *Manager) CreateShare(rel string, ttl time.Duration, password string) (token string, share *store.FileShare, err error) {
	root, err := m.openRoot()
	if err != nil {
		return "", nil, err
	}
	if rel == "" {
		return "", nil, ErrBadName
	}
	info, err := lstatNF(root, rel)
	if err != nil {
		return "", nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%w: only a file or a folder can be shared", ErrBadName)
	}
	token, digest := auth.NewToken()
	hash := ""
	if password != "" {
		if hash, err = auth.HashPassword(password); err != nil {
			return "", nil, err
		}
	}
	expires := ""
	if ttl > 0 {
		expires = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	share, err = m.Store.CreateFileShare(digest, rel, expires, hash, inodeOf(info))
	if err != nil {
		return "", nil, err
	}
	return token, share, nil
}

// ListShares returns the links for one entry, or all of them for "". All
// of them are marked: suspended in the trash, or broken because their path
// no longer holds their item. One entry's are only its own live links.
func (m *Manager) ListShares(rel string) ([]store.FileShare, error) {
	list, err := m.Store.ListFileShares(rel)
	if err != nil {
		return nil, err
	}
	root, err := m.openRoot()
	if err != nil {
		return list, nil
	}
	out := list[:0]
	for _, sh := range list {
		if !sh.InTrash {
			_, ok := m.shareTarget(root, &sh)
			sh.Broken = !ok
		}
		if rel != "" && sh.Broken {
			continue
		}
		out = append(out, sh)
	}
	return out, nil
}

// shareTarget says whether sh's path holds sh's item now, binding a link
// made before links had an identity to what it finds.
func (m *Manager) shareTarget(root *os.Root, sh *store.FileShare) (fs.FileInfo, bool) {
	info, err := lstatNF(root, sh.Path)
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return nil, false
	}
	ino := inodeOf(info)
	if sh.Inode == 0 && ino != 0 {
		if m.Store.BindFileShare(sh.ID, ino) == nil {
			sh.Inode = ino
		}
		return info, true
	}
	return info, sh.Inode == ino
}

// rebindShares binds the live links at rel or beneath it to whatever their
// paths hold now: the items were carried there by a copy (a move to another
// disk), which gives every file and folder a new inode.
func (m *Manager) rebindShares(rel string) {
	root, err := m.openRoot()
	if err != nil {
		return
	}
	list, err := m.Store.LiveFileSharesUnder(rel)
	if err != nil {
		return
	}
	for _, sh := range list {
		if info, err := lstatNF(root, sh.Path); err == nil {
			m.Store.BindFileShare(sh.ID, inodeOf(info))
		}
	}
}

func (m *Manager) RevokeShare(id int64) error {
	return m.Store.DeleteFileShare(id)
}

// ResolveShare turns a presented token into its share, refusing expired
// ones (and, once a day, sweeping them). Counts the visit.
func (m *Manager) ResolveShare(token string) (*store.FileShare, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	sh, err := m.Store.FileShareByDigest(auth.DigestToken(token))
	if err != nil {
		return nil, ErrNotFound
	}
	if sh.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, sh.ExpiresAt); err == nil && time.Now().After(t) {
			return nil, ErrShareExpired
		}
	}
	if sh.InTrash {
		return nil, ErrNotFound
	}
	root, err := m.openRoot()
	if err != nil {
		return nil, err
	}
	if _, ok := m.shareTarget(root, sh); !ok {
		return nil, ErrNotFound
	}
	m.Store.TouchFileShare(sh.ID, sh.LastUsedAt)
	return sh, nil
}

// sweepShares drops links past their expiry.
func (m *Manager) sweepShares() {
	all, err := m.Store.ListFileShares("")
	if err != nil {
		return
	}
	now := time.Now()
	for _, sh := range all {
		if sh.ExpiresAt == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, sh.ExpiresAt); err == nil && now.After(t.Add(24*time.Hour)) {
			m.Store.DeleteFileShare(sh.ID)
		}
	}
}
