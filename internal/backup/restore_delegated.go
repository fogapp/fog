// Delegated restore (fog-backup/1): fog stages the artifact plus an arming
// marker inside the app's first managed volume, then lets the app's own
// restore-on-boot guard consume it — the entrypoint takes a Postgres advisory
// lock, preflights, pg_restores as the app's own role, and removes the marker
// on success. Fog's part is file placement and watching the marker disappear.
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
)

// restoreMarker mirrors the app-side pending.json (FogBackup::Staging.arm!).
type restoreMarker struct {
	Armed           bool `json:"armed"`
	ConfirmIdentity bool `json:"confirm_identity"`
	// ForgetSecrets: the operator restores knowing the secrets the artifact
	// was written with are gone; the app clears what they encrypted.
	ForgetSecrets bool   `json:"forget_secrets,omitempty"`
	StagedBy      string `json:"staged_by"`
	StagedAt      string `json:"staged_at"`
}

// stagingLoc is where a restore stages: fog/restore inside the app's FIRST
// own volume — the contract's convention. Own means the same thing the
// snapshot means by it — a managed directory or the app's own folder in Drive
// — so an app whose data lives in Drive stages its restore beside that data
// rather than nowhere. It is addressed from a trusted anchor and only ever
// walked no-follow (volfs.go), never used as a path.
func (m *Manager) stagingLoc(appName string, spec *apps.AppSpec) (loc volumeLoc, owner string, err error) {
	for _, v := range spec.Volumes {
		if l, ok := m.volumeLoc(appName, v); ok {
			return l.join("fog", "restore"), v.Owner, nil
		}
	}
	return volumeLoc{}, "", fmt.Errorf("%s has no volume of its own to stage a restore in", appName)
}

