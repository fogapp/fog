// Package backup snapshots and restores app state: Fog-managed volumes as
// tar.gz plus pg_dump of managed databases, staged under a backup directory
// (local by default; point it at a NAS mount in settings). Snapshots capture
// DB and files together so blobs and rows stay consistent: a legacy snapshot
// freezes the app's containers (compose pause — SIGSTOP, not a restart) while
// the volumes are tarred and the databases dumped, so both describe the same
// instant instead of a torn read of files mid-write.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/lucid"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

const (
	SettingBackupDir       = "backup_dir"       // default <datadir>/backups
	SettingBackupRetention = "backup_retention" // per app, default 7
	SettingBackupHourUTC   = "backup_hour_utc"  // "" = scheduled backups off
	// SettingBackupBeforeUpdate is the fleet default for snapshotting an
	// app before an update or a rollback: "" | "on" | "off", empty means
	// on. An app's backup.beforeUpdate overrides it.
	SettingBackupBeforeUpdate = "backup_before_update"
	// SettingBackupMaxBytes is the fleet-wide cap (in bytes) on a delegated
	// artifact download; an app's spec.backup.maxBytes overrides it, and both
	// fall back to DefaultMaxArtifactBytes.
	SettingBackupMaxBytes = "backup_max_artifact_bytes"
)

// SnapshotsBeforeUpdate reports whether an update or a rollback of an app
// with this spec starts with a snapshot: never for an app whose backups
// are off, otherwise the app's own choice, falling back to the fleet
// default (the backup_before_update setting's value, passed as global).
func SnapshotsBeforeUpdate(spec *apps.AppSpec, global string) bool {
	if !spec.BackupsEnabled() {
		return false
	}
	if spec.Backup.BeforeUpdate != nil {
		return *spec.Backup.BeforeUpdate
	}
	return global != "off"
}

type Manager struct {
	DataDir string
	Store   *store.Store
	PG      *pg.Manager
	Mat     *apps.Materializer
	Log     *slog.Logger
	Notify  func(title, msg string, ok bool)
	// Vault seals the recovery-kit passphrase at rest (KitPassphrase); nil in
	// tests falls back to a plaintext setting.
	Vault vault.Cipher
	// Engine supplies the per-app lock and the live-operation tracker.
	// Snapshotting reads the very files a converge rewrites, and a restore
	// replaces them wholesale, so both must take the same exclusion a deploy
	// does rather than racing it.
	Engine *apps.Engine
	// startSeam stands in for the engine's bring-up in tests of what a
	// restore does around it (restore_legacy_test.go).
	startSeam func(ctx context.Context, name string) error

	attempts    notify.Throttle // scheduler retries of a failing app, per app
	staleAlerts notify.Throttle // staleness ntfy pushes, per app

	restores sync.Map // app name -> *restoreOp, for CancelRestore

	// verifyMu runs one test-restore at a time. The scheduled verifier and
	// the offsite verifier both restore into fog_verify_<app>, and each
	// begins by dropping it — terminating the other's pg_restore mid-load,
	// which then fails every remaining statement ("no connection to the
	// server", errors ignored on restore: 117).
	verifyMu sync.Mutex

	// dirGate is held shared by work in the backup directory and
	// exclusively by a move of it; moving is set while a move waits for or
	// holds it (dirgate.go).
	dirGate sync.RWMutex
	moving  atomic.Bool
}

// composeTimeout bounds the stop/up bracketing a restore.
const composeTimeout = 5 * time.Minute

// mountedPoints and unresponsive are the nas hooks behind the share guards —
// vars so tests can simulate a mounted-but-dead share on a dev machine.
var (
	mountedPoints = nas.MountedPoints
	unresponsive  = nas.Unresponsive
)

func (m *Manager) baseDir() string {
	if d, _ := m.Store.GetSetting(SettingBackupDir); d != "" {
		return d
	}
	return filepath.Join(m.DataDir, "backups")
}

// Root exposes the backups base directory (orphan scanning needs it).
func (m *Manager) Root() string { return m.baseDir() }

