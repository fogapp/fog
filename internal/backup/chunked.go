package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/store"
)

// The incremental store (docs/store-contract.md) lives in the backups root
// beside the per-app folders. Every backup fog takes is also written into
// it: a complete snapshot whose unchanged bytes cost nothing because they
// are chunks the store already holds. After it is written the snapshot is
// read back in full and checked against the backup it copies — for a
// delegated app, against the very digest fog verified at download — and
// the row records that it matched.

// StoreDirName holds the incremental store.
const StoreDirName = "fog-store"

// SettingStoreMaintainedAt is when the store's nightly upkeep (collection
// and a slice of the scrub) last ran.
const SettingStoreMaintainedAt = "backup_store_maintained_at"

// StoreMeta is what a store snapshot of an app backup records about it: all
// a fog needs to list and restore the backup from the store alone, with no
// database row — after a recovery kit from before it, say.
type StoreMeta struct {
	App       string `json:"app"`
	Kind      string `json:"kind"`
	Mode      string `json:"mode"` // "delegated" | "volumes"
	CreatedAt string `json:"created_at"`
	Format    string `json:"format,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Manifest  string `json:"manifest,omitempty"`
	// SourceBackupID is the row this was written from, for reading logs;
	// rows are not portable, so nothing depends on it.
	SourceBackupID int64 `json:"source_backup_id,omitempty"`
}

// Series is the store series an app's snapshots belong to.
func Series(app string) string { return "app/" + app }

type chunkHandle struct {
	mu  sync.Mutex
	s   *chunkstore.Store
	dir string
}

// ChunkStore opens the store under the current backups root, creating it
// on first use. The root must be usable: on a NAS share that is away it
// fails rather than make a store on the bare mountpoint.
func (m *Manager) ChunkStore() (*chunkstore.Store, error) {
	if err := m.checkTarget(); err != nil {
		return nil, err
	}
	dir := filepath.Join(m.baseDir(), StoreDirName)
	m.chunks.mu.Lock()
	defer m.chunks.mu.Unlock()
	if m.chunks.s != nil && m.chunks.dir == dir {
		return m.chunks.s, nil
	}
	if m.chunks.s != nil {
		// The backups root moved (its holders are drained by the move's
		// exclusive hold); the next user opens the store where it is now.
		m.chunks.s.Close()
		m.chunks.s = nil
	}
	m.recoverConversion()
	key, err := m.storeKey(dir)
	if err != nil {
		return nil, fmt.Errorf("opening the backup store: %w", err)
	}
	s, err := chunkstore.Open(dir, chunkstore.Options{IndexPath: m.indexPath(), Create: true, Key: key})
	if err != nil {
		return nil, fmt.Errorf("opening the backup store: %w", err)
	}
	m.chunks.s, m.chunks.dir = s, dir
	return s, nil
}

// CloseChunkStore releases the store (shutdown, tests).
func (m *Manager) CloseChunkStore() {
	m.chunks.mu.Lock()
	defer m.chunks.mu.Unlock()
	if m.chunks.s != nil {
		m.chunks.s.Close()
		m.chunks.s = nil
	}
}

func backupMode(b *store.Backup) string {
	if b.Format != "" {
		return "delegated"
	}
	return "volumes"
}

// storeBackup writes a backup's snapshot directory into the store, reads
// it back, and records the outcome on the row. It never fails the backup:
// the directory is still the backup of record; a store that cannot take it
// is reported on the row and in the log.
func (m *Manager) storeBackup(ctx context.Context, appName string, id int64) {
	b, err := m.Store.BackupByID(id)
	if err != nil {
		return
	}
	snapID, err := m.writeToStore(ctx, appName, b)
	if err != nil {
		m.Log.Warn("backup store copy failed", "app", appName, "backup", id, "err", err)
		m.Store.SetBackupStore(id, snapID, "", err.Error())
		return
	}
	m.Store.SetBackupStore(id, snapID, time.Now().UTC().Format(time.RFC3339), "")
	if b.Format == "" {
		m.notePass(appName) // a volume app's proof is the read-back itself
	}
	// The store holds it whole: the directory can go (retireClassic keeps
	// an anchor while the app has not yet proven its store copies restore).
	m.retireClassic(appName)
}

func (m *Manager) writeToStore(ctx context.Context, appName string, b *store.Backup) (string, error) {
	cs, err := m.ChunkStore()
	if err != nil {
		return "", err
	}
	ents, err := os.ReadDir(b.Path)
	if err != nil {
		return "", err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	w, err := cs.Begin(Series(appName))
	if err != nil {
		return "", err
	}
	defer w.Abort()
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// A snapshot directory is flat: artifact.tar and manifest.json, or
		// meta.json, volume tars and dumps. Anything else (a link, a
		// subdirectory) is not part of what restore reads.
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", err
		}
		f, err := os.Open(filepath.Join(b.Path, e.Name()))
		if err != nil {
			return "", err
		}
		name, r, err := storedForm(e.Name(), f)
		if err == nil {
			_, err = w.Add(chunkstore.Entry{Path: name, Type: chunkstore.TypeFile,
				Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()}, r)
		}
		f.Close()
		if err != nil {
			return "", fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	created, _ := time.Parse(time.RFC3339, b.CreatedAt)
	if created.IsZero() {
		created = time.Now()
	}
	meta, _ := json.Marshal(StoreMeta{App: appName, Kind: b.Kind, Mode: backupMode(b), CreatedAt: b.CreatedAt,
		Format: b.Format, SHA256: b.SHA256, Manifest: b.Manifest, SourceBackupID: b.ID})
	snap, err := w.Commit(filepath.Base(b.Path), created, meta)
	if err != nil {
		return "", err
	}
	return snap.ID, m.verifyInStore(ctx, cs, snap, b)
}

// storedForm is how a snapshot directory's file goes into the store: as it
// is, except a volume archive from before the store (volume-<name>.tar.gz),
// which goes in unpacked as volume-<name>.tar — what the store's own volume
// backups hold, and what restore reads first (volumeArchive). Compressed,
// one night's archive shares next to nothing with the next, and every
// imported night would cost the store its full size. gzip's own checksum,
// read at the end of the stream, proves the archive whole on the way in.
func storedForm(name string, f io.Reader) (string, io.Reader, error) {
	if !strings.HasPrefix(name, "volume-") || !strings.HasSuffix(name, ".tar.gz") {
		return name, f, nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSuffix(name, ".gz"), zr, nil
}

// verifyInStore reads a store snapshot back in full — every chunk checked
// against its id, every file against the digest taken as it went in — and
// for a delegated backup also against the artifact digest fog verified at
// download, so the store copy is proven the same artifact end to end.
func (m *Manager) verifyInStore(ctx context.Context, cs *chunkstore.Store, snap *chunkstore.Snapshot, b *store.Backup) error {
	sawArtifact := false
	err := cs.Walk(snap, func(e chunkstore.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type != chunkstore.TypeFile {
			return nil
		}
		r, err := cs.Open(e)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, r)
		r.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if e.Path == "artifact.tar" && b.SHA256 != "" {
			sawArtifact = true
			if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, b.SHA256) {
				return fmt.Errorf("artifact read back from the store has sha256 %s, not the %s verified at download", got, b.SHA256)
			}
		}
		return nil
	})
	if err == nil && b.SHA256 != "" && !sawArtifact {
		err = fmt.Errorf("the store snapshot has no artifact.tar")
	}
	return err
}

// dropFromStore removes a backup's store snapshot ahead of its row. Its
// chunks go when collection finds nothing else uses them.
func (m *Manager) dropFromStore(b *store.Backup) {
	if b.StoreSnapshot == "" {
		return
	}
	cs, err := m.ChunkStore()
	if err != nil {
		return
	}
	if err := cs.Delete(b.StoreSnapshot); err != nil {
		m.Log.Warn("backup store: removing snapshot", "snapshot", b.StoreSnapshot, "err", err)
	}
}

// maintenanceDue says whether the nightly upkeep has yet to run since the
// most recent backup hour.
func maintenanceDue(st *store.Store, now time.Time, hour int) bool {
	anchor := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	if anchor.After(now) {
		anchor = anchor.Add(-24 * time.Hour)
	}
	last, _ := st.GetSetting(SettingStoreMaintainedAt)
	t, err := time.Parse(time.RFC3339, last)
	return last == "" || err != nil || t.Before(anchor)
}

// guardSpaceBetween looks at the backup disk once an hour between nightly
// upkeeps. Nearly full, it makes room (guardSpace) — a disk that fills in
// the afternoon is not left that way until tomorrow night. Below that line
// but with a backup failing for want of room (SpaceStarved), it collects
// what is already let go and thins nothing: backups deleted by hand give
// their space back within the hour, wherever the line happens to sit.
func (m *Manager) guardSpaceBetween(ctx context.Context, now time.Time) {
	if now.Sub(m.spaceGuardedAt) < time.Hour {
		return
	}
	m.spaceGuardedAt = now
	used, total, ok := diskUseSeam(m.baseDir())
	high := ok && float64(used) >= spaceHigh*float64(total)
	if !high && (m.SpaceStarved == nil || !m.SpaceStarved()) {
		return
	}
	release, err := m.HoldDir()
	if err != nil {
		return
	}
	defer release()
	cs, err := m.ChunkStore()
	if err != nil {
		return
	}
	if high {
		m.guardSpace(ctx, cs)
		return
	}
	m.collectNow(ctx, cs)
}

// dropSeriesFromStore removes every store snapshot of an app (the app is
// being removed along with its backups).
func (m *Manager) dropSeriesFromStore(appName string) error {
	cs, err := m.ChunkStore()
	if err != nil {
		return err
	}
	snaps, err := cs.Snapshots(nil)
	if err != nil {
		return err
	}
	for _, s := range snaps {
		if s.Series == Series(appName) {
			if err := cs.Delete(s.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// maintainStore is the store's nightly upkeep, after the night's backups:
// finish an interrupted conversion and apply a change of the encryption
// setting; bring backups taken before the store into it; make room if the
// disk is nearly full; collect what retention let go; and scrub a thirtieth
// of the packs so the whole store is re-read every month. It runs once per
// day, on the first look after the nightly backup hour (hour, UTC) — so an
// app with its own hour does not pull it elsewhere in the day, and a fogd
// restarted mid-night catches up. Between upkeeps, a nearly full disk is
// still seen to within the hour (guardSpaceBetween).
func (m *Manager) maintainStore(ctx context.Context, now time.Time, hour int) {
	if !maintenanceDue(m.Store, now, hour) {
		m.guardSpaceBetween(ctx, now)
		return
	}
	if err := m.convertStore(ctx); err != nil {
		m.Log.Error("backup store: applying the encryption setting", "err", err)
		m.Notify("Backup encryption change failed", "Fog could not rewrite the backups for the new encryption setting and left them as they were; it will try again tomorrow night. "+err.Error(), false)
	}
	// A new store key goes into a recovery kit tonight, not when the daily
	// kit is next due: until then it exists only in fog's own database.
	if m.kitIsDue() && m.kitPassphrase() != "" {
		if err := m.WriteRecoveryKit(ctx); err != nil {
			m.Log.Error("recovery kit after a backup key change", "err", err)
		}
	}
	release, err := m.HoldDir()
	if err != nil {
		return
	}
	defer release()
	cs, err := m.ChunkStore()
	if err != nil {
		return
	}
	m.importClassic(ctx, 3*time.Hour)
	m.guardSpace(ctx, cs)
	rep, err := cs.GC(ctx, chunkstore.GCOptions{})
	if err != nil {
		m.Log.Warn("backup store collection", "err", err)
	} else if rep.Deleted > 0 || rep.Repacked > 0 {
		m.Log.Info("backup store collection", "deleted", rep.Deleted, "freed", rep.FreedBytes, "repacked", rep.Repacked)
	}
	st, err := cs.Stats()
	if err == nil {
		limit := st.Packs/30 + 1
		crep, err := cs.CheckPacks(ctx, limit)
		switch {
		case err != nil:
			m.Log.Warn("backup store check", "err", err)
		case !crep.OK():
			m.Log.Error("backup store check found damage", "packs", crep.BadPacks, "chunks", len(crep.BadChunks))
			healed := false
			if m.HealChunks != nil {
				failed, err := m.HealChunks(ctx, cs, crep.BadChunks)
				healed = err == nil && len(failed) == 0
			}
			if !healed {
				m.Notify("Backup damage found",
					fmt.Sprintf("Part of the backup disk no longer reads back as it was written (%d pieces) and there was no other copy to repair it from. Check the backup disk; the backups that use those pieces will fail their next check.", len(crep.BadChunks)), false)
			}
		}
	}
	m.Store.SetSetting(SettingStoreMaintainedAt, time.Now().UTC().Format(time.RFC3339))
}
