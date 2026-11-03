package pg

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// The real thing, end to end, against docker: a cluster born on the old
// major in the legacy layout, provisioned like feed (vector + pg_trgm,
// an HNSW index, a password role), moved to the target major by the same
// code path the button runs, and checked row by row — then the same move
// with the verification forced to fail, to prove the rollback puts the old
// cluster back intact. Gated because it pulls images and takes a minute:
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestUpgradeIntegration -v
func TestUpgradeIntegration(t *testing.T) {
	if os.Getenv("FOG_PG_INTEGRATION") == "" {
		t.Skip("set FOG_PG_INTEGRATION=1 to run the docker-backed upgrade test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	for name, run := range map[string]func(t *testing.T, m *Manager, u *Upgrader){
		"success":              upgradeSucceeds,
		"rollback":             upgradeRollsBack,
		"refuses-live-writer":  upgradeRefusesLiveWriter,
		"denies-superuser":     upgradeDeniesSuperuserRestore,
		"app-extension-schema": upgradeCarriesAppExtensionSchema,
		"provision-seals":      provisionSealsUntilReady,
		"odd-schema-names":     upgradeCarriesOddSchemaNames,
		"trial-catches-bad-db": upgradeTrialCatchesUnrestorableDatabase,
		"trial-sees-roles":     upgradeTrialSeesTheRoles,
		"hang-rolls-back":      upgradeHangRollsBack,
		"cancel":               upgradeCancelsCleanly,
		"stop-fails":           upgradeStopFailureBringsClusterBack,
		"dump-cancel":          upgradeDumpCancelStopsContainer,
	} {
		t.Run(name, func(t *testing.T) {
			m := newIntegrationManager(t)
			run(t, m, &Upgrader{PG: m})
		})
	}
}

func newIntegrationManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	network := fmt.Sprintf("fog-pgtest-%d-%s", os.Getpid(), strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "-")))
	if out, err := exec.Command("docker", "network", "create", network).CombinedOutput(); err != nil {
		t.Fatalf("network create: %s", out)
	}
	// Provision keys the app's role password by app ID; the row must exist.
	if _, err := st.CreateApp("feed"); err != nil {
		t.Fatal(err)
	}
	m := &Manager{DataDir: dir, Store: st, Vault: vlt, Log: slog.Default(), Network: network}
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "down", "-v")
		down.Dir = m.dir()
		down.Run()
		exec.Command("docker", "network", "rm", network).Run()
	})
	// Born on the previous major, legacy layout — what every existing
	// install looks like.
	freshMajor, freshLayout = 16, LayoutLegacy
	t.Cleanup(func() { freshMajor, freshLayout = TargetMajor, LayoutNested })
	return m
}

