package offsite

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/store"
)

// A backup held in the incremental store (docs/store-contract.md) goes
// offsite as the store's own pieces rather than as a tar of its files: the
// packs that hold its chunks, sealed one object each, and a sealed row
// carrying its snapshot document. Packs are shared by every snapshot that
// uses their chunks, so a night uploads only the packs the locker does not
// already have — roughly what changed (docs/locker-contract.md §6).

// Kinds of copy for the store's pieces. A store's settings (and key, for an
// encrypted one) are one copy per store; each pack is one.
const (
	kindStore = "store"
	kindPack  = "store-pack"
)

func (l Layout) StorePrefix(storeID string) string { return l.Root() + "store/" + storeID + "/" }
func (l Layout) StoreConfig(storeID string) string { return l.StorePrefix(storeID) + "store.json" }
func (l Layout) StoreKey(storeID string) string    { return l.StorePrefix(storeID) + "key" }
func (l Layout) StorePack(storeID, name string) string {
	return l.StorePrefix(storeID) + "packs/" + name[:2] + "/" + name
}

func packSubject(storeID, name string) string { return storeID + "/" + name }

// packStored says the locker holds a pack now. Not ix.done: a pack the
// sweep hid keeps its row (hidden) for weeks, and a row written naming it
// would name an object the provider is about to delete — so a hidden pack
// a backup needs again is uploaded again.
func packStored(ix copyIndex, subject string) bool {
	c, ok := ix[subject]
	return ok && c.State == store.OffsiteStored
}

// storeBacked says whether a backup goes up as store pieces: it has a store
// snapshot that read back whole, and fog has a store to read it from.
func (m *Manager) storeBacked(b *store.Backup) bool {
	return m.ChunkStore != nil && b.StoreSnapshot != "" && b.StoreVerifiedAt != "" && b.StoreError == ""
}

// syncStoreConfig puts a store's settings — and an encrypted store's key —
// in the locker once, sealed: a reader needs them to open the packs.
func (m *Manager) syncStoreConfig(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, cs *chunkstore.Store) error {
	ix, err := m.copyIndex(t, kindStore)
	if err != nil {
		return err
	}
	if ix.done(cs.ID()) {
		return nil
	}
	conf, err := cs.ConfigBytes()
	if err != nil {
		return err
	}
	var key []byte
	if cs.Encrypted() {
		if m.StoreKey == nil {
			return errors.New("the backup store is encrypted and its key is not available to copy")
		}
		if key = m.StoreKey(); key == nil {
			return errors.New("the backup store is encrypted and its key could not be read")
		}
	}
	for _, o := range []struct {
		key  string
		body []byte
	}{{r.Layout.StoreConfig(cs.ID()), conf}, {r.Layout.StoreKey(cs.ID()), []byte(hex.EncodeToString(key))}} {
		sealed, err := r.Keys.SealBytes(r.Layout.Name(o.key), o.body)
		if err != nil {
			return err
		}
		if _, err := r.Client.PutBytes(ctx, o.key, sealed, withContentType(up.Lock, "application/octet-stream")); err != nil {
			return fmt.Errorf("writing the store's settings: %w", err)
		}
	}
	return m.stored(ix, store.OffsiteCopy{TargetID: t.ID, Kind: kindStore, Subject: cs.ID(), Key: r.Layout.StoreConfig(cs.ID()),
		Bytes: int64(len(conf)), UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)})
}

