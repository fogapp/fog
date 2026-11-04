package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
)

// The store is the backup of record. A backup's snapshot directory (the
// "classic" copy) is removed once the store holds the same bytes and they
// have read back whole — except, for an app that has not yet proven its
// store copies restore, the newest one, kept as an anchor until the app's
// weekly test restores have passed from the store GraduationPasses times.
// Everything that reads a backup goes through BackupFiles or
// OpenBackupFile, which use the directory while it exists and rebuild from
// the store when it does not.

const (
	// GraduationPasses is how many test restores read from the store an app
	// passes before fog stops keeping a classic copy of its newest backup.
	GraduationPasses = 4
	// settingPassesPrefix + app counts those passes.
	settingPassesPrefix = "backup_store_passes:"

	// SettingBackupHistory is how far back backups go: "" (a year) |
	// "month" | "quarter" | "year" | "two-years".
	SettingBackupHistory = "backup_history"
	// SettingStoreEncrypt asks for the store to be encrypted: "" | "on" |
	// "off". A change takes effect at the next nightly upkeep, which
	// rewrites the store.
	SettingStoreEncrypt = "backup_store_encrypt"
	// settingStoreKey holds an encrypted store's key, vault-sealed (and so
	// carried by the recovery kit with the rest of fog's database).
	settingStoreKey     = "backup_store_key"
	settingStoreKeyNext = "backup_store_key_next"
	// settingKitDue says the store's key changed since the last recovery
	// kit, so a kit is written tonight rather than when the daily one is
	// due: a key only in fog's database is lost with fog's disk.
	settingKitDue = "backup_store_kit_due"

	// spaceHigh is the share of the backup disk past which fog thins the
	// oldest history; spaceTarget is where it stops.
	spaceHigh   = 0.90
	spaceTarget = 0.85
	// keepNewest backups of each app are never thinned for space.
	keepNewest = 3
)

// ValidHistory is the check for SettingBackupHistory.
func ValidHistory(v string) bool {
	switch v {
	case "", "month", "quarter", "year", "two-years":
		return true
	}
	return false
}

// HistoryPolicy counts how many distinct days, weeks and months keep a
// backup: the newest backup of each of the newest Daily days, Weekly weeks
// and Monthly months that have one.
type HistoryPolicy struct{ Daily, Weekly, Monthly int }

// PolicyFor reads a SettingBackupHistory value.
func PolicyFor(v string) HistoryPolicy {
	switch v {
	case "month":
		return HistoryPolicy{14, 4, 1}
	case "quarter":
		return HistoryPolicy{14, 8, 3}
	case "two-years":
		return HistoryPolicy{14, 8, 24}
	}
	return HistoryPolicy{14, 8, 12}
}

// keepByHistory picks the backups (newest first) a policy keeps; the newest
// keepNewest are always among them.
func keepByHistory(list []store.Backup, p HistoryPolicy) map[int64]bool {
	keep := map[int64]bool{}
	days, weeks, months := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, b := range list {
		if i < keepNewest {
			keep[b.ID] = true
		}
		t, err := time.Parse(time.RFC3339, b.CreatedAt)
		if err != nil {
			keep[b.ID] = true // an unreadable date is not a reason to delete
			continue
		}
		t = t.UTC()
		y, w := t.ISOWeek()
		for _, tier := range []struct {
			seen map[string]bool
			key  string
			n    int
		}{
			{days, t.Format("2006-01-02"), p.Daily},
			{weeks, fmt.Sprintf("%d-W%02d", y, w), p.Weekly},
			{months, t.Format("2006-01"), p.Monthly},
		} {
			if !tier.seen[tier.key] && len(tier.seen) < tier.n {
				tier.seen[tier.key] = true
				keep[b.ID] = true
			}
		}
	}
	return keep
}

// hasClassic reports whether a backup's snapshot directory is still there.
func hasClassic(b *store.Backup) bool {
	if b.Path == "" {
		return false
	}
	for _, name := range []string{"artifact.tar", "meta.json"} {
		if _, err := os.Stat(filepath.Join(b.Path, name)); err == nil {
			return true
		}
	}
	return false
}

// HasFiles reports whether a backup can be read at all: its directory, or
// its store snapshot.
func HasFiles(b *store.Backup) bool { return hasClassic(b) || b.StoreSnapshot != "" }