// seed provisions a feed-shaped database and fills it; returns the row
// counts the upgrade must preserve.
func seed(t *testing.T, m *Manager) (map[string]int64, *Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := m.Provision(ctx, "feed", 1, []string{"feed_production", "feed_production_cache"}, []string{"vector", "pg_trgm", "citext"})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := m.Inspect(); err != nil || st.Major != 16 || st.Layout != LayoutLegacy {
		t.Fatalf("expected a legacy 16 cluster, got %+v, %v", st, err)
	}
	if _, err := m.psql(ctx, "feed_production", `
		CREATE TABLE items (id serial primary key, name citext, emb vector(3));
		CREATE INDEX ON items USING hnsw (emb vector_cosine_ops);
		CREATE INDEX ON items USING gin (name gin_trgm_ops);
		INSERT INTO items (name, emb) SELECT 'item ' || g, ARRAY[g, g*2, g*3]::vector FROM generate_series(1, 1000) g;
		CREATE TABLE empty_table (id int);
		CREATE SCHEMA audit; CREATE TABLE audit.log (id serial primary key, note text);
		INSERT INTO audit.log (note) VALUES ('a'), ('b'), ('c');
		ALTER TABLE items OWNER TO feed; ALTER TABLE audit.log OWNER TO feed;`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.psql(ctx, "feed_production_cache", `CREATE TABLE entries (k text primary key, v bytea); INSERT INTO entries VALUES ('x', '\x00'), ('y', '\x01');`); err != nil {
		t.Fatal(err)
	}
	rows, err := m.TableRowCounts(ctx, "feed_production")
	if err != nil {
		t.Fatal(err)
	}
	if rows["public.items"] != 1000 || rows["audit.log"] != 3 || rows["public.empty_table"] != 0 {
		t.Fatalf("seed counts: %v", rows)
	}
	return rows, conn
}

func upgradeSucceeds(t *testing.T, m *Manager, u *Upgrader) {
	before, conn := seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var stopped, started []string
	var strayErr strings.Builder
	u.Hooks = UpgradeHooks{
		// "Stopping" the app leaves a client session behind — the stray the
		// drain step exists for. It must be terminated before the inventory
		// is taken, not raced with.
		StopApps: func(context.Context) ([]string, error) {
			stopped = []string{"feed"}
			stray := exec.Command("docker", "run", "--rm", "--network", m.Network, "-e", "PGPASSWORD="+conn.Password, Images[16],
				"psql", "-h", ContainerName, "-U", conn.User, "-d", conn.DBName, "-c", "SELECT pg_sleep(120);")
			stray.Stderr = &strayErr
			if err := stray.Start(); err != nil {
				return stopped, err
			}
			go stray.Wait()
			// Wait until the session is visible server-side, so the test
			// proves the drain rather than the stray's connect latency.
			for i := 0; i < 20; i++ {
				rs, _ := m.psqlRows(context.Background(), "postgres", `SELECT count(*) FROM pg_stat_activity WHERE usename = 'feed';`)
				if len(rs) == 1 && rs[0] != "0" {
					return stopped, nil
				}
				time.Sleep(500 * time.Millisecond)
			}
			return stopped, fmt.Errorf("stray session never appeared: %s", strayErr.String())
		},
		StartApps: func(_ context.Context, names []string) error { started = names; return nil },
	}
	if err := u.run(ctx); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, strings.Join(u.Status().Lines, "\n"))
	}
	t.Log(strings.Join(u.Status().Lines, "\n"))
	if log := strings.Join(u.Status().Lines, "\n"); !strings.Contains(log, "terminating sessions still connected") {
		t.Fatalf("the stray session must have been terminated (stray stderr: %q):\n%s", strayErr.String(), log)
	}
	if len(started) != 1 || started[0] != "feed" {
		t.Fatalf("apps stopped %v must be started again, got %v", stopped, started)
	}
	st, err := m.Inspect()
	if err != nil || st.Major != TargetMajor || st.Layout != LayoutNested {
		t.Fatalf("expected a nested %d cluster, got %+v, %v", TargetMajor, st, err)
	}
	if _, err := os.Stat(m.dataDir() + ".pg16"); err != nil {
		t.Fatalf("the 16 data must be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.dataDir(), "16")); err == nil {
		t.Fatal("no 16 directory may exist inside the new data directory")
	}
	after, err := m.TableRowCounts(ctx, "feed_production")
	if err != nil {
		t.Fatal(err)
	}
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: %d rows, want %d", table, after[table], n)
		}
	}
	exts, err := m.Extensions(ctx, "feed_production")
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"vector", "pg_trgm", "citext"} {
		if _, ok := exts[ext]; !ok {
			t.Errorf("extension %s missing after upgrade: %v", ext, exts)
		}
	}
	// The app's role still logs in with its vault-stored password, the
	// server really is the target major, and the vector index answers.
	conn, err = m.Provision(ctx, "feed", 1, []string{"feed_production", "feed_production_cache"}, []string{"vector", "pg_trgm", "citext"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.psql(ctx, "feed_production", `SHOW server_version;`)
	if err != nil || !strings.Contains(out, fmt.Sprint(TargetMajor)+".") {
		t.Fatalf("server_version: %q, %v", out, err)
	}
	q := exec.Command("docker", "run", "--rm", "--network", m.Network, "-e", "PGPASSWORD="+conn.Password, Images[TargetMajor],
		"psql", "-h", ContainerName, "-U", conn.User, "-d", conn.DBName, "-A", "-t", "-c",
		`SELECT name FROM items ORDER BY emb <-> '[1,2,3]' LIMIT 1;`)
	if o, err := q.CombinedOutput(); err != nil || strings.TrimSpace(string(o)) != "item 1" {
		t.Fatalf("role login + vector query after upgrade: %q, %v", o, err)
	}
	if got := m.ClusterStatus(); got.Major != TargetMajor || got.UpgradeAvailable || len(got.Retired) != 1 {
		t.Fatalf("cluster status after upgrade: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(m.dir(), "upgrade")); err == nil {
		t.Error("the upgrade's dumps (every app's data) were left on disk")
	}
}

func upgradeRollsBack(t *testing.T, m *Manager, u *Upgrader) {
	before, _ := seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var started []string
	u.Hooks = UpgradeHooks{
		StopApps:  func(context.Context) ([]string, error) { return []string{"feed"}, nil },
		StartApps: func(_ context.Context, names []string) error { started = names; return nil },
	}
	u.failAfterRestore = true
	err := u.run(ctx)
	t.Log(strings.Join(u.Status().Lines, "\n"))
	if err == nil || !strings.Contains(err.Error(), "rolled back to Postgres 16") {
		t.Fatalf("expected a rolled-back error, got %v", err)
	}
	if !u.Status().RolledBack {
		t.Fatal("status must record the rollback")
	}
	if len(started) != 1 {
		t.Fatalf("apps must be restarted after a rollback, got %v", started)
	}
	st, err := m.Inspect()
	if err != nil || st.Major != 16 || st.Layout != LayoutLegacy {
		t.Fatalf("expected the legacy 16 cluster back, got %+v, %v", st, err)
	}
	if _, err := os.Stat(m.dataDir() + ".pg16"); err == nil {
		t.Fatal("data.pg16 must have been renamed back to data")
	}
	ents, _ := os.ReadDir(m.dir())
	var failed bool
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "data.pg18.failed-") {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the failed attempt must be kept aside")
	}
	after, err := m.TableRowCounts(ctx, "feed_production")
	if err != nil {
		t.Fatal(err)
	}
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s after rollback: %d rows, want %d", table, after[table], n)
		}
	}
	out, err := m.psql(ctx, "feed_production", `SHOW server_version;`)
	if err != nil || !strings.Contains(out, "16.") {
		t.Fatalf("server_version after rollback: %q, %v", out, err)
	}
}

