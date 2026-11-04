package offsite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/store"
)

// The sync: whatever verified thing exists locally and has no copy in a
// target gets one. Reconcile, never "on event" — a reboot mid-upload, a
// night the NAS was away, a target added on a Tuesday all converge the
// same way. Per target, in order: kits (the object that makes every other
// one usable), app snapshots, the index, retention, lock extension.

// A copy row's kind and state are store.OffsiteKind* and store.Offsite*:
// the store owns the column vocabulary, this package what is done with it.

const (
	NoticeStale      = "offsite-stale"
	NoticeKitStale   = "offsite-kit-stale"
	NoticeSyncFailed = "offsite-sync-failed"
	NoticeCorrupt    = "offsite-local-corrupt"

	syncInterval = 15 * time.Minute
	// Copies go stale on the local dead-man's switch (backup.StaleAfter);
	// kits are written daily, so theirs is two days.
	kitStaleAfter = 48 * time.Hour
	// lockSlack: a retained object whose lock lapses within this is
	// re-locked by the nightly pass.
	lockSlack = 7 * 24 * time.Hour
	// indexKeep is how many index documents stay visible.
	indexKeep = 3
	// housekeepingEvery is how often the index, retention and lock passes
	// run when no copy has moved.
	housekeepingEvery = 24 * time.Hour
	// stalenessEvery is how often the dead-man's switch is re-evaluated
	// when no sync has moved a copy; its windows are a day and more.
	stalenessEvery = time.Hour
	// spoolMaxAge bounds what a crash mid-part leaves in the spool.
	spoolMaxAge = 48 * time.Hour
)

var ErrSyncRunning = errors.New("a sync to this target is already running")

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) jobs() *jobs.Jobs {
	m.ringMu.Lock()
	defer m.ringMu.Unlock()
	if m.Jobs == nil {
		m.Jobs = jobs.New()
	}
	return m.Jobs
}

func (m *Manager) available(path string) bool {
	return m.PathAvailable == nil || m.PathAvailable(path)
}

// holdBackups holds the backup directory; held is false while it is being
// moved, and release is then a no-op.
func (m *Manager) holdBackups() (release func(), held bool) {
	if m.HoldBackupDir == nil {
		return func() {}, true
	}
	release, err := m.HoldBackupDir()
	if err != nil {
		return func() {}, false
	}
	return release, true
}

func (m *Manager) spool() string { return filepath.Join(m.DataDir, "offsite-spool") }

func syncKind(target string) string { return "offsite-sync:" + target }

// SyncLoop runs every target's sync on an interval, one target at a time,
// then the staleness check. The first pass fires shortly after boot.
func (m *Manager) SyncLoop(ctx context.Context) {
	loop.Every(ctx, 30*time.Second, syncInterval, m.syncAll)
}

func (m *Manager) syncAll(ctx context.Context) {
	SweepSpool(m.spool(), spoolMaxAge)
	targets, err := m.Store.ListOffsiteTargets()
	if err != nil {
		return
	}
	moved := false
	for _, t := range targets {
		if t.Role != "active" {
			continue
		}
		before, _ := m.Store.OffsiteCopiesFingerprint(t.ID)
		job, err := m.StartSync(t.Name)
		if err != nil {
			continue // already running: it will finish on its own
		}
		m.jobs().Wait(ctx, job.ID)
		if ctx.Err() != nil {
			return
		}
		if after, _ := m.Store.OffsiteCopiesFingerprint(t.ID); after != before {
			moved = true
		}
	}
	// The alarm keys on copies landing, so a pass that moved one clears it
	// at once; otherwise its day-long windows need no more than an hourly
	// look, and each look reads every app's backups.
	if moved || m.now().Sub(m.stalenessAt) >= stalenessEvery {
		m.stalenessAt = m.now()
		m.checkStaleness()
	}
}

// StartSync runs one target's sync as a job the UI can watch.
func (m *Manager) StartSync(name string) (jobs.Job, error) {
	t, err := m.Store.OffsiteTargetByName(name)
	if err != nil {
		return jobs.Job{}, err
	}
	job, ok := m.jobs().StartExclusive(syncKind(name), "Offsite: "+name, func(ctx context.Context, p *jobs.Progress) error {
		err := m.syncTarget(ctx, t, p)
		if err != nil {
			m.Log.Error("offsite sync", "target", name, "err", err)
			m.Store.SetOffsiteTargetSync(name, false, err.Error())
			m.Store.CreateNotice(NoticeSyncFailed, name, "Offsite sync to "+name+" failed",
				err.Error()+" — fog keeps retrying every "+syncInterval.String()+"; the staleness alarm fires if nothing lands.")
			return err
		}
		m.Store.SetOffsiteTargetSync(name, true, "")
		m.Store.DismissNoticeByKind(NoticeSyncFailed, name)
		m.forgetIndex(name)
		return nil
	})
	if !ok {
		return jobs.Job{}, ErrSyncRunning
	}
	return job, nil
}

// inWindow reports whether now falls inside an "HH:MM-HH:MM" UTC window
// (which may wrap midnight). An empty window is always open.
func inWindow(window string, now time.Time) bool {
	if window == "" {
		return true
	}
	a, b, ok := strings.Cut(window, "-")
	if !ok {
		return true
	}
	parse := func(s string) int {
		var h, mm int
		fmt.Sscanf(s, "%d:%d", &h, &mm)
		return h*60 + mm
	}
	start, end := parse(a), parse(b)
	cur := now.UTC().Hour()*60 + now.UTC().Minute()
	if start <= end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end
}