// syncOneStore copies one store-backed backup: its missing packs, then its
// row. The packs are checked against their names on the way out — what
// leaves the house is what the store wrote.
func (m *Manager) syncOneStore(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress,
	ix copyIndex, app *store.App, b *store.Backup, subject string) error {
	stamp := stampOfBackup(b)
	cs, err := m.ChunkStore()
	if err != nil {
		return err
	}
	snap, err := cs.Snapshot(b.StoreSnapshot)
	if err != nil {
		return err
	}
	doc, err := cs.DocBytes(snap.ID)
	if err != nil {
		return err
	}
	packs, err := cs.PacksOf(snap)
	if err != nil {
		return err
	}
	if err := m.syncStoreConfig(ctx, t, r, up, cs); err != nil {
		return err
	}
	pix, err := m.copyIndex(t, kindPack)
	if err != nil {
		return err
	}
	var todo []string
	for _, name := range packs {
		if !packStored(pix, packSubject(cs.ID(), name)) {
			todo = append(todo, name)
		}
	}
	p.Current(fmt.Sprintf("%s %s · %d of %d pieces to copy", app.Name, stamp, len(todo), len(packs)))
	if err := m.uploadPacks(ctx, t, r, up, p, pix, cs, todo); err != nil {
		return err
	}
	mode := apps.BackupModeVolumes
	if b.Format == backup.ArtifactFormat {
		mode = apps.BackupModeDelegated
	}
	files := []string{}
	cs.Walk(snap, func(e chunkstore.Entry) error {
		if e.Type == chunkstore.TypeFile {
			files = append(files, e.Path)
		}
		return nil
	})
	row := AppRow{Format: Format, App: app.Name, Stamp: stamp, Kind: b.Kind, Mode: mode, CreatedAt: b.CreatedAt,
		SizeBytes: snap.Bytes, ArtifactFormat: b.Format, ArtifactSHA256: b.SHA256,
		Manifest: b.Manifest, VerifiedAt: b.VerifiedAt, Files: files, SourceBackupID: b.ID,
		Store: cs.ID(), StoreSnapshot: snap.ID, StoreDoc: doc, Packs: packs}
	row.Identity = backup.ManifestApp(b.Manifest)
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	rowKey := r.Layout.AppRow(app.Name, stamp)
	sealed, err := r.Keys.SealBytes(r.Layout.Name(rowKey), body)
	if err != nil {
		return err
	}
	if _, err := r.Client.PutBytes(ctx, rowKey, sealed, withContentType(up.Lock, "application/octet-stream")); err != nil {
		return fmt.Errorf("writing the row: %w", err)
	}
	if err := m.stored(ix, store.OffsiteCopy{TargetID: t.ID, Kind: store.OffsiteKindApp, Subject: subject, Key: rowKey,
		Bytes: int64(len(sealed)), Doc: string(body),
		UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)}); err != nil {
		return err
	}
	p.Add(0, 1)
	m.Log.Info("offsite: backup copied", "target", t.Name, "app", app.Name, "stamp", stamp, "packs", len(todo))
	return nil
}

func (m *Manager) uploadPacks(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress,
	pix copyIndex, cs *chunkstore.Store, todo []string) error {
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(blobWorkers)
	for _, name := range todo {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			subject := packSubject(cs.ID(), name)
			key := r.Layout.StorePack(cs.ID(), name)
			mu.Lock()
			state := pix.resume(subject)
			save := m.saver(t, pix, kindPack, subject, key)
			mu.Unlock()
			res, err := up.Upload(gctx, key, r.Layout.Name(key), FileSource(cs.PackPath(name)), &state, func(st *UploadState) error {
				mu.Lock()
				defer mu.Unlock()
				return save(st)
			})
			if err != nil {
				return fmt.Errorf("pack %s: %w", name[:12], err)
			}
			if !strings.EqualFold(res.SHA256, name) {
				m.Store.CreateNotice(NoticeCorrupt, "backup store", "Part of the backup disk has changed since it was written",
					"A piece of the backup store no longer matches its checksum, so it was not copied offsite. Fog will try to repair it from the offsite copy; check the backup disk.")
				return fmt.Errorf("pack %s changed on disk since it was written — not copied", name[:12])
			}
			mu.Lock()
			defer mu.Unlock()
			return m.stored(pix, store.OffsiteCopy{TargetID: t.ID, Kind: kindPack, Subject: subject, Key: key, Bytes: res.Bytes,
				UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)})
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return ctx.Err()
}