// StageArtifact places an artifact and arming marker into the app's staging
// path. The next boot (release phase or entrypoint) performs the restore.
// Used both by restore-at-create (before the first deploy) and by in-place
// restore (between stop and start).
//
// The staging directory lives in a volume the app can write — and for an app
// with nested volumes, the volume root and the directories above it may be
// rewritable too. So the directory is reached component by component from a
// trusted anchor with O_NOFOLLOW, refusing a symlink anywhere (volume root
// included), and pending.tar / pending.json replace whatever entry was there
// with a fresh O_EXCL file: no link an app planted can redirect root's write
// outside its own volume, or into another app's.
func (m *Manager) StageArtifact(appName string, spec *apps.AppSpec, artifactPath string, confirmIdentity, forgetSecrets bool, stagedBy string) error {
	loc, owner, err := m.stagingLoc(appName, spec)
	if err != nil {
		return err
	}
	marker, _ := json.MarshalIndent(restoreMarker{
		Armed:           true,
		ConfirmIdentity: confirmIdentity,
		ForgetSecrets:   forgetSecrets,
		StagedBy:        stagedBy,
		StagedAt:        time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	return stageIntoVolume(loc, artifactPath, marker, owner)
}

// stageIntoVolume writes pending.tar then pending.json into the staging
// directory at loc, reached no-follow, and hands them to the app's owner.
func stageIntoVolume(loc volumeLoc, artifactPath string, marker []byte, owner string) error {
	fd, err := openDirNoFollow(loc, true)
	if err != nil {
		return fmt.Errorf("staging directory: %w", err)
	}
	defer unix.Close(fd)
	in, err := os.Open(artifactPath)
	if err != nil {
		return err
	}
	defer in.Close()
	// fsync'd: the staged artifact arms a restore that may run on the very
	// next boot — a power cut must not leave a truncated pending.tar.
	if err := writeAt(fd, "pending.tar", in); err != nil {
		return fmt.Errorf("staging artifact: %w", err)
	}
	// pending.json arms the restore, so it is written last and fsynced: a
	// power cut must not leave an armed marker beside a truncated artifact.
	if err := writeAt(fd, "pending.json", bytes.NewReader(marker)); err != nil {
		return fmt.Errorf("staging marker: %w", err)
	}
	// The app runs as the volume's owner and must be able to read, consume,
	// and delete the staged files.
	if owner != "" {
		return ensureStagingAccess(fd, loc.path(), owner)
	}
	return nil
}

// canWriteAs is swapped by tests; the real probe needs root.
var canWriteAs = fsx.CanWriteAs

// chownStagingFD re-owns the staging directory and the two files fog just
// created in it, through the directory's descriptor — fchown on the
// directory, fchownat with AT_SYMLINK_NOFOLLOW on the files — so nothing a
// path could be redirected through is involved. A seam for tests.
var chownStagingFD = func(dirfd, uid, gid int) error {
	if err := unix.Fchown(dirfd, uid, gid); err != nil {
		return err
	}
	for _, name := range []string{"pending.tar", "pending.json"} {
		if err := unix.Fchownat(dirfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return nil
}

// ensureStagingAccess hands the staging directory to the app's uid. On a
// share that refuses chown even to root — a CIFS mount with fixed uid and
// modes, an NFS export that squashes root — ownership is the mount's to
// decide, not fog's, and the chown failing says nothing about whether the
// app can consume the files. A write as the owner does: the staged files are
// world-readable, so creating and removing a file in the directory is
// exactly what the app's restore-on-boot needs. Only a refusal there fails.
// (That probe runs as the app's uid, so following a link there grants
// nothing.)
func ensureStagingAccess(dirfd int, dir, owner string) error {
	uid, gid, ok := fsx.ParseOwner(owner)
	if !ok {
		return fmt.Errorf("volume owner %q is not uid:gid", owner)
	}
	chownErr := chownStagingFD(dirfd, uid, gid)
	if chownErr == nil {
		return nil
	}
	writable, why, err := canWriteAs(context.Background(), dir, uid, gid)
	switch {
	case err != nil:
		return fmt.Errorf("chown staging: %w (and the app's access could not be checked: %v)", chownErr, err)
	case !writable:
		return fmt.Errorf("the app's uid %s cannot write the restore staging directory %s (%s), and fog cannot chown it there: %w", owner, dir, why, chownErr)
	}
	return nil
}

// RestoreAppFromArtifact restores a foreign artifact (an upload, or a file
// already on this host) into an existing delegated app: stop → stage → start
// → wait for the app's restore-on-boot to consume the marker.
func (m *Manager) RestoreAppFromArtifact(ctx context.Context, appName, artifactPath string, confirmIdentity, forgetSecrets bool) error {
	app, err := m.Store.AppByName(appName)
	if err != nil {
		return err
	}
	spec, err := apps.LatestSpec(m.Store, app.ID)
	if err != nil {
		return err
	}
	if !spec.DelegatedBackups() {
		return fmt.Errorf("%s does not implement delegated backups — only fog-backup/1 apps can restore artifacts", appName)
	}
	if m.Engine == nil {
		return m.delegatedRestoreFromPath(ctx, appName, spec, artifactPath, confirmIdentity, forgetSecrets)
	}
	if !m.Engine.BeginOp(appName, "restoring from an artifact") {
		return fmt.Errorf("%s is busy with a deploy or update — wait for it to finish before restoring", appName)
	}
	defer m.Engine.EndOp(appName)
	return m.Engine.WithLock(appName, func() error {
		return m.delegatedRestoreFromPath(ctx, appName, spec, artifactPath, confirmIdentity, forgetSecrets)
	})
}

// delegatedRestore restores one of fog's own recorded artifacts.
func (m *Manager) delegatedRestore(ctx context.Context, b *store.Backup, appRow *store.App, spec *apps.AppSpec, forgetSecrets bool) error {
	dir, release, err := m.BackupFiles(ctx, b)
	if err != nil {
		return err
	}
	defer release()
	artifact := filepath.Join(dir, "artifact.tar")
	if _, err := os.Stat(artifact); err != nil {
		return fmt.Errorf("backup %d has no artifact.tar", b.ID)
	}
	// Restoring one of our own verified artifacts back into the app it came
	// from is identity-preserving by construction; the app-side preflight
	// still gets the final word.
	return m.delegatedRestoreFromPath(ctx, appRow.Name, spec, artifact, true, forgetSecrets)
}

func (m *Manager) delegatedRestoreFromPath(ctx context.Context, appName string, spec *apps.AppSpec, artifactPath string, confirmIdentity, forgetSecrets bool) error {
	ctx, op := m.beginRestore(ctx, appName)
	defer m.endRestore(appName, op)

	// Before the app is stopped, let alone armed: an artifact that cannot
	// restore should cost nothing. This also covers fog's own snapshots —
	// secrets rotated since the backup was taken would decrypt its rows to
	// nothing, and that is worth refusing rather than discovering on boot.
	pf := PreflightArtifact(artifactPath, appName, m.knownIdentity(ctx, appName, spec), m.SecretReader(appName), confirmIdentity, forgetSecrets)
	if !pf.OK() {
		return fmt.Errorf("artifact preflight refused the restore: %w", pf.Err())
	}
	for _, w := range pf.Warnings {
		m.Log.Warn("restore preflight", "app", appName, "warning", w)
	}

	// The app must be down before fog stages into its volume and chowns it:
	// a running app could swap a staging entry for a symlink mid-write, and
	// root would follow it. A stop that fails is a refusal, not a warning.
	sctx, cancelStop := context.WithTimeout(ctx, composeTimeout)
	out, stopErr := m.Mat.ComposeCmd(sctx, appName, "stop").CombinedOutput()
	cancelStop()
	if stopErr != nil {
		return fmt.Errorf("refusing to restore %s: it could not be stopped first (%v): %s", appName, stopErr, strings.TrimSpace(string(out)))
	}

	if err := m.StageArtifact(appName, spec, artifactPath, confirmIdentity, forgetSecrets, "fog-restore"); err != nil {
		return err
	}

	if err := m.startApp(ctx, appName); err != nil {
		return fmt.Errorf("start after staging: %w", err)
	}

	if err := m.waitRestoreConsumed(ctx, appName, spec); err != nil {
		// An operator calling the restore off is a decision, not a fault:
		// report it as itself rather than as the "context canceled" the
		// plumbing would otherwise surface.
		if op.cancelled.Load() {
			return fmt.Errorf("restore of %s was cancelled", appName)
		}
		return err
	}
	m.Store.Audit("system", "backup.restore", appName, fmt.Sprintf(`{"artifact":%q,"forgetSecrets":%t}`, filepath.Base(artifactPath), forgetSecrets))
	m.Notify("Restored: "+appName, "artifact restored and consumed on boot", true)
	return nil
}

// ---- cancelling a restore ----

// restoreOp is the handle on an in-flight delegated restore: the cancel that
// interrupts it, and a channel that closes once it has actually returned and
// let go of the app lock.
type restoreOp struct {
	cancel    context.CancelFunc
	done      chan struct{}
	cancelled atomic.Bool // an operator called it off, rather than a timeout
}

func (m *Manager) beginRestore(ctx context.Context, appName string) (context.Context, *restoreOp) {
	ctx, cancel := context.WithCancel(ctx)
	op := &restoreOp{cancel: cancel, done: make(chan struct{})}
	m.restores.Store(appName, op)
	return ctx, op
}

func (m *Manager) endRestore(appName string, op *restoreOp) {
	m.restores.CompareAndDelete(appName, op)
	op.cancel()
	close(op.done)
}

// RestoreStaged reports whether an armed restore marker is sitting in any of
// the app's managed volumes.
//
// Read from disk rather than from the in-flight map on purpose: the marker
// outlives the process that staged it. If fogd restarts (or its wait times
// out) while a restore is failing on the app side, the app keeps re-running
// that restore on every boot with nothing in fog's memory to show for it —
// which is exactly the state an operator needs to see and call off.
func (m *Manager) RestoreStaged(appName string) bool {
	return len(m.stagedMarkers(appName)) > 0
}

// stagedMarkers finds the staging directories holding an arming marker
// across the app's own volumes. The staging convention puts it in the first
// own volume, but cancel must clear whatever is actually armed — including a
// marker left in a volume the spec has since reordered or dropped, so every
// directory under the app's data dir is looked in, and the spec's own Drive
// volumes too (an app whose data lives in Drive stages there).
//
// Every look is a no-follow walk from the trusted anchor: a volume root (or a
// directory above it) that is a symlink — into another app's volume, say — is
// skipped, never followed, so one app's planted link can neither show
// another app's restore as its own nor get it disarmed.
func (m *Manager) stagedMarkers(appName string) []volumeLoc {
	var hits []volumeLoc
	seen := map[string]bool{}
	add := func(vol volumeLoc) {
		loc := vol.join("fog", "restore")
		if seen[loc.path()] {
			return
		}
		seen[loc.path()] = true
		if present, _ := markerPresent(loc); present {
			hits = append(hits, loc)
		}
	}
	appData := volumeLoc{anchor: m.Mat.DataDir, rel: []string{"data", appName}}
	if fd, err := openDirNoFollow(appData, false); err == nil {
		f := os.NewFile(uintptr(fd), appData.path())
		ents, _ := f.ReadDir(-1)
		f.Close()
		for _, e := range ents {
			// DirEntry's type is the entry's own (lstat), so a link is not
			// a directory here and is passed over.
			if e.IsDir() {
				add(appData.join(e.Name()))
			}
		}
	}
	if m.Store == nil {
		return hits
	}
	app, err := m.Store.AppByName(appName)
	if err != nil {
		return hits
	}
	spec, err := apps.LatestSpec(m.Store, app.ID)
	if err != nil {
		return hits
	}
	for _, v := range spec.AllVolumes() {
		if v.Managed() {
			continue // the data-dir listing has it
		}
		if loc, ok := m.volumeLoc(appName, v); ok {
			add(loc)
		}
	}
	return hits
}

// markerPresent reports whether the staging directory at loc, reached
// no-follow, holds a regular pending.json. An absent directory is "no";
// one that turns out to be a link is an error, never followed.
func markerPresent(loc volumeLoc) (bool, error) {
	fd, err := openDirNoFollow(loc, false)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && errors.Is(pe.Err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	defer unix.Close(fd)
	return regularAt(fd, "pending.json"), nil
}

// CancelRestore calls off a delegated restore: it interrupts fog's wait,
// disarms the staged marker so the next boot stops retrying a restore that is
// failing, and starts the app back up.
//
// Both halves matter and neither is enough alone. Interrupting the wait
// without disarming leaves the app booting into the same failing restore
// forever, with no fog operation left to cancel. Disarming without taking the
// app lock races the restore still writing those files. And because the wait
// lives in memory while the marker lives on disk, the disk half runs whether
// or not this process still has an operation in flight.
func (m *Manager) CancelRestore(ctx context.Context, appName, actor string) error {
	if _, err := m.Store.AppByName(appName); err != nil {
		return err
	}
	interrupted := m.interruptRestore(ctx, appName)
	markers := m.stagedMarkers(appName)
	if !interrupted && len(markers) == 0 {
		return fmt.Errorf("%s has no restore to cancel", appName)
	}

	disarmAndStart := func() error {
		// Re-read under the lock: the restore we just interrupted may have
		// staged its marker between our first look and its release.
		for _, loc := range m.stagedMarkers(appName) {
			if err := disarmRestore(loc); err != nil {
				return err
			}
		}
		if err := m.startApp(ctx, appName); err != nil {
			// The disarm is the part that matters and it succeeded, so say so
			// — the operator's next move is a start, not another cancel.
			return fmt.Errorf("restore disarmed, but starting %s failed: %w", appName, err)
		}
		return nil
	}
	var err error
	if m.Engine == nil {
		err = disarmAndStart()
	} else {
		err = m.Engine.WithLock(appName, disarmAndStart)
	}
	if err != nil {
		return err
	}
	m.Store.Audit(actor, "backup.restore.cancel", appName, "")
	if m.Notify != nil {
		m.Notify("Restore cancelled: "+appName, "the staged restore was disarmed and the app started back up", true)
	}
	return nil
}

// interruptRestore signals an in-flight restore and waits for it to return, so
// the cleanup that follows isn't racing it for the app's files. Reports
// whether there was one to interrupt.
func (m *Manager) interruptRestore(ctx context.Context, appName string) bool {
	v, ok := m.restores.Load(appName)
	if !ok {
		return false
	}
	op := v.(*restoreOp)
	op.cancelled.Store(true)
	op.cancel()
	select {
	case <-op.done:
	case <-ctx.Done():
		// Fall through to the disarm regardless: WithLock still serialises us
		// behind whatever the restore is finishing, and leaving an armed
		// marker on the way out is the one outcome worth avoiding.
	}
	return true
}

// disarmRestore removes the arming marker and the staged artifact from a
// staging directory, reached no-follow and unlinked through its descriptor
// (a link is removed as itself, never followed). The marker goes first: it
// is what the app's boot guard reads, so a failure to delete the (large)
// artifact afterwards still leaves the restore disarmed rather than
// half-armed.
func disarmRestore(loc volumeLoc) error {
	fd, err := openDirNoFollow(loc, false)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && errors.Is(pe.Err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("disarming restore: %w", err)
	}
	defer unix.Close(fd)
	for _, f := range []string{"pending.json", "pending.tar"} {
		if err := unix.Unlinkat(fd, f, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("disarming restore: %s: %w", f, err)
		}
	}
	return nil
}

// waitRestoreConsumed watches the arming marker. The app's restore-on-boot
// removes it only after a successful restore; a failed restore leaves it in
// place (and the container exits nonzero), so a marker that persists past
// the timeout is a real verdict, not a guess. The marker is looked at
// no-follow: a staging directory swapped for a link is reported, not read
// through.
func (m *Manager) waitRestoreConsumed(ctx context.Context, appName string, spec *apps.AppSpec) error {
	loc, _, err := m.stagingLoc(appName, spec)
	if err != nil {
		return err
	}
	marker := filepath.Join(loc.path(), "pending.json")
	// Ticker rather than Sleep so a cancel lands at once instead of waiting
	// out the current poll — an operator calling off a stuck restore should
	// not sit through another five seconds of it.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	deadline := time.Now().Add(delegatedTimeout(spec))
	for time.Now().Before(deadline) {
		present, err := markerPresent(loc)
		if err != nil {
			return fmt.Errorf("watching the restore marker: %w", err)
		}
		if !present {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return fmt.Errorf("restore was staged but the app has not consumed it after %s — check %s's logs, then cancel the restore to disarm it (the marker at %s holds the state, and the app retries it on every boot until it is cleared)",
		delegatedTimeout(spec), appName, marker)
}

// DeleteBackup removes one backup's files and row, under the app lock so it
// can't yank an artifact out from under a running restore. Refuses to delete
// an app's only remaining backup unless force is set.
func (m *Manager) DeleteBackup(ctx context.Context, id int64, force bool) error {
	release, err := m.HoldDir()
	if err != nil {
		return err
	}
	defer release()
	b, err := m.Store.BackupByID(id)
	if err != nil {
		return err
	}
	// Files on a share that isn't answering right now are invisible, not
	// gone: RemoveAll against the bare mountpoint would "succeed" having
	// deleted nothing, and dropping the row would erase fog's only memory
	// that the artifact exists — leaking it on the NAS forever. Same guard
	// prune applies, but loud: a deliberate delete deserves an explanation.
	if sh, reason := m.shareState(b.Path); reason != "" {
		return fmt.Errorf("this backup is on the NAS share %q, which is %s — deleting now would remove nothing while erasing fog's record of the files. Fix the share in Settings → Storage → Disks & shares, then delete", sh, reason)
	}
	appRow, err := m.Store.AppByID(b.AppID)
	if err != nil {
		// The app is gone; the row would have cascaded with it, but the
		// files may remain — delete both unconditionally.
		os.RemoveAll(b.Path)
		m.dropFromStore(b)
		return m.Store.DeleteBackup(b.ID)
	}
	remove := func() error {
		if !force {
			list, err := m.Store.ListBackups(b.AppID, 2)
			if err == nil && len(list) <= 1 {
				return fmt.Errorf("this is %s's only backup — deleting it leaves nothing to restore", appRow.Name)
			}
		}
		if err := os.RemoveAll(b.Path); err != nil {
			return err
		}
		m.dropFromStore(b)
		return m.Store.DeleteBackup(b.ID)
	}
	if m.Engine == nil {
		return remove()
	}
	return m.Engine.WithLock(appRow.Name, remove)
}

// knownIdentity is the software an existing app runs, as far as fog can
// tell: the descriptor of the running app first, else the `app` its newest
// artifact declares, else "" — restore preflight then warns instead of
// refusing, and the app's own preflight decides.
func (m *Manager) knownIdentity(ctx context.Context, appName string, spec *apps.AppSpec) string {
	app, err := m.Store.AppByName(appName)
	if err != nil {
		return ""
	}
	if app.DesiredState == "running" {
		if d, err := m.Descriptor(ctx, app, spec); err == nil && d.App != "" {
			return d.App
		}
	}
	if b := m.newestArtifact(app.ID); b != nil {
		if id := ManifestApp(b.Manifest); id != "" {
			return id
		}
	}
	return ""
}
