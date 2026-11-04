package backup

import (
	"archive/tar"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

func TestBackupHour(t *testing.T) {
	h := 4
	withOverride := &apps.AppSpec{Backup: &apps.BackupSpec{Enabled: true, HourUTC: &h}}
	plain := &apps.AppSpec{Backup: &apps.BackupSpec{Enabled: true}}

	if got, ok := backupHour(withOverride, ""); !ok || got != 4 {
		t.Fatalf("override: got %d ok=%v", got, ok)
	}
	if got, ok := backupHour(plain, "3"); !ok || got != 3 {
		t.Fatalf("global: got %d ok=%v", got, ok)
	}
	if _, ok := backupHour(plain, ""); ok {
		t.Fatal("no hour configured should mean off")
	}
	if _, ok := backupHour(plain, "25"); ok {
		t.Fatal("out-of-range global hour should mean off")
	}
}

func TestSnapshotsBeforeUpdate(t *testing.T) {
	on, off := true, false
	spec := func(enabled bool, before *bool) *apps.AppSpec {
		return &apps.AppSpec{Backup: &apps.BackupSpec{Enabled: enabled, BeforeUpdate: before}}
	}
	cases := []struct {
		name   string
		spec   *apps.AppSpec
		global string
		want   bool
	}{
		{"no backup spec", &apps.AppSpec{}, "on", false},
		{"backups off beats override", spec(false, &on), "on", false},
		{"unset global means on", spec(true, nil), "", true},
		{"follows global on", spec(true, nil), "on", true},
		{"follows global off", spec(true, nil), "off", false},
		{"app on overrides global off", spec(true, &on), "off", true},
		{"app off overrides global on", spec(true, &off), "", false},
	}
	for _, c := range cases {
		if got := SnapshotsBeforeUpdate(c.spec, c.global); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	manifest := `{"format":"fog-backup/1","app":"feed"}`
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(manifest))
	payload := "not the manifest"
	tw.WriteHeader(&tar.Header{Name: "db/primary.dump", Mode: 0o600, Size: int64(len(payload))})
	tw.Write([]byte(payload))
	tw.Close()
	f.Close()

	got, err := readManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != manifest {
		t.Fatalf("got %q", got)
	}
}

// prune must give pre-update snapshots their own clock and never delete the
// final remaining backup.
func TestPrune(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app, err := st.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	mk := func(kind string) int64 {
		p := filepath.Join(dir, kind+"-"+time.Now().Format("150405.000000000"))
		os.MkdirAll(p, 0o700)
		id, err := st.CreateBackup(app.ID, kind, p, 1)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	m := &Manager{Store: st}

	// Ten snapshots on one day: the history keeps one per day, but the
	// newest three always stay.
	for i := 0; i < 10; i++ {
		mk("scheduled")
	}
	// A fresh pre-update snapshot must survive regardless of count.
	preID := mk("pre-update")
	m.prune(app.ID)
	list, _ := st.ListBackups(app.ID, 100)
	if len(list) != 4 {
		t.Fatalf("want 3 regular + 1 pre-update, got %d", len(list))
	}
	foundPre := false
	for _, b := range list {
		if b.ID == preID {
			foundPre = true
		}
	}
	if !foundPre {
		t.Fatal("fresh pre-update snapshot was pruned")
	}

	// Snapshots from before a version change are capped by count as well as
	// age: five fresh ones (updates and rollbacks alike) keep the newest three.
	var pre []int64
	for i := 0; i < 5; i++ {
		kind := "pre-update"
		if i == 3 {
			kind = "pre-rollback"
		}
		pre = append(pre, mk(kind))
	}
	m.prune(app.ID)
	list, _ = st.ListBackups(app.ID, 100)
	if len(list) != 6 {
		t.Fatalf("want 3 regular + 3 pre-update, got %d", len(list))
	}
	kept := map[int64]bool{}
	for _, b := range list {
		kept[b.ID] = true
	}
	for _, id := range pre[2:] {
		if !kept[id] {
			t.Fatalf("one of the newest three pre-update snapshots (%d) was pruned", id)
		}
	}
	if kept[preID] {
		t.Fatal("the oldest pre-update snapshot outlived the count cap")
	}
}

// The history keeps a backup per day for two weeks, per week for eight,
// per month for a year — and nothing older.
func TestKeepByHistory(t *testing.T) {
	now := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	var list []store.Backup
	for d := 0; d < 800; d++ { // a nightly backup for 800 days, newest first
		list = append(list, store.Backup{ID: int64(1000 - d), CreatedAt: now.AddDate(0, 0, -d).Format(time.RFC3339)})
	}
	keep := keepByHistory(list, PolicyFor(""))
	kept := 0
	oldest := now
	for _, b := range list {
		if keep[b.ID] {
			kept++
			t0, _ := time.Parse(time.RFC3339, b.CreatedAt)
			oldest = t0
		}
	}
	// 14 daily, weeks 3–8 beyond them, months beyond those up to 12.
	if kept < 14+6+9 || kept > 14+8+12 {
		t.Fatalf("kept %d backups", kept)
	}
	if age := now.Sub(oldest); age < 300*24*time.Hour || age > 370*24*time.Hour {
		t.Fatalf("oldest kept is %v old, want about a year", age)
	}
	for _, b := range list[:14] {
		if !keep[b.ID] {
			t.Fatal("one of the last 14 nights was dropped")
		}
	}
	if k := keepByHistory(list, PolicyFor("month")); len(k) > 14+4+1 {
		t.Fatalf("a month's history kept %d", len(k))
	}
}

// A backup directory sitting under an unmounted NAS share must refuse writes
// (backups would otherwise land silently on the local disk), and prune must
// not forget snapshots whose share is currently away.
func TestUnmountedShareGuards(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateNasShare(store.NasShare{
		Name: "backups", Proto: "nfs", Server: "nas.lan",
		Remote: "/volume1/backups", MountPoint: "/mnt/nas-test-nonexistent", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	st.SetSetting(SettingBackupDir, "/mnt/nas-test-nonexistent/backups")

	m := &Manager{Store: st}
	if err := m.checkTarget(); err == nil {
		t.Fatal("checkTarget should refuse a backup dir on an unmounted share")
	}

	// Rows for snapshots on the absent share survive pruning untouched.
	app, err := st.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := st.CreateBackup(app.ID, "scheduled", "/mnt/nas-test-nonexistent/backups/demo/x", 1); err != nil {
			t.Fatal(err)
		}
	}
	m.prune(app.ID)
	list, _ := st.ListBackups(app.ID, 100)
	if len(list) != 10 {
		t.Fatalf("prune deleted records of snapshots on an unavailable share: %d left", len(list))
	}

	// With the share mounted (simulated by pointing the dir elsewhere), the
	// guard lifts.
	st.SetSetting(SettingBackupDir, t.TempDir())
	if err := m.checkTarget(); err != nil {
		t.Fatalf("checkTarget should pass for a local dir: %v", err)
	}
}

// swapVar replaces a package-level seam for one test and restores it after.
func swapVar[T any](t *testing.T, v *T, val T) {
	t.Helper()
	orig := *v
	*v = val
	t.Cleanup(func() { *v = orig })
}

// A share that is in the mount table but no longer answering — a hard mount
// whose server died — must refuse backups exactly like an unmounted one.
// Passing it through would not just misplace data: the first tar or hash
// against it blocks in uninterruptible sleep and wedges the calling loop.
func TestStaleMountedShareRefused(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const mp = "/mnt/nas-test-stale"
	if _, err := st.CreateNasShare(store.NasShare{
		Name: "backups", Proto: "nfs", Server: "nas.lan",
		Remote: "/volume1/backups", MountPoint: mp, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	st.SetSetting(SettingBackupDir, mp+"/backups")

	swapVar(t, &mountedPoints, func() map[string]bool { return map[string]bool{mp: true} })
	swapVar(t, &unresponsive, func(string) bool { return true })

	m := &Manager{Store: st}
	err = m.checkTarget()
	if err == nil {
		t.Fatal("checkTarget accepted a backup dir on a mounted-but-dead share")
	}
	if !strings.Contains(err.Error(), "not responding") {
		t.Fatalf("error should name the stale state, got: %v", err)
	}

	// prune must also treat the stale share as away: rows survive untouched.
	app, err := st.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := st.CreateBackup(app.ID, "scheduled", mp+"/backups/demo/x", 1); err != nil {
			t.Fatal(err)
		}
	}
	m.prune(app.ID)
	if list, _ := st.ListBackups(app.ID, 100); len(list) != 10 {
		t.Fatalf("prune deleted records of snapshots on a stale share: %d left", len(list))
	}

	// The same share answering again lifts the guard.
	unresponsive = func(string) bool { return false }
	if err := m.checkTarget(); err != nil {
		t.Fatalf("checkTarget should pass once the share answers: %v", err)
	}
}

// DeleteBackup against files on an unavailable share must refuse loudly:
// RemoveAll would delete nothing while dropping the row erases fog's only
// memory that the files exist — leaked on the NAS with no record.
func TestDeleteBackupRefusesUnavailableShare(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const mp = "/mnt/nas-test-nonexistent"
	if _, err := st.CreateNasShare(store.NasShare{
		Name: "backups", Proto: "nfs", Server: "nas.lan",
		Remote: "/volume1/backups", MountPoint: mp, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateBackup(app.ID, "scheduled", mp+"/backups/demo/x", 1)
	if err != nil {
		t.Fatal(err)
	}

	m := &Manager{Store: st}
	err = m.DeleteBackup(context.Background(), id, true)
	if err == nil {
		t.Fatal("DeleteBackup accepted a path on an unmounted share")
	}
	if !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("error should name the share state, got: %v", err)
	}
	if _, err := st.BackupByID(id); err != nil {
		t.Fatalf("refused delete must leave the row intact: %v", err)
	}

	// The same delete against local files goes through, files and row both.
	localDir := filepath.Join(t.TempDir(), "demo", "x")
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	localID, err := st.CreateBackup(app.ID, "scheduled", localDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteBackup(context.Background(), localID, true); err != nil {
		t.Fatalf("delete of a local backup: %v", err)
	}
	if _, err := os.Stat(localDir); !os.IsNotExist(err) {
		t.Fatal("backup files survived the delete")
	}
	if _, err := st.BackupByID(localID); err == nil {
		t.Fatal("backup row survived the delete")
	}
}

// The verify scratch dump must land in the directory the caller picked
// (DataDir), never os.TempDir(): /tmp is tmpfs on the target OS, so a
// multi-GB dump extracted there is consumed straight out of RAM.
func TestExtractEntryUsesGivenDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	payload := "pretend pg_dump output"
	if err := tw.WriteHeader(&tar.Header{Name: "db/primary.dump", Mode: 0o600, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(payload))
	tw.Close()
	f.Close()

	scratch := t.TempDir()
	art, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer art.Close()
	dump, err := extractEntry(art, "db/primary.dump", scratch, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dump) != scratch {
		t.Fatalf("dump extracted to %s, want it under %s", dump, scratch)
	}
	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("extracted %q", got)
	}
}

// fakeDockerRunning stubs docker like fakeDocker (restore_legacy_test.go),
// but reports one running container for `compose ps -q`, so the snapshot
// path exercises its pause/unpause bracketing.
func fakeDockerRunning(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	logPath := filepath.Join(bin, "invocations.log")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logPath + "\"\ncase \"$*\" in *\"ps -q\"*) echo cid123 ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// legacyBackupManager builds a Manager plus a stored app with one managed
// volume holding a file, ready for backupApp's legacy path. No postgres in
// the spec, so PG stays nil and untouched.
func legacyBackupManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app, err := st.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"source":{"type":"image","image":"demo:latest"},"volumes":[{"name":"data","mount":"/data"}],"backup":{"enabled":true}}`
	if _, err := st.CreateRevision(app.ID, spec, "", "test"); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		DataDir: t.TempDir(),
		Store:   st,
		Mat:     &apps.Materializer{DataDir: t.TempDir()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notify:  func(string, string, bool) {},
	}
	if err := os.MkdirAll(m.Mat.AppDir("demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	volDir := filepath.Join(m.Mat.DataPath("demo"), "data")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "state.txt"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.SetSetting(SettingBackupDir, t.TempDir())
	return m, st
}

// A legacy snapshot of a running app must freeze its containers around the
// volume tar — the consistency the package doc promises — and unfreeze them
// after, exactly once.
func TestLegacyBackupPausesRunningApp(t *testing.T) {
	dockerLog := fakeDockerRunning(t)
	m, st := legacyBackupManager(t)

	id, err := m.BackupApp(context.Background(), "demo", "manual")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.BackupByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.Path, "volume-data.tar")); err != nil {
		t.Fatalf("snapshot archive missing: %v", err)
	}
	log, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	if lines := composeVerbs(string(log)); lines != "compose ps -q\ncompose pause\ncompose unpause" {
		t.Fatalf("compose invocations around the snapshot: %q", lines)
	}
}

// A stopped app has nothing to freeze: its files aren't changing, and a
// pause that cannot succeed must not fail the backup.
func TestLegacyBackupSkipsPauseWhenStopped(t *testing.T) {
	dockerLog := fakeDocker(t, "0") // ps -q prints nothing: no containers
	m, st := legacyBackupManager(t)

	id, err := m.BackupApp(context.Background(), "demo", "manual")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.BackupByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.Path, "volume-data.tar")); err != nil {
		t.Fatalf("snapshot archive missing: %v", err)
	}
	log, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	if lines := composeVerbs(string(log)); lines != "compose ps -q" {
		t.Fatalf("a stopped app should only be probed, got: %q", lines)
	}
}

// fog's own directories under the backups root must be names no app can
// take, or that app's backups and fog's trees would share a directory.
func TestReservedDirsAreReservedAppNames(t *testing.T) {
	for _, d := range ReservedDirs {
		if apps.ValidateName(d) == nil {
			t.Errorf("an app may be named %q, which is a backups-root directory of fog's", d)
		}
	}
}

// Two snapshots of one app in the same second used to share a directory —
// MkdirAll adopted the first one's, and the second wrote over its files —
// so both rows pointed at one snapshot. Each must get its own.
func TestSameSecondSnapshotsGetTheirOwnDirectories(t *testing.T) {
	fakeDocker(t, "0")
	m, st := legacyBackupManager(t)

	var paths []string
	for range 3 {
		id, err := m.BackupApp(context.Background(), "demo", "manual")
		if err != nil {
			t.Fatal(err)
		}
		b, err := st.BackupByID(id)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, b.Path)
	}
	if paths[0] == paths[1] || paths[1] == paths[2] || paths[0] == paths[2] {
		t.Fatalf("snapshots share a directory: %q", paths)
	}
	// Older directories are retired once the store holds them; every
	// backup still reads whole, wherever it lives.
	t.Cleanup(m.CloseChunkStore)
	list, _ := st.ListBackups(1, 10)
	for i := range list {
		r, _, err := m.OpenBackupFile(&list[i], "meta.json")
		if err != nil {
			t.Fatalf("snapshot %s incomplete: %v", list[i].Path, err)
		}
		r.Close()
	}
}

// A snapshot whose row cannot be recorded is not a backup, and its
// directory must not stay behind for the orphan scan to find.
func TestFailedSnapshotLeavesNoDirectory(t *testing.T) {
	fakeDocker(t, "0")
	m, st := legacyBackupManager(t)
	if _, err := st.DB().Exec(`CREATE TRIGGER no_backups BEFORE INSERT ON backups BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := m.BackupApp(context.Background(), "demo", "manual"); err == nil {
		t.Fatal("backup succeeded with its row refused")
	}
	ents, err := os.ReadDir(filepath.Join(m.baseDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("a failed snapshot left %d entries behind, first %s", len(ents), ents[0].Name())
	}
}

// composeVerbs reduces a fake docker's log to "compose <subcommand…>" per
// line, dropping the global options ComposeCmd adds (project name and
// directory, compose file, env file), so a test asserts what was asked of
// compose rather than how the invocation is spelled.
func composeVerbs(log string) string {
	withValue := map[string]bool{"--project-name": true, "-p": true, "--project-directory": true, "-f": true, "--file": true, "--env-file": true}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		kept := []string{f[0]}
		for i := 1; i < len(f); i++ {
			if withValue[f[i]] {
				i++
				continue
			}
			kept = append(kept, f[i])
		}
		out = append(out, strings.Join(kept, " "))
	}
	return strings.Join(out, "\n")
}