// The backups root holds one directory per app plus these, which belong to
// fog itself. Everything that writes or scans the root names them through
// these constants, and the app name validator reserves them, so an app can
// never share a directory with them.
const (
	// KitsDirName holds the encrypted recovery kits.
	KitsDirName = "fog-recovery"
	// FilesDirName holds the Files snapshots.
	FilesDirName = "files"
)

// ReservedDirs lists the backups-root directories that are not an app's.
var ReservedDirs = []string{KitsDirName, FilesDirName}

// IsReservedDir reports whether name, a directory directly under the
// backups root, belongs to fog rather than to an app.
func IsReservedDir(name string) bool {
	return slices.Contains(ReservedDirs, name)
}

// checkTarget refuses a backup directory that is supposed to be a NAS mount
// but currently isn't. Without this, MkdirAll happily recreates the path on
// the local disk and every snapshot — including the recovery kit, the one
// artifact meant to survive this machine — lands on the machine itself,
// silently, until the share mounts again and shadows them. Failing loudly
// here is what keeps the dead-man's-switch honest: no backup happens, the
// staleness alarm fires, the operator learns the NAS is gone.
func (m *Manager) checkTarget() error {
	dir := m.baseDir()
	sh, reason := m.shareState(dir)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("the backup directory %s is on the NAS share %q, which is %s — refusing to write to the local disk in its place. Fix the share in Settings → Storage → Disks & shares", dir, sh, reason)
}

// shareUnder reports whether path sits under a stored share's mount point
// while that share is unusable. name is the share's name when so.
func (m *Manager) shareUnder(path string) (name string, unavailable bool) {
	name, reason := m.shareState(path)
	return name, reason != ""
}

// PathAvailable is shareUnder for other subsystems (offsite copies read
// the backup directory too): false when the path is on a share that is
// away or not answering, and while the backup directory is being moved.
func (m *Manager) PathAvailable(path string) bool {
	if m.Moving() {
		return false
	}
	_, unavailable := m.shareUnder(path)
	return !unavailable
}

// shareState is shareUnder with the why: reason is "" for a usable share (or
// no share at all), otherwise a short phrase for error messages. A share
// counts as unusable both when it is absent from the mount table and when it
// is mounted but no longer answering — a hard mount whose server died. The
// second case matters as much as the first: any file I/O against it (tar,
// a verify hash, even MkdirAll) blocks in uninterruptible sleep, and the
// loop that walked in — ScheduleLoop, VerifyLoop, the dead-man's-switch
// itself — never comes back. Failing loudly here is the only safe answer.
func (m *Manager) shareState(path string) (name, reason string) {
	shares, err := m.Store.ListNasShares()
	if err != nil || len(shares) == 0 {
		return "", "" // can't tell; never block backups on a store hiccup
	}
	return nas.ShareState(shares, path, mountedPoints(), unresponsive)
}

// RemoveAppBackups deletes every snapshot for an app — the file-side
// counterpart of the DB cascade that removes its backup rows.
func (m *Manager) RemoveAppBackups(appName string) error {
	release, err := m.HoldDir()
	if err != nil {
		return err
	}
	defer release()
	return os.RemoveAll(filepath.Join(m.baseDir(), appName))
}

// snapshotDir is one snapshot's directory while it is being written. It
// is removed by cleanup unless commit was called first, so every way a
// snapshot can fail — including recording its row — leaves nothing behind
// that a later scan would take for a snapshot nobody knows about.
type snapshotDir struct {
	path string
	kept bool
}

// commit keeps the directory: its row is recorded.
func (d *snapshotDir) commit() { d.kept = true }

// cleanup removes the directory unless it was committed; defer it.
func (d *snapshotDir) cleanup() {
	if !d.kept {
		os.RemoveAll(d.path)
	}
}

