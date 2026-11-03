package pg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitPGReadyTCP blocks until the shared container accepts a TCP connection
// on 127.0.0.1. The postgres image runs a temporary init server on a unix
// socket first, then restarts for the real one; a unix-socket "SELECT 1"
// probe can land on that init server and race the restart. The init server
// does not listen on TCP, so pg_isready -h 127.0.0.1 is true only once the
// real server is up.
func waitPGReadyTCP(t *testing.T) {
	t.Helper()
	for i := 0; ; i++ {
		if exec.Command("docker", "exec", ContainerName, "pg_isready", "-h", "127.0.0.1", "-U", "postgres").Run() == nil {
			return
		}
		if i == 120 {
			t.Fatal("postgres never became ready on TCP")
		}
		time.Sleep(time.Second)
	}
}

// A backup restore hands the database and its tables back to the app's
// role. Restored with --no-owner as postgres, the app could no longer read
// its own data. Runs against a bare postgres container under fog's
// container name, so it needs that name free:
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestRestoreKeepsOwner -v
func TestRestoreKeepsOwner(t *testing.T) {
	if os.Getenv("FOG_PG_INTEGRATION") == "" {
		t.Skip("set FOG_PG_INTEGRATION=1 to run the docker-backed restore test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if exec.Command("docker", "inspect", ContainerName).Run() == nil {
		t.Skipf("a %s container already exists", ContainerName)
	}
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", ContainerName, "-e", "POSTGRES_PASSWORD=x", Images[16]).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %s", out)
	}
	// -v: the image declares a volume, and without it every run leaks an
	// anonymous volume holding the test cluster's data.
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", ContainerName).Run() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m := &Manager{}
	waitPGReadyTCP(t)
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE feed LOGIN; CREATE DATABASE feed_production OWNER feed;`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.psql(ctx, "feed_production", `CREATE EXTENSION IF NOT EXISTS pg_trgm;
		CREATE TABLE items (id serial primary key, name text); INSERT INTO items (name) VALUES ('a'), ('b');
		ALTER TABLE items OWNER TO feed;`); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "feed.dump")
	f, err := os.Create(dump)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DumpDatabase(ctx, "feed_production", f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := m.RestoreDatabase(ctx, "feed_production", RoleName("feed"), dump, []string{"pg_trgm"}); err != nil {
		t.Fatal(err)
	}
	owners, err := m.psqlRows(ctx, "feed_production", `SELECT tableowner FROM pg_tables WHERE tablename = 'items';
		SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'feed_production';`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(owners, " "); got != "feed feed" {
		t.Fatalf("table and database owners after restore: %s", got)
	}
	out, err := exec.CommandContext(ctx, "docker", "exec", ContainerName, "psql", "-tA", "-U", "feed", "-d", "feed_production", "-c", "SELECT count(*) FROM items").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "2" {
		t.Fatalf("the app role must read its restored table: %s %v", out, err)
	}
	// The scratch restore runs as the non-superuser VerifyRole and still works.
	if err := m.RestoreDatabase(ctx, "scratch", VerifyRole, dump, []string{"pg_trgm"}); err != nil {
		t.Fatalf("scratch restore: %v", err)
	}
	// And it must have run with no more than that role's power: the scratch
	// database is owned by VerifyRole, not postgres.
	owner, err := m.psqlRows(ctx, "postgres", `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'scratch';`)
	if err != nil || len(owner) != 1 || owner[0] != VerifyRole {
		t.Fatalf("scratch database owner: %v %v", owner, err)
	}
	if super, _ := m.psqlRows(ctx, "postgres", `SELECT rolsuper FROM pg_roles WHERE rolname = '`+VerifyRole+`';`); len(super) != 1 || super[0] != "f" {
		t.Fatalf("%s must be NOSUPERUSER, got %v", VerifyRole, super)
	}
}

// A delegated app produces its own dump, so a malicious app can doctor it to
// run SQL as whoever restores it. The classic Postgres vector: a CHECK
// constraint whose function COPY … TO PROGRAM's a shell command, which fires
// when the constraint validates the restored rows. Restored as the superuser
// it is remote code execution inside fog-postgres and a read of every other
// app's database; restored as a non-superuser role it is denied. This proves
// RestoreDatabase runs the dump with no more than that role's power — the
// program never runs — while still restoring the data.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestRestoreDeniesSuperuserDump -v
func TestRestoreDeniesSuperuserDump(t *testing.T) {
	if os.Getenv("FOG_PG_INTEGRATION") == "" {
		t.Skip("set FOG_PG_INTEGRATION=1 to run the docker-backed restore test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if exec.Command("docker", "inspect", ContainerName).Run() == nil {
		t.Skipf("a %s container already exists", ContainerName)
	}
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", ContainerName, "-e", "POSTGRES_PASSWORD=x", Images[18]).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %s", out)
	}
	// -v: the image declares a volume, and without it every run leaks an
	// anonymous volume holding the test cluster's data.
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", ContainerName).Run() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m := &Manager{}
	waitPGReadyTCP(t)
	// The app, legitimately, owns its own database and plants the booby trap
	// in it. The marker fires only when current_user is a superuser.
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE evilapp LOGIN; CREATE DATABASE evilapp_db OWNER evilapp;`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.psql(ctx, "evilapp_db", `
		CREATE FUNCTION public.chk(int) RETURNS boolean LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) THEN
				EXECUTE 'COPY (SELECT 1) TO PROGRAM ''touch /tmp/PWNED-BY-RESTORE''';
			END IF;
			RETURN true;
		END $$;
		ALTER FUNCTION public.chk(int) OWNER TO evilapp;
		CREATE TABLE loot (id int CHECK (public.chk(id)));
		INSERT INTO loot VALUES (1), (2), (3);
		ALTER TABLE loot OWNER TO evilapp;`); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "evil.dump")
	f, err := os.Create(dump)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DumpDatabase(ctx, "evilapp_db", f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Seeding the rows above ran as the superuser, so the CHECK already fired
	// once — which is the proof the vector is real. Clear the marker so the
	// assertion below is strictly about what the restore does.
	if out, err := exec.CommandContext(ctx, "docker", "exec", ContainerName, "sh", "-c", "ls /tmp/PWNED-BY-RESTORE && rm -f /tmp/PWNED-BY-RESTORE").CombinedOutput(); err != nil {
		t.Fatalf("the booby trap should have fired at superuser seed time, proving the vector: %s %v", out, err)
	}

	// The verify path: restore the app-supplied dump into a scratch database.
	if err := m.RestoreDatabase(ctx, "fog_verify_evilapp", VerifyRole, dump, nil); err != nil {
		t.Fatalf("restore (expected to succeed under a non-superuser role): %v", err)
	}
	// The booby trap must not have run.
	if out, _ := exec.CommandContext(ctx, "docker", "exec", ContainerName, "sh", "-c", "ls /tmp/PWNED-BY-RESTORE 2>&1").CombinedOutput(); !strings.Contains(string(out), "No such file") {
		t.Fatalf("COPY TO PROGRAM ran during restore — escalation not contained: %s", out)
	}
	// …and the restore is real: the table and its rows came across, validated
	// by the CHECK (which returned true for a non-superuser).
	rows, err := m.psqlRows(ctx, "fog_verify_evilapp", `SELECT count(*) FROM loot;`)
	if err != nil || len(rows) != 1 || rows[0] != "3" {
		t.Fatalf("the data must still restore: %v %v", rows, err)
	}
}

