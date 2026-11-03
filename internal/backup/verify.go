// Verification: "backups aren't real until restored." Every delegated
// artifact gets (a) re-hashed against the digest fog verified at download —
// bit-rot on a NAS is real — and (b) test-restored into a scratch database
// with a table-count sanity check, on a rolling weekly schedule. An app with
// no database (every component a file archive) has nothing to load, so its
// components are proven against their manifest digests instead. Only a
// backup that has survived both carries a verified_at stamp, and the
// staleness alert keys on *that*: absence of a recent verified backup, not
// presence of failure events — the dead-man's-switch lesson from every
// backup postmortem worth reading.
package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// ReverifyAfter: how old a verified stamp may get before the artifact is
	// proven again. Offsite copies follow the same rule.
	ReverifyAfter = 7 * 24 * time.Hour
	// StaleAfter: how old an app's newest backup may be before the operator
	// hears about it. A daily schedule plus generous grace. The offsite
	// dead-man's switch uses the same number, so the two alarms agree.
	StaleAfter = 26 * time.Hour
	// NoticeStale is the dead-man's-switch notice, keyed by app name. It
	// describes an ongoing condition, so checkStaleness retracts it as soon
	// as a backup lands.
	NoticeStale = "backup-stale"
)

// VerifyLoop periodically proves the newest artifact of every delegated app
// restorable, and raises the staleness alarm for backup-enabled apps whose
// newest backup is overdue.
func (m *Manager) VerifyLoop(ctx context.Context) {
	loop.Every(ctx, 2*time.Minute, 6*time.Hour, func(ctx context.Context) {
		m.verifyDue(ctx)
		m.checkStaleness()
	})
}

func (m *Manager) verifyDue(ctx context.Context) {
	release, err := m.HoldDir()
	if err != nil {
		return // a move is running; the next pass verifies at the new place
	}
	defer release()
	list, err := m.Store.ListApps()
	if err != nil {
		return
	}
	specs, err := apps.LatestSpecs(m.Store)
	if err != nil {
		return
	}
	artifacts, err := m.Store.LatestBackupsOfFormat(ArtifactFormat, newestArtifactWithin)
	if err != nil {
		return
	}
	for _, a := range list {
		spec := specs[a.ID]
		if !spec.BackupsEnabled() || !spec.DelegatedBackups() {
			continue
		}
		art, ok := artifacts[a.ID]
		if !ok {
			continue
		}
		if recentlyVerified(&art) {
			continue
		}
		// Each verify is a test restore that can take minutes, so the batch
		// read may be stale by this app's turn: a newer backup may have
		// landed and retention pruned this one. Verifying a pruned artifact
		// would raise a false corruption alarm; take the current newest.
		b := m.newestArtifact(a.ID)
		if b == nil || recentlyVerified(b) {
			continue
		}
		// An artifact on an unmounted share is unreadable because it's away,
		// not because it rotted. Alarming about corruption here would send
		// the operator hunting the wrong failure — the share-unavailable
		// issue already names the real one.
		if _, unavailable := m.shareUnder(b.Path); unavailable {
			m.Log.Warn("backup verification skipped: artifact is on an unavailable share", "app", a.Name, "backup", b.ID)
			continue
		}
		if err := m.verifyArtifact(ctx, a.Name, b); err != nil {
			m.Log.Error("backup verification", "app", a.Name, "backup", b.ID, "err", err)
			m.Store.CreateNotice("backup-verify-failed", a.Name,
				fmt.Sprintf("%s's newest backup failed verification", a.Name),
				err.Error()+" — the artifact may be unusable; take a fresh backup and investigate.")
			m.Notify("Backup verification failed: "+a.Name, err.Error(), false)
			continue
		}
		m.Store.SetBackupVerified(b.ID)
		m.Log.Info("backup verified", "app", a.Name, "backup", b.ID)
	}
}

// recentlyVerified: b was proven restorable within ReverifyAfter.
func recentlyVerified(b *store.Backup) bool {
	if b.VerifiedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, b.VerifiedAt)
	return err == nil && time.Since(t) < ReverifyAfter
}