// The upgrade dumps and restores every app's database. An app controls its
// own database, so it can plant a CHECK constraint whose function COPY … TO
// PROGRAM's a shell command — which runs as whoever restores the data. The
// upgrade must restore each database as its own non-superuser owner, so the
// trap cannot fire, while the data still comes across.
func upgradeDeniesSuperuserRestore(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Planted and filled as feed (non-super), so it does not fire now.
	trap := `
		CREATE FUNCTION public.chk(int) RETURNS boolean LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) THEN
				EXECUTE 'COPY (SELECT 1) TO PROGRAM ''touch /tmp/PWNED-UPGRADE''';
			END IF;
			RETURN true;
		END $$;
		ALTER FUNCTION public.chk(int) OWNER TO feed;
		CREATE TABLE loot (id int CHECK (public.chk(id)));
		ALTER TABLE loot OWNER TO feed;
		INSERT INTO loot VALUES (1), (2), (3);`
	plant := exec.Command("docker", "exec", "-i", ContainerName, "psql", "-v", "ON_ERROR_STOP=1", "-U", "feed", "-d", "feed_production")
	plant.Stdin = strings.NewReader(trap)
	if out, err := plant.CombinedOutput(); err != nil {
		t.Fatalf("planting the trap as feed: %s %v", out, err)
	}
	notFired := func(stage string) {
		t.Helper()
		if out, _ := exec.Command("docker", "exec", ContainerName, "sh", "-c", "ls /tmp/PWNED-UPGRADE 2>&1").CombinedOutput(); !strings.Contains(string(out), "No such file") {
			t.Fatalf("the CHECK function ran as a superuser %s: %s", stage, out)
		}
	}
	notFired("while seeding")

	u.Hooks = UpgradeHooks{
		StopApps:  func(context.Context) ([]string, error) { return []string{"feed"}, nil },
		StartApps: func(_ context.Context, names []string) error { return nil },
	}
	if err := u.run(ctx); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, strings.Join(u.Status().Lines, "\n"))
	}
	// The booby trap must not have fired during the restore into the new
	// cluster (same container name)...
	notFired("during the upgrade restore")
	// ...and the data came across — the restore really happened.
	rows, err := m.psqlRows(ctx, "feed_production", `SELECT count(*) FROM loot;`)
	if err != nil || len(rows) != 1 || rows[0] != "3" {
		t.Fatalf("the trapped table must still restore: %v %v", rows, err)
	}
}

