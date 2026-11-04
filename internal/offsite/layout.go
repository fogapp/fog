package offsite

import (
	"path"
	"regexp"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// The object names inside a locker (docs/locker-contract.md §2). Names are
// plaintext by design — the operator can see their backups in the
// provider's console — and contents are sealed. Every name is under
// <prefix>/fog-locker/1/<locker id>/ so several fog instances can share a
// bucket and a retired locker never mixes with its successor.

// Layout addresses one locker in one bucket.
type Layout struct {
	// Prefix is the operator's bucket prefix, without slashes at either
	// end; "" for the bucket root.
	Prefix string
	ID     string
}

// LockersRoot is where every locker under a prefix lives; listing its
// sub-prefixes discovers lockers.
func LockersRoot(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "fog-locker/1/"
	}
	return prefix + "/fog-locker/1/"
}

// Root is the locker's own prefix, with the trailing slash.
func (l Layout) Root() string { return LockersRoot(l.Prefix) + l.ID + "/" }

func (l Layout) LockerJSON() string { return l.Root() + "locker.json" }

func (l Layout) KitsPrefix() string          { return l.Root() + "kits/" }
func (l Layout) Kit(name string) string      { return l.KitsPrefix() + name }
func (l Layout) AppsPrefix() string          { return l.Root() + "apps/" }
func (l Layout) AppPrefix(app string) string { return l.AppsPrefix() + app + "/" }

// AppSnapshot is the sealed tar of one backup's snapshot directory.
func (l Layout) AppSnapshot(app, stamp string) string { return l.AppPrefix(app) + stamp + ".snapshot" }

// AppRow is the sealed JSON describing that backup.
func (l Layout) AppRow(app, stamp string) string { return l.AppPrefix(app) + stamp + ".json" }

// ObjectsOf is every object one copy row owns, its own Key first. An app
// backup is two objects under one row — the snapshot (the row's Key) and
// the sealed row describing it — so whatever deletes, locks or moves a
// copy must do it to both; every other kind is the one object.
func (l Layout) ObjectsOf(c *store.OffsiteCopy) []string {
	if c.Kind == store.OffsiteKindApp {
		parts := strings.Split(l.Name(c.Key), "/")
		if len(parts) == 3 && parts[0] == "apps" && strings.HasSuffix(parts[2], ".snapshot") {
			return []string{c.Key, l.AppRow(parts[1], strings.TrimSuffix(parts[2], ".snapshot"))}
		}
	}
	if c.Kind == kindStore {
		return []string{c.Key, l.StoreKey(c.Subject)}
	}
	return []string{c.Key}
}

func (l Layout) FilesSnapshotsPrefix() string      { return l.Root() + "files/snapshots/" }
func (l Layout) FilesSnapshot(stamp string) string { return l.FilesSnapshotsPrefix() + stamp + ".json" }
func (l Layout) BlobsPrefix() string               { return l.Root() + "files/blobs/" }
func (l Layout) Blob(id string) string             { return l.BlobsPrefix() + id[:2] + "/" + id }
func (l Layout) IndexPrefix() string               { return l.Root() + "index/" }
func (l Layout) Index(stamp string) string         { return l.IndexPrefix() + stamp + ".json" }
func (l Layout) ProbePrefix() string               { return l.Root() + "probe/" }

// Name strips a key back to its path inside the locker — what the
// additional data of a sealed object is bound to, so the same object
// copied to another bucket or prefix still opens.
func (l Layout) Name(key string) string { return strings.TrimPrefix(key, l.Root()) }

// stampRe matches a name that is a stamp (fsx.StampLayout), optionally after
// a plain identifier prefix ending in a dash (fog-recovery-kit-<stamp>), and
// before any extensions. Anchored at both ends: a name with arbitrary leading
// bytes — control characters, spaces, a path — is not a stamp, so a planted
// object cannot be assigned a stamp it should not have and sorted "newest".
// The optional "-N" is the suffix a second snapshot in the same second gets
// locally; it is part of the stamp, so the two stay two objects offsite.
var stampRe = regexp.MustCompile(`^(?:[A-Za-z0-9_-]+-)?(\d{8}-\d{6}(?:-\d+)?)(?:\.[A-Za-z0-9]+)*$`)

// StampOf pulls the <stamp> out of a snapshot, row, index or kit name:
// the trailing YYYYMMDD-HHMMSS[-N] before any extensions.
func StampOf(key string) string {
	m := stampRe.FindStringSubmatch(path.Base(key))
	if m == nil {
		return ""
	}
	return m[1]
}