// newSnapshotDir makes a fresh directory for a snapshot of appName, named
// by the time. A second snapshot in the same second gets a "-N" suffix
// rather than writing into the first one's directory.
func (m *Manager) newSnapshotDir(appName string) (*snapshotDir, error) {
	dir, err := fsx.MkdirStamped(filepath.Join(m.baseDir(), appName), time.Now(), 0o700)
	if err != nil {
		return nil, err
	}
	return &snapshotDir{path: dir}, nil
}

type Meta struct {
	App       string   `json:"app"`
	Rev       int64    `json:"rev"`
	Volumes   []string `json:"volumes"`
	Databases []string `json:"databases"`
	CreatedAt string   `json:"createdAt"`
}

// BackupApp snapshots one app. Returns the backup record ID.
//
// Runs under the app's lock so a deploy can't rewrite volumes mid-tar, and
// joins any enclosing operation (a pre-update snapshot belongs to its update's
// progress) so the UI shows one continuous busy state rather than two.
func (m *Manager) BackupApp(ctx context.Context, appName, kind string) (_ int64, err error) {
	ctx, txn := lucid.Start(ctx, "job", "backup")
	txn.Attr("app", appName)
	txn.Attr("kind", kind)
	defer func() { txn.End(err) }()
	release, err := m.HoldDir()
	if err != nil {
		return 0, err
	}
	defer release()
	if m.Engine == nil {
		return m.backupApp(ctx, appName, kind)
	}
	done := m.Engine.JoinOp(appName, "taking a snapshot")
	defer done()
	var id int64
	err = m.Engine.WithLock(appName, func() error {
		var err error
		id, err = m.backupApp(ctx, appName, kind)
		return err
	})
	return id, err
}

