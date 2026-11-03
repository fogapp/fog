package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/jobs"
)

// Trash is a rename into <root>/.fog/trash/<id>/<name> beside a small
// JSON receipt, <id>.json, that remembers where the entry came from. A
// rename is instant at any size and reversible, which is what lets the
// page delete without a confirm and offer Undo instead. The receipts live
// on disk, not in the index, so an index rebuild never forgets what is in
// the trash — and a shell can see and understand it.
//
// An entry in Apps or Content while that folder lives on another disk is
// renamed into the folder's own <folder>/.fog/trash instead — the rename
// has to stay on one filesystem — and its receipt says so (Bin). The
// receipts themselves all stay in the root's trash, so one listing finds
// every entry.

const trashDir = ReservedDir + "/trash"

// TrashRetention is how long a trashed entry survives before the sweep.
const TrashRetention = 30 * 24 * time.Hour

// TrashItem is one entry in the trash.
type TrashItem struct {
	ID        string `json:"id"`
	Path      string `json:"path"` // where it lived
	Name      string `json:"name"`
	Kind      string `json:"kind" ts:"\"file\" | \"dir\" | \"other\""`
	Size      int64  `json:"size"`
	TrashedAt string `json:"trashedAt"`
	// Bin is the placed folder whose own trash holds the entry; empty for
	// the root's.
	Bin string `json:"bin,omitempty"`
}

func (m *Manager) trashMetaPath(id string) string { return trashDir + "/" + id + ".json" }

// trashItemDir is the directory holding one entry, in its bin.
func (m *Manager) trashItemDir(it TrashItem) string {
	if it.Bin != "" && isPlaceable(it.Bin) {
		return stagingDir(it.Bin) + "/trash/" + it.ID
	}
	return trashDir + "/" + it.ID
}

func (m *Manager) trashItemPath(it TrashItem) string {
	return m.trashItemDir(it) + "/" + it.Name
}

// trashOne moves rel into the trash and fixes the index for its parent.
func (m *Manager) trashOne(root *os.Root, ix *Index, rel string, om *opMounts) (TrashItem, error) {
	return m.trashOneAs(root, ix, rel, false, om)
}

func (m *Manager) trashOneAs(root *os.Root, ix *Index, rel string, owned bool, om *opMounts) (TrashItem, error) {
	if rel == "" {
		return TrashItem{}, ErrBadName
	}
	if !owned {
		if err := m.guardRemove(om, rel); err != nil {
			return TrashItem{}, err
		}
	} else if err := checkNoLinks(root, rel); err != nil {
		return TrashItem{}, err
	}
	info, err := lstatNF(root, rel)
	if err != nil {
		return TrashItem{}, err
	}
	parent, name := Split(rel)
	it := TrashItem{ID: jobs.NewID(), Path: rel, Name: name, Kind: kindOf(info.Mode()), TrashedAt: time.Now().UTC().Format(time.RFC3339), Bin: m.placedFolder(rel)}
	if row, err := ix.Get(rel); err == nil {
		it.Size = row.Size
	} else if info.Mode().IsRegular() {
		it.Size = info.Size()
	}
	dir := m.trashItemDir(it)
	if err := mkdirAllNF(root, dir, 0o700); err != nil {
		// A delete that fails inside fog's own trash is usually not about
		// this file at all: the share has changed who fog is since the
		// folder was made. retryReserved takes it back, or says so.
		if err = m.retryReserved(err, func() error { return mkdirAllNF(root, dir, 0o700) }); err != nil {
			return TrashItem{}, mapOSError(err)
		}
	}
	if err := nf(root).RenameNoReplace(rel, m.trashItemPath(it)); err != nil {
		nf(root).Remove(dir)
		return TrashItem{}, mapOSError(err)
	}
	b, _ := json.Marshal(it)
	if err := nf(root).MkdirAll(trashDir, 0o700); err == nil {
		err = nf(root).WriteFile(m.trashMetaPath(it.ID), b, 0o600)
	}
	if err != nil {
		// Without the receipt the entry would be unrestorable; put it back.
		nf(root).RenameNoReplace(m.trashItemPath(it), rel)
		nf(root).Remove(dir)
		return TrashItem{}, err
	}
	// Its links go to the trash with it: whatever takes its name next is
	// not what they were made for.
	if err := m.Store.SuspendFileShares(rel, it.ID); err != nil {
		m.Log.Warn("files: suspending the links of a trashed entry", "path", rel, "err", err)
	}
	m.refreshDir(root, ix, parent, false)
	return it, nil
}

// Trash moves entries into the trash.
func (m *Manager) Trash(rels []string) ([]TrashItem, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return nil, err
	}
	var out []TrashItem
	om := &opMounts{}
	for _, rel := range rels {
		it, err := m.trashOne(root, ix, rel, om)
		if err != nil {
			return out, err
		}
		out = append(out, it)
	}
	if out == nil {
		out = []TrashItem{}
	}
	return out, nil
}

// TrashOwned moves rel to the trash for the subsystem that owns its folder
// — a peer sync pruning what its origin no longer shares — so that what a
// peer's word removes can be restored. Guards aren't consulted (the folder
// is the caller's); links are still refused.
func (m *Manager) TrashOwned(rel string) (TrashItem, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return TrashItem{}, err
	}
	return m.trashOneAs(root, ix, rel, true, &opMounts{})
}