// startBarePG runs a bare postgres container under fog's name and returns a
// Manager that talks to it over docker exec (no compose, no host binds). The
// search_path and ownership checks run entirely through that API.
func startBarePG(t *testing.T) *Manager {
	t.Helper()
	if os.Getenv("FOG_PG_INTEGRATION") == "" {
		t.Skip("set FOG_PG_INTEGRATION=1 to run the docker-backed test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if exec.Command("docker", "inspect", ContainerName).Run() == nil {
		t.Skipf("a %s container already exists", ContainerName)
	}
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", ContainerName, "-e", "POSTGRES_PASSWORD=x", Images[18]).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %s", out)
	}
	// -v: the image declares a volume, and without it every run leaks an
	// anonymous volume holding the test cluster's data.
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", ContainerName).Run() })
	waitPGReadyTCP(t)
	return &Manager{}
}

func marker(t *testing.T, path string) bool {
	t.Helper()
	out, _ := exec.Command("docker", "exec", ContainerName, "sh", "-c", "ls "+path+" 2>&1").CombinedOutput()
	return !strings.Contains(string(out), "No such file")
}

// An app owns its own database, so it can set a database-level search_path and
// plant objects in public (a pg_extension view, an = operator) that a
// superuser catalog query would resolve in place of the real ones — and run
// as fog. Every such query must pin its search_path and qualify, so the
// planted objects are never reached.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestSearchPathHijackDenied -v
func TestSearchPathHijackDenied(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	// Planted as app (non-superuser), so nothing fires during setup.
	if _, err := m.psql(ctx, "appdb", `
		CREATE FUNCTION public.boom() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user) THEN
				EXECUTE 'COPY (SELECT 1) TO PROGRAM ''touch /tmp/PWN-EXT''';
			END IF; RETURN 'v';
		END $$;
		CREATE VIEW public.pg_extension AS SELECT 'x'::name AS extname, public.boom() AS extversion;
		CREATE FUNCTION public.opboom(name, name) RETURNS boolean LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user) THEN
				EXECUTE 'COPY (SELECT 1) TO PROGRAM ''touch /tmp/PWN-OP''';
			END IF; RETURN pg_catalog.texteq($1::text, $2::text);
		END $$;
		CREATE OPERATOR public.= (leftarg = name, rightarg = name, procedure = public.opboom);
		CREATE TABLE public.realtable (id int);
		ALTER DATABASE appdb SET search_path = public, pg_catalog;`); err != nil {
		t.Fatal(err)
	}
	// Extensions: must read pg_catalog.pg_extension (which lists plpgsql),
	// never the planted public.pg_extension view.
	exts, err := m.Extensions(ctx, "appdb")
	if err != nil {
		t.Fatalf("Extensions: %v", err)
	}
	if _, ok := exts["plpgsql"]; !ok {
		t.Fatalf("Extensions must read the real catalog (expected plpgsql), got %v", exts)
	}
	if marker(t, "/tmp/PWN-EXT") {
		t.Fatal("the planted pg_extension view ran as the superuser")
	}
	// PublicTableCount: the = operator must bind to pg_catalog, not public.
	n, err := m.PublicTableCount(ctx, "appdb")
	if err != nil {
		t.Fatalf("PublicTableCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("PublicTableCount = %d, want 1", n)
	}
	if marker(t, "/tmp/PWN-OP") {
		t.Fatal("the planted = operator ran as the superuser")
	}
}

// A database name is an app's claim, not a ticket to a neighbour's data.
// Provisioning, dropping, or restoring a database another app owns is
// refused, and another app's role cannot even connect.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestAppDatabaseOwnership -v
func TestAppDatabaseOwnership(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// appa owns shared_db, secured exactly as Provision does.
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE appa LOGIN; CREATE ROLE appb LOGIN;
		CREATE DATABASE shared_db OWNER appa; REVOKE CONNECT ON DATABASE shared_db FROM PUBLIC;`); err != nil {
		t.Fatal(err)
	}
	// appb's role cannot connect to appa's database.
	if out, err := exec.Command("docker", "exec", ContainerName, "psql", "-U", "appb", "-d", "shared_db", "-c", "SELECT 1").CombinedOutput(); err == nil {
		t.Fatalf("appb must not connect to appa's database: %s", out)
	}
	// Deprovision by appb must not drop appa's database.
	if err := m.Deprovision(ctx, "appb", []string{"shared_db"}); err == nil {
		t.Fatal("Deprovision of a database appb does not own must be refused")
	}
	if owner, exists, err := m.databaseOwner(ctx, "shared_db"); err != nil || !exists || owner != "appa" {
		t.Fatalf("shared_db must survive appb's deprovision, owned by appa: %q exists=%v err=%v", owner, exists, err)
	}
	// Restoring appb's backup must not recreate appa's database owned by appb.
	dump := filepath.Join(t.TempDir(), "x.dump")
	f, _ := os.Create(dump)
	f.Close()
	if err := m.RestoreDatabase(ctx, "shared_db", "appb", dump, nil); err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("restoring over a database appb does not own must be refused, got %v", err)
	}
	if owner, _, _ := m.databaseOwner(ctx, "shared_db"); owner != "appa" {
		t.Fatalf("shared_db owner changed to %q", owner)
	}
}

// Provisioning adds a newly declared extension to a database the app has
// already had. An extension's install script runs as the superuser with its
// search_path set to the schema it installs into, and fog used to let that be
// whatever the app's database-level search_path named first — a schema the
// app owns and has filled. The install must land in public via a schema only
// the superuser can write, never in the app's schema, and leave nothing behind.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestExtensionInstallIgnoresAppSearchPath -v
func TestExtensionInstallIgnoresAppSearchPath(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	// As the app: its own schema, first on the database's search_path.
	plant := exec.Command("docker", "exec", "-i", ContainerName, "psql", "-v", "ON_ERROR_STOP=1", "-U", "app", "-d", "appdb")
	plant.Stdin = strings.NewReader(`CREATE SCHEMA evil; ALTER DATABASE appdb SET search_path = evil, public;`)
	if out, err := plant.CombinedOutput(); err != nil {
		t.Fatalf("planting as the app: %s %v", out, err)
	}
	if err := m.installExtensions(ctx, "appdb", publicTargets([]string{"pg_trgm"}), false); err != nil {
		t.Fatalf("installExtensions: %v", err)
	}
	rows, err := m.psqlRowsSafe(ctx, "appdb", `SELECT n.nspname FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) e.extnamespace WHERE e.extname OPERATOR(pg_catalog.=) 'pg_trgm';`)
	if err != nil || len(rows) != 1 || rows[0] != "public" {
		t.Fatalf("pg_trgm must land in public, never the app's schema: %v %v", rows, err)
	}
	if left, _ := m.psqlRowsSafe(ctx, "appdb", `SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname LIKE 'fog\_ext\_%';`); len(left) != 0 {
		t.Fatalf("the staging schema must be dropped: %v", left)
	}
	// Idempotent.
	if err := m.installExtensions(ctx, "appdb", publicTargets([]string{"pg_trgm"}), false); err != nil {
		t.Fatalf("a second install must be a no-op: %v", err)
	}
}

// asApp runs SQL connected as a non-superuser role (local trust), the way an
// app's own session would.
func asApp(t *testing.T, role, db, sql string) {
	t.Helper()
	c := exec.Command("docker", "exec", "-i", ContainerName, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", role, "-d", db)
	c.Stdin = strings.NewReader(sql)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("as %s on %s: %s %v", role, db, out, err)
	}
}

func dumpOf(t *testing.T, ctx context.Context, m *Manager, db string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), db+".dump")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := m.DumpDatabase(ctx, db, f); err != nil {
		t.Fatal(err)
	}
	return p
}

// An app's database owner may create trusted extensions itself and put them
// in a schema of its own. A restore used to strip those and recreate only the
// spec's, in public — and fail with the live database already dropped. Now
// every extension the dump uses comes back, in its own schema, and the data
// with it.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestRestoreRecreatesAppExtensionsInTheirSchemas -v
func TestRestoreRecreatesAppExtensionsInTheirSchemas(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	asApp(t, "app", "appdb", `CREATE SCHEMA ext; CREATE EXTENSION citext SCHEMA ext; CREATE EXTENSION pgcrypto;
		CREATE TABLE people (name ext.citext, h bytea); INSERT INTO people VALUES ('Ada', digest('x', 'sha256'));`)
	dump := dumpOf(t, ctx, m, "appdb")
	asApp(t, "app", "appdb", `INSERT INTO people VALUES ('written after the dump', NULL);`)

	if err := m.RestoreDatabase(ctx, "appdb", "app", dump, nil); err != nil {
		t.Fatalf("restore over the live database: %v", err)
	}
	schemas, err := m.ExtensionSchemas(ctx, "appdb")
	if err != nil || schemas["citext"] != "ext" || schemas["pgcrypto"] != "public" {
		t.Fatalf("extensions must come back in the dump's schemas: %v %v", schemas, err)
	}
	rows, err := m.psqlRows(ctx, "appdb", `SELECT count(*) FROM people WHERE name OPERATOR(ext.=) 'ADA'::ext.citext;`) // citext's own =, case-insensitive; the post-dump row is gone
	if err != nil || len(rows) != 1 || rows[0] != "1" {
		t.Fatalf("the restored data (and citext's behaviour) must be the dump's: %v %v", rows, err)
	}
	if left, _ := m.psqlRows(ctx, "postgres", `SELECT datname FROM pg_database WHERE datname LIKE 'fog\_%';`); len(left) != 0 {
		t.Fatalf("no temporary or replaced database may be left behind: %v", left)
	}
}

// A restore that cannot complete must leave the live database exactly as it
// was — whether it is refused up front (an extension the app is not allowed)
// or fails part way (an object only a superuser can create).
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestFailedRestoreKeepsLiveDatabase -v
func TestFailedRestoreKeepsLiveDatabase(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;
		CREATE DATABASE needsdblink OWNER app; CREATE DATABASE needsevt OWNER app;`); err != nil {
		t.Fatal(err)
	}
	asApp(t, "app", "appdb", `CREATE TABLE live (x int); INSERT INTO live VALUES (42);`)
	intact := func(stage string) {
		t.Helper()
		rows, err := m.psqlRows(ctx, "appdb", `SELECT x FROM live;`)
		if err != nil || len(rows) != 1 || rows[0] != "42" {
			t.Fatalf("%s: the live database was disturbed: %v %v", stage, rows, err)
		}
		if left, _ := m.psqlRows(ctx, "postgres", `SELECT datname FROM pg_database WHERE datname LIKE 'fog\_%';`); len(left) != 0 {
			t.Fatalf("%s: a temporary database was left behind: %v", stage, left)
		}
	}

	// Refused up front: dblink is not trusted and the app never declared it.
	if _, err := m.psql(ctx, "needsdblink", `CREATE EXTENSION dblink; CREATE TABLE t (x int);`); err != nil {
		t.Fatal(err)
	}
	dblinkDump := dumpOf(t, ctx, m, "needsdblink")
	err := m.RestoreDatabase(ctx, "appdb", "app", dblinkDump, nil)
	if err == nil || !strings.Contains(err.Error(), "dblink") || !strings.Contains(err.Error(), "declare") {
		t.Fatalf("an untrusted, undeclared extension must be refused by name: %v", err)
	}
	intact("refused extension")

	// Fails part way: an event trigger only a superuser can create.
	if _, err := m.psql(ctx, "needsevt", `CREATE TABLE t (x int); INSERT INTO t VALUES (1);
		CREATE FUNCTION evt() RETURNS event_trigger LANGUAGE plpgsql AS $$ BEGIN END $$;
		CREATE EVENT TRIGGER e ON ddl_command_start EXECUTE FUNCTION evt();`); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreDatabase(ctx, "appdb", "app", dumpOf(t, ctx, m, "needsevt"), nil); err == nil {
		t.Fatal("a dump that cannot fully restore as the app's role must fail")
	}
	intact("failed part way")

	// A truncated dump.
	trunc := filepath.Join(t.TempDir(), "trunc.dump")
	full, _ := os.ReadFile(dblinkDump)
	os.WriteFile(trunc, full[:len(full)/2], 0o600)
	if err := m.RestoreDatabase(ctx, "appdb", "app", trunc, nil); err == nil {
		t.Fatal("a truncated dump must fail")
	}
	intact("truncated dump")

	// And with dblink declared by the spec, the same dump restores.
	if err := m.RestoreDatabase(ctx, "appdb", "app", dblinkDump, []string{"dblink"}); err != nil {
		t.Fatalf("a declared extension must be recreated: %v", err)
	}
}