func (m *Manager) backupApp(ctx context.Context, appName, kind string) (int64, error) {
	if err := m.checkTarget(); err != nil {
		return 0, err
	}
	app, err := m.Store.AppByName(appName)
	if err != nil {
		return 0, err
	}
	spec, err := apps.LatestSpec(m.Store, app.ID)
	if err != nil {
		return 0, err
	}
	// Delegated mode (fog-backup/1): the app produces one artifact and fog
	// verifies it. The legacy path below stays for apps without the contract.
	if spec.DelegatedBackups() {
		return m.delegatedBackup(ctx, app, spec, kind)
	}
	// The delegated path above bounds itself; this legacy path must too. Its
	// callers include ScheduleLoop, whose ctx is fogd's lifetime — under
	// that, a tar against a backup dir on a hard mount whose server died
	// would block forever, and runDue is synchronous, so one wedged app
	// would halt every other app's nightly backup, the staleness
	// bookkeeping, and the recovery kit, while its held lock blocks deploys.
	// Same knob as the delegated default: spec.backup.timeout, or 30m.
	ctx, cancel := context.WithTimeout(ctx, delegatedTimeout(spec))
	defer cancel()

	rev, err := m.Store.LatestRevision(app.ID)
	if err != nil {
		return 0, err
	}

	snap, err := m.newSnapshotDir(appName)
	if err != nil {
		return 0, err
	}
	defer snap.cleanup()
	dir := snap.path
	meta := Meta{App: appName, Rev: rev.Rev, CreatedAt: time.Now().UTC().Format(time.RFC3339)}

	// Everything the app owns: its managed volumes and its own folder in
	// Drive (Apps/<App>). Aux sidecar volumes — an app's own database,
	// broker state — are part of the app and included. What is left out is
	// what the app does not own: a content library several apps read, a
	// folder belonging to another app, a host path. Those are shared pools
	// — media libraries — covered by the nightly Drive snapshot instead.
	//
	// A volume's root is checked by a no-follow walk from its trusted anchor,
	// not stat'ed by path: an app with nested volumes can swap a volume root
	// (or a directory above it) for a link to another app's volume, and
	// tarring through it would hand that app's data to this one in its own
	// backup. A missing volume is skipped; a linked one fails the backup.
	var tarVols []apps.Volume
	for _, v := range spec.AllVolumes() {
		loc, ok := m.volumeLoc(appName, v)
		if !ok {
			continue
		}
		exists, err := checkVolumeDir(loc)
		if err != nil {
			return 0, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if exists {
			tarVols = append(tarVols, v)
		}
	}

	// Freeze the app while its files are read. tar over live, changing files
	// is at best torn and at worst a hard failure (GNU tar exits 1 on "file
	// changed as we read it" — a busy app would fail every night), and the
	// package doc's promise — blobs and rows captured together — only holds
	// if writes stop while both the volume tars and the pg_dump run. pause
	// (SIGSTOP) rather than stop/start: connections survive, restart
	// policies don't fire, unpause is instant.
	resume := func() {}
	if len(tarVols) > 0 {
		var err error
		if resume, err = m.pauseForSnapshot(ctx, appName); err != nil {
			return 0, err
		}
	}
	// resume is idempotent; the deferred call covers every error return and
	// the explicit one below lifts the freeze before slower bookkeeping.
	defer resume()

	for _, v := range tarVols {
		// Again now the app is frozen: nothing can swap a link in between this
		// check and the tar.
		loc, _ := m.volumeLoc(appName, v)
		if _, err := checkVolumeDir(loc); err != nil {
			return 0, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		src := m.volumePath(appName, v)
		tarPath := filepath.Join(dir, "volume-"+v.Name+".tar.gz")
		if _, err := execx.Output(ctx, "tar", "-czf", tarPath, "-C", filepath.Dir(src), filepath.Base(src)); err != nil {
			return 0, fmt.Errorf("tar %s: %w", v.Name, err)
		}
		meta.Volumes = append(meta.Volumes, v.Name)
	}

	// Managed databases.
	if spec.Needs != nil && spec.Needs.Postgres != nil {
		for _, db := range spec.Needs.Postgres.Databases {
			f, err := os.Create(filepath.Join(dir, "db-"+db+".dump"))
			if err != nil {
				return 0, err
			}
			err = m.PG.DumpDatabase(ctx, db, f)
			f.Close()
			if err != nil {
				return 0, err
			}
			meta.Databases = append(meta.Databases, db)
		}
	}
	resume()

	// meta.json is what restore reads; a snapshot without it is dead weight
	// that can never be restored, so a failed write fails the backup.
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, "meta.json"), mb, 0o600); err != nil {
		return 0, err
	}

	id, err := m.Store.CreateBackup(app.ID, kind, dir, fsx.DirSize(context.Background(), dir))
	if err != nil {
		return 0, err
	}
	snap.commit()
	m.Store.Audit("system", "backup.create", appName, fmt.Sprintf(`{"kind":%q}`, kind))
	m.prune(app.ID)
	return id, nil
}

// pauseForSnapshot freezes the app's running containers and returns an
// idempotent resume func. An app with no running containers (stopped, or
// never deployed) needs no freeze — its files aren't changing — so resume is
// a no-op and the snapshot proceeds unbracketed. A pause that fails on a
// running app fails the backup: an archive tarred over live writes recorded
// as good is worse than a failure the scheduler retries.
func (m *Manager) pauseForSnapshot(ctx context.Context, appName string) (resume func(), err error) {
	noop := func() {}
	psCtx, cancelPs := context.WithTimeout(ctx, composeTimeout)
	out, psErr := m.Mat.ComposeCmd(psCtx, appName, "ps", "-q").Output()
	cancelPs()
	if psErr != nil || strings.TrimSpace(string(out)) == "" {
		return noop, nil
	}
	pctx, cancelPause := context.WithTimeout(ctx, composeTimeout)
	defer cancelPause()
	if _, err := execx.Run(m.Mat.ComposeCmd(pctx, appName, "pause")); err != nil {
		return noop, fmt.Errorf("pausing %s for a consistent snapshot: %w", appName, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Detached from the snapshot's ctx: if the tar timed out, the
			// unfreeze must still happen or the app stays stopped-in-place.
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), composeTimeout)
			defer cancel()
			if _, err := execx.Run(m.Mat.ComposeCmd(uctx, appName, "unpause")); err != nil {
				msg := err.Error()
				m.Log.Error("unpause after snapshot", "app", appName, "err", msg)
				m.Notify("Snapshot left "+appName+" paused",
					"docker compose unpause failed after the snapshot — the app is frozen until it runs: "+msg, false)
			}
		})
	}, nil
}