// BackupFiles is a directory holding the backup's snapshot files — the
// snapshot directory itself while it exists, otherwise the store snapshot
// rebuilt into a scratch directory beside the store, every file verified as
// it is written. release removes the scratch copy.
func (m *Manager) BackupFiles(ctx context.Context, b *store.Backup) (dir string, release func(), err error) {
	if hasClassic(b) {
		return b.Path, func() {}, nil
	}
	return m.materialize(ctx, b)
}

func (m *Manager) materialize(ctx context.Context, b *store.Backup) (string, func(), error) {
	if b.StoreSnapshot == "" {
		return "", nil, fmt.Errorf("backup %d's files are missing from %s", b.ID, b.Path)
	}
	cs, err := m.ChunkStore()
	if err != nil {
		return "", nil, err
	}
	snap, err := cs.Snapshot(b.StoreSnapshot)
	if err != nil {
		return "", nil, fmt.Errorf("backup %d: %w", b.ID, err)
	}
	dir, err := cs.TempDir("backup-*")
	if err != nil {
		return "", nil, err
	}
	release := func() { os.RemoveAll(dir) }
	if err := cs.Materialize(ctx, snap, dir, nil); err != nil {
		release()
		return "", nil, fmt.Errorf("reading backup %d back from the store: %w", b.ID, err)
	}
	return dir, release, nil
}

// BackupFile is one file of a backup.
type BackupFile struct {
	Name string
	Size int64
	Mode fs.FileMode
}

// ListBackupFiles names a backup's files.
func (m *Manager) ListBackupFiles(b *store.Backup) ([]BackupFile, error) {
	if hasClassic(b) {
		ents, err := os.ReadDir(b.Path)
		if err != nil {
			return nil, err
		}
		var out []BackupFile
		for _, e := range ents {
			if info, err := e.Info(); err == nil && e.Type().IsRegular() {
				out = append(out, BackupFile{Name: e.Name(), Size: info.Size(), Mode: info.Mode().Perm()})
			}
		}
		return out, nil
	}
	cs, snap, err := m.storeSnapshotOf(b)
	if err != nil {
		return nil, err
	}
	var out []BackupFile
	err = cs.Walk(snap, func(e chunkstore.Entry) error {
		if e.Type == chunkstore.TypeFile {
			out = append(out, BackupFile{Name: e.Path, Size: e.Size, Mode: fs.FileMode(e.Mode).Perm()})
		}
		return nil
	})
	return out, err
}

// OpenBackupFile streams one file of a backup, from its directory or from
// the store (verified as it is read; a mismatch is the stream's error).
func (m *Manager) OpenBackupFile(b *store.Backup, name string) (io.ReadCloser, int64, error) {
	if hasClassic(b) {
		f, err := os.Open(filepath.Join(b.Path, filepath.Base(name)))
		if err != nil {
			return nil, 0, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		return f, info.Size(), nil
	}
	cs, snap, err := m.storeSnapshotOf(b)
	if err != nil {
		return nil, 0, err
	}
	e, err := cs.Find(snap, name)
	if err != nil {
		return nil, 0, err
	}
	r, err := cs.Open(e)
	return r, e.Size, err
}

func (m *Manager) storeSnapshotOf(b *store.Backup) (*chunkstore.Store, *chunkstore.Snapshot, error) {
	if b.StoreSnapshot == "" {
		return nil, nil, fmt.Errorf("backup %d's files are missing from %s", b.ID, b.Path)
	}
	cs, err := m.ChunkStore()
	if err != nil {
		return nil, nil, err
	}
	snap, err := cs.Snapshot(b.StoreSnapshot)
	if err != nil {
		return nil, nil, fmt.Errorf("backup %d: %w", b.ID, err)
	}
	return cs, snap, nil
}

// ---- retiring the classic copies ----

func (m *Manager) passes(app string) int {
	v, _ := m.Store.GetSetting(settingPassesPrefix + app)
	n, _ := strconv.Atoi(v)
	return n
}

// notePass records a test restore that read its artifact from the store.
func (m *Manager) notePass(app string) {
	m.Store.SetSetting(settingPassesPrefix+app, strconv.Itoa(m.passes(app)+1))
}

// graduated says the store has proven itself for an app: GraduationPasses
// passes, counted as they happen (retention may since have removed the
// backups they were made on). A delegated app passes when a test restore
// reads its artifact from the store; a volume app, which has no test
// restore, when a backup reads back from the store whole.
func (m *Manager) graduated(app *store.App) bool {
	return m.passes(app.Name) >= GraduationPasses
}

// retireClassic removes the snapshot directories of an app's backups that
// the store holds, read back and matched — keeping the newest one while the
// app has not graduated.
func (m *Manager) retireClassic(appName string) {
	app, err := m.Store.AppByName(appName)
	if err != nil {
		return
	}
	list, err := m.Store.ListBackups(app.ID, 1000)
	if err != nil {
		return
	}
	anchor := !m.graduated(app)
	for i := range list {
		b := &list[i]
		if b.StoreVerifiedAt == "" || b.StoreError != "" || !hasClassic(b) {
			continue
		}
		if anchor {
			anchor = false // the newest verified one stays
			continue
		}
		if _, unavailable := m.shareUnder(b.Path); unavailable {
			continue
		}
		if err := os.RemoveAll(b.Path); err != nil {
			m.Log.Warn("backup store: removing a retired snapshot directory", "path", b.Path, "err", err)
		}
	}
}

// importClassic writes into the store the backups taken before it existed
// (or that it could not take at the time), then retires their directories —
// so history survives the move to the store and the disk gets its space
// back. Bounded by budget; what is left carries on the next night.
func (m *Manager) importClassic(ctx context.Context, budget time.Duration) {
	refs, err := m.Store.ListAllBackupRefs()
	if err != nil {
		return
	}
	deadline := time.Now().Add(budget)
	touched := map[int64]string{}
	for _, ref := range refs {
		if ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		b, err := m.Store.BackupByID(ref.ID)
		if err != nil || (b.StoreSnapshot != "" && b.StoreError == "") || !hasClassic(b) {
			continue
		}
		app, err := m.Store.AppByID(b.AppID)
		if err != nil {
			continue
		}
		m.storeBackup(ctx, app.Name, b.ID)
		touched[app.ID] = app.Name
	}
	for _, name := range touched {
		m.retireClassic(name)
	}
}

// ---- space ----

func diskUse(dir string) (used, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	total = st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return 0, 0, false
	}
	return total - free, total, true
}