func (m *Manager) lockOptions(t *store.OffsiteTarget, policy Policy) objstore.PutOptions {
	if t.Grade != "A" {
		return objstore.PutOptions{}
	}
	return objstore.PutOptions{LockMode: "COMPLIANCE", RetainUntil: m.now().Add(time.Duration(policy.LockDays()) * 24 * time.Hour)}
}

func (m *Manager) syncTarget(ctx context.Context, t *store.OffsiteTarget, p *jobs.Progress) error {
	p.Current("opening the locker")
	r, err := m.Reader(ctx, t)
	if err != nil {
		return err
	}
	policy := ReadPolicy(m.Store)
	lock := m.lockOptions(t, policy)
	up := &Uploader{Client: r.Client, Keys: r.Keys, Spool: m.spool(), Limit: NewRateLimiter(t.MaxMbps), Lock: lock,
		Progress: func(n int64) { p.Add(n, 0) }}
	var errs []string
	release, held := m.holdBackups()
	switch {
	case !held:
		p.Current("the backup directory is being moved — uploads wait")
	case inWindow(t.Window, m.now()):
		if err := m.syncKits(ctx, t, r, up, p); err != nil {
			errs = append(errs, err.Error())
		}
		if err := m.syncApps(ctx, t, r, up, p); err != nil {
			errs = append(errs, err.Error())
		}
		if err := m.syncFiles(ctx, t, r, up, p); err != nil {
			errs = append(errs, err.Error())
		}
	default:
		p.Current("outside the upload window " + t.Window + " — uploads wait")
	}
	release()
	errs = append(errs, m.housekeep(ctx, t, r, policy, p)...)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// housekeeping is when a target's index, retention, blob sweep and
// hidden-row pruning last ran, and the copy rows' fingerprint and policy
// they ran against. Kept in a setting so a restart doesn't redo them.
type housekeeping struct {
	At          time.Time `json:"at"`
	Fingerprint string    `json:"fingerprint"`
	Policy      Policy    `json:"policy"`
	LocksAt     time.Time `json:"locksAt"`
}

func housekeepingSetting(target string) string { return "offsite_housekeeping:" + target }

func (m *Manager) readHousekeeping(target string) housekeeping {
	var hk housekeeping
	if raw, _ := m.Store.GetSetting(housekeepingSetting(target)); raw != "" {
		json.Unmarshal([]byte(raw), &hk)
	}
	return hk
}

func (m *Manager) saveHousekeeping(target string, hk housekeeping) {
	if doc, err := json.Marshal(hk); err == nil {
		m.Store.SetSetting(housekeepingSetting(target), string(doc))
	}
}

// housekeep rewrites the index, applies retention, sweeps blobs and prunes
// hidden rows — but only when something they read has moved since they
// last ran (a copy landed or was hidden, the policy changed), or a day has
// passed, since retention and expiry also move with the clock. Most sync
// ticks upload nothing, and without this each one wrote a fresh locked
// index and parsed every Files manifest. Lock extension runs daily on its
// own clock: it looks a week ahead, so a day late costs nothing.
func (m *Manager) housekeep(ctx context.Context, t *store.OffsiteTarget, r *Reader, policy Policy, p *jobs.Progress) []string {
	hk := m.readHousekeeping(t.Name)
	now := m.now()
	fp, err := m.Store.OffsiteCopiesFingerprint(t.ID)
	if err != nil {
		return []string{"housekeeping: " + err.Error()}
	}
	var errs []string
	var stored []store.OffsiteCopy
	if fp != hk.Fingerprint || policy != hk.Policy || now.Sub(hk.At) >= housekeepingEvery {
		stored, err = m.Store.ListOffsiteCopiesSlim(t.ID, "", store.OffsiteStored)
		if err != nil {
			return []string{"housekeeping: " + err.Error()}
		}
		var hkErrs []string
		p.Current("writing the index")
		if err := m.writeIndex(ctx, t, r, stored); err != nil {
			hkErrs = append(hkErrs, "index: "+err.Error())
		}
		p.Current("applying retention")
		stored, err = m.applyRetention(ctx, t, r, policy, stored)
		if err != nil {
			hkErrs = append(hkErrs, "retention: "+err.Error())
		}
		stored, err = m.sweepBlobs(ctx, t, r, stored)
		if err != nil {
			hkErrs = append(hkErrs, "blob sweep: "+err.Error())
		}
		stored, err = m.sweepPacks(ctx, t, r, stored)
		if err != nil {
			hkErrs = append(hkErrs, "pack sweep: "+err.Error())
		}
		if err := m.pruneHidden(t, policy); err != nil {
			hkErrs = append(hkErrs, "pruning hidden copies: "+err.Error())
		}
		// A pass that failed anywhere runs again next tick. The fingerprint
		// is taken after the pass, so its own hides don't trigger another.
		if len(hkErrs) == 0 {
			if fp, err := m.Store.OffsiteCopiesFingerprint(t.ID); err == nil {
				hk.At, hk.Fingerprint, hk.Policy = now, fp, policy
			}
		}
		errs = append(errs, hkErrs...)
	}
	if t.Grade == "A" && now.Sub(hk.LocksAt) >= housekeepingEvery {
		if stored == nil {
			if stored, err = m.Store.ListOffsiteCopiesSlim(t.ID, "", store.OffsiteStored); err != nil {
				return append(errs, "locks: "+err.Error())
			}
		}
		p.Current("extending locks")
		if err := m.extendLocks(ctx, t, r, policy, stored); err != nil {
			errs = append(errs, "locks: "+err.Error())
		} else {
			hk.LocksAt = now
		}
	}
	m.saveHousekeeping(t.Name, hk)
	return errs
}

// copyState turns a copy row into the upload's resume state.
func copyState(c *store.OffsiteCopy) UploadState {
	if c == nil {
		return UploadState{}
	}
	return UploadState{UploadID: c.UploadID, Nonce: decodeNonce(c.Nonce)}
}

// saver records an upload's progress so a later run can resume it, in the
// store and in the phase's copy index.
func (m *Manager) saver(t *store.OffsiteTarget, ix copyIndex, kind, subject, key string) func(*UploadState) error {
	return func(st *UploadState) error {
		c := store.OffsiteCopy{TargetID: t.ID, Kind: kind, Subject: subject, Key: key,
			State: store.OffsiteUploading, UploadID: st.UploadID, Nonce: st.encodeNonce()}
		if err := m.Store.UpsertOffsiteCopy(c); err != nil {
			return err
		}
		ix[subject] = c
		return nil
	}
}

// stored records a finished upload, in the store and in the phase's copy
// index.
func (m *Manager) stored(ix copyIndex, c store.OffsiteCopy) error {
	c.State = store.OffsiteStored
	if err := m.Store.UpsertOffsiteCopy(c); err != nil {
		return err
	}
	ix[c.Subject] = c
	return nil
}

// copyIndex is a target's copies of one kind by subject: read once per
// sync phase, not looked up per item, and kept current as the phase
// uploads.
type copyIndex map[string]store.OffsiteCopy

func (m *Manager) copyIndex(t *store.OffsiteTarget, kind string) (copyIndex, error) {
	rows, err := m.Store.ListOffsiteCopiesSlim(t.ID, kind)
	if err != nil {
		return nil, err
	}
	ix := make(copyIndex, len(rows))
	for _, c := range rows {
		ix[c.Subject] = c
	}
	return ix, nil
}

// done reports whether subject has a copy that is not mid-upload.
func (ix copyIndex) done(subject string) bool {
	c, ok := ix[subject]
	return ok && c.State != store.OffsiteUploading
}

// resume is where an upload of subject picks up.
func (ix copyIndex) resume(subject string) UploadState {
	c, ok := ix[subject]
	if !ok {
		return UploadState{}
	}
	return copyState(&c)
}

func retainUntil(lock objstore.PutOptions) string {
	if lock.RetainUntil.IsZero() {
		return ""
	}
	return lock.RetainUntil.UTC().Format(time.RFC3339)
}

// syncKits copies every recovery kit on disk that the target lacks. Kits
// go up as they are — already sealed under the same passphrase — so a
// fresh machine can open one with nothing but DecryptKit.
func (m *Manager) syncKits(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress) error {
	if m.BackupRoot == nil {
		return nil
	}
	dir := filepath.Join(m.BackupRoot(), backup.KitsDirName)
	if !m.available(dir) {
		return errors.New("the backup directory is on a share that is not available")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no kits yet
	}
	raw := *up
	raw.Keys = nil
	ix, err := m.copyIndex(t, store.OffsiteKindKit)
	if err != nil {
		return err
	}
	var errs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".enc") {
			continue
		}
		if ix.done(name) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.Current("kit " + name)
		key := r.Layout.Kit(name)
		// The digest of what goes up is fog's own record of the kit: kept
		// with the copy, sealed into the index, and what a recovery and the
		// verifier check the bucket's bytes against.
		path := filepath.Join(dir, name)
		sum, err := fsx.FileSHA256(path)
		if err != nil {
			errs = append(errs, "kit "+name+": "+err.Error())
			continue
		}
		state := ix.resume(name)
		res, err := raw.Upload(ctx, key, r.Layout.Name(key), FileSource(path), &state, m.saver(t, ix, store.OffsiteKindKit, name, key))
		if err != nil {
			errs = append(errs, "kit "+name+": "+err.Error())
			continue
		}
		if after, err := fsx.FileSHA256(path); err != nil || after != sum {
			errs = append(errs, "kit "+name+": changed while it was uploaded")
			continue
		}
		summary, _ := json.Marshal(kitSummary{SHA256: sum})
		if err := m.stored(ix, store.OffsiteCopy{TargetID: t.ID, Kind: store.OffsiteKindKit, Subject: name, Key: key, Bytes: res.Bytes,
			Summary: string(summary), UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)}); err != nil {
			return err
		}
		m.recordLatestKit(t, name, sum)
		p.Add(0, 1)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// stampOfBackup is the backup's stamp; see stampOf.