// RestoreApp restores a snapshot: stop app → untar volumes → drop+recreate
// and pg_restore databases → deploy the app again.
//
// Refuses outright if the app is mid-deploy: a restore that lands between a
// converge's release step and its swap would be silently undone, and the
// database drop halfway through would take the live app down with it.
func (m *Manager) RestoreApp(ctx context.Context, backupID int64) error {
	return m.RestoreAppWith(ctx, backupID, false)
}

// RestoreAppWith is RestoreApp with the operator's forgetSecrets decision
// (see PreflightArtifact). It only means something to a delegated
// artifact; a volume snapshot carries no secrets fingerprint to forgive.
func (m *Manager) RestoreAppWith(ctx context.Context, backupID int64, forgetSecrets bool) error {
	release, err := m.HoldDir()
	if err != nil {
		return err
	}
	defer release()
	b, err := m.Store.BackupByID(backupID)
	if err != nil {
		return err
	}
	appRow, err := m.Store.AppByID(b.AppID)
	if err != nil {
		return err
	}
	if m.Engine == nil {
		return m.restoreApp(ctx, b, appRow, forgetSecrets)
	}
	if !m.Engine.BeginOp(appRow.Name, "restoring from a snapshot") {
		return fmt.Errorf("%s is busy with a deploy or update — wait for it to finish before restoring", appRow.Name)
	}
	defer m.Engine.EndOp(appRow.Name)
	return m.Engine.WithLock(appRow.Name, func() error {
		return m.restoreApp(ctx, b, appRow, forgetSecrets)
	})
}

func (m *Manager) restoreApp(ctx context.Context, b *store.Backup, appRow *store.App, forgetSecrets bool) error {
	// Delegated artifacts restore through the app's own restore-on-boot
	// guard; only legacy volume snapshots take the untar+pg_restore path.
	if b.Format == ArtifactFormat {
		spec, err := apps.LatestSpec(m.Store, appRow.ID)
		if err != nil {
			return err
		}
		return m.delegatedRestore(ctx, b, appRow, spec, forgetSecrets)
	}

	meta, err := m.readMeta(b.Path)
	if err != nil {
		return err
	}

	// A snapshot rotting on the NAS must fail the restore here, while the
	// live volumes are still intact — not halfway through replacing them.
	for _, vol := range meta.Volumes {
		if err := verifyTarGz(ctx, filepath.Join(b.Path, "volume-"+vol+".tar.gz")); err != nil {
			return fmt.Errorf("snapshot archive for %s is unreadable — refusing to touch live data: %w", vol, err)
		}
	}

	// Replacing volumes under running containers corrupts both the old state
	// and the new, so a stop that didn't take aborts the restore.
	sctx, cancelStop := context.WithTimeout(ctx, composeTimeout)
	_, err = execx.Run(m.Mat.ComposeCmd(sctx, appRow.Name, "stop"))
	cancelStop()
	if err != nil {
		return fmt.Errorf("stop before restore: %w", err)
	}

	// Where each volume lives is read from the spec as it stands now, not
	// from where it lived when the snapshot was taken: a volume moved to
	// another folder in Drive since then is restored into the folder it
	// occupies today.
	spec, err := apps.LatestSpec(m.Store, appRow.ID)
	if err != nil {
		return err
	}
	for _, vol := range meta.Volumes {
		v := volumeNamed(spec, vol)
		dst := m.volumePath(appRow.Name, v)
		loc, ok := m.volumeLoc(appRow.Name, v)
		if dst == "" || !ok {
			return fmt.Errorf("%s no longer has a volume named %s to restore into", appRow.Name, vol)
		}
		// The app is stopped; prove the path to the volume runs through no
		// link before renaming anything there — the parent no-follow, and the
		// volume root itself a real directory if present.
		if _, err := checkVolumeDir(volumeLoc{anchor: loc.anchor, rel: loc.rel[:len(loc.rel)-1]}); err != nil {
			return fmt.Errorf("volume %s: %w", vol, err)
		}
		if _, err := checkVolumeDir(loc); err != nil {
			return fmt.Errorf("volume %s: %w", vol, err)
		}
		if err := swapInVolume(ctx, filepath.Join(b.Path, "volume-"+vol+".tar.gz"), vol, dst); err != nil {
			return err
		}
	}
	// The dump is the app's own database, which the app controls — so it is
	// replayed as the app's own non-superuser role, never as the cluster
	// superuser. Extensions come from the spec (fog pre-creates them as
	// admin), the app role cannot.
	var pgExts []string
	if spec.Needs != nil && spec.Needs.Postgres != nil {
		pgExts = spec.Needs.Postgres.Extensions
	}
	for _, db := range meta.Databases {
		if err := m.PG.RestoreDatabase(ctx, db, pg.RoleName(appRow.Name), filepath.Join(b.Path, "db-"+db+".dump"), pgExts); err != nil {
			return err
		}
	}

	if err := m.startApp(ctx, appRow.Name); err != nil {
		return fmt.Errorf("start after restore: %w", err)
	}
	m.Store.Audit("system", "backup.restore", appRow.Name, fmt.Sprintf(`{"backup":%d}`, b.ID))
	m.Notify("Restored: "+appRow.Name, fmt.Sprintf("snapshot %s restored", filepath.Base(b.Path)), true)
	return nil
}