// An app's role may create a trusted extension itself, in a schema of its
// own. That used to make every major upgrade roll back for the whole fleet:
// the extension was stripped and recreated only in public. It must come
// across, in its own schema.
func upgradeCarriesAppExtensionSchema(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := exec.Command("docker", "exec", "-i", ContainerName, "psql", "-v", "ON_ERROR_STOP=1", "-U", "feed", "-d", "feed_production")
	c.Stdin = strings.NewReader(`CREATE SCHEMA ext; CREATE EXTENSION pgcrypto SCHEMA ext; CREATE TABLE hashed (h bytea); INSERT INTO hashed VALUES (ext.digest('x', 'sha256'));`)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("app creating its own extension: %s %v", out, err)
	}
	u.Hooks = UpgradeHooks{
		StopApps:  func(context.Context) ([]string, error) { return []string{"feed"}, nil },
		StartApps: func(_ context.Context, names []string) error { return nil },
	}
	if err := u.run(ctx); err != nil {
		t.Fatalf("an app's own extension schema must not fail the upgrade: %v\n%s", err, strings.Join(u.Status().Lines, "\n"))
	}
	schemas, err := m.ExtensionSchemas(ctx, "feed_production")
	if err != nil || schemas["pgcrypto"] != "ext" || schemas["vector"] != "public" {
		t.Fatalf("extensions must come across in their schemas: %v %v", schemas, err)
	}
}

// One app names a schema with a newline in it. Its dump used to be
// unreadable to the restore's line-by-line parser, failing that database
// after the old cluster was retired and rolling the whole fleet back. The
// names are read whole now, the preflight reads every dump the same way
// before anything stops, and the upgrade goes through.
func upgradeCarriesOddSchemaNames(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := exec.Command("docker", "exec", "-i", ContainerName, "psql", "-v", "ON_ERROR_STOP=1", "-U", "feed", "-d", "feed_production")
	c.Stdin = strings.NewReader("CREATE SCHEMA \"we\nird\"; CREATE SCHEMA \"a;b\"; CREATE TABLE \"a;b\".t (n int); INSERT INTO \"a;b\".t VALUES (7);\n")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("app creating odd schemas: %s %v", out, err)
	}
	u.Hooks = UpgradeHooks{
		StopApps:  func(context.Context) ([]string, error) { return []string{"feed"}, nil },
		StartApps: func(_ context.Context, names []string) error { return nil },
	}
	if err := u.run(ctx); err != nil {
		t.Fatalf("an app's schema name must not fail the fleet's upgrade: %v\n%s", err, strings.Join(u.Status().Lines, "\n"))
	}
	rows, err := m.psqlRows(ctx, "feed_production", "SELECT n FROM \"a;b\".t;")
	if err != nil || len(rows) != 1 || rows[0] != "7" {
		t.Fatalf("data in an oddly named schema: %v %v", rows, err)
	}
}