// diskUseSeam lets tests fill the disk.
var diskUseSeam = diskUse

// guardSpace thins the oldest history when the backup disk is nearly full,
// so tonight's backups still land: the oldest backups first, across every
// app, never an app's newest keepNewest nor its newest verified one, and
// collected straight away. It says what it did in plain words.
func (m *Manager) guardSpace(ctx context.Context, cs *chunkstore.Store) {
	root := m.baseDir()
	used, total, ok := diskUseSeam(root)
	if !ok || float64(used) < spaceHigh*float64(total) {
		return
	}
	// Thinning history only helps when history is what fills the disk: a
	// backup folder sharing a disk with Drive or media must not give up its
	// backups for space it never held.
	if st, err := cs.Stats(); err == nil && float64(st.Bytes) < 0.1*float64(used) {
		m.Notify("Backup disk is nearly full",
			"The disk the backups are on is nearly full, but the backups take little of it, so fog is keeping them all. New backups may fail until there is more room — Settings → Storage → Drive.", false)
		return
	}
	candidates := m.thinnable()
	thinned := 0
	drive := m.ThinDrive != nil
	for round := 0; round < 20 && (len(candidates) > 0 || drive); round++ {
		if ctx.Err() != nil {
			break
		}
		// Drive's history gives up its oldest night alongside the apps'.
		if drive {
			if drive = m.ThinDrive(); drive {
				thinned++
			}
		}
		n := min(5, len(candidates))
		for _, b := range candidates[:n] {
			if _, unavailable := m.shareUnder(b.Path); unavailable {
				continue
			}
			os.RemoveAll(b.Path)
			m.dropFromStore(&b)
			m.Store.DeleteBackup(b.ID)
			thinned++
		}
		candidates = candidates[n:]
		// Two collections: the first finds the packs, the second removes
		// them. Writers are kept out while each runs. A collection that
		// cannot run frees nothing, so nothing more is thinned for it.
		collected := true
		for range 2 {
			if _, err := cs.GC(ctx, chunkstore.GCOptions{Urgent: true}); err != nil {
				m.Log.Warn("backup store: collecting to free space", "err", err)
				collected = false
				break
			}
		}
		before := used
		if used, total, ok = diskUseSeam(root); !ok || !collected || float64(used) < spaceTarget*float64(total) {
			break
		}
		// A round that freed (almost) nothing will not be helped by more.
		if before-used < total/1000 {
			break
		}
	}
	if thinned > 0 {
		m.Notify("Backup disk was nearly full",
			fmt.Sprintf("Fog removed the %d oldest backups to make room for new ones. Your recent history is untouched; to keep more, give backups a bigger disk in Settings → Storage → Drive.", thinned), false)
		m.Store.Audit("system", "backup.thin", "", fmt.Sprintf(`{"removed":%d}`, thinned))
	} else {
		m.Notify("Backup disk is nearly full",
			"There is no older history left to remove. New backups may fail until the backup disk has more room — Settings → Storage → Drive.", false)
	}
}

