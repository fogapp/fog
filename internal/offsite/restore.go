package offsite

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/store"
)

// The restore doors. Every one of them reads the locker through Reader
// and hands the bytes to a door fog already has: a fetched app snapshot
// lands in the backup directory as the row it was, and restores through
// the delegated or the volume path; a Files snapshot streams into the
// files root; "recover everything" is those two for every app, after a
// kit restore has made this box the old server.

// OffsiteRow is one app backup as the locker (its index) describes it,
// with whether this box holds it locally.
type OffsiteRow struct {
	Target     string `json:"target"`
	App        string `json:"app"`
	Stamp      string `json:"stamp"`
	Kind       string `json:"kind"`
	Mode       string `json:"mode"`
	CreatedAt  string `json:"createdAt"`
	SizeBytes  int64  `json:"sizeBytes"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
	Identity   string `json:"identity,omitempty"`
	// Local: the snapshot directory exists here; BackupID is its row.
	Local    bool  `json:"local"`
	BackupID int64 `json:"backupId,omitempty"`
}

// OffsiteFilesSnapshot is one Files snapshot as the locker describes it.
type OffsiteFilesSnapshot struct {
	Target    string `json:"target"`
	Stamp     string `json:"stamp"`
	CreatedAt string `json:"createdAt"`
	Files     int64  `json:"files"`
	Bytes     int64  `json:"bytes"`
	Local     bool   `json:"local"`
}

// indexCache keeps each target's index for a minute: the app tab and the
// clock menu poll, and the index only changes nightly.
type indexCache struct {
	mu      sync.Mutex
	entries map[string]cachedIndex
}

type cachedIndex struct {
	at     time.Time
	ix     *Index
	choice IndexChoice
	err    error
}

const indexTTL = time.Minute

// ErrIndexMissing: the index this fog recorded writing is gone or will not
// open; recovering from the bucket's listing instead needs the operator's
// go-ahead.
var ErrIndexMissing = errors.New("the index this fog last wrote to that locker is missing or will not open — someone may have tampered with the bucket; recovering from the bucket's listing instead needs your go-ahead")

// NoticeIndexFallback is raised while a target's index reads fall back past
// the one fog recorded writing last.
const NoticeIndexFallback = "offsite-index-fallback"

// indexRetryAfter is how long a failed or fallen-back index read is kept:
// the app tab polls every minute, and each read of a bucket whose newest
// index will not open downloads and tries several. Fixing the bucket and
// forgetIndex (a sync) clear it sooner.
const indexRetryAfter = 15 * time.Minute

// IndexOf reads a target's index, cached briefly.
func (m *Manager) IndexOf(ctx context.Context, t *store.OffsiteTarget) (*Index, error) {
	ix, _, err := m.IndexChoiceOf(ctx, t)
	return ix, err
}

// IndexChoiceOf is IndexOf with the account of which index it read.
func (m *Manager) IndexChoiceOf(ctx context.Context, t *store.OffsiteTarget) (*Index, IndexChoice, error) {
	m.indexes.mu.Lock()
	if m.indexes.entries == nil {
		m.indexes.entries = map[string]cachedIndex{}
	}
	if c, ok := m.indexes.entries[t.Name]; ok {
		ttl := indexTTL
		if c.err != nil || c.choice.FellBack {
			ttl = indexRetryAfter
		}
		if m.now().Sub(c.at) < ttl {
			m.indexes.mu.Unlock()
			return c.ix, c.choice, c.err
		}
	}
	m.indexes.mu.Unlock()
	r, err := m.Reader(ctx, t)
	if err != nil {
		return nil, IndexChoice{}, err
	}
	ix, choice, err := r.IndexInfo(ctx)
	if err != nil && ctx.Err() != nil {
		return nil, choice, err // a cancelled read says nothing about the bucket
	}
	switch {
	case choice.FellBack:
		if m.Log != nil {
			m.Log.Warn("offsite: the index fog recorded writing last could not be read; using an older one",
				"target", t.Name, "using", choice.Key, "skipped", choice.Skipped)
		}
		m.Store.CreateNotice(NoticeIndexFallback, t.Name, "The offsite index in "+t.Name+" is not the one fog wrote last",
			fmt.Sprintf("The newest index fog wrote to %s is missing or will not open, so what fog shows of that locker comes from an older one (%s). "+
				"Someone with write access to the bucket may have removed or replaced it; the next sync writes a fresh one. If this persists, check the bucket.",
				t.Name, StampOf(choice.Key)))
	case errors.Is(err, ErrNoIndex):
		m.Store.CreateNotice(NoticeIndexFallback, t.Name, "The offsite locker in "+t.Name+" has no readable index",
			"No index in "+t.Name+" opens, so the offsite backup lists are read from the bucket's listing instead (slower). "+
				"If fog has synced to this target before, someone with write access to the bucket may have removed or spoiled them; "+
				"the next sync writes a fresh one.")
	case err == nil:
		m.Store.DismissNoticeByKind(NoticeIndexFallback, t.Name)
	}
	m.indexes.mu.Lock()
	m.indexes.entries[t.Name] = cachedIndex{at: m.now(), ix: ix, choice: choice, err: err}
	m.indexes.mu.Unlock()
	return ix, choice, err
}

func (m *Manager) forgetIndex(name string) {
	m.indexes.mu.Lock()
	delete(m.indexes.entries, name)
	m.indexes.mu.Unlock()
}

// localBackups maps an app's local snapshot stamps to their rows, for
// directories that actually exist.
func (m *Manager) localBackups(app *store.App) map[string]store.Backup {
	out := map[string]store.Backup{}
	list, err := m.Store.ListBackups(app.ID, 1000)
	if err != nil {
		return out
	}
	for _, b := range list {
		if !backup.HasFiles(&b) {
			continue
		}
		out[stampOfBackup(&b)] = b
	}
	return out
}

// AppRows lists an app's backups across every target's locker.
func (m *Manager) AppRows(ctx context.Context, appName string) ([]OffsiteRow, error) {
	targets, err := m.Store.ListOffsiteTargets()
	if err != nil {
		return nil, err
	}
	var local map[string]store.Backup
	if app, err := m.Store.AppByName(appName); err == nil {
		local = m.localBackups(app)
	}
	var out []OffsiteRow
	var errs []string
	for i := range targets {
		t := &targets[i]
		ix, err := m.IndexOf(ctx, t)
		if err != nil {
			if errors.Is(err, ErrNoIndex) {
				continue
			}
			errs = append(errs, t.Name+": "+err.Error())
			continue
		}
		for _, row := range ix.Apps[appName] {
			r := OffsiteRow{Target: t.Name, App: row.App, Stamp: row.Stamp, Kind: row.Kind, Mode: row.Mode, CreatedAt: row.CreatedAt,
				SizeBytes: row.SizeBytes, VerifiedAt: row.VerifiedAt, Identity: row.Identity}
			if b, ok := local[row.Stamp]; ok {
				r.Local, r.BackupID = true, b.ID
			}
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp > out[j].Stamp
		}
		return out[i].Target < out[j].Target
	})
	if out == nil {
		out = []OffsiteRow{}
	}
	if len(errs) > 0 && len(out) == 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

// FilesSnapshots lists the Files snapshots across every target's locker.
func (m *Manager) FilesSnapshots(ctx context.Context) ([]OffsiteFilesSnapshot, error) {
	targets, err := m.Store.ListOffsiteTargets()
	if err != nil {
		return nil, err
	}
	local := map[string]bool{}
	if rows, err := m.Store.ListFileSnapshots(); err == nil {
		for i := range rows {
			if _, err := os.Stat(filepath.Join(rows[i].Path, filesManifestName)); err == nil {
				local[stampOfSnapshot(&rows[i])] = true
			}
		}
	}
	out := []OffsiteFilesSnapshot{}
	for i := range targets {
		t := &targets[i]
		ix, err := m.IndexOf(ctx, t)
		if err != nil {
			continue
		}
		for _, f := range ix.Files {
			out = append(out, OffsiteFilesSnapshot{Target: t.Name, Stamp: f.Stamp, CreatedAt: f.CreatedAt, Files: f.Files, Bytes: f.Bytes, Local: local[f.Stamp]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp > out[j].Stamp })
	return out, nil
}

// ---- fetching an app snapshot back ----

// Fetch pulls one app snapshot out of a target's locker into the backup
// directory as the row it was, and returns the local backup id. A
// snapshot already present locally is left alone.
func (m *Manager) Fetch(ctx context.Context, p *jobs.Progress, targetName, appName, stamp string) (int64, error) {
	t, err := m.Store.OffsiteTargetByName(targetName)
	if err != nil {
		return 0, err
	}
	app, err := m.Store.AppByName(appName)
	if err != nil {
		return 0, fmt.Errorf("no app named %s on this fog — install it first, then fetch", appName)
	}
	if b, ok := m.localBackups(app)[stamp]; ok {
		return b.ID, nil
	}
	if m.BackupRoot == nil {
		return 0, errors.New("no backup directory")
	}
	release, held := m.holdBackups()
	if !held {
		return 0, backup.ErrDirMoving
	}
	defer release()
	dest := filepath.Join(m.BackupRoot(), appName, stamp)
	if !m.available(filepath.Dir(dest)) {
		return 0, errors.New("the backup directory is on a share that is not available")
	}
	r, err := m.Reader(ctx, t)
	if err != nil {
		return 0, err
	}
	var row AppRow
	if err := r.Document(ctx, r.Layout.AppRow(appName, stamp), &row); err != nil {
		return 0, fmt.Errorf("reading the backup's row from %s: %w", targetName, err)
	}
	if p != nil {
		p.Current(appName + " " + stamp)
		p.Total(row.SizeBytes)
	}
	// The sealed tar unpacks as it arrives into a staging directory beside
	// the other snapshots, and moves into place only once the stream's
	// last frame has authenticated and the whole matches its row — a
	// tampered or truncated object leaves nothing behind.
	staging := dest + ".fetching"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return 0, err
	}
	var ferr error
	if row.StoreSnapshot != "" {
		// A store-backed copy: its packs are fetched and the snapshot
		// rebuilt from them, verified, into the staging directory.
		os.Remove(staging)
		ferr = FetchStoreSnapshot(ctx, r, &row, filepath.Dir(dest), staging, func(done, total int) {
			if p != nil {
				p.Current(fmt.Sprintf("%s %s · piece %d of %d", appName, stamp, done+1, total))
			}
		})
	} else {
		ferr = fetchUnpacked(ctx, r, r.Layout.AppSnapshot(appName, stamp), row.SnapshotSHA256, staging, p, nil)
	}
	if ferr != nil {
		os.RemoveAll(staging)
		return 0, ferr
	}
	if err := os.Rename(staging, dest); err != nil {
		os.RemoveAll(staging)
		return 0, err
	}
	size := fsx.DirSize(context.Background(), dest)
	id, err := m.Store.CreateArtifactBackupAt(app.ID, row.Kind, dest, size, row.ArtifactFormat, row.ArtifactSHA256, row.Manifest, row.CreatedAt)
	if err != nil {
		return 0, err
	}
	m.Store.Audit("system", "offsite.fetch", appName, fmt.Sprintf(`{"target":%q,"stamp":%q,"backup":%d}`, targetName, stamp, id))
	m.Log.Info("offsite: snapshot fetched", "target", targetName, "app", appName, "stamp", stamp, "backup", id)
	return id, nil
}

// StartFetch runs Fetch as a job; the result carries {"backupId": n}.
func (m *Manager) StartFetch(targetName, appName, stamp string) (jobs.Job, error) {
	if _, err := m.Store.OffsiteTargetByName(targetName); err != nil {
		return jobs.Job{}, err
	}
	if stamp == "" || !stampRe.MatchString(stamp) {
		return jobs.Job{}, errors.New("a snapshot stamp is required")
	}
	kind := "offsite-fetch:" + appName + ":" + stamp
	job, ok := m.jobs().StartExclusive(kind, "Fetch "+appName+" "+stamp+" from "+targetName, func(ctx context.Context, p *jobs.Progress) error {
		id, err := m.Fetch(ctx, p, targetName, appName, stamp)
		if err != nil {
			return err
		}
		p.Result(map[string]any{"backupId": id})
		return nil
	})
	if !ok {
		return jobs.Job{}, errors.New("that snapshot is already being fetched")
	}
	return job, nil
}

// fetchUnpacked opens key and unpacks its plaintext tar into dir in one
// pass, hashing the stream as it goes. It reads the stream to its end, so
// every frame — the last one included — has authenticated before it
// returns nil, and wantSHA (when set) has been checked against the whole.
// A non-nil digests map receives each entry's sha256.
func fetchUnpacked(ctx context.Context, r *Reader, key, wantSHA, dir string, p *jobs.Progress, digests map[string]string) error {
	src, err := r.Open(ctx, key)
	if err != nil {
		return err
	}
	defer src.Close()
	h := sha256.New()
	tee := io.TeeReader(src, h)
	if err := unpackFlatTar(tee, dir, p, digests); err != nil {
		return fmt.Errorf("fetching %s: %w", key, err)
	}
	// The tar ends before its padding does; the rest of the stream still
	// has to be read for the final frame to authenticate.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return fmt.Errorf("fetching %s: %w", key, err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); wantSHA != "" && !strings.EqualFold(sum, wantSHA) {
		return fmt.Errorf("the fetched snapshot does not match its row (sha256 %s, expected %s)", sum[:12], wantSHA[:min(12, len(wantSHA))])
	}
	return nil
}

// maxSnapshotEntries bounds a snapshot tar: a snapshot directory is a few
// files (an artifact and its manifest, or volume archives and dumps).
const maxSnapshotEntries = 1000

// unpackFlatTar extracts a snapshot tar's top-level regular files into
// dir. Anything else — directories, links, a path with a slash — is not a
// snapshot directory and is refused. A non-nil digests map receives each
// file's sha256, computed as it is written.
func unpackFlatTar(src io.Reader, dir string, p *jobs.Progress, digests map[string]string) error {
	tr := tar.NewReader(src)
	seen := map[string]struct{}{}
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// A snapshot directory holds a handful of files with short names;
		// bound both so a hostile tar cannot grow the duplicate check without
		// limit.
		if n++; n > maxSnapshotEntries {
			return fmt.Errorf("snapshot tar has more than %d entries", maxSnapshotEntries)
		}
		name := hdr.Name
		if len(name) > 255 {
			return fmt.Errorf("snapshot tar has an entry name %d bytes long", len(name))
		}
		if hdr.Typeflag != tar.TypeReg || backup.SparseEntry(hdr) || name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return fmt.Errorf("snapshot tar holds an entry that is not a plain file: %q", name)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("snapshot tar has more than one entry named %q", name)
		}
		seen[name] = struct{}{}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		var w io.Writer = out
		var h hash.Hash
		if digests != nil {
			h = sha256.New()
			w = io.MultiWriter(out, h)
		}
		n, err := io.Copy(w, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if h != nil {
			digests[name] = hex.EncodeToString(h.Sum(nil))
		}
		if p != nil {
			p.Add(n, 1)
		}
	}
}

// ---- Files ----

// StartFilesRestore streams a Files snapshot from a target into the files
// root (or a folder under it). The job runs in the Files registry when
// one is wired, so the Drive page's tray shows it.
func (m *Manager) StartFilesRestore(targetName, stamp, to string) (jobs.Job, error) {
	t, err := m.Store.OffsiteTargetByName(targetName)
	if err != nil {
		return jobs.Job{}, err
	}
	if !stampRe.MatchString(stamp) {
		return jobs.Job{}, errors.New("a snapshot stamp is required")
	}
	if m.FilesRoot == nil {
		return jobs.Job{}, errors.New("Drive is not wired")
	}
	rel, ok := safeRel(to)
	if to != "" && !ok {
		return jobs.Job{}, errors.New("the destination must be a folder inside Drive")
	}
	dest := m.FilesRoot()
	if to != "" {
		dest = filepath.Join(dest, rel)
	}
	registry := m.jobs()
	if m.FilesJobs != nil {
		registry = m.FilesJobs
	}
	kind := "offsite-files-restore"
	job, ok := registry.StartExclusive(kind, "Restore Drive "+stamp+" from "+targetName, func(ctx context.Context, p *jobs.Progress) error {
		r, err := m.Reader(ctx, t)
		if err != nil {
			return err
		}
		var snap FilesSnapshot
		if err := r.Document(ctx, r.Layout.FilesSnapshot(stamp), &snap); err != nil {
			return err
		}
		p.Set("starting", 0, snap.Bytes)
		var done int64
		n, bytes, err := r.RestoreFiles(ctx, stamp, dest, func(path string) {
			p.Current(path)
			p.Add(0, 1)
		})
		done = bytes
		p.Set("", done, snap.Bytes)
		if m.FilesRestored != nil {
			m.FilesRestored()
		}
		if err != nil {
			return err
		}
		p.Result(map[string]any{"files": n, "bytes": bytes, "to": dest})
		m.Store.Audit("system", "offsite.files.restore", stamp, fmt.Sprintf(`{"target":%q,"files":%d,"bytes":%d}`, targetName, n, bytes))
		return nil
	})
	if !ok {
		return jobs.Job{}, errors.New("a restore from the locker is already running")
	}
	return job, nil
}

// ---- recover everything ----

// RecoverResult is the recover job's report card.
type RecoverResult struct {
	Apps  []RecoverStep `json:"apps"`
	Files *RecoverStep  `json:"files,omitempty"`
}

type RecoverStep struct {
	Name   string `json:"name"`
	Stamp  string `json:"stamp,omitempty"`
	Status string `json:"status" ts:"\"restored\" | \"fetched\" | \"skipped\" | \"failed\""` // restored | fetched | skipped | failed
	Detail string `json:"detail,omitempty"`
}

// StartRecover restores every app this fog knows from its newest snapshot
// in the target's locker, then the newest Files snapshot into the files
// root — the second half of a full-server recovery, after the kit has
// made this box the old server. Apps not deployed yet are deployed with
// the restore staged (delegated) or deployed and then restored (volumes);
// deployed apps go through their normal restore door.
//
// fromListing is the operator's go-ahead to recover from the bucket's own
// listing when the index this fog recorded writing is gone or will not
// open (ErrIndexMissing): without it, that case is refused, since it is
// what tampering looks like and the listing may be missing what the index
// had.
func (m *Manager) StartRecover(targetName string, fromListing bool) (jobs.Job, error) {
	t, err := m.Store.OffsiteTargetByName(targetName)
	if err != nil {
		return jobs.Job{}, err
	}
	if !fromListing {
		m.forgetIndex(t.Name)
		if _, err := m.IndexOf(context.Background(), t); errors.Is(err, ErrNoIndex) {
			if latest, _ := m.Store.GetSetting(latestIndexSetting(t)); latest != "" {
				return jobs.Job{}, ErrIndexMissing
			}
		}
	}
	kind := "offsite-recover:" + targetName
	job, ok := m.jobs().StartExclusive(kind, "Recover everything from "+targetName, func(ctx context.Context, p *jobs.Progress) error {
		return m.recover(ctx, p, t)
	})
	if !ok {
		return jobs.Job{}, errors.New("a recovery from this target is already running")
	}
	return job, nil
}

func (m *Manager) recover(ctx context.Context, p *jobs.Progress, t *store.OffsiteTarget) error {
	ix, err := m.IndexOf(ctx, t)
	if errors.Is(err, ErrNoIndex) {
		// The index is a cache (locker-contract §3.5): with none, the
		// locker's own listing says what is there.
		p.Current("no index in the locker; reading its listing instead")
		r, rerr := m.Reader(ctx, t)
		if rerr != nil {
			return rerr
		}
		ix, err = r.IndexFromListing(ctx)
	}
	if err != nil {
		return err
	}
	var res RecoverResult
	failed := 0
	apps, err := m.Store.ListApps()
	if err != nil {
		return err
	}
	for _, app := range apps {
		rows := ix.Apps[app.Name]
		if len(rows) == 0 {
			res.Apps = append(res.Apps, RecoverStep{Name: app.Name, Status: "skipped", Detail: "no snapshot in the locker"})
			continue
		}
		row := newestRow(rows)
		step := RecoverStep{Name: app.Name, Stamp: row.Stamp}
		p.Current(app.Name + " " + row.Stamp)
		id, err := m.Fetch(ctx, nil, t.Name, app.Name, row.Stamp)
		if err != nil {
			step.Status, step.Detail = "failed", "fetch: "+err.Error()
			res.Apps = append(res.Apps, step)
			failed++
			continue
		}
		if err := m.restoreFetched(ctx, &app, row, id); err != nil {
			step.Status, step.Detail = "failed", err.Error()
			failed++
		} else {
			step.Status = "restored"
		}
		res.Apps = append(res.Apps, step)
		p.Add(0, 1)
		p.Result(res)
	}
	if len(ix.Files) > 0 && m.FilesRoot != nil {
		f := ix.Files[0]
		step := &RecoverStep{Name: "files", Stamp: f.Stamp}
		p.Current("Drive " + f.Stamp)
		r, err := m.Reader(ctx, t)
		if err == nil {
			_, _, err = r.RestoreFiles(ctx, f.Stamp, m.FilesRoot(), func(path string) { p.Current("Files: " + path) })
		}
		if m.FilesRestored != nil {
			m.FilesRestored()
		}
		if err != nil {
			step.Status, step.Detail = "failed", err.Error()
			failed++
		} else {
			step.Status = "restored"
		}
		res.Files = step
	}
	p.Result(res)
	m.Store.Audit("system", "offsite.recover", t.Name, fmt.Sprintf(`{"apps":%d,"failed":%d}`, len(res.Apps), failed))
	if failed > 0 {
		return fmt.Errorf("%d of %d steps failed — see the report", failed, len(res.Apps)+1)
	}
	return nil
}

func newestRow(rows []AppRow) AppRow {
	best := rows[0]
	for _, r := range rows[1:] {
		if r.Stamp > best.Stamp {
			best = r
		}
	}
	return best
}

// restoreFetched puts a fetched snapshot into the app: deployed apps take
// their normal door; undeployed delegated apps are deployed with the
// restore staged for first boot; undeployed volume apps are deployed and
// then restored.
func (m *Manager) restoreFetched(ctx context.Context, app *store.App, row AppRow, backupID int64) error {
	if m.RestoreApp == nil {
		return errors.New("restore is not wired")
	}
	deployed := m.Deployed == nil || m.Deployed(app.Name)
	if deployed {
		return m.RestoreApp(ctx, backupID)
	}
	if row.Mode == "delegated" && m.DeployRestored != nil {
		b, err := m.Store.BackupByID(backupID)
		if err != nil {
			return err
		}
		return m.DeployRestored(ctx, app.Name, filepath.Join(b.Path, "artifact.tar"))
	}
	if m.Deploy == nil {
		return errors.New("the app is not deployed and deploy is not wired")
	}
	if err := m.Deploy(ctx, app.Name); err != nil {
		return fmt.Errorf("deploy before restore: %w", err)
	}
	return m.RestoreApp(ctx, backupID)
}

// CurrentJobFor is the running sync, recovery or copy for a target.
func (m *Manager) CurrentJobFor(name string) (jobs.Job, bool) {
	for _, kind := range []string{syncKind(name), "offsite-recover:" + name, copyKind(name)} {
		if j, ok := m.jobs().Current(kind); ok {
			return j, true
		}
	}
	return jobs.Job{}, false
}

// Job looks a job up in the manager's registry.
func (m *Manager) Job(id string) (jobs.Job, bool) { return m.jobs().Get(id) }

// ---- the setup-screen door ----

// LockerPreview is what the setup screen shows before it restores: the
// locker, its kits, and what else is in it.
type LockerPreview struct {
	LockerID  string     `json:"lockerId"`
	Name      string     `json:"name"`
	CreatedAt string     `json:"createdAt"`
	Kits      []IndexKit `json:"kits"`
	// Index says which index the lists below came from; FellBack means a
	// newer one would not open — shown prominently, as it may be tampering.
	Index *IndexChoice `json:"index,omitempty"`
	Apps  []string     `json:"apps"`
	Files []IndexFiles `json:"files"`
	// NewestKit is the verdict on the kit a restore would use, filled by the
	// setup door once it has decrypted it: whether its date is sealed or it
	// is a legacy kit whose date cannot be verified.
	NewestKit *NewestKit `json:"newestKit,omitempty"`
}

// NewestKit is what the setup screen shows about the kit it would restore.
type NewestKit struct {
	Name string `json:"name"`
	// Legacy: an older fog's kit with no sealed date — Warning says why that
	// matters, and a restore needs the operator's explicit go-ahead.
	Legacy  bool   `json:"legacy"`
	Date    string `json:"date,omitempty"`
	Warning string `json:"warning,omitempty"`
	// Error: no kit would be usable.
	Error string `json:"error,omitempty"`
	// SHA256 is the sealed kit's digest. Consent to restore a legacy kit
	// names this kit and digest, so it cannot carry over to another one.
	SHA256 string `json:"sha256,omitempty"`
	// Skipped lists newer kits that were passed over, and why — a decoy or a
	// replay planted in the bucket is reported, not silently stepped over.
	Skipped []SkippedKit `json:"skipped,omitempty"`
	// Recorded: the kit is one a fog recorded uploading (in the sealed
	// index, with this digest). A kit that is not was only found in the
	// bucket listing.
	Recorded bool `json:"recorded"`
	// FellBack: a newer kit exists that this one was chosen over — newer
	// by name, or the one the index records as fog's latest. Shown
	// prominently: restoring it rolls the machine back to an older state.
	FellBack bool `json:"fellBack"`
	// Latest is the kit the index records as fog's newest upload, if any.
	Latest string `json:"latest,omitempty"`
}

// OpenForRecovery connects to a bucket with nothing but the operator's
// credentials and passphrase, and returns a reader over its locker. The
// fresh-machine door: no store, no vault. dialControl (when non-nil) vets
// every connection's address, so a redirect or DNS rebinding past the
// caller's URL-level check cannot reach an internal host.
func OpenForRecovery(ctx context.Context, spec TargetSpec, keyID, secret, passphrase string, dialControl func(network, address string) error) (*Reader, *Locker, error) {
	if err := spec.normalize(); err != nil {
		return nil, nil, err
	}
	if passphrase == "" {
		return nil, nil, errors.New("the kit passphrase is required")
	}
	c, err := objstore.New(objstore.Config{Endpoint: spec.Endpoint, Region: spec.Region, Bucket: spec.Bucket,
		AccessKey: keyID, SecretKey: secret, PathStyle: spec.PathStyle, DialControl: dialControl})
	if err != nil {
		return nil, nil, err
	}
	id := spec.LockerID
	if id == "" {
		ids, err := Discover(ctx, c, spec.Prefix)
		if err != nil {
			return nil, nil, fmt.Errorf("listing lockers: %w", err)
		}
		switch len(ids) {
		case 0:
			return nil, nil, fmt.Errorf("no locker under %s in bucket %s", LockersRoot(spec.Prefix), spec.Bucket)
		case 1:
			id = ids[0]
		default:
			return nil, nil, fmt.Errorf("this prefix holds %d lockers (%s) — enter the id of yours", len(ids), strings.Join(ids, ", "))
		}
	}
	layout := Layout{Prefix: spec.Prefix, ID: id}
	lk, err := FetchLocker(ctx, c, layout)
	if err != nil {
		return nil, nil, err
	}
	keys, err := lk.OpenWithPassphraseCtx(ctx, passphrase)
	if err != nil {
		return nil, nil, err
	}
	return &Reader{Client: c, Layout: layout, Keys: keys}, lk, nil
}

// Preview describes a locker for the setup screen.
func Preview(ctx context.Context, r *Reader, lk *Locker) (*LockerPreview, error) {
	kits, err := r.Kits(ctx)
	if err != nil {
		return nil, err
	}
	pv := &LockerPreview{LockerID: lk.ID, Name: lk.Name, CreatedAt: lk.CreatedAt, Kits: kits, Apps: []string{}, Files: []IndexFiles{}}
	if ix, choice, err := r.IndexInfo(ctx); err == nil {
		pv.Index = &choice
		for app := range ix.Apps {
			pv.Apps = append(pv.Apps, app)
		}
		sort.Strings(pv.Apps)
		pv.Files = ix.Files
	}
	if pv.Kits == nil {
		pv.Kits = []IndexKit{}
	}
	return pv, nil
}

// SkippedKit is a kit OpenNewestKit passed over, and why.
type SkippedKit struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// OpenedKit is the kit a recovery would use: its sealed and opened bytes,
// what VerifyKit found, and any newer kits that had to be skipped.
type OpenedKit struct {
	Name    string
	Sealed  []byte
	Plain   []byte
	SHA256  string
	Check   backup.KitCheck
	Skipped []SkippedKit
	// Recorded, FellBack, Latest: as in NewestKit.
	Recorded bool
	FellBack bool
	Latest   string
}

// maxKitAttempts is how many decryptions — each a deliberately costly key
// derivation (256 MiB, about a second) — run at full speed; past it they
// are paced by kitDecrypts, not refused, so decoys can slow a recovery but
// never exhaust it before the real kit is reached. Only kits that get that
// far count: a name dated after the bucket received it, an object of the
// wrong size, one that does not start like a kit or does not match the
// digest fog recorded are passed over without one.
const maxKitAttempts = 10

// kitDecrypts paces decryptions process-wide, so the recovery door cannot
// be made to run key derivations back to back for long.
var kitDecrypts = rate.NewLimiter(rate.Every(2*time.Second), maxKitAttempts)

// maxKitFetches bounds the downloads a choice makes, cheap or not.
const maxKitFetches = 200

// kitFetches bounds the kits being downloaded at once, process-wide.
var kitFetches = make(chan struct{}, 2)

// kitFutureSkew is how far past the moment the bucket received it a kit's
// name may be dated: a kit is named when it is sealed, before it is
// uploaded, so a name later than its arrival is a decoy built to sort
// newest. The bucket's clock, not this machine's, is the reference — a
// fresh machine with a wrong clock judges kits correctly.
const kitFutureSkew = 15 * time.Minute

// minKitBytes is the smallest sealed kit: magic, salt, nonce and GCM tag.
const minKitBytes = 8 + 16 + 12 + 16

// KitPin names the exact kit a restore may use: the one the preview showed.
type KitPin struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// OpenNewestKit chooses the kit a recovery uses and opens it. In order:
//
//   - names dated after now (plus a small skew) are ignored;
//   - kits a fog recorded uploading — in the locker's sealed index, with
//     the digest of their bytes — come first, newest first, and one whose
//     bytes do not match that digest is refused; kits only seen in the
//     bucket listing follow, newest first;
//   - current-format kits (FOGKIT2, which seal their own stamp) are all
//     tried before any legacy one: an old legacy kit renamed to sort newest
//     cannot hide the current kits, and a legacy kit is offered only when
//     no current one opens;
//   - only decryptions count against maxKitAttempts.
//
// A junk, corrupt or replayed kit is skipped and reported in Skipped, and
// a choice that is not the newest kit says so (FellBack) for the operator.
func OpenNewestKit(ctx context.Context, r *Reader, passphrase string) (*OpenedKit, error) {
	return OpenKit(ctx, r, passphrase, nil, time.Now())
}

// OpenKit is OpenNewestKit as of now; with pin, it opens that kit or
// nothing — the restore after a preview, which must restore what the
// operator was shown.
func OpenKit(ctx context.Context, r *Reader, passphrase string, pin *KitPin, now time.Time) (*OpenedKit, error) {
	listed, err := r.Kits(ctx)
	if err != nil {
		return nil, err
	}
	if len(listed) == 0 {
		return nil, errors.New("the locker holds no recovery kit")
	}
	recorded := map[string]string{}
	latest := ""
	if ix, err := r.Index(ctx); err == nil {
		for _, k := range ix.Kits {
			if k.SHA256 != "" {
				recorded[k.Name] = k.SHA256
			}
		}
		if ix.LatestKit != nil && ix.LatestKit.SHA256 != "" {
			recorded[ix.LatestKit.Name] = ix.LatestKit.SHA256
			latest = ix.LatestKit.Name
		}
	}

	var skipped []SkippedKit
	skip := func(name, reason string) { skipped = append(skipped, SkippedKit{Name: name, Reason: reason}) }
	var first, rest []IndexKit
	newest := ""
	for _, k := range listed {
		if pin != nil && k.Name != pin.Name {
			continue
		}
		received := k.Received
		if received.IsZero() {
			received = now
		}
		if t, ok := fsx.ParseStamp(StampOf(k.Name)); !ok || t.After(received.Add(kitFutureSkew)) {
			skip(k.Name, fmt.Sprintf("named for %s, after the bucket received it (%s) — not a kit fog wrote",
				StampOf(k.Name), received.UTC().Format(time.RFC3339)))
			continue
		}
		if newest == "" {
			newest = k.Name
		}
		if recorded[k.Name] != "" {
			first = append(first, k)
		} else {
			rest = append(rest, k)
		}
	}
	if pin != nil && len(first)+len(rest) == 0 {
		return nil, fmt.Errorf("the kit the preview showed, %s, is no longer in the locker", pin.Name)
	}
	candidates := append(first, rest...)

	fetches, attempts := 0, 0
	fetch := func(k IndexKit) ([]byte, bool) {
		if k.Bytes < minKitBytes || k.Bytes > MaxKitBytes {
			skip(k.Name, fmt.Sprintf("its size (%d bytes) is not a kit's", k.Bytes))
			return nil, false
		}
		if fetches >= maxKitFetches {
			return nil, false
		}
		fetches++
		var buf bytes.Buffer
		if _, err := r.FetchKit(ctx, k.Name, &buf); err != nil {
			skip(k.Name, "could not be read: "+err.Error())
			return nil, false
		}
		sealed := buf.Bytes()
		sum := sha256.Sum256(sealed)
		digest := hex.EncodeToString(sum[:])
		if want := recorded[k.Name]; want != "" && want != digest {
			skip(k.Name, "its bytes do not match the digest fog recorded uploading — replaced in the bucket")
			return nil, false
		}
		if pin != nil && pin.SHA256 != digest {
			skip(k.Name, "its bytes changed since the preview")
			return nil, false
		}
		if len(sealed) < minKitBytes || !backup.IsKit(sealed[:8]) {
			skip(k.Name, "not a fog recovery kit")
			return nil, false
		}
		return sealed, true
	}
	paced := false
	open := func(k IndexKit, sealed []byte) (*OpenedKit, bool) {
		if err := kitDecrypts.Wait(ctx); err != nil {
			paced = true
			return nil, false
		}
		attempts++
		plain, err := backup.DecryptKit(sealed, passphrase)
		if err != nil {
			skip(k.Name, err.Error())
			return nil, false
		}
		check, err := backup.VerifyKit(sealed, plain, k.Name)
		if err != nil {
			clear(plain)
			skip(k.Name, err.Error())
			return nil, false
		}
		sum := sha256.Sum256(sealed)
		return &OpenedKit{Name: k.Name, Sealed: sealed, Plain: plain, SHA256: hex.EncodeToString(sum[:]), Check: check,
			Recorded: recorded[k.Name] != "", Latest: latest}, true
	}

	// tryKit fetches, checks and (when it gets that far) decrypts one kit,
	// all inside the process's one heavy slot (takeHeavy): the kit's bytes
	// are not even fetched until the slot is held, so however many
	// requests arrive at once, one kit at a time is in memory being opened.
	// isLegacy reports a legacy kit set aside for later, unopened.
	tryKit := func(k IndexKit, legacyOK bool) (kit *OpenedKit, isLegacy bool, err error) {
		// Fetched first, outside the heavy slot (a slow bucket must not
		// hold it), but at most two at a time process-wide, each capped
		// in size and in time (FetchKit); then opened inside the slot.
		select {
		case kitFetches <- struct{}{}:
		case <-ctx.Done():
			paced = true
			return nil, false, ctx.Err()
		}
		sealed, ok := fetch(k)
		<-kitFetches
		if !ok {
			return nil, false, nil
		}
		if !legacyOK && backup.IsLegacyKit(sealed) {
			return nil, true, nil
		}
		release, err := takeHeavy(ctx)
		if err != nil {
			paced = true
			return nil, false, err
		}
		defer release()
		if kit, ok := open(k, sealed); ok {
			return kit, false, nil
		}
		return nil, false, nil
	}

	// The oldest kit only seen in the listing is always tried early: decoys
	// are planted to sort newest, so however many there are, the oldest
	// unrecorded kit is reached within the first maxKitAttempts decryptions.
	var oldest *IndexKit
	if len(rest) > 1 {
		oldest = &rest[len(rest)-1]
	}
	tried := map[string]bool{}
	var legacy []IndexKit
	for i := 0; i < len(candidates); i++ {
		k := candidates[i]
		if oldest != nil && !tried[oldest.Name] && attempts == maxKitAttempts-1 {
			k, i = *oldest, i-1
		}
		if tried[k.Name] {
			continue
		}
		tried[k.Name] = true
		if ctx.Err() != nil || paced {
			break
		}
		if fetches >= maxKitFetches {
			break
		}
		kit, isLegacy, err := tryKit(k, false)
		if err != nil {
			break
		}
		if isLegacy {
			legacy = append(legacy, k) // only once no current kit opens
			continue
		}
		if kit != nil {
			return finishKit(kit, skipped, newest), nil
		}
	}
	for _, k := range legacy {
		if ctx.Err() != nil || paced {
			break
		}
		kit, _, err := tryKit(k, true)
		if err != nil {
			break
		}
		if kit != nil {
			return finishKit(kit, skipped, newest), nil
		}
	}
	msg := "no recovery kit in the locker opens with this passphrase and checks out"
	if paced || ctx.Err() != nil {
		msg += fmt.Sprintf(" (stopped after %d decryptions: out of time)", attempts)
	}
	if len(skipped) > 0 {
		var parts []string
		for i, sk := range skipped {
			if i == 12 {
				parts = append(parts, fmt.Sprintf("and %d more", len(skipped)-i))
				break
			}
			parts = append(parts, sk.Name+": "+sk.Reason)
		}
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	return nil, errors.New(msg)
}

// finishKit fills in what the choice passed over: FellBack when a newer
// kit than the one chosen exists, by name or as the index's recorded latest.
func finishKit(kit *OpenedKit, skipped []SkippedKit, newest string) *OpenedKit {
	kit.Skipped = skipped
	kit.FellBack = (newest != "" && newest > kit.Name) || (kit.Latest != "" && kit.Latest != kit.Name)
	return kit
}