// newestArtifactWithin is how far back among an app's backups its newest
// artifact is looked for.
const newestArtifactWithin = 50

func (m *Manager) newestArtifact(appID int64) *store.Backup {
	list, err := m.Store.ListBackups(appID, newestArtifactWithin)
	if err != nil {
		return nil
	}
	for i := range list {
		if list[i].Format == ArtifactFormat {
			return &list[i]
		}
	}
	return nil
}

// verifyArtifact re-hashes the artifact and test-restores its database dump
// into a scratch database, proving the two failure modes checksums alone
// can't: silent corruption since download, and a dump that doesn't restore.
func (m *Manager) verifyArtifact(ctx context.Context, appName string, b *store.Backup) error {
	// (a) The bytes on disk are still the bytes fog verified at download,
	// and (b) the database dump actually restores — one read of the
	// artifact serves both.
	return m.testRestore(ctx, appName, filepath.Join(b.Path, "artifact.tar"), b.Manifest, b.SHA256)
}

// TestRestore proves an artifact's database dump restores: it is extracted
// and loaded into a scratch database with a table-count sanity check. The
// offsite verifier calls it on an artifact fetched back from the locker.
//
// The dump is extracted under DataDir, never os.TempDir(): /tmp is tmpfs
// on the target OS, so a multi-GB dump there would consume RAM mid-verify
// and OOM fogd or its apps. DataDir is real disk with the space to hold
// what it backs up.
func (m *Manager) TestRestore(ctx context.Context, appName, artifact, manifest string) error {
	return m.testRestore(ctx, appName, artifact, manifest, "")
}

// testRestore is TestRestore that, given wantSHA, also proves the artifact
// still hashes to it — in the same pass that extracts the dump, and before
// anything is restored. Calls run one at a time (verifyMu): two verifiers
// on one app share its scratch database, and it spares the disk two
// multi-GB extractions at once.
func (m *Manager) testRestore(ctx context.Context, appName, artifact, manifest, wantSHA string) error {
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	f, err := os.Open(artifact)
	if err != nil {
		return fmt.Errorf("artifact unreadable: %w", err)
	}
	defer f.Close()
	if filesOnly(manifest) {
		return verifyComponents(f, manifest, wantSHA)
	}
	scratchDir, err := os.MkdirTemp(m.DataDir, "verify-scratch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratchDir)
	h := sha256.New()
	src := io.TeeReader(f, h)
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	dump, xerr := extractEntry(src, "db/primary.dump", scratchDir, size)
	if wantSHA != "" {
		// Corruption explains a broken tar better than the tar error does,
		// so the digest is settled first.
		if _, err := io.Copy(io.Discard, src); err != nil {
			return fmt.Errorf("artifact unreadable: %w", err)
		}
		if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, wantSHA) {
			return fmt.Errorf("artifact has changed on disk since download (sha256 %s, recorded %s) — storage corruption", sum[:12], wantSHA[:min(12, len(wantSHA))])
		}
	}
	if xerr != nil {
		return fmt.Errorf("extracting db/primary.dump: %w", xerr)
	}
	if m.PG == nil {
		return errors.New("no Postgres to test-restore into")
	}

	// Assumes app names never contain underscores (naming enforces this), so
	// the dash flattening cannot collide two apps onto one scratch database.
	scratch := "fog_verify_" + strings.ReplaceAll(appName, "-", "_")
	// The dump is app-supplied and runs SQL as whoever replays it, so it is
	// restored as a fresh, random-named, non-superuser role — dropped right
	// after — into a scratch database that role owns. A doctored dump cannot
	// escape into the shared cluster, and cannot set a usable password on a
	// role an attacker could then log in as. Extensions come from fog's own
	// manifest, not the dump, created by the admin before the restore.
	// A scratch database a crashed verify left behind belongs to a scratch
	// role that is gone; clear it so the restore below starts clean.
	m.PG.DropDatabase(ctx, scratch)
	scratchRole, dropRole, err := m.PG.ScratchRole(ctx)
	if err != nil {
		return err
	}
	// Drop the role only after the database it owns is gone (defers run
	// last-registered-first), or DROP ROLE refuses over the owned database.
	defer dropRole()
	defer m.PG.DropDatabase(context.WithoutCancel(ctx), scratch)
	if err := m.PG.RestoreDatabase(ctx, scratch, scratchRole, dump, m.declaredExtensions(appName)); err != nil {
		return fmt.Errorf("test-restore: %w", explainRestore(err, manifest, m.serverMajor()))
	}
	tables, err := m.PG.PublicTableCount(ctx, scratch)
	if err != nil {
		return fmt.Errorf("test-restore sanity check: %w", err)
	}
	if tables == 0 {
		return fmt.Errorf("test-restore produced zero tables — the dump is empty (the GitLab failure mode)")
	}
	return nil
}