// sweepPacks hides every stored pack no retained row names — the same rule,
// and the same spare for packs newer than the newest row, as the Drive blob
// sweep.
func (m *Manager) sweepPacks(ctx context.Context, t *store.OffsiteTarget, r *Reader, stored []store.OffsiteCopy) ([]store.OffsiteCopy, error) {
	hasPacks := false
	for _, c := range stored {
		if c.Kind == kindPack {
			hasPacks = true
			break
		}
	}
	if !hasPacks {
		return stored, nil
	}
	rows, err := m.Store.ListOffsiteCopies(t.ID, store.OffsiteKindApp, store.OffsiteStored)
	if err != nil {
		return stored, err
	}
	all, err := m.Store.ListOffsiteCopiesSlim(t.ID, store.OffsiteKindApp)
	if err != nil {
		return stored, err
	}
	newest := ""
	for _, c := range all {
		newest = max(newest, c.UploadedAt)
	}
	referenced := map[string]bool{}
	for _, c := range rows {
		var row AppRow
		if json.Unmarshal([]byte(c.Doc), &row) != nil {
			return stored, fmt.Errorf("backup copy %s has an unreadable row; packs not swept", c.Subject)
		}
		for _, p := range row.Packs {
			referenced[packSubject(row.Store, p)] = true
		}
	}
	// Drive snapshots held in the store name packs too.
	drive, err := m.Store.ListOffsiteCopies(t.ID, store.OffsiteKindFilesSnapshot, store.OffsiteStored)
	if err != nil {
		return stored, err
	}
	for _, c := range drive {
		var fs FilesSnapshot
		if json.Unmarshal([]byte(c.Doc), &fs) != nil {
			return stored, fmt.Errorf("files snapshot copy %s has an unreadable manifest; packs not swept", c.Subject)
		}
		for _, p := range fs.Packs {
			referenced[packSubject(fs.Store, p)] = true
		}
		newest = max(newest, c.UploadedAt)
	}
	var errs []string
	hidden := map[int64]bool{}
	for _, c := range stored {
		if c.Kind != kindPack || referenced[c.Subject] || c.UploadedAt > newest {
			continue
		}
		if err := r.Client.Delete(ctx, c.Key); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		m.Store.SetOffsiteCopyState(c.ID, store.OffsiteHidden)
		hidden[c.ID] = true
	}
	kept := withoutHidden(stored, hidden)
	if len(errs) > 0 {
		return kept, errors.New(strings.Join(errs, "; "))
	}
	return kept, nil
}

// FetchStoreSnapshot reads a store-backed backup back out of a locker into
// dest: the packs it names are fetched into a scratch copy of the store
// beside dest, each checked against its name, and the snapshot is rebuilt
// from them — every chunk and file verified, and a delegated artifact
// against the digest fog verified. scratch is a directory on a disk with
// room for the packs. An empty dest reads the backup through without
// writing it anywhere: a check, needing room only for the packs.
func FetchStoreSnapshot(ctx context.Context, r *Reader, row *AppRow, scratch, dest string, progress func(done, total int)) error {
	if row.Store == "" || row.StoreSnapshot == "" {
		return errors.New("this row is not a store snapshot")
	}
	return fetchFromStore(ctx, r, storeRef{row.Store, row.StoreSnapshot, row.StoreDoc, row.Packs}, scratch, dest, row.ArtifactSHA256, progress)
}

// storeRef is what a row says about the store snapshot that holds it.
type storeRef struct {
	store, snapshot string
	doc             []byte
	packs           []string
}

