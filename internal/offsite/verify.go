package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/store"
)

// Verification of what is offsite, on the same rule as local backups:
// "a backup you haven't restored is a hope." Weekly, each app's newest
// copy is fetched back from the bucket, its digest checked against its
// row, and — for a delegated artifact — its database dump test-restored
// into a scratch database; a sample of the newest Files snapshot's blobs
// is fetched and re-hashed. Only then does the copy carry verified_at,
// and the fleet table says "verified offsite". On B2 the egress is free.

const (
	NoticeVerifyFailed = "offsite-verify-failed"
	NoticeVerifyNoRoom = "offsite-verify-no-room"
	verifyInterval     = 6 * time.Hour
	// Blob sampling: the local sampler's numbers.
	sampleFraction = 0.01
	sampleMin      = 5
	sampleMax      = 200
)

// VerifyLoop runs verifyDue on an interval, a couple of minutes after
// boot first.
func (m *Manager) VerifyLoop(ctx context.Context) {
	loop.Every(ctx, 4*time.Minute, verifyInterval, m.verifyDue)
}

func (m *Manager) verifyDue(ctx context.Context) {
	targets, err := m.Store.ListOffsiteTargets()
	if err != nil {
		return
	}
	for i := range targets {
		t := &targets[i]
		if t.Role != "active" {
			continue
		}
		if m.jobs().Running(syncKind(t.Name)) {
			continue // the sync owns the bucket for now; next tick
		}
		// locker.json is unsealed and has no Object Lock, and fog otherwise
		// caches its keys for the life of the process — so a bucket writer
		// could hide or rewrite the key wraps and nightly verify would keep
		// passing on the cached keys. Re-read and re-check it from the bucket
		// every pass, and alert if it has gone or changed.
		if err := m.recheckLocker(ctx, t); err != nil {
			m.Log.Error("offsite locker re-check", "target", t.Name, "err", err)
			continue
		}
		if err := m.recheckKits(ctx, t); err != nil {
			m.Log.Error("offsite kit re-check", "target", t.Name, "err", err)
		}
		if err := m.verifyTarget(ctx, t); err != nil {
			m.Log.Error("offsite verify", "target", t.Name, "err", err)
		}
	}
}

func (m *Manager) due(c *store.OffsiteCopy) bool {
	if c.VerifiedAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, c.VerifiedAt)
	return err != nil || m.now().Sub(t) > backup.ReverifyAfter
}

// NoticeLockerChanged is raised while a target's locker.json is missing or no
// longer matches the keys fog opened it with.
const NoticeLockerChanged = "offsite-locker-changed"

// recheckLocker re-reads locker.json from the bucket and re-derives its keys,
// comparing them to the ring fog has cached for this target. A missing,
// unparseable, or re-keyed locker.json raises an Inbox notice and a push, and
// fails the pass — the cached keys are not trusted to paper over it.
func (m *Manager) recheckLocker(ctx context.Context, t *store.OffsiteTarget) error {
	c, err := m.Client(t)
	if err != nil {
		return err
	}
	alert := func(detail string) error {
		m.Store.CreateNotice(NoticeLockerChanged, t.Name,
			"The offsite locker's key file changed on "+t.Name,
			detail+" Someone with write access to the bucket may have tampered with it. Check the target; recovery needs locker.json intact.")
		m.notify("Offsite locker key file changed: "+t.Name, detail, false)
		return errors.New(detail)
	}
	lk, err := FetchLocker(ctx, c, LayoutOf(t))
	if err != nil {
		if errors.Is(err, objstore.ErrNotFound) {
			return alert("locker.json is gone from the bucket.")
		}
		return err // a transient read error is not a tamper verdict
	}
	// fog upgraded this locker to bound wraps; a file without the marker is a
	// rollback attempt, whatever its wraps say.
	if m.enforceBound(lk) {
		return alert("locker.json has lost its locker-bound key-wrap marker — a rollback to old-format wraps.")
	}
	cached, err := m.Keyring(ctx, t)
	if err != nil {
		return alert("locker.json no longer opens with this machine's keys.")
	}
	// Both doors, every pass. The vault wrap is what fog itself uses nightly;
	// the passphrase wraps are what a fresh machine will need, so stripped,
	// corrupted or rolled-back passphrase wraps must surface now — not on the
	// day of a recovery. Each must open, and yield the very roots fog holds.
	if byMaster, err := lk.OpenWithMaster(m.Vault); err == nil {
		if !sameRoots(byMaster, cached) {
			return alert("locker.json's vault wraps now yield different keys than fog opened it with.")
		}
	} else if !errors.Is(err, ErrNoMasterWrap) {
		return alert("locker.json's vault wraps no longer open.")
	}
	pass, err := m.passphrase()
	if err != nil {
		return err
	}
	byPass, err := lk.OpenWithPassphrase(pass)
	if err != nil {
		return alert("locker.json's passphrase wraps no longer open with the recovery-kit passphrase — a fresh machine could not recover from this locker.")
	}
	if !sameRoots(byPass, cached) {
		return alert("locker.json's passphrase wraps yield different keys than fog holds — a fresh machine would recover the wrong locker.")
	}
	m.Store.DismissNoticeByKind(NoticeLockerChanged, t.Name)
	return nil
}