// verifyTarGz proves an archive is fully readable (a truncated gzip stream
// fails only at its tail, so the whole thing must be walked).
func verifyTarGz(ctx context.Context, path string) error {
	cmd := execx.Command(ctx, "tar", "-tzf", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr // entry listing goes to the default null stdout
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

// swapInVolume replaces dst with the snapshot's copy without a window where
// the live data is gone but its replacement isn't in place: extract into a
// sibling staging dir, move the old dir aside, rename the new one in, and
// only then discard the old. A failed swap puts the old dir back. tar runs
// with the same flags the old in-place extract used, so ownership and
// permission behavior are unchanged, and renames preserve both.
func swapInVolume(ctx context.Context, archive, vol, dst string) error {
	staging := dst + ".restoring"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if _, err := execx.Output(ctx, "tar", "-xzf", archive, "-C", staging); err != nil {
		return fmt.Errorf("untar %s: %w", vol, err)
	}
	// The snapshot was created as `tar -C <parent> <vol>`, so the extracted
	// tree sits at staging/<vol>.
	newDir := filepath.Join(staging, filepath.Base(dst))
	if _, err := os.Stat(newDir); err != nil {
		return fmt.Errorf("archive for %s does not contain %s: %w", vol, filepath.Base(dst), err)
	}
	old := dst + ".old"
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	hadOld := true
	if err := os.Rename(dst, old); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		hadOld = false
	}
	if err := os.Rename(newDir, dst); err != nil {
		if hadOld {
			os.Rename(old, dst) // stale data beats no data
		}
		return err
	}
	if hadOld {
		// The swap is complete; a leftover .old dir is a space leak, not a
		// failed restore, and failing here would keep the app stopped.
		os.RemoveAll(old)
	}
	return nil
}

func (m *Manager) readMeta(dir string) (*Meta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	meta := &Meta{}
	return meta, json.Unmarshal(raw, meta)
}

// prune applies retention: keep the newest N manual/scheduled snapshots,
// give the snapshots taken before a version change their own clock (they
// are the way back from an update or a rollback — useful for weeks, then
// dead weight) and their own count, and never delete the last remaining
// backup of an app no matter what the policy says.
//
// The count matters as much as the clock. An app under active development
// updates several times a day, and on the clock alone each of those
// snapshots stayed three weeks: one app held twelve beside its seven
// scheduled ones. Only the newest few are ever the way back — an update
// three versions ago is undone by rolling back, not by restoring the
// data from before it.
func (m *Manager) prune(appID int64) {
	keep := 7
	if v, _ := m.Store.GetSetting(SettingBackupRetention); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keep = n
		}
	}
	list, err := m.Store.ListBackups(appID, 1000)
	if err != nil || len(list) <= 1 {
		return
	}
	const (
		preUpdateAge  = 21 * 24 * time.Hour
		preUpdateKeep = 3
	)
	cutoff := time.Now().UTC().Add(-preUpdateAge)
	var doomed []store.Backup
	regular, preUpdate := 0, 0
	for _, b := range list { // newest first
		if b.Kind == "pre-update" || b.Kind == "pre-rollback" {
			preUpdate++
			if t, err := time.Parse(time.RFC3339, b.CreatedAt); preUpdate > preUpdateKeep || (err == nil && t.Before(cutoff)) {
				doomed = append(doomed, b)
			}
			continue
		}
		regular++
		if regular > keep {
			doomed = append(doomed, b)
		}
	}
	if len(doomed) == len(list) {
		doomed = doomed[1:] // doomed[0] is list[0], the newest — spare it
	}
	for _, b := range doomed {
		// A snapshot on a share that isn't mounted right now is invisible,
		// not gone. RemoveAll against the bare mountpoint would "succeed"
		// having deleted nothing, and dropping the row would erase fog's
		// only memory that the snapshot exists. Skip it; retention catches
		// up when the share is back.
		if _, unavailable := m.shareUnder(b.Path); unavailable {
			continue
		}
		// The files must actually be gone before the row goes: dropping the
		// record over a directory that survived would leak it on disk with
		// nothing left pointing at it. RemoveAll succeeds on an already-
		// missing path, so a half-pruned snapshot still clears next pass.
		if err := os.RemoveAll(b.Path); err != nil {
			continue
		}
		m.Store.DeleteBackup(b.ID)
	}
}