func stampOfBackup(b *store.Backup) string { return stampOf(b.Path, b.CreatedAt, b.ID) }

// stampOf names a backup or Files snapshot offsite: its directory name,
// which fog writes as YYYYMMDD-HHMMSS, or the creation time when the path
// is odd (the rolling Files mirror), or the row id as a last resort.
func stampOf(dir, createdAt string, id int64) string {
	if s := filepath.Base(dir); stampRe.MatchString(s) {
		return s
	}
	if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
		return fsx.Stamp(t)
	}
	return strconv.FormatInt(id, 10)
}

// syncApps copies every recorded backup the target lacks: the snapshot
// directory as one sealed tar, then the row that describes it.
func (m *Manager) syncApps(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress) error {
	apps, err := m.Store.ListApps()
	if err != nil {
		return err
	}
	ix, err := m.copyIndex(t, store.OffsiteKindApp)
	if err != nil {
		return err
	}
	// Every pass walks every backup, almost all of them already copied, so
	// the walk reads refs without the manifest column; the full row is read
	// only for a backup that is actually going up.
	all, err := m.Store.ListAllBackupRefs()
	if err != nil {
		return err
	}
	byApp := map[int64][]store.BackupRef{}
	for _, b := range all {
		byApp[b.AppID] = append(byApp[b.AppID], b)
	}
	var errs []string
	for _, app := range apps {
		for _, ref := range byApp[app.ID] {
			subject := strconv.FormatInt(ref.ID, 10)
			if ix.done(subject) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !m.available(ref.Path) {
				m.Log.Warn("offsite: backup is on an unavailable share; skipped", "app", app.Name, "backup", ref.ID)
				continue
			}
			b, err := m.Store.BackupByID(ref.ID)
			if err != nil {
				continue // the row went with the prune
			}
			if !backup.HasFiles(b) {
				continue // pruned locally between listing and now, or never landed
			}
			if err := m.syncOne(ctx, t, r, up, p, ix, &app, b, subject); err != nil {
				errs = append(errs, app.Name+" "+stampOfBackup(b)+": "+err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (m *Manager) syncOne(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress,
	ix copyIndex, app *store.App, b *store.Backup, subject string) error {
	if m.storeBacked(b) {
		return m.syncOneStore(ctx, t, r, up, p, ix, app, b, subject)
	}
	stamp := stampOfBackup(b)
	p.Current(app.Name + " " + stamp)
	// A delegated artifact is re-hashed on the way out: what leaves the
	// house must be what fog verified at download. A mismatch is local
	// corruption — an alarm, not an upload.
	dir, release := b.Path, func() {}
	if m.BackupFiles != nil {
		var err error
		if dir, release, err = m.BackupFiles(ctx, b); err != nil {
			return err
		}
	}
	defer release()
	if b.Format == backup.ArtifactFormat && b.SHA256 != "" {
		sum, err := fsx.FileSHA256(filepath.Join(dir, "artifact.tar"))
		if err != nil {
			return fmt.Errorf("reading artifact: %w", err)
		}
		if !strings.EqualFold(sum, b.SHA256) {
			m.Store.CreateNotice(NoticeCorrupt, app.Name, app.Name+"'s backup "+stamp+" has changed on disk",
				"Its artifact no longer matches the digest fog verified when it was taken — storage corruption. It was not copied offsite; take a fresh backup and check the disk or share it lives on.")
			return errors.New("artifact changed on disk since it was verified — not copied")
		}
	}
	src, names, err := DirSource(dir)
	if err != nil {
		return err
	}
	p.Total(src.Size)
	key := r.Layout.AppSnapshot(app.Name, stamp)
	state := ix.resume(subject)
	res, err := up.Upload(ctx, key, r.Layout.Name(key), src, &state, m.saver(t, ix, store.OffsiteKindApp, subject, key))
	if err != nil {
		return err
	}
	mode := apps.BackupModeVolumes
	if b.Format == backup.ArtifactFormat {
		mode = apps.BackupModeDelegated
	}
	row := AppRow{Format: Format, App: app.Name, Stamp: stamp, Kind: b.Kind, Mode: mode, CreatedAt: b.CreatedAt,
		SizeBytes: b.SizeBytes, SnapshotSHA256: res.SHA256, ArtifactFormat: b.Format, ArtifactSHA256: b.SHA256,
		Manifest: b.Manifest, VerifiedAt: b.VerifiedAt, Files: names, SourceBackupID: b.ID}
	row.Identity = backup.ManifestApp(b.Manifest)
	doc, err := json.Marshal(row)
	if err != nil {
		return err
	}
	rowKey := r.Layout.AppRow(app.Name, stamp)
	sealed, err := r.Keys.SealBytes(r.Layout.Name(rowKey), doc)
	if err != nil {
		return err
	}
	if _, err := r.Client.PutBytes(ctx, rowKey, sealed, withContentType(up.Lock, "application/octet-stream")); err != nil {
		return fmt.Errorf("writing the row: %w", err)
	}
	if err := m.stored(ix, store.OffsiteCopy{TargetID: t.ID, Kind: store.OffsiteKindApp, Subject: subject, Key: key,
		Bytes: res.Bytes + int64(len(sealed)), Doc: string(doc),
		UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)}); err != nil {
		return err
	}
	p.Add(0, 1)
	m.Log.Info("offsite: backup copied", "target", t.Name, "app", app.Name, "stamp", stamp, "bytes", res.Bytes)
	return nil
}

func withContentType(lock objstore.PutOptions, ct string) objstore.PutOptions {
	lock.ContentType = ct
	return lock
}

// writeIndex rewrites the rollup from the stored copy rows and trims old
// ones. The index is written without an object lock: it is a cache of
// what the locked rows and snapshots already say, rebuilt on every
// housekeeping pass, and locking each one kept a month of them alive.
func (m *Manager) writeIndex(ctx context.Context, t *store.OffsiteTarget, r *Reader, stored []store.OffsiteCopy) error {
	// App rows carry their documents into the index; kits and Files
	// snapshots need only the slim rows and their summaries.
	appCopies, err := m.Store.ListOffsiteCopies(t.ID, store.OffsiteKindApp, store.OffsiteStored)
	if err != nil {
		return err
	}
	ix := Index{Format: Format, LockerID: t.LockerID, Fog: m.serverName(), WrittenAt: m.now().UTC().Format(time.RFC3339),
		Kits: []IndexKit{}, Apps: map[string][]AppRow{}, Files: []IndexFiles{}}
	for _, c := range appCopies {
		var row AppRow
		if json.Unmarshal([]byte(c.Doc), &row) == nil && row.App != "" {
			ix.Apps[row.App] = append(ix.Apps[row.App], row)
		}
	}
	for _, c := range stored {
		switch c.Kind {
		case store.OffsiteKindKit:
			k := IndexKit{Name: c.Subject, Bytes: c.Bytes, Stamp: StampOf(c.Subject), SHA256: kitDigestOf(&c)}
			ix.Kits = append(ix.Kits, k)
			if k.SHA256 != "" && (ix.LatestKit == nil || k.Name > ix.LatestKit.Name) {
				latest := k
				ix.LatestKit = &latest
			}
		case store.OffsiteKindFilesSnapshot:
			if f, ok := filesSummary(&c); ok {
				ix.Files = append(ix.Files, f)
			}
		}
	}
	m.inherit(ctx, r, &ix)
	sort.Slice(ix.Kits, func(i, j int) bool { return ix.Kits[i].Name > ix.Kits[j].Name })
	for app := range ix.Apps {
		rows := ix.Apps[app]
		sort.Slice(rows, func(i, j int) bool { return rows[i].Stamp > rows[j].Stamp })
		// The index carries the rows without their manifests: a restore
		// wizard wants the list, and the manifest is in the row object.
		for i := range rows {
			rows[i].Manifest = ""
		}
	}
	sort.Slice(ix.Files, func(i, j int) bool { return ix.Files[i].Stamp > ix.Files[j].Stamp })
	doc, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	stamp := fsx.Stamp(m.now())
	key := r.Layout.Index(stamp)
	sealed, err := r.Keys.SealBytes(r.Layout.Name(key), doc)
	if err != nil {
		return err
	}
	if _, err := r.Client.PutBytes(ctx, key, sealed, objstore.PutOptions{}); err != nil {
		return err
	}
	// Recorded here, so this fog's own reads prefer it (Reader.LatestIndex)
	// and notice when it has gone.
	m.Store.SetSetting(latestIndexSetting(t), key)
	m.forgetIndex(t.Name)
	// Older indexes are hidden past the newest few; they are caches. Only
	// fog's own kind of object is ever deleted: a well-formed index name
	// older than the one just written. Anything else under index/ — junk,
	// or a well-formed name newer than ours that fog did not write — is
	// left alone and does not count toward the few kept, so planting
	// objects there cannot make fog delete its real indexes (which would
	// leave recovery with none). It is reported instead.
	var older, junk []string
	if err := r.Client.List(ctx, r.Layout.IndexPrefix(), func(o objstore.Object) error {
		name := strings.TrimPrefix(o.Key, r.Layout.IndexPrefix())
		switch {
		case o.Key == key:
		case StampOf(name) == "" || !strings.HasSuffix(name, ".json") || o.Key > key:
			junk = append(junk, name)
		default:
			older = append(older, o.Key)
		}
		return nil
	}); err != nil {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(older)))
	for i, k := range older {
		if i >= indexKeep-1 {
			r.Client.Delete(ctx, k)
		}
	}
	m.noteIndexJunk(t, junk)
	return nil
}

// inherit adds to the index what the locker holds but this fog did not
// write: a locker adopted after a rebuild carries the old box's snapshots,
// rows and kits, and an index built from this fog's own copy records alone
// hid every one of them — the restore wizard came up empty with the
// backups sitting right there. The bucket listing is the truth; a line the
// previous index already had is reused, so only a document new to the
// index is read. Anything unreadable is left out, never fatal: the index is
// a cache, and this fog's own lines are already in it.
func (m *Manager) inherit(ctx context.Context, r *Reader, ix *Index) {
	var prev Index
	if p, err := r.Index(ctx); err == nil {
		prev = *p
	}
	known := map[string]bool{}
	for app, rows := range ix.Apps {
		for _, row := range rows {
			known[app+"/"+row.Stamp] = true
		}
	}
	prevRows := map[string]AppRow{}
	for app, rows := range prev.Apps {
		for _, row := range rows {
			prevRows[app+"/"+row.Stamp] = row
		}
	}
	objects := map[string]bool{}
	if err := r.Client.List(ctx, r.Layout.AppsPrefix(), func(o objstore.Object) error {
		objects[o.Key] = true
		return nil
	}); err != nil {
		m.Log.Warn("offsite: index lists only this fog's copies", "err", err)
		return
	}
	for key := range objects {
		rel, ok := strings.CutSuffix(strings.TrimPrefix(key, r.Layout.AppsPrefix()), ".json")
		app, stamp, found := strings.Cut(rel, "/")
		if !ok || !found || known[rel] || !objects[r.Layout.AppSnapshot(app, stamp)] {
			continue
		}
		row, ok := prevRows[rel]
		if !ok {
			if err := r.Document(ctx, key, &row); err != nil {
				m.Log.Warn("offsite: row left out of the index", "key", key, "err", err)
				continue
			}
		}
		ix.Apps[app] = append(ix.Apps[app], row)
	}

	// Kits this fog did not upload: those the previous index recorded keep
	// the digest it recorded (an earlier fog's own uploads); one seen only
	// in the bucket listing is listed without one, and a recovery prefers
	// every recorded kit to it.
	haveKit := map[string]bool{}
	for _, k := range ix.Kits {
		haveKit[k.Name] = true
	}
	prevKits := map[string]IndexKit{}
	for _, k := range prev.Kits {
		prevKits[k.Name] = k
	}
	if kits, err := r.Kits(ctx); err == nil {
		for _, k := range kits {
			if haveKit[k.Name] {
				continue
			}
			if pk, ok := prevKits[k.Name]; ok && pk.SHA256 != "" {
				k.SHA256 = pk.SHA256
			}
			ix.Kits = append(ix.Kits, k)
		}
	}
	if ix.LatestKit == nil && prev.LatestKit != nil {
		ix.LatestKit = prev.LatestKit
	}

	haveFiles := map[string]bool{}
	for _, f := range ix.Files {
		haveFiles[f.Stamp] = true
	}
	prevFiles := map[string]IndexFiles{}
	for _, f := range prev.Files {
		prevFiles[f.Stamp] = f
	}
	stamps, err := r.FilesSnapshots(ctx)
	if err != nil {
		return
	}
	for _, stamp := range stamps {
		if haveFiles[stamp] {
			continue
		}
		line, ok := prevFiles[stamp]
		if !ok {
			var snap FilesSnapshot
			if err := r.Document(ctx, r.Layout.FilesSnapshot(stamp), &snap); err != nil {
				m.Log.Warn("offsite: Files snapshot left out of the index", "stamp", stamp, "err", err)
				continue
			}
			line = filesIndexLine(&snap)
		}
		ix.Files = append(ix.Files, line)
	}
}

// filesSummary is a stored Files snapshot's line in the index, from its
// summary (migration 0047 filled it for rows written before it was kept).
func filesSummary(c *store.OffsiteCopy) (IndexFiles, bool) {
	var f IndexFiles
	return f, json.Unmarshal([]byte(c.Summary), &f) == nil && f.Stamp != ""
}

func filesIndexLine(snap *FilesSnapshot) IndexFiles {
	return IndexFiles{Stamp: snap.Stamp, CreatedAt: snap.CreatedAt, Files: snap.Count, Bytes: snap.Bytes}
}

// seriesOf names the retention series a copy belongs to and its time.
func seriesOf(layout Layout, c *store.OffsiteCopy) (series string, at time.Time, ok bool) {
	name := layout.Name(c.Key)
	switch c.Kind {
	case store.OffsiteKindKit:
		t, ok := StampTime(StampOf(c.Subject))
		return "kits", t, ok
	case store.OffsiteKindApp:
		parts := strings.Split(name, "/")
		if len(parts) != 3 {
			return "", time.Time{}, false
		}
		t, ok := StampTime(StampOf(parts[2]))
		return "app:" + parts[1], t, ok
	case store.OffsiteKindFilesSnapshot:
		t, ok := StampTime(StampOf(name))
		return "files", t, ok
	}
	return "", time.Time{}, false
}

// applyRetention hides what the policy no longer keeps. Hiding is a
// delete marker; the provider's lifecycle rule frees the space later, and
// the operator can unhide from the console until it does. It returns the
// stored copies still stored afterwards.
func (m *Manager) applyRetention(ctx context.Context, t *store.OffsiteTarget, r *Reader, policy Policy, stored []store.OffsiteCopy) ([]store.OffsiteCopy, error) {
	series := map[string][]Item{}
	byKey := map[string]store.OffsiteCopy{}
	for _, c := range stored {
		s, at, ok := seriesOf(r.Layout, &c)
		if !ok {
			continue
		}
		series[s] = append(series[s], Item{Name: c.Key, At: at})
		byKey[c.Key] = c
	}
	now := m.now()
	var errs []string
	hidden := map[int64]bool{}
	for _, items := range series {
		keep := policy.Retain(items, now)
		for _, it := range items {
			if keep[it.Name] {
				continue
			}
			c := byKey[it.Name]
			// A copy that failed to hide whole stays stored, so the next
			// pass tries every one of its objects again.
			failed := false
			for _, key := range r.Layout.ObjectsOf(&c) {
				if err := r.Client.Delete(ctx, key); err != nil {
					errs = append(errs, err.Error())
					failed = true
				}
			}
			if failed {
				continue
			}
			m.Store.SetOffsiteCopyState(c.ID, store.OffsiteHidden)
			hidden[c.ID] = true
			m.Log.Info("offsite: hidden by retention", "target", t.Name, "key", r.Layout.Name(c.Key))
		}
	}
	kept := withoutHidden(stored, hidden)
	if len(errs) > 0 {
		return kept, errors.New(strings.Join(errs, "; "))
	}
	return kept, nil
}

// withoutHidden is copies less the ones a pass just hid.
func withoutHidden(copies []store.OffsiteCopy, hidden map[int64]bool) []store.OffsiteCopy {
	if len(hidden) == 0 {
		return copies
	}
	kept := make([]store.OffsiteCopy, 0, len(copies)-len(hidden))
	for _, c := range copies {
		if !hidden[c.ID] {
			kept = append(kept, c)
		}
	}
	return kept
}

// extendLocks re-locks retained objects whose lock is about to lapse, and
// locks objects written before the target reached grade A. Blobs are
// included: the sweep has already hidden the unreferenced ones, so every
// stored blob is one a retained snapshot names.
func (m *Manager) extendLocks(ctx context.Context, t *store.OffsiteTarget, r *Reader, policy Policy, stored []store.OffsiteCopy) error {
	now := m.now()
	until := now.Add(time.Duration(policy.LockDays()) * 24 * time.Hour)
	var errs []string
	for _, c := range stored {
		if c.RetainUntil != "" {
			if cur, err := time.Parse(time.RFC3339, c.RetainUntil); err == nil && cur.After(now.Add(lockSlack)) {
				continue
			}
		}
		// The new lock is recorded only once every object of the copy
		// holds it; otherwise the next pass extends them all again.
		failed := false
		for _, key := range r.Layout.ObjectsOf(&c) {
			if err := r.Client.PutRetention(ctx, key, "", "COMPLIANCE", until); err != nil {
				errs = append(errs, err.Error())
				failed = true
			}
		}
		if failed {
			continue
		}
		m.Store.SetOffsiteCopyRetention(c.ID, until.UTC().Format(time.RFC3339))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// hiddenPruneSlack is how long past the bucket's hidden-file expiry a
// hidden copy's row is kept, for providers whose rule is set a little
// longer than fog's.
const hiddenPruneSlack = 7 * 24 * time.Hour

// pruneHidden forgets hidden copies the provider has had time to expire:
// hidden longer than the hidden-file expiry fog gives a bucket (plus
// slack), no longer locked, and not something the next sync would upload
// again — a hidden row is also what keeps a local backup that retention
// dropped from going back up, so a kit, backup or Files snapshot still on
// disk keeps its row.
func (m *Manager) pruneHidden(t *store.OffsiteTarget, policy Policy) error {
	hidden, err := m.Store.ListOffsiteCopiesSlim(t.ID, "", store.OffsiteHidden)
	if err != nil {
		return err
	}
	now := m.now()
	window := time.Duration(B2HiddenDays(policy))*24*time.Hour + hiddenPruneSlack
	for _, c := range hidden {
		at, err := time.Parse(time.RFC3339, c.HiddenAt)
		if err != nil || now.Sub(at) < window {
			continue
		}
		if c.RetainUntil != "" {
			if until, err := time.Parse(time.RFC3339, c.RetainUntil); err != nil || until.After(now) {
				continue
			}
		}
		if !m.goneLocally(&c) {
			continue
		}
		if err := m.Store.DeleteOffsiteCopy(c.ID); err != nil {
			return err
		}
	}
	return nil
}

// goneLocally reports whether a copy's subject has left this fog, so a
// sync would not upload it again. When in doubt, it has not.
func (m *Manager) goneLocally(c *store.OffsiteCopy) bool {
	switch c.Kind {
	case store.OffsiteKindBlob, kindPack, kindStore:
		// Blobs and the store's pieces are uploaded from what a snapshot
		// needs, not from a row.
		return true
	case store.OffsiteKindKit:
		if m.BackupRoot == nil {
			return false
		}
		dir := filepath.Join(m.BackupRoot(), backup.KitsDirName)
		if !m.available(dir) {
			return false
		}
		_, err := os.Stat(filepath.Join(dir, c.Subject))
		return errors.Is(err, os.ErrNotExist)
	case store.OffsiteKindApp:
		id, err := strconv.ParseInt(c.Subject, 10, 64)
		if err != nil {
			return false
		}
		_, err = m.Store.BackupByID(id)
		return errors.Is(err, store.ErrNotFound)
	case store.OffsiteKindFilesSnapshot:
		id, err := strconv.ParseInt(c.Subject, 10, 64)
		if err != nil {
			return false
		}
		_, err = m.Store.FileSnapshotByID(id)
		return errors.Is(err, store.ErrNotFound)
	}
	return false
}

// ---- the dead-man's switch ----

// checkStaleness raises (and clears) offsite-stale per app and
// offsite-kit-stale: silence is the failure mode this exists to catch, so
// the alarm keys on absence of a recent successful copy, not on failure
// events. An app whose local backups are themselves stale is left to the
// local alarm.
func (m *Manager) checkStaleness() {
	targets, err := m.Store.ListOffsiteTargets()
	if err != nil {
		return
	}
	var active []store.OffsiteTarget
	for _, t := range targets {
		if t.Role == "active" {
			active = append(active, t)
		}
	}
	now := m.now()
	fleet, err := backup.FleetStatus(m.Store, now)
	if err != nil {
		return
	}
	// The newest stored copy per (target, series). Only app backups and
	// kits are watched here, so only those rows are loaded — not the
	// blobs, one per Drive file.
	newest := map[int64]map[string]time.Time{}
	for _, t := range active {
		newest[t.ID] = map[string]time.Time{}
		copies, err := m.Store.ListOffsiteCopiesOfKinds(t.ID, []string{store.OffsiteKindApp, store.OffsiteKindKit}, store.OffsiteStored)
		if err != nil {
			continue
		}
		for _, c := range copies {
			s, at, ok := seriesOf(LayoutOf(&t), &c)
			if ok && at.After(newest[t.ID][s]) {
				newest[t.ID][s] = at
			}
		}
	}
	graced := func(t store.OffsiteTarget, window time.Duration) bool {
		created, err := time.Parse(time.RFC3339, t.CreatedAt)
		return err == nil && now.Sub(created) < window
	}
	for _, row := range fleet {
		expected := row.Enabled && row.HourUTC != nil && row.LastAt != "" && !row.Stale && len(active) > 0
		var late []string
		if expected {
			for _, t := range active {
				if graced(t, backup.StaleAfter) {
					continue
				}
				at := newest[t.ID]["app:"+row.App]
				if at.IsZero() || now.Sub(at) > backup.StaleAfter {
					late = append(late, t.Name)
				}
			}
		}
		if len(late) == 0 {
			m.Store.DismissNoticeByKind(NoticeStale, row.App)
			continue
		}
		m.Store.CreateNotice(NoticeStale, row.App, row.App+" has no recent offsite copy",
			fmt.Sprintf("Its backups are landing locally, but nothing has reached %s in over a day. Check the target on Settings → Storage → Offsite.", strings.Join(late, ", ")))
		if m.shouldAlert(NoticeStale + ":" + row.App) {
			m.notify("Offsite copy overdue: "+row.App, "nothing has reached "+strings.Join(late, ", ")+" in over a day", false)
		}
	}
	// Kits: expected once fog writes them at all.
	hour, _ := m.Store.GetSetting(backup.SettingBackupHourUTC)
	pass, _ := m.passphrase()
	var lateKits []string
	if hour != "" && pass != "" {
		for _, t := range active {
			if graced(t, kitStaleAfter) {
				continue
			}
			at := newest[t.ID]["kits"]
			if at.IsZero() || now.Sub(at) > kitStaleAfter {
				lateKits = append(lateKits, t.Name)
			}
		}
	}
	if len(lateKits) == 0 {
		m.Store.DismissNoticeByKind(NoticeKitStale, "")
	} else {
		m.Store.CreateNotice(NoticeKitStale, "", "The recovery kit has no recent offsite copy",
			"No kit newer than two days has reached "+strings.Join(lateKits, ", ")+". Without a recent kit in the locker, a restore from the cloud comes back with stale identity and secrets.")
		if m.shouldAlert(NoticeKitStale) {
			m.notify("Offsite kit overdue", "no recent kit has reached "+strings.Join(lateKits, ", "), false)
		}
	}
}

func (m *Manager) notify(title, msg string, ok bool) {
	if m.Notify != nil {
		m.Notify(title, msg, ok)
	}
}

// shouldAlert rate-limits ntfy to once a day per alarm key.
func (m *Manager) shouldAlert(key string) bool {
	return m.alerts.Allow(key, 24*time.Hour, m.now())
}

// kitSummary is a kit copy's Summary: the digest of its sealed bytes.
type kitSummary struct {
	SHA256 string `json:"sha256"`
}

// kitDigestOf is a kit copy's recorded digest, "" for one recorded before
// fog kept them.
func kitDigestOf(c *store.OffsiteCopy) string {
	var ks kitSummary
	if c.Summary == "" || json.Unmarshal([]byte(c.Summary), &ks) != nil {
		return ""
	}
	return ks.SHA256
}

func latestIndexSetting(t *store.OffsiteTarget) string {
	return fmt.Sprintf("offsite_latest_index_%d", t.ID)
}

func latestKitSetting(t *store.OffsiteTarget) string {
	return fmt.Sprintf("offsite_latest_kit_%d", t.ID)
}

// LatestKitRecord is fog's local record of the newest kit it uploaded to a
// target: what the verifier expects a recovery to find.
type LatestKitRecord struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// recordLatestKit keeps the newest kit this fog uploaded to t.
func (m *Manager) recordLatestKit(t *store.OffsiteTarget, name, sum string) {
	if cur, ok := m.latestKit(t); ok && cur.Name >= name {
		return
	}
	doc, _ := json.Marshal(LatestKitRecord{Name: name, SHA256: sum})
	m.Store.SetSetting(latestKitSetting(t), string(doc))
}

// latestKit is the newest kit this fog recorded uploading to t.
func (m *Manager) latestKit(t *store.OffsiteTarget) (LatestKitRecord, bool) {
	raw, _ := m.Store.GetSetting(latestKitSetting(t))
	var rec LatestKitRecord
	if raw == "" || json.Unmarshal([]byte(raw), &rec) != nil || rec.Name == "" {
		return LatestKitRecord{}, false
	}
	return rec, true
}

// NoticeIndexJunk is raised while a target's index/ prefix holds objects
// fog did not write.
const NoticeIndexJunk = "offsite-index-junk"

// noteIndexJunk raises or clears NoticeIndexJunk for t.
func (m *Manager) noteIndexJunk(t *store.OffsiteTarget, junk []string) {
	if len(junk) == 0 {
		m.Store.DismissNoticeByKind(NoticeIndexJunk, t.Name)
		return
	}
	shown := junk[:min(len(junk), 5)]
	m.Store.CreateNotice(NoticeIndexJunk, t.Name, "Unexpected objects in "+t.Name+"'s index folder",
		fmt.Sprintf("The locker's index/ folder holds %d object(s) fog did not write (%s). Someone with write access to the bucket put them "+
			"there. Fog leaves them alone and keeps its own indexes, but check the bucket.", len(junk), strings.Join(shown, ", ")))
}