// NoticeKitUnknown is raised while the newest kit in a target's bucket is one
// this fog did not upload.
const NoticeKitUnknown = "offsite-kit-unknown"

// recheckKits checks the kits as a fresh-machine recovery would meet them.
// The bucket's newest kit must be one fog uploaded (anything else was put
// there by someone else with write access: a decoy, or an old kit renamed).
// And the kit a recovery would actually choose — opened with the passphrase
// and its sealed stamp checked, exactly as OpenNewestKit does — must be the
// newest one fog recorded uploading, byte for byte: a newer kit deleted or
// spoiled would roll a recovery back to older secrets, and only opening it
// shows that. Any of this raises NoticeKitUnknown.
func (m *Manager) recheckKits(ctx context.Context, t *store.OffsiteTarget) error {
	r, err := m.Reader(ctx, t)
	if err != nil {
		return err
	}
	kits, err := r.Kits(ctx)
	if err != nil || len(kits) == 0 {
		return err
	}
	var problems []string
	newest := kits[0].Name
	if _, err := m.Store.OffsiteCopy(t.ID, store.OffsiteKindKit, newest); err != nil {
		problems = append(problems, "The newest recovery kit in the bucket, "+newest+", is not one this fog uploaded. Someone with write access to the bucket may have planted it; a recovery skips it if it does not check out.")
	}
	if pass, err := m.passphrase(); err == nil {
		rec, haveRec := m.latestKit(t)
		kit, err := OpenNewestKit(ctx, r, pass)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			problems = append(problems, "No recovery kit in the bucket opens and checks out, so a recovery from it would fail: "+err.Error())
		case haveRec && (kit.Name != rec.Name || kit.SHA256 != rec.SHA256):
			problems = append(problems, fmt.Sprintf("A recovery from this bucket would restore kit %s, not %s — the newest one fog uploaded. The newer kit is missing or does not match what fog sent; a recovery would roll this machine back to older secrets.", kit.Name, rec.Name))
		}
		if kit != nil {
			clear(kit.Plain)
		}
	}
	if len(problems) == 0 {
		m.Store.DismissNoticeByKind(NoticeKitUnknown, t.Name)
		return nil
	}
	detail := strings.Join(problems, " ") + " Check the target. (Right after this machine was itself restored from a kit, this clears with the next nightly kit.)"
	m.Store.CreateNotice(NoticeKitUnknown, t.Name, "Recovery kits in "+t.Name+" need attention", detail)
	m.notify("Recovery kits in "+t.Name+" need attention", problems[0], false)
	return errors.New(detail)
}

// sameRoots reports whether two rings hold exactly the same root keys.
func sameRoots(a, b *Keyring) bool {
	if len(a.roots) != len(b.roots) {
		return false
	}
	for id, r := range a.roots {
		if !bytes.Equal(r, b.roots[id]) {
			return false
		}
	}
	return true
}