// One app's database dumps fine but will not restore: a CHECK whose
// function reads another table that is loaded after it. The schema-only
// preflight cannot see that; it used to surface after the old cluster was
// retired, rolling the whole fleet back. The trial restore — the real
// restore, into a scratch Postgres of the target version, before anything
// is retired — refuses the upgrade and names the database, and nothing
// changes.
func upgradeTrialCatchesUnrestorableDatabase(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := exec.Command("docker", "exec", "-i", ContainerName, "psql", "-v", "ON_ERROR_STOP=1", "-U", "feed", "-d", "feed_production")
	c.Stdin = strings.NewReader(`CREATE TABLE z_allowed (v int); INSERT INTO z_allowed VALUES (1);
CREATE FUNCTION public.ok(x int) RETURNS bool LANGUAGE sql AS $$ SELECT EXISTS (SELECT 1 FROM public.z_allowed a WHERE a.v = x) $$;
CREATE TABLE a_items (v int CHECK (public.ok(v))); INSERT INTO a_items VALUES (1);
`)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("app creating a data-dependent CHECK: %s %v", out, err)
	}
	u.Hooks = UpgradeHooks{
		StopApps:  func(context.Context) ([]string, error) { return []string{"feed"}, nil },
		StartApps: func(_ context.Context, names []string) error { return nil },
	}
	err := u.run(ctx)
	if err == nil || !strings.Contains(err.Error(), "feed_production") || !strings.Contains(err.Error(), "would not restore") {
		t.Fatalf("the trial restore must refuse, naming the database: %v\n%s", err, strings.Join(u.Status().Lines, "\n"))
	}
	if u.Status().RolledBack {
		t.Fatal("refused before the point of no return: nothing to roll back")
	}
	st, ierr := m.Inspect()
	if ierr != nil || st.Major != 16 {
		t.Fatalf("the cluster must still be Postgres 16: %+v %v", st, ierr)
	}
	if _, serr := os.Stat(m.dataDir() + ".pg16"); serr == nil {
		t.Fatal("the old cluster must not have been retired")
	}
	if rows, rerr := m.psqlRows(ctx, "feed_production", "SELECT count(*) FROM a_items;"); rerr != nil || len(rows) != 1 || rows[0] != "1" {
		t.Fatalf("the live data must be untouched: %v %v", rows, rerr)
	}
	if out, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+trialContainer, "-q").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Fatal("the trial cluster was left behind")
	}
}

// plantHang gives feed_production a CHECK whose function loops forever
// once it is being restored and cond (SQL) holds.
func plantHang(t *testing.T, cond string) {
	t.Helper()
	c := exec.Command("docker", "exec", "-i", ContainerName, "psql", "-v", "ON_ERROR_STOP=1", "-U", "feed", "-d", "feed_production")
	c.Stdin = strings.NewReader(`CREATE FUNCTION public.spin(x int) RETURNS bool LANGUAGE plpgsql AS $$
BEGIN
  IF current_database() LIKE 'fog\_rst\_%' AND (` + cond + `) THEN
    LOOP PERFORM pg_sleep(0.2); END LOOP;
  END IF;
  RETURN true;
END $$;
CREATE TABLE spun (v int CHECK (public.spin(v))); INSERT INTO spun VALUES (1);
`)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("planting a hanging check: %s %v", out, err)
	}
}

func feedHooks(u *Upgrader) {
	u.Hooks = UpgradeHooks{
		StopApps:  func(context.Context) ([]string, error) { return []string{"feed"}, nil },
		StartApps: func(_ context.Context, names []string) error { return nil },
	}
}

func shortDeadline(t *testing.T) {
	old := RestoreStall
	RestoreStall = 20 * time.Second
	t.Cleanup(func() { RestoreStall = old })
}

func assertStillOld(t *testing.T, ctx context.Context, m *Manager, table string) {
	t.Helper()
	st, err := m.Inspect()
	if err != nil || st.Major != 16 {
		t.Fatalf("the cluster must be Postgres 16 again: %+v %v", st, err)
	}
	if _, err := os.Stat(m.dataDir() + ".pg16"); err == nil {
		t.Fatal("data.pg16 must not be left set aside")
	}
	if rows, err := m.psqlRows(ctx, "feed_production", "SELECT count(*) FROM "+table+";"); err != nil || len(rows) != 1 || rows[0] != "1" {
		t.Fatalf("the live data must be intact: %v %v", rows, err)
	}
	if out, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+trialContainer, "-q").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Fatal("the trial cluster was left behind")
	}
}