// Installing an extension into a database the app has already reached — even
// one fog only just created — must never run the install script with a path
// the app can steer: the install goes through a schema only the superuser can
// write, then is moved where it belongs.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestExtensionInstallStagesEvenWhenFresh -v
func TestExtensionInstallStagesEvenWhenFresh(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE fresh OWNER app;`); err != nil {
		t.Fatal(err)
	}
	// The app gets in first and steers the database's search_path.
	asApp(t, "app", "fresh", `CREATE SCHEMA evil; ALTER DATABASE fresh SET search_path = evil, public;`)
	if err := m.installExtensions(ctx, "fresh", publicTargets([]string{"pg_trgm", "citext"}), true); err != nil {
		t.Fatal(err)
	}
	schemas, err := m.ExtensionSchemas(ctx, "fresh")
	if err != nil || schemas["pg_trgm"] != "public" || schemas["citext"] != "public" {
		t.Fatalf("extensions must land in public, never the app's schema: %v %v", schemas, err)
	}
	if left, _ := m.psqlRowsSafe(ctx, "fresh", `SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname LIKE 'fog\_ext\_%';`); len(left) != 0 {
		t.Fatalf("the staging schema must be dropped: %v", left)
	}
}

// An app names a schema with a newline and another with a semicolon, puts
// a table and an extension in them, and its dump restores whole: the names
// are read as names, not as lines.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestRestoreOddSchemaNames -v
func TestRestoreOddSchemaNames(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	asApp(t, "app", "appdb", "CREATE SCHEMA \"we\nird\"; CREATE SCHEMA \"a;b\"; CREATE EXTENSION citext SCHEMA \"we\nird\";\n"+
		"CREATE TABLE \"a;b\".t (n \"we\nird\".citext); INSERT INTO \"a;b\".t VALUES ('Ada');\n")
	dump := dumpOf(t, ctx, m, "appdb")
	if err := m.RestoreDatabase(ctx, "appdb", "app", dump, nil); err != nil {
		t.Fatalf("restore of a dump with odd schema names: %v", err)
	}
	rows, err := m.psqlRows(ctx, "appdb", "SELECT count(*) FROM \"a;b\".t WHERE n OPERATOR(\"we\nird\".=) 'ADA';")
	if err != nil || len(rows) != 1 || rows[0] != "1" {
		t.Fatalf("restored data: %v %v", rows, err)
	}
}

// A doctored dump that places a relocatable extension in pg_catalog is
// refused: fog would otherwise install it there as the superuser, ahead of
// everything on every role's search path.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestRestoreRefusesExtensionInSystemSchema -v
func TestRestoreRefusesExtensionInSystemSchema(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// The superuser builds the shape an app's doctored dump would carry.
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE src; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.psql(ctx, "src", `CREATE EXTENSION citext SCHEMA pg_catalog;`); err != nil {
		t.Fatal(err)
	}
	dump := dumpOf(t, ctx, m, "src")
	err := m.RestoreDatabase(ctx, "appdb", "app", dump, []string{"citext"})
	if err == nil || !strings.Contains(err.Error(), "system schema") {
		t.Fatalf("restore placing an extension in pg_catalog: %v", err)
	}
	if rows, _ := m.psqlRows(ctx, "appdb", `SELECT count(*) FROM pg_extension WHERE extname = 'citext';`); len(rows) != 1 || rows[0] != "0" {
		t.Fatalf("the live database gained the extension: %v", rows)
	}
}

// A restore leftover — the temporary database or the replaced one, left by
// a crash — is swept (Provision runs the sweep), and nothing else
// is touched.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestLeakedRestoreDatabasesAreSwept -v
func TestLeakedRestoreDatabasesAreSwept(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE fog_old_0123456789abcdef OWNER app; CREATE DATABASE fog_rst_fedcba9876543210 OWNER app; CREATE DATABASE fog_other OWNER app;`); err != nil {
		t.Fatal(err)
	}
	m.sweepLeakedLocked(ctx)
	left, _ := m.psqlRows(ctx, "postgres", `SELECT datname FROM pg_database WHERE datname LIKE 'fog\_%' ORDER BY 1;`)
	if strings.Join(left, ",") != "fog_other" {
		t.Fatalf("after the sweep: %v", left)
	}
}