func fetchFromStore(ctx context.Context, r *Reader, ref storeRef, scratch, dest, artifactSHA string, progress func(done, total int)) error {
	tmp, err := os.MkdirTemp(scratch, "store-fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	sdir := filepath.Join(tmp, "store")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		return err
	}
	conf, err := r.bytes(ctx, r.Layout.StoreConfig(ref.store), 1<<20)
	if err != nil {
		return fmt.Errorf("reading the store's settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "store.json"), conf, 0o600); err != nil {
		return err
	}
	var key []byte
	if keyHex, err := r.bytes(ctx, r.Layout.StoreKey(ref.store), 256); err == nil && len(keyHex) > 0 {
		if key, err = hex.DecodeString(string(keyHex)); err != nil {
			return fmt.Errorf("the store's key is unreadable: %w", err)
		}
	}
	cs, err := chunkstore.Open(sdir, chunkstore.Options{IndexPath: filepath.Join(tmp, "index.db"), Key: key})
	if err != nil {
		return err
	}
	defer cs.Close()
	for i, name := range ref.packs {
		if progress != nil {
			progress(i, len(ref.packs))
		}
		p := filepath.Join(tmp, "fetch-"+name)
		sum, _, err := r.FetchTo(ctx, r.Layout.StorePack(ref.store, name), p)
		if err != nil {
			return err
		}
		if sum != name {
			return fmt.Errorf("pack %s in the bucket does not match its name", name[:12])
		}
		if err := cs.AdoptPack(name, p); err != nil {
			return err
		}
	}
	if err := cs.PutDoc(ref.snapshot, ref.doc); err != nil {
		return fmt.Errorf("the snapshot document: %w", err)
	}
	snap, err := cs.Snapshot(ref.snapshot)
	if err != nil {
		return err
	}
	if dest == "" {
		// Verify only: read every file back, write none of it.
		if err := cs.VerifySnapshot(ctx, snap); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return err
		}
		if err := cs.Materialize(ctx, snap, dest, nil); err != nil {
			return err
		}
	}
	if artifactSHA != "" {
		e, err := cs.Find(snap, "artifact.tar")
		if err != nil {
			return err
		}
		if !strings.EqualFold(e.SHA256, artifactSHA) {
			return errors.New("the artifact read back from the bucket does not match the digest fog verified")
		}
	}
	return nil
}

// syncFilesStore copies one store-held Drive snapshot: its missing packs,
// then its manifest, which names them.
func (m *Manager) syncFilesStore(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress,
	ix copyIndex, row *store.FileSnapshot, subject string) error {
	stamp := stampOfSnapshot(row)
	cs, err := m.ChunkStore()
	if err != nil {
		return err
	}
	snap, err := cs.Snapshot(row.StoreSnapshot)
	if err != nil {
		return err
	}
	doc, err := cs.DocBytes(snap.ID)
	if err != nil {
		return err
	}
	packs, err := cs.PacksOf(snap)
	if err != nil {
		return err
	}
	if err := m.syncStoreConfig(ctx, t, r, up, cs); err != nil {
		return err
	}
	pix, err := m.copyIndex(t, kindPack)
	if err != nil {
		return err
	}
	var todo []string
	for _, name := range packs {
		if !packStored(pix, packSubject(cs.ID(), name)) {
			todo = append(todo, name)
		}
	}
	p.Current(fmt.Sprintf("Drive %s · %d of %d pieces to copy", stamp, len(todo), len(packs)))
	if err := m.uploadPacks(ctx, t, r, up, p, pix, cs, todo); err != nil {
		return err
	}
	fs := FilesSnapshot{Format: Format, Stamp: stamp, CreatedAt: row.CreatedAt, Files: []FileEntry{},
		Bytes: row.Bytes, Count: row.Files, SourceSnapshotID: row.ID,
		Store: cs.ID(), StoreSnapshot: snap.ID, StoreDoc: doc, Packs: packs}
	body, err := json.Marshal(fs)
	if err != nil {
		return err
	}
	key := r.Layout.FilesSnapshot(stamp)
	sealed, err := r.Keys.SealBytes(r.Layout.Name(key), body)
	if err != nil {
		return err
	}
	if _, err := r.Client.PutBytes(ctx, key, sealed, withContentType(up.Lock, "application/octet-stream")); err != nil {
		return fmt.Errorf("writing the manifest: %w", err)
	}
	summary, err := json.Marshal(filesIndexLine(&fs))
	if err != nil {
		return err
	}
	return m.stored(ix, store.OffsiteCopy{TargetID: t.ID, Kind: store.OffsiteKindFilesSnapshot, Subject: subject, Key: key,
		Bytes: int64(len(sealed)), Doc: string(body), Summary: string(summary), UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)})
}