// RemoveEmptyDir removes the folder rel only if nothing is in it, for the
// subsystem that owns it: one entry inside, and it stays. Listed-only
// entries under it go with it.
func (m *Manager) RemoveEmptyDir(rel string) error {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return err
	}
	if rel == "" {
		return ErrBadName
	}
	info, err := lstatNF(root, rel)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrNotDirectory
	}
	if err := nf(root).Remove(rel); err != nil { // rmdir: refused unless empty
		return mapOSError(err)
	}
	ix.wmu.Lock()
	ix.dropDehydratedLocked(rel)
	ix.wmu.Unlock()
	parent, _ := Split(rel)
	_, err = m.refreshDir(root, ix, parent, false)
	return err
}

// TrashAppFolder sends an app's own folder — Apps/<App> — to the trash, for
// an app deleted with its data. No folder there is not an error: the app
// kept its data elsewhere.
func (m *Manager) TrashAppFolder(appName string) error {
	rel := apps.AppDataFolder(appName)
	if _, err := m.Stat(rel); err != nil {
		return nil
	}
	_, err := m.Trash([]string{rel})
	return err
}

// ListTrash reads every receipt, newest first.
func (m *Manager) ListTrash() ([]TrashItem, error) {
	root, err := m.openRoot()
	if err != nil {
		return nil, err
	}
	f, err := openDirNF(root, trashDir)
	if err != nil {
		return []TrashItem{}, nil
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	out := []TrashItem{}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := nf(root).ReadFile(trashDir + "/" + e.Name())
		if err != nil {
			continue
		}
		var it TrashItem
		if json.Unmarshal(b, &it) == nil && it.ID != "" {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TrashedAt > out[j].TrashedAt })
	return out, nil
}

func (m *Manager) trashItem(root *os.Root, id string) (TrashItem, error) {
	if strings.ContainsAny(id, "/.") || id == "" {
		return TrashItem{}, ErrNotFound
	}
	b, err := nf(root).ReadFile(m.trashMetaPath(id))
	if err != nil {
		return TrashItem{}, ErrNotFound
	}
	var it TrashItem
	if err := json.Unmarshal(b, &it); err != nil || it.ID != id {
		return TrashItem{}, ErrNotFound
	}
	return it, nil
}

// RestoreTrash puts entries back where they came from; a name taken in
// the meantime gets the "name 2" treatment rather than a refusal.
func (m *Manager) RestoreTrash(ids []string) ([]string, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		it, err := m.trashItem(root, id)
		if err != nil {
			return out, err
		}
		parent, name := Split(it.Path)
		if err := m.guardWrite(it.Path); err != nil {
			return out, err
		}
		if parent != "" {
			if err := mkdirAllNF(root, parent, 0o755); err != nil {
				return out, err
			}
		}
		dest := it.Path
		if exists(root, dest) {
			dest = Join(parent, uniqueName(root, parent, name))
		}
		if err := nf(root).RenameNoReplace(m.trashItemPath(it), dest); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				return out, fmt.Errorf("%s went to the trash before its folder moved to another disk, so it can't be put back in place; restore it from a Drive snapshot, or empty the trash once you no longer need it", it.Name)
			}
			return out, mapOSError(err)
		}
		nf(root).Remove(m.trashMetaPath(id))
		nf(root).Remove(m.trashItemDir(it))
		if err := m.Store.RestoreFileShares(id, it.Path, dest); err != nil {
			m.Log.Warn("files: restoring the links of a restored entry", "path", dest, "err", err)
		}
		if it.Kind == "dir" {
			m.walkDir(context.Background(), root, ix, dest, time.Now().UnixNano())
		}
		m.refreshDir(root, ix, parent, false)
		out = append(out, dest)
	}
	if out == nil {
		out = []string{}
	}
	m.kickHasher()
	return out, nil
}

// PurgeTrash deletes one trashed entry for good.
func (m *Manager) PurgeTrash(id string) error {
	root, err := m.openRoot()
	if err != nil {
		return err
	}
	it, err := m.trashItem(root, id)
	if err != nil {
		return err
	}
	if it.Bin != "" {
		// A bin on a disk that is not there now: purging would only find
		// the empty mount point. The entry waits for its disk.
		if reason := m.placementOffline(it.Bin); reason != "" {
			return fmt.Errorf("%s is not available: %s", it.Bin, reason)
		}
	}
	if err := nf(root).RemoveAll(m.trashItemDir(it)); err != nil {
		return mapOSError(err)
	}
	if err := m.Store.DeleteTrashedFileShares(id); err != nil {
		return err
	}
	return nf(root).Remove(m.trashMetaPath(id))
}

// EmptyTrash deletes everything in the trash.
func (m *Manager) EmptyTrash() (int, error) {
	items, err := m.ListTrash()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		if err := m.PurgeTrash(it.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// TrashStats is the tally the page shows.
type TrashStats struct {
	Items int   `json:"items"`
	Bytes int64 `json:"bytes"`
}

func (m *Manager) TrashStats() TrashStats {
	items, _ := m.ListTrash()
	var s TrashStats
	for _, it := range items {
		s.Items++
		s.Bytes += it.Size
	}
	return s
}

// sweep reclaims what nothing will come back for: trash past its
// retention, and upload sessions abandoned for a day.
func (m *Manager) sweep() {
	root, err := m.openRoot()
	if err != nil {
		return
	}
	items, _ := m.ListTrash()
	cutoff := time.Now().Add(-TrashRetention)
	for _, it := range items {
		if t, err := time.Parse(time.RFC3339, it.TrashedAt); err == nil && t.Before(cutoff) {
			if err := m.PurgeTrash(it.ID); err != nil {
				m.Log.Warn("trash sweep", "id", it.ID, "err", err)
			} else {
				m.Log.Info("trash sweep: purged", "path", it.Path, "bytes", it.Size)
			}
		}
	}
	m.sweepUploads(root)
	m.sweepShares()
}