// thinnable lists the backups space may claim, oldest first.
func (m *Manager) thinnable() []store.Backup {
	apps, err := m.Store.ListApps()
	if err != nil {
		return nil
	}
	var out []store.Backup
	for _, a := range apps {
		list, err := m.Store.ListBackups(a.ID, 1000)
		if err != nil {
			continue
		}
		verifiedSeen := false
		for i, b := range list {
			protected := i < keepNewest
			if !verifiedSeen && b.VerifiedAt != "" {
				verifiedSeen, protected = true, true
			}
			if !protected {
				out = append(out, b)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// ---- encryption ----

func (m *Manager) readKey(setting string) []byte {
	raw, _ := m.Store.GetSetting(setting)
	ct, ok := strings.CutPrefix(raw, sealedPassPrefix)
	if !ok || m.Vault == nil {
		return nil
	}
	h, err := m.Vault.Decrypt(ct)
	if err != nil {
		return nil
	}
	k, err := hex.DecodeString(h)
	if err != nil || len(k) != 32 {
		return nil
	}
	return k
}

func (m *Manager) writeKey(setting string, key []byte) error {
	if key == nil {
		return m.Store.SetSetting(setting, "")
	}
	if m.Vault == nil {
		return errors.New("no vault to seal the backup store key with")
	}
	ct, err := m.Vault.Encrypt(hex.EncodeToString(key))
	if err != nil {
		return err
	}
	return m.Store.SetSetting(setting, sealedPassPrefix+ct)
}

// storeKey is the key an encrypted store opens with; nil for a plain one.
// A new store is created encrypted when the setting asks for it.
func (m *Manager) storeKey(dir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "store.json"))
	if errors.Is(err, fs.ErrNotExist) {
		if v, _ := m.Store.GetSetting(SettingStoreEncrypt); v == "on" {
			if k := m.readKey(settingStoreKey); k != nil {
				return k, nil
			}
			k := make([]byte, 32)
			rand.Read(k)
			if err := m.writeKey(settingStoreKey, k); err != nil {
				return nil, err
			}
			m.Store.SetSetting(settingKitDue, "1")
			return k, nil
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c struct {
		Encrypted bool `json:"encrypted"`
	}
	json.Unmarshal(raw, &c)
	if !c.Encrypted {
		return nil, nil
	}
	if k := m.readKey(settingStoreKey); k != nil {
		return k, nil
	}
	// A conversion that swapped the store in but crashed before promoting
	// its key.
	if k := m.readKey(settingStoreKeyNext); k != nil {
		return k, nil
	}
	return nil, chunkstore.ErrLocked
}

// StoreKeyBytes is an encrypted store's key, nil for a plain store or when
// it cannot be read — for copying, sealed, to the offsite locker.
func (m *Manager) StoreKeyBytes() []byte { return m.readKey(settingStoreKey) }

func (m *Manager) kitIsDue() bool {
	v, _ := m.Store.GetSetting(settingKitDue)
	return v == "1"
}

// wantEncrypted is the setting, as a decision.
func (m *Manager) wantEncrypted() bool {
	v, _ := m.Store.GetSetting(SettingStoreEncrypt)
	return v == "on"
}

const (
	nextStoreDir  = StoreDirName + ".next"
	oldStoreDir   = StoreDirName + ".old"
	convertedMark = "CONVERTED"
	// movedFile, in a rewritten store, maps every old snapshot id to its
	// new one until each row has been pointed at the new id.
	movedFile = "MOVED.json"
)

// recoverConversion finishes or undoes a conversion a crash interrupted:
// a complete new store (marked) is put in place, an incomplete one dropped,
// and rows still naming the old store's snapshots are pointed at the new
// ones. Called with the store handle's lock held.
func (m *Manager) recoverConversion() {
	root := m.baseDir()
	cur, next, old := filepath.Join(root, StoreDirName), filepath.Join(root, nextStoreDir), filepath.Join(root, oldStoreDir)
	_, curErr := os.Stat(cur)
	_, markErr := os.Stat(filepath.Join(next, convertedMark))
	switch {
	case errors.Is(curErr, fs.ErrNotExist) && markErr == nil:
		os.Rename(next, cur)
		os.Rename(m.indexPath()+".next", m.indexPath())
	case errors.Is(curErr, fs.ErrNotExist):
		if _, err := os.Stat(old); err == nil {
			os.Rename(old, cur)
		}
	}
	if _, err := os.Stat(cur); err != nil {
		return
	}
	if markErr != nil {
		os.RemoveAll(next)
	}
	// The old store goes only once every row points into the new one.
	if m.repointMoved(cur) {
		os.Remove(filepath.Join(cur, convertedMark))
		os.RemoveAll(old)
	}
}

// repointMoved applies a rewritten store's id map to the rows, promotes
// its key, and removes the map once every row has been moved. It reports
// whether nothing is left to do.
func (m *Manager) repointMoved(cur string) bool {
	raw, err := os.ReadFile(filepath.Join(cur, movedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var moved map[string]string
	if err != nil || json.Unmarshal(raw, &moved) != nil {
		m.Log.Error("backup store: the id map of a rewrite is unreadable; keeping the old store", "err", err)
		return false
	}
	for from, to := range moved {
		if err := m.Store.RepointBackupStore(from, to); err != nil {
			m.Log.Error("backup store: pointing backups at the rewritten store", "err", err)
			return false
		}
	}
	if m.RepointSnapshot != nil {
		m.RepointSnapshot(moved)
	}
	if k := m.readKey(settingStoreKeyNext); k != nil {
		if err := m.writeKey(settingStoreKey, k); err != nil {
			return false
		}
		m.writeKey(settingStoreKeyNext, nil)
	} else if !m.wantEncrypted() {
		m.writeKey(settingStoreKey, nil)
	}
	m.Store.SetSetting(settingKitDue, "1")
	if err := os.Remove(filepath.Join(cur, movedFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return true
}

func (m *Manager) indexPath() string { return filepath.Join(m.DataDir, "backup-store", "index.db") }

// convertStore rewrites the whole store encrypted or plain, as the setting
// now asks. Every snapshot is copied — read back verified from the old
// store, written into the new, then read back from the new in full — before
// the new store replaces the old. The id map goes into the new store before
// the swap, so a crash after it still points every row at its copy; the old
// store is removed only once they all have been. Runs with the backup
// directory held exclusively.
func (m *Manager) convertStore(ctx context.Context) error {
	old, err := m.ChunkStore()
	if err != nil {
		return err
	}
	encrypt := m.wantEncrypted()
	if old.Encrypted() == encrypt {
		return nil
	}
	unlock, err := m.LockDir(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	root := m.baseDir()
	next := filepath.Join(root, nextStoreDir)
	os.RemoveAll(next)
	os.Remove(m.indexPath() + ".next")
	var key []byte
	if encrypt {
		key = make([]byte, 32)
		rand.Read(key)
		if err := m.writeKey(settingStoreKeyNext, key); err != nil {
			return err
		}
	}
	ns, err := chunkstore.Open(next, chunkstore.Options{IndexPath: m.indexPath() + ".next", Create: true, Key: key})
	if err != nil {
		return err
	}
	abandon := func(err error) error {
		ns.Close()
		os.RemoveAll(next)
		os.Remove(m.indexPath() + ".next")
		return err
	}
	// A document that will not read now might be a backup someone needs;
	// rewriting without it would lose it.
	var unreadable []string
	snaps, err := old.Snapshots(func(id string, _ error) { unreadable = append(unreadable, id) })
	if err != nil {
		return abandon(err)
	}
	if len(unreadable) > 0 {
		return abandon(fmt.Errorf("%d backups in the store could not be read; nothing was rewritten", len(unreadable)))
	}
	moved := map[string]string{}
	for _, s := range snaps {
		id, err := copySnapshot(ctx, old, ns, s)
		if err != nil {
			return abandon(fmt.Errorf("copying snapshot %s: %w", s.ID, err))
		}
		moved[s.ID] = id
	}
	mb, err := json.Marshal(moved)
	if err != nil {
		return abandon(err)
	}
	if err := fsx.WriteFileAtomic(filepath.Join(next, movedFile), mb, 0o600); err != nil {
		return abandon(err)
	}
	ns.Close()
	if err := fsx.WriteFileAtomic(filepath.Join(next, convertedMark), nil, 0o600); err != nil {
		os.RemoveAll(next)
		return err
	}
	syncDir(next)

	// The swap, with the store handle held so nothing opens either store
	// half-way through it.
	m.chunks.mu.Lock()
	if m.chunks.s != nil {
		m.chunks.s.Close()
		m.chunks.s = nil
	}
	cur := filepath.Join(root, StoreDirName)
	if err := os.Rename(cur, filepath.Join(root, oldStoreDir)); err != nil {
		m.chunks.mu.Unlock()
		os.RemoveAll(next)
		return err
	}
	if err := os.Rename(next, cur); err != nil {
		os.Rename(filepath.Join(root, oldStoreDir), cur)
		m.chunks.mu.Unlock()
		return err
	}
	syncDir(root)
	os.Rename(m.indexPath()+".next", m.indexPath())
	m.recoverConversion()
	m.chunks.mu.Unlock()
	m.Store.Audit("system", "backup.store.convert", "", fmt.Sprintf(`{"encrypted":%t,"snapshots":%d}`, encrypt, len(moved)))
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// copySnapshot writes snapshot s of src into dst, entry for entry, and
// reads the copy back from dst in full before trusting it.
func copySnapshot(ctx context.Context, src, dst *chunkstore.Store, s *chunkstore.Snapshot) (string, error) {
	w, err := dst.Begin(s.Series)
	if err != nil {
		return "", err
	}
	defer w.Abort()
	err = src.Walk(s, func(e chunkstore.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type != chunkstore.TypeFile {
			_, err := w.Add(e, nil)
			return err
		}
		r, err := src.Open(e)
		if err != nil {
			return err
		}
		got, err := w.Add(chunkstore.Entry{Path: e.Path, Type: e.Type, Mode: e.Mode, MTime: e.MTime}, r)
		if err != nil {
			return err
		}
		if got.SHA256 != e.SHA256 || got.Size != e.Size {
			return fmt.Errorf("%s copied as %s, not %s", e.Path, got.SHA256, e.SHA256)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	created, _ := time.Parse(time.RFC3339, s.CreatedAt)
	ns, err := w.Commit(s.Stamp, created, s.Meta)
	if err != nil {
		return "", err
	}
	if err := chunkstore.ReadBack(ctx, dst, ns); err != nil {
		return "", fmt.Errorf("the copy does not read back: %w", err)
	}
	return ns.ID, nil
}

// ---- what the settings page shows ----

// StoreSummary is the store at a glance, for people: how far back backups
// go and what they take up, nothing about how.
type StoreSummary struct {
	// UsedBytes is what the store occupies on the backup disk.
	UsedBytes int64 `json:"usedBytes"`
	// OldestAt is the oldest backup fog holds (RFC3339), "" with none.
	OldestAt string `json:"oldestAt,omitempty"`
	// Encrypted says whether the backups on the disk are encrypted;
	// EncryptPending that a change of the setting has yet to be applied.
	Encrypted      bool `json:"encrypted"`
	EncryptPending bool `json:"encryptPending"`
	// History is the SettingBackupHistory in force ("year" when unset).
	History string `json:"history" ts:"\"month\" | \"quarter\" | \"year\" | \"two-years\""`
}

// Summary reads the store at a glance. An unopenable store (a share that
// is away) answers with what the database knows.
func (m *Manager) Summary() StoreSummary {
	var s StoreSummary
	s.History, _ = m.Store.GetSetting(SettingBackupHistory)
	if s.History == "" {
		s.History = "year"
	}
	s.OldestAt, _ = m.Store.OldestBackupAt()
	if m.Moving() {
		return s
	}
	cs, err := m.ChunkStore()
	if err != nil {
		return s
	}
	if st, err := cs.Stats(); err == nil {
		s.UsedBytes = st.Bytes
	}
	s.Encrypted = cs.Encrypted()
	s.EncryptPending = s.Encrypted != m.wantEncrypted()
	return s
}

// KeepByHistory says which of a series of snapshots, given by creation
// time (RFC3339) newest first, the history policy in setting keeps — the
// rule app backups follow, for other series (Drive).
func KeepByHistory(createdAt []string, setting string) []bool {
	list := make([]store.Backup, len(createdAt))
	for i, at := range createdAt {
		list[i] = store.Backup{ID: int64(i + 1), CreatedAt: at}
	}
	keep := keepByHistory(list, PolicyFor(setting))
	out := make([]bool, len(createdAt))
	for i := range out {
		out[i] = keep[int64(i+1)]
	}
	return out
}