// verifyTarget proves the newest copy of every series in one target.
func (m *Manager) verifyTarget(ctx context.Context, t *store.OffsiteTarget) error {
	// Only snapshots are verified; the blob rows they reference (one per
	// Drive file) are not worth loading here.
	copies, err := m.Store.ListOffsiteCopiesOfKinds(t.ID, []string{store.OffsiteKindApp, store.OffsiteKindFilesSnapshot}, store.OffsiteStored)
	if err != nil {
		return err
	}
	layout := LayoutOf(t)
	// Newest stored copy per series.
	newest := map[string]store.OffsiteCopy{}
	newestAt := map[string]time.Time{}
	for _, c := range copies {
		s, at, ok := seriesOf(layout, &c)
		if !ok {
			continue
		}
		if at.After(newestAt[s]) {
			newest[s], newestAt[s] = c, at
		}
	}
	if len(newest) == 0 {
		return nil
	}
	var r *Reader
	var errs []string
	for series, c := range newest {
		if !m.due(&c) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if r == nil {
			if r, err = m.Reader(ctx, t); err != nil {
				return err
			}
		}
		// The listing left the documents out; the one being checked
		// needs its own.
		full, err := m.Store.OffsiteCopy(t.ID, c.Kind, c.Subject)
		if err != nil {
			errs = append(errs, series+": "+err.Error())
			continue
		}
		var verr error
		if c.Kind == store.OffsiteKindApp {
			verr = m.verifyAppCopy(ctx, r, full)
		} else {
			verr = m.verifyFilesCopy(ctx, r, full)
		}
		subject := strings.TrimPrefix(series, "app:")
		if verr != nil && noRoom(verr) {
			// This server's disk, not the copy: say so, and leave whatever
			// the copy's last real check found standing.
			errs = append(errs, series+": "+verr.Error())
			m.Store.CreateNotice(NoticeVerifyNoRoom, subject, fmt.Sprintf("The offsite copy of %s could not be checked", subject),
				fmt.Sprintf("On %s: %s. Nothing is known to be wrong with the copy; fog checks it again once this server has room — free space on its disk (Settings → Storage).", t.Name, verr.Error()))
			continue
		}
		m.Store.DismissNoticeByKind(NoticeVerifyNoRoom, subject)
		if verr != nil {
			errs = append(errs, series+": "+verr.Error())
			m.Store.CreateNotice(NoticeVerifyFailed, subject, fmt.Sprintf("The offsite copy of %s failed verification", subject),
				fmt.Sprintf("On %s: %s — the copy may be unusable. The next sync re-checks; if this persists, re-add the target or the bucket needs attention.", t.Name, verr.Error()))
			m.notify("Offsite verification failed: "+subject, verr.Error(), false)
			continue
		}
		m.Store.SetOffsiteCopyVerified(c.ID)
		m.Store.DismissNoticeByKind(NoticeVerifyFailed, subject)
		m.Log.Info("offsite copy verified", "target", t.Name, "series", series)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// verifyAppCopy fetches the sealed snapshot, checks it against its row,
// and test-restores a delegated artifact's dump.
func (m *Manager) verifyAppCopy(ctx context.Context, r *Reader, c *store.OffsiteCopy) error {
	var row AppRow
	if err := json.Unmarshal([]byte(c.Doc), &row); err != nil {
		return fmt.Errorf("unreadable row: %w", err)
	}
	if row.StoreSnapshot != "" {
		return m.verifyStoreCopy(ctx, r, &row)
	}
	if !m.testRestorable(&row) {
		// Nothing to test-restore: reading the object through, every frame
		// authenticating, and matching its row is the whole check.
		sum, n, err := r.Hash(ctx, c.Key)
		if err != nil {
			return err
		}
		if row.SnapshotSHA256 != "" && !strings.EqualFold(sum, row.SnapshotSHA256) {
			return fmt.Errorf("the snapshot in the bucket does not match its row (sha256 %s, expected %s)", sum[:12], row.SnapshotSHA256[:12])
		}
		if n == 0 {
			return errors.New("the snapshot is empty")
		}
		return nil
	}
	if err := os.MkdirAll(m.spool(), 0o700); err != nil {
		return err
	}
	if err := m.ensureRoom(m.spool(), row.SizeBytes); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(m.spool(), "verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	// One pass: the tar unpacks as it arrives, the snapshot and each entry
	// hashing on the way.
	digests := map[string]string{}
	if err := fetchUnpacked(ctx, r, c.Key, row.SnapshotSHA256, scratch, nil, digests); err != nil {
		return err
	}
	if len(digests) == 0 {
		return errors.New("the snapshot is empty")
	}
	if row.ArtifactSHA256 != "" && !strings.EqualFold(digests["artifact.tar"], row.ArtifactSHA256) {
		return fmt.Errorf("artifact.tar inside the snapshot does not match its digest")
	}
	return m.TestRestore(ctx, row.App, filepath.Join(scratch, "artifact.tar"), row.Manifest)
}

// verifyStoreCopy fetches a store-backed copy's packs and reads the backup
// back from them — every chunk and file checked on the way — and
// test-restores a delegated artifact's dump. Only a test-restore needs the
// backup written out; otherwise it is read through and nothing but the
// packs lands on disk.
func (m *Manager) verifyStoreCopy(ctx context.Context, r *Reader, row *AppRow) error {
	if err := os.MkdirAll(m.spool(), 0o700); err != nil {
		return err
	}
	restore := m.testRestorable(row)
	if err := m.ensureRoom(m.spool(), storeFetchNeed(row, restore)); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(m.spool(), "verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if !restore {
		return FetchStoreSnapshot(ctx, r, row, scratch, "", nil)
	}
	dest := filepath.Join(scratch, "backup")
	if err := FetchStoreSnapshot(ctx, r, row, scratch, dest, nil); err != nil {
		return err
	}
	return m.TestRestore(ctx, row.App, filepath.Join(dest, "artifact.tar"), row.Manifest)
}

// testRestorable says whether a copy's dump is test-restored, not only read
// back: a delegated artifact, of an app still installed here. A removed
// app's spec — what decides the extensions a scratch restore may recreate —
// went with it, so its copies, kept until they age out, are checked whole
// and unaltered but not restored.
func (m *Manager) testRestorable(row *AppRow) bool {
	if row.Mode != "delegated" || m.TestRestore == nil {
		return false
	}
	if m.Store != nil {
		if _, err := m.Store.AppByName(row.App); errors.Is(err, store.ErrNotFound) {
			return false
		}
	}
	return true
}

// verifyFilesCopy re-hashes a random sample of the newest snapshot's
// blobs against the manifest.
func (m *Manager) verifyFilesCopy(ctx context.Context, r *Reader, c *store.OffsiteCopy) error {
	var snap FilesSnapshot
	if err := json.Unmarshal([]byte(c.Doc), &snap); err != nil {
		return fmt.Errorf("unreadable manifest: %w", err)
	}
	if snap.StoreSnapshot != "" {
		return verifyPacks(ctx, r, snap.Store, snap.Packs, 3) // a few packs: each is tens of MB
	}
	if len(snap.Files) == 0 {
		return nil
	}
	n := int(float64(len(snap.Files)) * sampleFraction)
	if n < sampleMin {
		n = sampleMin
	}
	if n > sampleMax {
		n = sampleMax
	}
	if n > len(snap.Files) {
		n = len(snap.Files)
	}
	perm := rand.Perm(len(snap.Files))[:n]
	checked := map[string]bool{}
	for _, i := range perm {
		f := snap.Files[i]
		if checked[f.Blob] {
			continue
		}
		checked[f.Blob] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		rc, err := r.Open(ctx, r.Layout.Blob(f.Blob))
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return fmt.Errorf("%s: the blob in the bucket does not hash to the manifest's digest", f.Path)
		}
	}
	return nil
}