// A dump whose CHECK constraint loops forever once it sees it is being
// restored (current_database() is a fog_rst_ name) used to hold the restore
// — and fog's lock on the cluster, and with it every other app's backups —
// for good. A restore that stops moving is now stopped: the looping session
// terminated, the temporary database dropped, the live database left as it
// was, and the lock released — however small the dump, whatever it claims.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestHangingDumpIsStopped -v
func TestHangingDumpIsStopped(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	old := RestoreStall
	RestoreStall = 10 * time.Second
	t.Cleanup(func() { RestoreStall = old })
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	asApp(t, "app", "appdb", `CREATE FUNCTION public.spin(x int) RETURNS bool LANGUAGE plpgsql AS $$
BEGIN
  IF current_database() LIKE 'fog\_rst\_%' THEN
    LOOP PERFORM pg_sleep(0.2); END LOOP;
  END IF;
  RETURN true;
END $$;
CREATE TABLE items (v int CHECK (public.spin(v))); INSERT INTO items VALUES (1), (2);`)
	dump := dumpOf(t, ctx, m, "appdb")

	start := time.Now()
	err := m.RestoreDatabase(ctx, "appdb", "app", dump, nil)
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("a hanging restore must be stopped once it stalls: %v", err)
	}
	if d := time.Since(start); d > 90*time.Second {
		t.Fatalf("stopping took %s", d)
	}
	if left, _ := m.psqlRows(ctx, "postgres", `SELECT datname FROM pg_database WHERE datname LIKE 'fog\_%';`); len(left) != 0 {
		t.Fatalf("the temporary database was left behind: %v", left)
	}
	if busy, _ := m.psqlRows(ctx, "postgres", `SELECT count(*) FROM pg_stat_activity WHERE datname LIKE 'fog\_rst\_%';`); len(busy) != 1 || busy[0] != "0" {
		t.Fatalf("the looping session was not terminated: %v", busy)
	}
	if rows, _ := m.psqlRows(ctx, "appdb", `SELECT count(*) FROM items;`); len(rows) != 1 || rows[0] != "2" {
		t.Fatalf("the live database changed: %v", rows)
	}
	if !m.mu.TryLock() {
		t.Fatal("the cluster lock is still held")
	}
	m.mu.Unlock()
}