// ScheduleLoop keeps daily backups current. Due-ness is computed from the
// backups table rather than in-memory state, so a fogd restart can neither
// skip a night nor double-run one: an app is due when its newest backup
// predates the most recent occurrence of its backup hour. The first check
// runs shortly after boot (catch-up), then every few minutes.
func (m *Manager) ScheduleLoop(ctx context.Context) {
	loop.Every(ctx, 30*time.Second, 5*time.Minute, func(ctx context.Context) {
		m.runDue(ctx, time.Now().UTC())
	})
}

// backupHour resolves an app's backup hour: per-app override first, then the
// global setting. ok=false means scheduled backups are off for this app.
func backupHour(spec *apps.AppSpec, global string) (int, bool) {
	if spec.Backup != nil && spec.Backup.HourUTC != nil {
		return *spec.Backup.HourUTC, true
	}
	if global == "" {
		return 0, false
	}
	h, err := strconv.Atoi(global)
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	return h, true
}

func (m *Manager) runDue(ctx context.Context, now time.Time) {
	global, _ := m.Store.GetSetting(SettingBackupHourUTC)
	list, err := m.Store.ListApps()
	if err != nil {
		return
	}
	specs, err := apps.LatestSpecs(m.Store)
	if err != nil {
		return
	}
	// Every app's newest backup in one read; most ticks find nothing due.
	// A failed read leaves the map empty and every app's turn falls to the
	// per-app check below, as it always did.
	latest, _ := m.Store.LatestBackups()
	covered := func(last string, due time.Time) bool {
		t, err := time.Parse(time.RFC3339, last)
		return last != "" && err == nil && !t.Before(due)
	}
	var ran, failed []string
	for _, a := range list {
		spec := specs[a.ID]
		if !spec.BackupsEnabled() {
			continue
		}
		hour, ok := backupHour(spec, global)
		if !ok {
			continue
		}
		due := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
		if due.After(now) {
			due = due.Add(-24 * time.Hour)
		}
		if covered(latest[a.ID].CreatedAt, due) {
			continue // already covered since the last scheduled point
		}
		// Earlier apps' backups may have taken minutes, long enough for a
		// manual backup of this one to land since the batch read; ask again
		// before taking another.
		if last, err := m.Store.LatestBackupAt(a.ID); err == nil && covered(last, due) {
			continue
		}
		// A move of the backup directory turns every backup away until it
		// is done; the next tick after it finds these apps still due.
		if m.Moving() {
			break
		}
		// A failing app stays due until it succeeds; retry hourly rather
		// than every tick so a broken producer can't spam notifications.
		if !m.attempts.Allow(a.Name, time.Hour, now) {
			continue
		}
		// Each app's turn gets its own deadline. runDue is synchronous from
		// ScheduleLoop, whose ctx lives as long as fogd — without a bound
		// here, one app wedged past its in-path timeouts (lock wait, a
		// D-state target) would starve every app behind it plus the recovery
		// kit. The grace on top of the backup's own budget is for the lock.
		actx, cancel := context.WithTimeout(ctx, delegatedTimeout(spec)+10*time.Minute)
		_, err = m.BackupApp(actx, a.Name, "scheduled")
		cancel()
		if err != nil {
			m.Log.Error("scheduled backup", "app", a.Name, "err", err)
			failed = append(failed, a.Name+": "+err.Error())
		} else {
			ran = append(ran, a.Name)
		}
	}
	if len(failed) > 0 {
		m.Notify("Backup failures", strings.Join(failed, "\n"), false)
	} else if len(ran) > 0 {
		m.Notify("Backups complete", "snapshots taken: "+strings.Join(ran, ", "), true)
	}
	m.maybeWriteKit(ctx)
}

