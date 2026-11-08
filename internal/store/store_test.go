package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := openTest(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s.Close()
}

func TestSettings(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if v, err := s.GetSetting("missing"); err != nil || v != "" {
		t.Fatalf("missing setting: v=%q err=%v", v, err)
	}
	if err := s.SetSetting(SettingBaseDomain, "home.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(SettingBaseDomain, "home2.example.com"); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetSetting(SettingBaseDomain)
	if v != "home2.example.com" {
		t.Fatalf("got %q", v)
	}
}

func TestPeopleAndSessions(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if n, _ := s.CountOperators(); n != 0 {
		t.Fatalf("expected 0 operators, got %d", n)
	}
	id, err := s.CreatePerson("matthew", "Matthew", PersonRoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountOperators(); n != 1 {
		t.Fatalf("expected 1 operator, got %d", n)
	}
	if _, err := s.CreatePerson("Matthew", "again", PersonRoleMember); err == nil {
		t.Fatal("expected unique violation for case-insensitive duplicate handle")
	}
	p, err := s.PersonByHandle("matthew")
	if err != nil || p.ID != id {
		t.Fatalf("lookup: %v", err)
	}

	if _, err := s.CreatePersonSession(id, 0, "digest1", "web", time.Hour, "ua", "1.2.3.4", nil); err != nil {
		t.Fatal(err)
	}
	sess, sp, err := s.SessionPerson("digest1")
	if err != nil || sess.PersonID != id || sp.Handle != "matthew" {
		t.Fatalf("session: %v", err)
	}
	if _, _, err := s.SessionPerson("nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Expired sessions are dropped.
	if _, err := s.CreatePersonSession(id, 0, "digest2", "web", -time.Minute, "ua", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionPerson("digest2"); err != ErrNotFound {
		t.Fatalf("expected expiry, got %v", err)
	}
}

// A device's last_seen_at moves on a request only once it is a minute old,
// so a burst of requests from one device writes nothing.
func TestSessionPersonTouchesDeviceWhenStale(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	pid, err := s.CreatePerson("ann", "Ann", PersonRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := s.CreatePersonDevice(pid, "phone", "ios", []byte("k1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePersonSession(pid, dev, "d", "native", time.Hour, "", "", nil); err != nil {
		t.Fatal(err)
	}
	seen := func() string {
		d, err := s.PersonDeviceByID(dev)
		if err != nil {
			t.Fatal(err)
		}
		return d.LastSeenAt
	}
	recent := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)
	s.db.Exec(`UPDATE people_devices SET last_seen_at = ? WHERE id = ?`, recent, dev)
	if _, _, err := s.SessionPerson("d"); err != nil {
		t.Fatal(err)
	}
	if got := seen(); got != recent {
		t.Fatalf("fresh last_seen_at rewritten: %q → %q", recent, got)
	}
	old := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	s.db.Exec(`UPDATE people_devices SET last_seen_at = ? WHERE id = ?`, old, dev)
	if _, _, err := s.SessionPerson("d"); err != nil {
		t.Fatal(err)
	}
	if got := seen(); got == old {
		t.Fatal("stale last_seen_at was not moved")
	}
}

func TestSystemSecretRotation(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if err := s.SetSecret(0, "dns_api_token", "ct-v1"); err != nil {
		t.Fatal(err)
	}
	// The regression: a second write must REPLACE, and reads must see it.
	if err := s.SetSecret(0, "dns_api_token", "ct-v2"); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetSecret(0, "dns_api_token")
	if err != nil || v != "ct-v2" {
		t.Fatalf("rotation failed: got %q, %v", v, err)
	}
	keys, _ := s.SecretKeys(0)
	if len(keys) != 1 {
		t.Fatalf("expected 1 system secret, got %v", keys)
	}
	if err := s.DeleteSecret(0, "dns_api_token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSecret(0, "dns_api_token"); err != ErrNotFound {
		t.Fatalf("expected deletion, got %v", err)
	}
}

func TestAudit(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if err := s.Audit("matthew", "settings.update", "base_domain", ""); err != nil {
		t.Fatal(err)
	}
	entries, err := s.RecentAudit(10)
	if err != nil || len(entries) != 1 || entries[0].Action != "settings.update" {
		t.Fatalf("audit: %v %v", entries, err)
	}
}

// The activity log reads back past its first page from the oldest entry it
// holds: strictly older, newest first.
func TestAuditBefore(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	for _, a := range []string{"one", "two", "three"} {
		if err := s.Audit("matthew", a, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.RecentAudit(10)
	if err != nil || len(all) != 3 {
		t.Fatalf("recent: %v %v", all, err)
	}
	older, err := s.AuditBefore(all[0].ID, 10)
	if err != nil || len(older) != 2 || older[0].Action != "two" || older[1].Action != "one" {
		t.Fatalf("before %d: %v %v", all[0].ID, older, err)
	}
}

// The app page's activity feed is one app's own entries: subject match or
// the app acting through fog, never a substring hit on another subject.
func TestAppAudit(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	for _, e := range [][3]string{
		{"admin", "app.restart", "hub"},
		{"system", "backup.create", "hub"},
		{"admin", "app.restart", "hubble"},
		{"app:hub", "federation.vouch.mint", "peer-1"},
		{"admin", "settings.update", "base_domain"},
	} {
		if err := s.Audit(e[0], e[1], e[2], ""); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.AppAudit("hub", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries for hub, got %d: %+v", len(entries), entries)
	}
	if entries[0].Action != "federation.vouch.mint" || entries[2].Action != "app.restart" {
		t.Fatalf("wrong order or set: %+v", entries)
	}
	for _, e := range entries {
		if e.Subject == "hubble" {
			t.Fatalf("substring match leaked: %+v", e)
		}
	}
}

// Notices persist until dismissed, and repeats of the same (kind, app)
// replace the active notice instead of stacking.
func TestNotices(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if err := s.CreateNotice("deploy-key-cleanup", "manor", "old title", "old body"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNotice("deploy-key-cleanup", "manor", "new title", "new body"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNotice("deploy-key-cleanup", "hub", "hub title", ""); err != nil {
		t.Fatal(err)
	}

	active, err := s.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("got %d active notices, want 2: %+v", len(active), active)
	}
	var manor *Notice
	for i := range active {
		if active[i].App == "manor" {
			manor = &active[i]
		}
	}
	if manor == nil || manor.Title != "new title" || manor.Body != "new body" {
		t.Fatalf("dedupe did not replace: %+v", manor)
	}

	if err := s.DismissNotice(manor.ID); err != nil {
		t.Fatal(err)
	}
	active, _ = s.ActiveNotices()
	if len(active) != 1 || active[0].App != "hub" {
		t.Fatalf("dismiss failed: %+v", active)
	}
	if n, err := s.CountActiveNotices(); err != nil || n != 1 {
		t.Fatalf("active count after dismiss: %d %v, want 1", n, err)
	}

	// A fresh notice after dismissal creates a new row, not a resurrection.
	if err := s.CreateNotice("deploy-key-cleanup", "manor", "again", ""); err != nil {
		t.Fatal(err)
	}
	active, _ = s.ActiveNotices()
	if len(active) != 2 {
		t.Fatalf("got %d active after re-create, want 2", len(active))
	}
}

// Concurrent producers of the same (kind, app) must converge on one active
// row: the partial unique index makes CreateNotice's upsert atomic where the
// old check-then-insert let racers stack duplicates.
func TestCreateNoticeConcurrent(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.CreateNotice("backup-stale", "manor", fmt.Sprintf("title %d", i), "body")
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	active, err := s.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("got %d active notices, want 1: %+v", len(active), active)
	}

	// Dismissal ends the row's life; the next create is a fresh row, not a
	// resurrection of the dismissed one.
	old := active[0].ID
	if err := s.DismissNotice(old); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNotice("backup-stale", "manor", "fresh", ""); err != nil {
		t.Fatal(err)
	}
	active, err = s.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID == old || active[0].Title != "fresh" {
		t.Fatalf("re-create after dismiss: %+v (old id %d)", active, old)
	}
}

// A database that accumulated duplicate active notices under the racy
// CreateNotice upgrades cleanly: 0024 keeps the newest of each (kind, app)
// group, leaves dismissed history alone, and the unique index holds after.
func TestNoticesDedupeMigration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "fog.db")
	db := openAtMigration(t, path, "0024")
	rows := []struct{ created, kind, app, title, dismissed string }{
		{"2026-01-01T00:00:00Z", "backup-stale", "manor", "oldest", ""},
		{"2026-01-03T00:00:00Z", "backup-stale", "manor", "keep", ""},
		{"2026-01-02T00:00:00Z", "backup-stale", "manor", "middle", ""},
		{"2026-01-01T00:00:00Z", "backup-stale", "manor", "gone", "2026-01-02T00:00:00Z"},
		{"2026-01-02T00:00:00Z", "backup-stale", "manor", "gone too", "2026-01-03T00:00:00Z"},
		{"2026-01-01T00:00:00Z", "dns-degraded", "", "solo", ""},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO notices (created_at, kind, app, title, body, dismissed_at)
			VALUES (?, ?, ?, ?, '', ?)`, r.created, r.kind, r.app, r.title, nullable(r.dismissed)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	active, err := s.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("got %d active after dedupe, want 2: %+v", len(active), active)
	}
	for _, n := range active {
		if n.App == "manor" && n.Title != "keep" {
			t.Fatalf("dedupe kept the wrong row: %+v", n)
		}
	}
	// Dismissed history is not the migration's business.
	var dismissed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notices WHERE dismissed_at IS NOT NULL`).Scan(&dismissed); err != nil {
		t.Fatal(err)
	}
	if dismissed != 2 {
		t.Fatalf("got %d dismissed rows, want 2", dismissed)
	}
}

// A '#' or '?' in the database path must not truncate the DSN's pragma list:
// the driver splits the DSN at the first '?', and SQLite's URI parser treats
// '#' as a fragment marker. journal_mode proves the pragmas landed — an
// unescaped path silently opens a different file in rollback-journal mode.
func TestOpenPathWithURIMetacharacters(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "we#ird?dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal (path metacharacters truncated the pragmas)", mode)
	}
	if _, err := os.Stat(filepath.Join(dir, "fog.db")); err != nil {
		t.Fatalf("database landed somewhere else: %v", err)
	}
}

func TestUpdateNasShare(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if _, err := s.CreateNasShare(NasShare{
		Name: "media", Proto: "nfs", Server: "192.168.1.111",
		Remote: "/volume1/media", MountPoint: "/mnt/nas/media", Enabled: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sh, _ := s.GetNasShare("media")
	sh.Server = "192.168.4.20"
	sh.Options = "soft"
	if err := s.UpdateNasShare("media", sh); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetNasShare("media")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Server != "192.168.4.20" || got.Options != "soft" {
		t.Errorf("update not applied: %+v", got)
	}
	if !got.Enabled || got.MountPoint != "/mnt/nas/media" {
		t.Errorf("update touched fields it shouldn't: %+v", got)
	}
}

func TestPruneAudit(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	for _, action := range []string{"app.create", "app.deploy", "app.stop"} {
		if err := s.Audit("matthew", action, "manor", ""); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate one entry past any sane retention window; the other two are
	// fresh and must survive a realistic cutoff.
	old := time.Now().UTC().Add(-365 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE audit_log SET at = ? WHERE action = 'app.create'`, old); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().UTC().Add(-180 * 24 * time.Hour).Format(time.RFC3339)
	n, err := s.PruneAudit(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	rest, err := s.RecentAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 {
		t.Fatalf("%d entries survive, want 2", len(rest))
	}
	for _, e := range rest {
		if e.Action == "app.create" {
			t.Fatal("the backdated entry survived its cutoff")
		}
	}
	// Idempotent: nothing left older than the cutoff.
	if n, _ := s.PruneAudit(cutoff); n != 0 {
		t.Fatalf("second prune deleted %d rows", n)
	}
}