// restoreFilesFromStore writes a store-held Drive snapshot out of the
// locker into dir.
func (r *Reader) restoreFilesFromStore(ctx context.Context, snap *FilesSnapshot, dir string) (int64, int64, error) {
	scratch, err := os.MkdirTemp(filepath.Dir(filepath.Clean(dir)), ".fog-restore-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(scratch)
	if err := fetchFromStore(ctx, r, storeRef{snap.Store, snap.StoreSnapshot, snap.StoreDoc, snap.Packs}, scratch, dir, "", nil); err != nil {
		return 0, 0, err
	}
	return snap.Count, snap.Bytes, nil
}

// verifyPacks re-reads a random sample of the packs a store-held copy
// names, each checked against its name.
func verifyPacks(ctx context.Context, r *Reader, storeID string, packs []string, n int) error {
	perm := rand.Perm(len(packs))
	for _, i := range perm[:min(n, len(perm))] {
		sum, _, err := r.Hash(ctx, r.Layout.StorePack(storeID, packs[i]))
		if err != nil {
			return err
		}
		if sum != packs[i] {
			return fmt.Errorf("pack %s in the bucket does not match its name", packs[i][:12])
		}
	}
	return nil
}

// HealChunks repairs damaged chunks of the local store from the packs the
// lockers hold: each damaged chunk's pack was copied offsite whole, so the
// same pack is fetched back and the chunk read out of it, verified, and
// rewritten locally. It returns the ids it could not heal.
func (m *Manager) HealChunks(ctx context.Context, cs *chunkstore.Store, ids []string) ([]string, error) {
	targets, err := m.Store.ListOffsiteTargets()
	if err != nil {
		return ids, err
	}
	if err := os.MkdirAll(m.spool(), 0o700); err != nil {
		return ids, err
	}
	fetched := map[string]string{} // pack → local copy
	defer func() {
		for _, p := range fetched {
			os.Remove(p)
		}
	}()
	fetch := func(id chunkstore.ChunkID) ([]byte, error) {
		pack, ok, err := cs.PackOf(id.String())
		if err != nil || !ok {
			return nil, chunkstore.ErrNotFound
		}
		if p, ok := fetched[pack]; ok {
			return cs.ChunkFromPackFile(p, pack, id)
		}
		for i := range targets {
			t := &targets[i]
			pix, err := m.copyIndex(t, kindPack)
			if err != nil || !packStored(pix, packSubject(cs.ID(), pack)) {
				continue
			}
			r, err := m.Reader(ctx, t)
			if err != nil {
				continue
			}
			p := filepath.Join(m.spool(), "heal-"+pack)
			if _, _, err := r.FetchTo(ctx, r.Layout.StorePack(cs.ID(), pack), p); err != nil {
				continue
			}
			fetched[pack] = p
			return cs.ChunkFromPackFile(p, pack, id)
		}
		return nil, chunkstore.ErrNotFound
	}
	failed, err := cs.Heal(ctx, ids, fetch)
	if len(failed) < len(ids) {
		m.Log.Info("backup store: repaired from the offsite copy", "chunks", len(ids)-len(failed))
		m.Store.Audit("system", "backup.store.heal", "", fmt.Sprintf(`{"healed":%d,"failed":%d}`, len(ids)-len(failed), len(failed)))
	}
	return failed, err
}

// FetchBackup writes one app backup out of a locker into dest (which must
// not exist), whichever way it was copied — as the store's packs or as a
// sealed tar of its directory — every byte checked against its row. It is
// what fog-locker restores with, needing nothing but the bucket and the
// passphrase.
func FetchBackup(ctx context.Context, r *Reader, app, stamp, scratch, dest string) (*AppRow, error) {
	var row AppRow
	if err := r.Document(ctx, r.Layout.AppRow(app, stamp), &row); err != nil {
		return nil, fmt.Errorf("reading the backup's row: %w", err)
	}
	if row.StoreSnapshot != "" {
		return &row, FetchStoreSnapshot(ctx, r, &row, scratch, dest, nil)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return nil, err
	}
	return &row, fetchUnpacked(ctx, r, r.Layout.AppSnapshot(app, stamp), row.SnapshotSHA256, dest, nil, nil)
}