// maybeWriteKit keeps the encrypted recovery kit at most a day old whenever
// scheduled backups are configured at all — app artifacts without the vault
// key are half a disaster recovery.
func (m *Manager) maybeWriteKit(ctx context.Context) {
	if global, _ := m.Store.GetSetting(SettingBackupHourUTC); global == "" || m.Moving() {
		return
	}
	dir := filepath.Join(m.baseDir(), KitsDirName)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".enc" {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
			return // fresh enough
		}
	}
	if err := m.WriteRecoveryKit(ctx); err != nil {
		m.Log.Error("recovery kit", "err", err)
		m.Notify("Recovery kit failed", err.Error(), false)
	}
}

// volumePath is where one of an app's volumes actually sits on the host, or
// "" for a volume this app does not own and must not snapshot. It is the
// single answer both halves of a snapshot use — the tar is made from it and
// the restore swaps back into it — so the two can never disagree about
// where a volume lives.
func (m *Manager) volumePath(appName string, v apps.Volume) string {
	switch {
	case v.Name == "":
		return ""
	case v.Managed():
		return filepath.Join(m.Mat.DataPath(appName), v.Name)
	case v.IsAppData(appName):
		root := ""
		if m.Mat.FilesRoot != nil {
			root = m.Mat.FilesRoot()
		}
		return v.HostPath(root)
	}
	return ""
}

// volumeNamed finds a volume by name across the app and its sidecars; the
// zero Volume when the spec no longer has one, which volumePath reads as
// "nowhere to put this".
func volumeNamed(spec *apps.AppSpec, name string) apps.Volume {
	for _, v := range spec.AllVolumes() {
		if v.Name == name {
			return v
		}
	}
	return apps.Volume{}
}

// startApp brings an app back up after a restore, as a start does —
// rendered afresh under fog's rules (apps.Engine.BringUpLocked), never by
// running the compose file on disk, which may be a render the rules have
// since refused. The caller holds the app's lock.
func (m *Manager) startApp(ctx context.Context, name string) error {
	if m.startSeam != nil {
		return m.startSeam(ctx, name)
	}
	if m.Engine == nil {
		return fmt.Errorf("no app engine to start %s under fog's rules", name)
	}
	uctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	return m.Engine.BringUpLocked(uctx, name)
}