// A dump that hangs only where another app's role exists used to pass a
// trial cluster that had no roles, and hang the real restore. The trial now
// restores the roles first, so it hangs there — stopped by the restore's
// deadline, refused before anything is retired.
func upgradeTrialSeesTheRoles(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	shortDeadline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE neighbour;`); err != nil {
		t.Fatal(err)
	}
	plantHang(t, `EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'neighbour')`)
	feedHooks(u)
	err := u.run(ctx)
	if err == nil || !strings.Contains(err.Error(), "would not restore") || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("the trial must hang on the roles and be stopped: %v\n%s", err, strings.Join(u.Status().Lines, "\n"))
	}
	if u.Status().RolledBack {
		t.Fatal("refused before the point of no return: nothing to roll back")
	}
	assertStillOld(t, ctx, m, "spun")
}

// A dump that hangs in the real restore — however it dodged the trial —
// is stopped by the restore's deadline, and the upgrade rolls back on its
// own: the fleet is not left down holding the cluster's lock.
func upgradeHangRollsBack(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	shortDeadline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	plantHang(t, `true`)
	feedHooks(u)
	u.skipTrial = true
	err := u.run(ctx)
	if err == nil || !strings.Contains(err.Error(), "rolled back") || !u.Status().RolledBack {
		t.Fatalf("a hang after the point of no return must roll back: %v\n%s", err, strings.Join(u.Status().Lines, "\n"))
	}
	assertStillOld(t, ctx, m, "spun")
	if !m.mu.TryLock() {
		t.Fatal("the cluster lock is still held")
	}
	m.mu.Unlock()
}

// The operator cancels while the trial restore hangs: the upgrade stops,
// the apps come back on the old cluster, nothing is retired.
func upgradeCancelsCleanly(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	plantHang(t, `true`)
	feedHooks(u)
	if err := u.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for !strings.Contains(u.Status().Stage, "trial-restoring feed") {
		if !u.Status().Running {
			t.Fatalf("the upgrade ended before the trial: %+v", u.Status())
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	if err := u.Cancel(); err != nil {
		t.Fatal(err)
	}
	for u.Status().Running {
		time.Sleep(200 * time.Millisecond)
	}
	st := u.Status()
	if st.Error == "" || st.RolledBack {
		t.Fatalf("a cancel before the point of no return: %+v", st)
	}
	assertStillOld(t, ctx, m, "spun")
}

// A cancel landing while the old cluster is being stopped used to leave it
// down: the apps were handed back to a cluster that was not running. It is
// brought back up first. Afterwards the upgrade's dumps (every app's data)
// are gone from disk.
func upgradeStopFailureBringsClusterBack(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	feedHooks(u)
	u.failStop = true
	if err := u.run(ctx); err == nil {
		t.Fatal("the injected stop failure must fail the upgrade")
	}
	rows, err := m.psqlRows(ctx, "feed_production", "SELECT 1;")
	if err != nil || len(rows) != 1 {
		t.Fatalf("the old cluster must be running again: %v %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(m.dir(), "upgrade")); err == nil {
		t.Fatal("the upgrade's dumps were left on disk")
	}
}

// Cancelling an upgrade's dump used to kill only the docker client; the
// pg_dump container ran on. It is now named and removed.
func upgradeDumpCancelStopsContainer(t *testing.T, m *Manager, u *Upgrader) {
	seed(t, m)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := u.pgDumpToFile(ctx, Images[TargetMajor], filepath.Join(dir, "x.dump"), false, []string{"sleep", "600"})
	if err == nil {
		t.Fatal("a cancelled dump must fail")
	}
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "name=fog-pg-dump-", "-q").Output()
	if len(strings.TrimSpace(string(out))) != 0 {
		t.Fatal("the dump's container was left running")
	}
}

// Provision leaves the app's role able to log in and its database closed to
// every other role; a new role is created NOLOGIN and a new database sealed
// from its owner until fog's setup is done, so the app cannot race it.
func provisionSealsUntilReady(t *testing.T, m *Manager, u *Upgrader) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := m.Provision(ctx, "feed", 1, []string{"feed_production"}, []string{"pg_trgm"}); err != nil {
		t.Fatal(err)
	}
	rs, err := m.psqlRows(ctx, "postgres", `SELECT rolcanlogin::text FROM pg_roles WHERE rolname = 'feed';
		SELECT coalesce(datacl::text, '') FROM pg_database WHERE datname = 'feed_production';`)
	if err != nil || len(rs) != 2 || rs[0] != "true" {
		t.Fatalf("the role must be able to log in once provisioned: %v %v", rs, err)
	}
	if acl := rs[1]; strings.Contains(acl, "{=c") || strings.Contains(acl, ",=c") || !strings.Contains(acl, "feed=") {
		t.Fatalf("PUBLIC must not have CONNECT and the owner must: %s", acl)
	}
	schemas, err := m.ExtensionSchemas(ctx, "feed_production")
	if err != nil || schemas["pg_trgm"] != "public" {
		t.Fatalf("declared extension must be in public: %v %v", schemas, err)
	}
}

// An app fog "stopped" that is in fact still alive reconnects the moment
// its sessions are terminated. The upgrade must refuse before the dump —
// with the old cluster untouched, still running, and the apps restarted —
// because that process would keep writing rows the dump never sees.
func upgradeRefusesLiveWriter(t *testing.T, m *Manager, u *Upgrader) {
	before, conn := seed(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var started []string
	// A client that holds a session, writes, and reconnects the moment it
	// is cut off — a Rails connection pool in a container fog did not stop.
	// Named so the cleanup can remove the container; killing the docker
	// client alone would leave it running on the test network.
	name := "fog-pgtest-writer-" + fmt.Sprint(os.Getpid())
	writer := exec.Command("docker", "run", "--rm", "--name", name, "--network", m.Network, "-e", "PGPASSWORD="+conn.Password, Images[16],
		"sh", "-c", `while true; do psql -q -h `+ContainerName+` -U `+conn.User+` -d `+conn.DBName+` -c "INSERT INTO audit.log (note) VALUES ('live')" -c "SELECT pg_sleep(600)" >/dev/null 2>&1; sleep 0.2; done`)
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	go writer.Wait()
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", name).Run() })
	for i := 0; i < 20; i++ {
		rs, _ := m.psqlRows(context.Background(), "postgres", `SELECT count(*) FROM pg_stat_activity WHERE usename = 'feed';`)
		if len(rs) == 1 && rs[0] != "0" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	u.Hooks = UpgradeHooks{
		StopApps:  func(context.Context) ([]string, error) { return []string{"feed"}, nil },
		StartApps: func(_ context.Context, names []string) error { started = names; return nil },
	}
	err := u.run(ctx)
	t.Log(strings.Join(u.Status().Lines, "\n"))
	if err == nil || !strings.Contains(err.Error(), "reconnected after being terminated") {
		t.Fatalf("expected a refusal naming the live writer, got %v", err)
	}
	if len(started) != 1 {
		t.Fatalf("apps must be restarted after a refusal, got %v", started)
	}
	if strings.Contains(strings.Join(u.Status().Lines, "\n"), "dumping") {
		t.Fatal("the refusal must come before the dump")
	}
	st, err := m.Inspect()
	if err != nil || st.Major != 16 || st.Layout != LayoutLegacy {
		t.Fatalf("the old cluster must be untouched, got %+v, %v", st, err)
	}
	if _, err := os.Stat(m.dataDir() + ".pg16"); err == nil {
		t.Fatal("nothing may have been retired")
	}
	after, err := m.TableRowCounts(ctx, "feed_production")
	if err != nil {
		t.Fatal(err)
	}
	if after["public.items"] != before["public.items"] || after["audit.log"] < before["audit.log"] {
		t.Fatalf("data must be intact and the writer still writing: before %v, after %v", before, after)
	}
}