// The stall detector must not cut off an honest restore that simply takes
// a while: a large, highly compressible table (a small dump that takes far
// longer than the stall window to load and index) restores whole, because
// it keeps moving the whole time.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestLargeHonestRestoreIsNotCutOff -v
func TestLargeHonestRestoreIsNotCutOff(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	old := RestoreStall
	RestoreStall = 3 * time.Second
	t.Cleanup(func() { RestoreStall = old })
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	asApp(t, "app", "appdb", `CREATE TABLE big (id int, body text);
INSERT INTO big SELECT g, repeat('fog', 40) FROM generate_series(1, 3000000) g;
CREATE INDEX big_body ON big (body, id);`)
	dump := dumpOf(t, ctx, m, "appdb")
	start := time.Now()
	if err := m.RestoreDatabase(ctx, "appdb", "app", dump, nil); err != nil {
		t.Fatalf("an honest restore was stopped: %v", err)
	}
	t.Logf("restored in %s with a %s stall window", time.Since(start).Round(time.Second), RestoreStall)
	if rows, _ := m.psqlRows(ctx, "appdb", `SELECT count(*) FROM big;`); len(rows) != 1 || rows[0] != "3000000" {
		t.Fatalf("rows: %v", rows)
	}
}

// A restore an app can make slow used to hold the cluster lock that every
// deploy (Provision) needed exclusively, so one app's slow dump stalled
// every other app. Provision now shares the lock with restores, and its
// leftover sweep leaves a restore in flight alone.
//
//	FOG_PG_INTEGRATION=1 go test ./internal/pg/ -run TestRestoreDoesNotBlockProvisioning -v
func TestRestoreDoesNotBlockProvisioning(t *testing.T) {
	m := startBarePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	old := RestoreStall
	RestoreStall = 15 * time.Second
	t.Cleanup(func() { RestoreStall = old })
	if _, err := m.psql(ctx, "postgres", `CREATE ROLE app LOGIN; CREATE DATABASE appdb OWNER app;`); err != nil {
		t.Fatal(err)
	}
	asApp(t, "app", "appdb", `CREATE FUNCTION public.spin(x int) RETURNS bool LANGUAGE plpgsql AS $$
BEGIN
  IF current_database() LIKE 'fog\_rst\_%' THEN LOOP PERFORM pg_sleep(0.2); END LOOP; END IF;
  RETURN true;
END $$;
CREATE TABLE items (v int CHECK (public.spin(v))); INSERT INTO items VALUES (1);`)
	dump := dumpOf(t, ctx, m, "appdb")
	done := make(chan error, 1)
	go func() { done <- m.RestoreDatabase(ctx, "appdb", "app", dump, nil) }()
	var tmp string
	for tmp == "" {
		select {
		case err := <-done:
			t.Fatalf("the restore ended before it could be watched: %v", err)
		default:
		}
		rows, _ := m.psqlRows(ctx, "postgres", `SELECT datname FROM pg_database WHERE datname LIKE 'fog\_rst\_%';`)
		if len(rows) == 1 {
			tmp = rows[0]
		}
		time.Sleep(200 * time.Millisecond)
	}
	// What Provision takes, it gets, while the restore runs.
	if !m.provMu.TryLock() || !m.mu.TryRLock() {
		t.Fatal("a running restore blocks provisioning")
	}
	m.sweepLeakedLocked(ctx)
	m.mu.RUnlock()
	m.provMu.Unlock()
	if rows, _ := m.psqlRows(ctx, "postgres", fmt.Sprintf(`SELECT count(*) FROM pg_database WHERE datname = '%s';`, tmp)); len(rows) != 1 || rows[0] != "1" {
		t.Fatal("the leftover sweep dropped a restore in flight")
	}
	if err := <-done; err == nil {
		t.Fatal("the hanging restore must still be stopped")
	}
}