// checkStaleness raises (and clears) the dead-man's-switch: a backup-enabled
// app whose newest backup is older than the schedule plus grace gets a
// persistent notice and one ntfy per day — silence is the failure mode this
// exists to catch, so the alert keys on absence of success. The verdict comes
// from FleetStatus, so the alarm and the fleet table can't drift apart — and
// both grant a freshly installed app the grace of its first scheduled run.
func (m *Manager) checkStaleness() {
	rows, err := FleetStatus(m.Store, time.Now())
	if err != nil {
		return
	}
	for _, row := range rows {
		if !row.Stale {
			// Includes the app that just caught up: the notice states an
			// ongoing condition, so it has to end when the condition does.
			m.Store.DismissNoticeByKind(NoticeStale, row.App)
			continue
		}
		age := "never"
		if row.LastAt != "" {
			if t, perr := time.Parse(time.RFC3339, row.LastAt); perr == nil {
				age = fmt.Sprintf("%.0f hours ago", time.Since(t).Hours())
			}
		}
		m.Store.CreateNotice(NoticeStale, row.App,
			fmt.Sprintf("%s has no recent backup", row.App),
			fmt.Sprintf("Newest backup: %s. Scheduled backups are enabled but not landing — check the app's backup surface and fog's logs.", age))
		// The push goes out once a day per app; the notice carries the
		// ongoing state.
		if m.staleAlerts.Allow(row.App, 24*time.Hour, time.Now()) {
			m.Notify("Backup overdue: "+row.App, "newest backup: "+age, false)
		}
	}
}

// declaredExtensions is the extensions the app's current spec declares, or
// none when there is no such app here. These — never the app-written
// manifest — are what widens the set of extensions a scratch restore may
// recreate beyond the trusted ones.
func (m *Manager) declaredExtensions(appName string) []string {
	if m.Store == nil {
		return nil
	}
	app, err := m.Store.AppByName(appName)
	if err != nil {
		return nil
	}
	spec, err := apps.LatestSpec(m.Store, app.ID)
	if err != nil || spec.Needs == nil || spec.Needs.Postgres == nil {
		return nil
	}
	return spec.Needs.Postgres.Extensions
}

// filesOnly reports whether a manifest declares components and none of them
// is a database: an app whose state is all files (newtab), with no dump to
// load into Postgres. A manifest that cannot be read is not files-only, so
// it meets the test-restore and fails there as before.
func filesOnly(manifest string) bool {
	var m Manifest
	if json.Unmarshal([]byte(manifest), &m) != nil || len(m.Components) == 0 {
		return false
	}
	for _, c := range m.Components {
		if c.Kind == "db" {
			return false
		}
	}
	return true
}

// verifyComponents proves a files-only artifact as far as fog can without
// knowing how the app unpacks it: the artifact still hashes to wantSHA, and
// every component the manifest lists is present and still the bytes its
// producer hashed. The app's own restore re-checks the same digests and
// has the final word on the contents.
func verifyComponents(artifact io.Reader, manifest, wantSHA string) error {
	var m Manifest
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		return fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	h := sha256.New()
	_, digests, err := scanTar(io.TeeReader(artifact, h))
	if wantSHA != "" {
		// As in testRestore: corruption explains a broken tar better than
		// the tar error does.
		if _, cerr := io.Copy(io.Discard, artifact); cerr == nil {
			if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, wantSHA) {
				return fmt.Errorf("artifact has changed on disk since download (sha256 %s, recorded %s) — storage corruption", sum[:12], wantSHA[:min(12, len(wantSHA))])
			}
		}
	}
	if err != nil {
		return fmt.Errorf("artifact unreadable: %w", err)
	}
	for _, c := range m.Components {
		got, ok := digests[c.Path]
		switch {
		case !ok:
			return fmt.Errorf("component %s is listed in the manifest but missing from the artifact", c.Path)
		case !strings.EqualFold(got.sha256, c.SHA256) || got.bytes != c.Bytes:
			return fmt.Errorf("component %s fails checksum verification — the artifact is corrupt", c.Path)
		}
	}
	return nil
}

// extractEntry copies one tar entry to a temp file under dir and returns its
// path. The caller picks dir precisely because os.TempDir() may be tmpfs.
// limit is the artifact's own size: a plain tar entry can never be larger
// than the tar it is in, so an entry claiming more (a sparse one, whose
// holes expand) is refused rather than written out.
func extractEntry(artifact io.Reader, name, dir string, limit int64) (string, error) {
	tr := tar.NewReader(artifact)
	out := ""
	guard := newEntryGuard()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if out != "" {
				os.Remove(out)
			}
			return "", err
		}
		if err := guard.count(); err != nil {
			if out != "" {
				os.Remove(out)
			}
			return "", err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		entry := strings.TrimPrefix(filepath.Clean(hdr.Name), "./")
		// A duplicate or ambiguous entry makes which copy "the" dump
		// undecidable — refuse rather than restore an arbitrary one.
		if err := guard.name(entry); err != nil {
			if out != "" {
				os.Remove(out)
			}
			return "", err
		}
		if entry != name {
			continue
		}
		if SparseEntry(hdr) || hdr.Size > limit {
			if out != "" {
				os.Remove(out)
			}
			if SparseEntry(hdr) {
				return "", errSparse(hdr.Name)
			}
			return "", fmt.Errorf("artifact entry %q claims %d bytes, more than the whole %d-byte artifact", hdr.Name, hdr.Size, limit)
		}
		f, err := os.CreateTemp(dir, "fog-verify-*.dump")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, limit)); err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", err
		}
		if err := f.Close(); err != nil {
			os.Remove(f.Name())
			return "", err
		}
		out = f.Name()
	}
	if out == "" {
		return "", fmt.Errorf("no %s in artifact", name)
	}
	return out, nil
}

// serverMajor is the managed cluster's on-disk major, 0 when unknown.
func (m *Manager) serverMajor() int {
	if m.PG == nil {
		return 0
	}
	st, err := m.PG.Inspect()
	if err != nil || st.Fresh {
		return 0
	}
	return st.Major
}

// explainRestore turns pg_restore's "unsupported version (1.16) in file
// header" into what it means: the app dumped with a newer pg_dump than
// fog's cluster can read. The artifact is fine; it is the verifier that
// is behind, and the fix is the cluster upgrade, not a fresh backup.
func explainRestore(err error, manifest string, serverMajor int) error {
	if err == nil || !strings.Contains(err.Error(), "unsupported version") || serverMajor == 0 {
		return err
	}
	var m struct {
		PG struct {
			DumpTool string `json:"dump_tool"`
		} `json:"pg"`
	}
	if json.Unmarshal([]byte(manifest), &m) != nil {
		return err
	}
	dumpMajor, ok := pg.ParseMajor(m.PG.DumpTool)
	if !ok || dumpMajor <= serverMajor {
		return err
	}
	return fmt.Errorf("the artifact was dumped by pg_dump %d, whose archive format fog's Postgres %d cannot read — the artifact is intact; it verifies once the cluster is upgraded (Settings → Storage → Database): %w", dumpMajor, serverMajor, err)
}
