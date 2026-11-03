// Moving the shared cluster to a new major. The data directory is the
// source of truth for which major runs (Inspect); this file is the one
// path that changes it, and it is built so that every step before the
// old cluster is retired is read-only, and every step after can be undone
// by putting the old directory back.
//
// Mechanism: dump and restore with the target major's own tools, into a
// fresh initdb. Not pg_upgrade — at home-server data sizes the simplicity
// wins, a fresh cluster carries no catalog cruft across, and the round trip
// is checked by exact row counts rather than trusted. The old data
// directory is renamed, never deleted; the janitor offers it for reclaim.
package pg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/randx"
)

// UpgradeHooks is what the upgrade needs from the rest of fog. Function
// fields rather than an engine dependency: apps imports pg, and the
// integration test drives the whole path with no engine at all.
type UpgradeHooks struct {
	// Busy reports an app mid-deploy (its name, true) — the upgrade refuses
	// to start under it rather than stopping it halfway through a release.
	Busy func() (string, bool)
	// StopApps stops every app that uses the shared cluster and returns the
	// names that were running, so exactly those come back afterwards.
	StopApps func(ctx context.Context) ([]string, error)
	// StartApps brings the named apps back — after success and after a
	// rollback alike.
	StartApps func(ctx context.Context, names []string) error
	// AfterUpgrade runs once the apps are back (an issues sweep, so every
	// finding this move resolves clears without waiting for the next tick).
	AfterUpgrade func()
}

// UpgradeStatus is the live/last state of the upgrade, polled by the UI.
type UpgradeStatus struct {
	Running    bool     `json:"running"`
	Done       bool     `json:"done"`
	Error      string   `json:"error,omitempty"`
	RolledBack bool     `json:"rolledBack"`
	Stage      string   `json:"stage,omitempty"`
	From       int      `json:"from,omitempty"`
	To         int      `json:"to,omitempty"`
	Lines      []string `json:"lines"`
	StartedAt  string   `json:"startedAt,omitempty"`
	FinishedAt string   `json:"finishedAt,omitempty"`
}

// Upgrader runs at most one upgrade at a time and remembers the last one.
type Upgrader struct {
	PG    *Manager
	Hooks UpgradeHooks

	mu     sync.Mutex
	status UpgradeStatus
	cancel context.CancelFunc

	// failAfterRestore makes the verification fail after a successful
	// restore — the integration test's way of proving the rollback path
	// puts the old cluster back with every row intact.
	failAfterRestore bool
	// failStop makes the cluster stop report failure after it has run —
	// the test's stand-in for a cancel landing mid-stop.
	failStop bool
	// skipTrial leaves out the trial restore — the integration test's way
	// of reaching the real restore with a dump that hangs there.
	skipTrial bool
}

// ErrUpgradeRunning is returned by Start while an upgrade is in flight.
var ErrUpgradeRunning = errors.New("a Postgres upgrade is already running")

func (u *Upgrader) Status() UpgradeStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.status
	st.Lines = append([]string(nil), st.Lines...)
	return st
}

func (u *Upgrader) set(f func(st *UpgradeStatus)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f(&u.status)
}

func (u *Upgrader) stage(s string) {
	u.PG.Log.Info("postgres upgrade", "stage", s)
	u.set(func(st *UpgradeStatus) {
		st.Stage = s
		st.Lines = append(st.Lines, time.Now().UTC().Format("15:04:05")+"  "+s)
	})
}

// Start begins the upgrade in the background and returns once it is
// claimed. The context should outlive the request that triggered it.
func (u *Upgrader) Start(ctx context.Context) error {
	u.mu.Lock()
	if u.status.Running {
		u.mu.Unlock()
		return ErrUpgradeRunning
	}
	u.status = UpgradeStatus{Running: true, StartedAt: time.Now().UTC().Format(time.RFC3339), Lines: []string{}}
	ctx, cancel := context.WithCancel(ctx)
	u.cancel = cancel
	u.mu.Unlock()
	go func() {
		defer cancel()
		err := u.run(ctx)
		u.set(func(st *UpgradeStatus) {
			st.Running = false
			st.Done = true
			st.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			if err != nil {
				st.Error = err.Error()
			}
		})
	}()
	return nil
}

// ErrNoUpgrade is Cancel with no upgrade running.
var ErrNoUpgrade = errors.New("no Postgres upgrade is running")

// Cancel stops a running upgrade. Before the old cluster is retired the
// apps come back on it as they were; after, the step in flight fails and
// the upgrade rolls back, exactly as it does on any failure.
func (u *Upgrader) Cancel() error {
	u.mu.Lock()
	running, cancel := u.status.Running, u.cancel
	u.mu.Unlock()
	if !running || cancel == nil {
		return ErrNoUpgrade
	}
	u.stage("cancel requested")
	cancel()
	return nil
}

// Timeouts for the individual steps. Generous: a stuck step must fail as
// itself, but nothing here should be cut off while making progress.
const (
	upgradePullTimeout    = 15 * time.Minute
	upgradeDumpTimeout    = 2 * time.Hour
	upgradeRestoreTimeout = 2 * time.Hour
	upgradeComposeTimeout = 5 * time.Minute
	upgradeQueryTimeout   = 30 * time.Minute // exact row counts over every table
)

func (u *Upgrader) run(ctx context.Context) (err error) {
	m := u.PG
	// The cluster is the upgrade's alone — but waiting for it must not
	// itself hold anyone up, nor ignore a cancel: TryLock, polled, never
	// queues as a pending writer that would stall every reader behind it.
	u.stage("waiting for restores and dumps in progress to finish")
	for !m.mu.TryLock() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled while waiting for the cluster: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	defer m.mu.Unlock()
	// The dumps are every app's data in the clear: gone once the upgrade
	// has finished or rolled back. Kept only when a rollback failed and a
	// person has to finish by hand.
	keepDumps := false
	defer func() {
		if !keepDumps {
			os.RemoveAll(filepath.Join(m.dir(), "upgrade"))
		}
	}()

	// ---- preflight: everything here is read-only ----
	u.stage("checking the cluster")
	st, err := m.Inspect()
	if err != nil {
		return err
	}
	if st.Fresh {
		return errors.New("there is no Postgres cluster to upgrade yet")
	}
	if st.Major >= TargetMajor {
		return fmt.Errorf("the cluster is already Postgres %d", st.Major)
	}
	from, to := st.Major, TargetMajor
	u.set(func(s *UpgradeStatus) { s.From, s.To = from, to })
	image := Images[to]
	if u.Hooks.Busy != nil {
		if name, busy := u.Hooks.Busy(); busy {
			return fmt.Errorf("%s is mid-deploy; try again when it finishes", name)
		}
	}
	retired := m.dataDir() + fmt.Sprintf(".pg%d", from)
	if _, err := os.Stat(retired); err == nil {
		return fmt.Errorf("%s already exists from an earlier upgrade — reclaim it (Settings → Storage → Orphaned data) or move it aside first", retired)
	}
	if err := u.healthy(ctx); err != nil {
		return err
	}
	size := fsx.DirSize(ctx, m.dataDir())
	if free, err := diskFree(m.dir()); err == nil && free < uint64(size)*2+512<<20 {
		return fmt.Errorf("not enough free space: the data directory is %s and the upgrade needs roughly twice that plus the dump; %s free", fsx.HumanBytes(size), fsx.HumanBytes(int64(free)))
	}
	// The trial restore's cluster lives in Docker's own storage.
	if root, err := execx.Output(ctx, "docker", "info", "--format", "{{.DockerRootDir}}"); err == nil {
		if free, err := diskFree(strings.TrimSpace(root)); err == nil && free < uint64(size)+512<<20 {
			return fmt.Errorf("not enough free space for the trial restore in Docker's storage (%s): it needs about %s; %s free", strings.TrimSpace(root), fsx.HumanBytes(size+512<<20), fsx.HumanBytes(int64(free)))
		}
	}
	u.stage("pulling " + image)
	if err := u.docker(ctx, upgradePullTimeout, "pull", image); err != nil {
		// A pull can fail for reasons that have nothing to do with the
		// image being usable: no route to the registry, or containerd's
		// content store mid-commit while the janitor prunes (seen: "failed
		// commit on ref ... no such file or directory" three minutes after
		// a fogd restart, which is exactly when the janitor's first sweep
		// fires). An image already on the box is what the upgrade needs.
		if u.docker(ctx, time.Minute, "image", "inspect", image) != nil {
			return err
		}
		u.stage("pull failed, using the copy already on this machine: " + err.Error())
	}

	// ---- can every database come across? Checked while nothing is stopped.
	// An extension the target image lacks would fail that database's restore
	// after the old cluster is retired, and the whole move would roll back;
	// so it is found now and the upgrade refused, naming the database, with
	// the fleet untouched. (Upgrading the others and leaving one behind is not
	// an option: the old cluster is retired, so that database's app would be
	// stranded.) Where an extension lives is no obstacle — each comes back in
	// the schema the dump names.
	u.stage("checking every database's extensions exist in Postgres " + fmt.Sprint(to))
	if err := u.checkTargetExtensions(ctx, image, to); err != nil {
		return err
	}
	// And can every database's dump be read the way the restore will read
	// it? Each database's schema is dumped with the target's tools and its
	// schemas and extensions parsed exactly as restoreDatabaseLocked does,
	// all while the apps still run. A dump fog cannot read is refused here,
	// naming its database, before anything stops — rather than after the
	// old cluster is retired, where it would roll the whole fleet back.
	u.stage("checking every database's dump can be read back by Postgres " + fmt.Sprint(to))
	if err := u.preflightDumps(ctx, image); err != nil {
		return err
	}

	// ---- stop writers, then take the inventory the restore is checked
	// against. The order matters: an inventory taken while an app still
	// runs is stale by the time the dump starts (a queue table gains a row
	// a second), and the dump-consistency check below would refuse a
	// perfectly quiet cluster.
	var stopped []string
	if u.Hooks.StopApps != nil {
		u.stage("stopping the apps that use the cluster")
		stopped, err = u.Hooks.StopApps(ctx)
		if err != nil {
			u.restart(ctx, stopped)
			return fmt.Errorf("stopping apps: %w", err)
		}
		if len(stopped) > 0 {
			u.stage("stopped " + strings.Join(stopped, ", "))
		}
	}
	if err := u.drainConnections(ctx); err != nil {
		u.restart(ctx, stopped)
		return err
	}
	u.stage("taking inventory of every database")
	before, err := u.inventory(ctx)
	if err != nil {
		u.restart(ctx, stopped)
		return err
	}
	u.stage(fmt.Sprintf("inventory: %d databases, %d tables, %d rows, %d roles", len(before.dbs), before.tables(), before.rows(), len(before.roles)))

	// ---- dump with the target's tools; the old cluster is still the truth ----
	//
	// Roles and globals go into one roles-only dump (fog-controlled: an app
	// role is NOSUPERUSER/NOCREATEROLE and cannot inject anything dangerous
	// there). Each database's *contents* are dumped on their own in custom
	// format, so the restore can replay them as that database's owner rather
	// than as the superuser — an app-planted CHECK function or trigger must
	// not run with superuser rights just because an upgrade restores it.
	upDir := filepath.Join(m.dir(), "upgrade")
	if err := os.MkdirAll(upDir, 0o700); err != nil {
		u.restart(ctx, stopped)
		return err
	}
	owners, err := u.databaseOwners(ctx)
	if err != nil {
		u.restart(ctx, stopped)
		return err
	}
	globalsPath := filepath.Join(upDir, "globals.sql")
	u.stage("dumping roles with Postgres " + fmt.Sprint(to) + "'s pg_dumpall --roles-only")
	if err := u.dumpGlobals(ctx, image, globalsPath); err != nil {
		u.restart(ctx, stopped)
		return err
	}
	dbDumps := map[string]string{}
	for db := range before.dbs {
		path := filepath.Join(upDir, "db-"+db+".dump")
		u.stage("dumping " + db)
		if err := u.dumpDatabaseCustom(ctx, image, db, path); err != nil {
			u.restart(ctx, stopped)
			return err
		}
		dbDumps[db] = path
	}
	// Nothing may have written between inventory and dump — the apps are
	// stopped, but a stray client would show up here as a count change.
	again, err := u.inventory(ctx)
	if err != nil {
		u.restart(ctx, stopped)
		return err
	}
	if diff := before.compare(again); diff != "" {
		u.restart(ctx, stopped)
		return fmt.Errorf("the cluster changed while the dump was taken (%s); something is still writing to it", diff)
	}
	u.stage(fmt.Sprintf("dump written: %s", fsx.HumanBytes(fsx.DirSize(ctx, upDir))))

	// ---- trial restore: every database, exactly as the real restore will,
	// into a throwaway Postgres M, while the old cluster is still the one in
	// place. A dump that will not come back — a CHECK that fails on this
	// data, a schema change the target refuses — is found here and refused,
	// naming its database, with nothing retired: the apps restart on the old
	// cluster. Found after the point of no return, it would roll the whole
	// fleet back.
	if u.skipTrial {
		u.stage("trial restore skipped (test)")
	} else if err := u.trialRestore(ctx, image, from, to, globalsPath, dbDumps, owners, before); err != nil {
		u.restart(ctx, stopped)
		return err
	}

	// ---- the point of no return: retire the old cluster ----
	u.stage(fmt.Sprintf("stopping Postgres %d", from))
	stopErr := u.compose(ctx, "down")
	if stopErr == nil && u.failStop {
		stopErr = errors.New("injected failure after the stop")
	}
	if err := stopErr; err != nil {
		// A cancel or failure part-way through the stop can leave the
		// cluster down: bring it back before handing the apps back to it.
		if e := m.ensureLocked(context.WithoutCancel(ctx)); e != nil {
			u.stage("bringing Postgres " + fmt.Sprint(from) + " back failed: " + e.Error())
		}
		u.restart(ctx, stopped)
		return err
	}
	u.stage(fmt.Sprintf("setting the Postgres %d data aside as %s", from, filepath.Base(retired)))
	if err := os.Rename(m.dataDir(), retired); err != nil {
		u.stage("could not move the data directory; bringing Postgres " + fmt.Sprint(from) + " back")
		if e := m.ensureLocked(ctx); e != nil {
			return fmt.Errorf("moving the data directory failed (%v) and restarting the old cluster failed too (%v) — see docs/postgres-upgrade.md", err, e)
		}
		u.restart(ctx, stopped)
		return err
	}
	// From here on every failure rolls back by putting `retired` back.
	rollback := func(cause error) error {
		u.stage("FAILED: " + cause.Error())
		u.stage(fmt.Sprintf("rolling back to Postgres %d", from))
		if err := u.rollback(ctx, from, retired); err != nil {
			keepDumps = true
			return fmt.Errorf("%v — and the rollback failed: %v. MANUAL ACTION NEEDED: the Postgres %d data is intact at %s; see docs/postgres-upgrade.md", cause, err, from, retired)
		}
		u.restart(ctx, stopped)
		u.set(func(s *UpgradeStatus) { s.RolledBack = true })
		return fmt.Errorf("%v — rolled back to Postgres %d; the apps are running on it again", cause, from)
	}

	u.stage(fmt.Sprintf("starting a fresh Postgres %d", to))
	if err := prepareDataDir(m.dataDir(), LayoutNested); err != nil {
		return rollback(err)
	}
	if err := m.writeCompose(to, LayoutNested); err != nil {
		return rollback(err)
	}
	if err := m.ensureNetworks(ctx); err != nil {
		return rollback(err)
	}
	if err := u.compose(ctx, "up", "-d", "--wait"); err != nil {
		// The rollback removes this container, and with it the only record
		// of why it died — read it first.
		if logs := u.containerLogs(ctx); logs != "" {
			u.stage("Postgres " + fmt.Sprint(to) + " log:\n" + logs)
		}
		return rollback(err)
	}
	u.stage("restoring roles")
	if err := u.restoreGlobals(ctx, globalsPath); err != nil {
		return rollback(err)
	}
	for db, dump := range dbDumps {
		// Replay each database's contents as its own owner. A database owned
		// by the superuser (none of fog's apps are) falls back to the
		// dedicated non-superuser restore role, so untrusted dump SQL is
		// never run as the superuser.
		role := owners[db]
		if role == "" || role == "postgres" || !pgIdent(role) {
			role = VerifyRole
		}
		u.stage("restoring " + db + " as " + role)
		if err := m.restoreDatabaseLocked(ctx, db, role, dump, extensionNames(before.dbs[db].extensions)); err != nil {
			return rollback(fmt.Errorf("restoring %s: %w", db, err))
		}
	}
	u.stage("verifying every table's row count against the inventory")
	after, err := u.inventory(ctx)
	if err != nil {
		return rollback(err)
	}
	if u.failAfterRestore {
		return rollback(errors.New("injected verification failure"))
	}
	if diff := before.compare(after); diff != "" {
		return rollback(fmt.Errorf("the restored cluster does not match the original: %s", diff))
	}
	u.stage("verified: every database, role, table, row, and extension came across")
	u.stage("rebuilding planner statistics")
	if err := u.docker(ctx, upgradeQueryTimeout, "exec", ContainerName, "vacuumdb", "-U", "postgres", "--all", "--analyze-in-stages", "--quiet"); err != nil {
		// Statistics rebuild themselves under autovacuum; a failure here
		// is worth a line, not a rollback.
		u.stage("statistics rebuild failed (autovacuum will catch up): " + err.Error())
	}
	u.restart(ctx, stopped)
	u.stage(fmt.Sprintf("done: Postgres %d. The Postgres %d data stays at %s until you reclaim it", to, from, retired))
	if u.Hooks.AfterUpgrade != nil {
		u.Hooks.AfterUpgrade()
	}
	return nil
}

// rollback retires whatever the failed attempt left in data/, puts the old
// cluster back, and starts it under its own major and layout.
func (u *Upgrader) rollback(ctx context.Context, from int, retired string) error {
	m := u.PG
	// Detached from ctx: a rollback must finish even if the trigger's
	// context is gone.
	ctx = context.WithoutCancel(ctx)
	if err := u.compose(ctx, "down"); err != nil {
		u.stage("compose down during rollback: " + err.Error())
	}
	if _, err := os.Stat(m.dataDir()); err == nil {
		failed := fmt.Sprintf("%s.pg%d.failed-%s", m.dataDir(), TargetMajor, time.Now().UTC().Format("20060102-150405"))
		if err := os.Rename(m.dataDir(), failed); err != nil {
			return fmt.Errorf("moving the failed cluster aside: %w", err)
		}
		u.stage("failed attempt kept at " + filepath.Base(failed))
	}
	if err := os.Rename(retired, m.dataDir()); err != nil {
		return fmt.Errorf("putting the Postgres %d data back: %w", from, err)
	}
	return m.ensureLocked(ctx)
}

func (u *Upgrader) restart(ctx context.Context, names []string) {
	if len(names) == 0 || u.Hooks.StartApps == nil {
		return
	}
	u.stage("starting " + strings.Join(names, ", "))
	if err := u.Hooks.StartApps(context.WithoutCancel(ctx), names); err != nil {
		u.stage("starting apps: " + err.Error())
	}
}

// drainConnections waits for client sessions to go away after the apps
// stop, terminates whatever lingers, and then insists they stay gone. The
// apps are the only clients fog knows about, and a stopped app cannot
// reconnect — so a session that comes back after termination is a live
// process fog failed to stop, and the only safe answer is to refuse:
// anything it wrote after the inventory would be missing from the dump
// and silently left behind in the retired data directory.
func (u *Upgrader) drainConnections(ctx context.Context) error {
	const list = `SELECT coalesce(string_agg(DISTINCT datname || '/' || coalesce(nullif(application_name, ''), usename) || ' from ' || coalesce(host(client_addr), 'local'), ', '), '')
	                FROM pg_stat_activity
	               WHERE backend_type = 'client backend' AND pid <> pg_backend_pid();`
	const terminate = `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid();`
	clients := func() (string, error) {
		rs, err := u.PG.psqlRows(ctx, "postgres", list)
		if err != nil {
			return "", fmt.Errorf("listing connections: %w", err)
		}
		return strings.Join(rs, " "), nil
	}
	// Stopped containers close their sockets; give the server a moment to
	// notice before treating anything as a straggler.
	var c string
	for i := 0; i < 10; i++ {
		var err error
		if c, err = clients(); err != nil {
			return err
		}
		if c == "" {
			return nil
		}
		time.Sleep(time.Second)
	}
	u.stage("terminating sessions still connected after the apps stopped: " + c)
	if _, err := u.PG.psql(ctx, "postgres", terminate); err != nil {
		return fmt.Errorf("terminating connections: %w", err)
	}
	// A terminated session from a dead process stays dead. One from a live
	// process is back within its pool's reconnect, which is well inside
	// this window.
	for i := 0; i < 5; i++ {
		time.Sleep(2 * time.Second)
		var err error
		if c, err = clients(); err != nil {
			return err
		}
		if c != "" {
			break
		}
	}
	if c != "" {
		return fmt.Errorf("sessions reconnected after being terminated (%s): something fog did not stop is still using the cluster, and its writes would be missing from the dump. Stop it, then try again", c)
	}
	return nil
}

// containerLogs is the tail of the cluster container's output, for the
// stage log when it fails to start.
func (u *Upgrader) containerLogs(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	out, _ := execx.Output(ctx, "docker", "logs", "--tail", "40", ContainerName)
	return out
}

func (u *Upgrader) healthy(ctx context.Context) error {
	if err := u.docker(ctx, time.Minute, "exec", ContainerName, "pg_isready", "-U", "postgres"); err != nil {
		return fmt.Errorf("the cluster is not healthy: %w", err)
	}
	return nil
}

func (u *Upgrader) docker(ctx context.Context, timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return execx.Step(ctx, timeout, "docker "+args[0], execx.Command(ctx, "docker", args...))
}

func (u *Upgrader) compose(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, upgradeComposeTimeout)
	defer cancel()
	cmd := execx.Command(ctx, "docker", append([]string{"compose"}, args...)...)
	cmd.Dir = u.PG.dir()
	return execx.Step(ctx, upgradeComposeTimeout, "docker compose "+args[0], cmd)
}

// checkTargetExtensions refuses an upgrade some database could not survive:
// one using an extension the target image does not ship. Read-only.
func (u *Upgrader) checkTargetExtensions(ctx context.Context, image string, to int) error {
	m := u.PG
	dbs, err := m.Databases(ctx)
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, upgradeComposeTimeout)
	defer cancel()
	out, err := execx.Output(ctx2, "docker", "run", "--rm", "--entrypoint", "ls", image, fmt.Sprintf("/usr/share/postgresql/%d/extension", to))
	if err != nil {
		return fmt.Errorf("listing the extensions Postgres %d ships: %w", to, err)
	}
	have := map[string]bool{}
	for _, f := range strings.Fields(out) {
		if name, ok := strings.CutSuffix(f, ".control"); ok {
			have[name] = true
		}
	}
	var missing []string
	for _, db := range dbs {
		exts, err := m.Extensions(ctx, db)
		if err != nil {
			return err
		}
		for ext := range exts {
			if !have[ext] {
				missing = append(missing, db+": "+ext)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("Postgres %d does not ship extension(s) these databases use, so they could not come across — nothing was changed: %s", to, strings.Join(missing, "; "))
	}
	return nil
}

// trialContainer is the throwaway target-version cluster trialRestore uses.
const trialContainer = "fog-postgres-trial"

// trialRestore starts a throwaway cluster of the target image and restores
// every database's dump into it with restoreDatabaseLocked — the same
// code, the same role, the same allowed extensions as the real restore.
// The first failure names its database. The container and its data are
// removed either way.
func (u *Upgrader) trialRestore(ctx context.Context, image string, from, to int, globalsPath string, dumps, owners map[string]string, before inventory) error {
	m := u.PG
	u.stage("trial-restoring every database into a scratch Postgres " + fmt.Sprint(to))
	// The trial cluster holds a plaintext copy of every app's data: it gets
	// no network, its password never touches argv, and it goes with its
	// volumes (rm -v) — including a leftover from an attempt fog did not
	// live to clean up.
	removeTrial := func() {
		execx.Command(context.WithoutCancel(ctx), "docker", "rm", "-f", "-v", trialContainer).Run()
	}
	removeTrial()
	pw := randx.Hex(16)
	envFile := filepath.Join(m.dir(), "upgrade", ".trial.env")
	if err := os.WriteFile(envFile, []byte("POSTGRES_PASSWORD="+pw+"\nPGPASSWORD="+pw+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(envFile)
	if err := u.docker(ctx, upgradeComposeTimeout, "run", "-d", "--name", trialContainer, "--network", "none", "--env-file", envFile, image); err != nil {
		removeTrial()
		return fmt.Errorf("starting a scratch Postgres %d for the trial restore: %w", to, err)
	}
	defer removeTrial()
	// Ready over TCP: the image's first-run initialisation serves only its
	// socket, so TCP answering means the real server is up.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := execx.Command(ctx, "docker", "exec", trialContainer,
			"psql", "-h", "127.0.0.1", "-U", "postgres", "-tAc", "SELECT 1").Run()
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the scratch Postgres %d for the trial restore did not start: %v", to, err)
		}
		time.Sleep(time.Second)
	}
	// The roles first, exactly as the real restore does: a dump can behave
	// differently depending on which roles exist (a function keyed on
	// another app's role), so the trial must see the same cluster.
	u.stage("trial: restoring roles")
	if err := u.restoreGlobalsInto(ctx, trialContainer, globalsPath); err != nil {
		return fmt.Errorf("the roles would not restore into Postgres %d — nothing was changed: %w", to, err)
	}
	trial := &Manager{Log: m.Log, container: trialContainer}
	names := make([]string, 0, len(dumps))
	for db := range dumps {
		names = append(names, db)
	}
	sort.Strings(names)
	for _, db := range names {
		role := owners[db]
		if role == "" || role == "postgres" || !pgIdent(role) {
			role = VerifyRole
		}
		u.stage("trial-restoring " + db + " as " + role)
		if err := trial.restoreDatabaseLocked(ctx, db, role, dumps[db], extensionNames(before.dbs[db].extensions)); err != nil {
			return fmt.Errorf("database %s would not restore into Postgres %d, so the upgrade would fail and roll back — nothing was changed, and the apps are back on Postgres %d: %w", db, to, from, err)
		}
	}
	u.stage("trial restore: every database came back")
	return nil
}

// preflightDumps dumps each database's schema with the target image and
// reads its schemas and extensions back as the restore will, in a
// throwaway container of the target image (no server needed: pg_restore
// reads the archive). Any failure names the database.
func (u *Upgrader) preflightDumps(ctx context.Context, image string) error {
	m := u.PG
	dbs, err := m.Databases(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Join(m.dir(), "upgrade-preflight")
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	read := func(argv ...string) (string, error) {
		ctx2, cancel := context.WithTimeout(ctx, upgradeComposeTimeout)
		defer cancel()
		name := "fog-pg-check-" + randx.Hex(6)
		defer execx.Command(context.WithoutCancel(ctx), "docker", "rm", "-f", "-v", name).Run()
		cmd := execx.Command(ctx2, "docker", append([]string{"run", "--rm", "--name", name, "--log-driver", "none", "--network", "none", "-v", dir + ":/w:ro", "--entrypoint", "pg_restore", image}, argv...)...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
	for _, db := range dbs {
		if !pgIdent(db) {
			return fmt.Errorf("database %q has a name fog cannot restore — nothing was changed", db)
		}
		dump := filepath.Join(dir, "db.dump")
		if err := u.pgDumpToFile(ctx, image, dump, false, []string{"pg_dump", "-h", ContainerName, "-U", "postgres", "-Fc", "--schema-only", db}); err != nil {
			return fmt.Errorf("database %s could not be dumped with Postgres %d's tools, so the upgrade would fail — nothing was changed: %w", db, TargetMajor, err)
		}
		os.Chmod(dump, 0o644)
		toc, err := read("-l", "/w/db.dump")
		if err == nil {
			if pick := schemaExtensionList(toc); pick != "" {
				if err = os.WriteFile(filepath.Join(dir, "pick.list"), []byte(pick), 0o644); err == nil {
					var script string
					if script, err = read("-L", "/w/pick.list", "-f", "-", "/w/db.dump"); err == nil {
						_, _, err = parseSchemaExtensionSQL(script)
					}
				}
			}
		}
		if err != nil {
			return fmt.Errorf("database %s's dump cannot be read back the way the upgrade restores it, so the upgrade would fail and roll back — nothing was changed: %w", db, err)
		}
		os.Remove(dump)
	}
	return nil
}

// databaseOwners maps each non-template database (bar postgres) to the role
// that owns it, so the restore can replay its contents as that role.
func (u *Upgrader) databaseOwners(ctx context.Context) (map[string]string, error) {
	rs, err := u.PG.psqlRows(ctx, "postgres", `SELECT datname || '|' || pg_get_userbyid(datdba) FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres';`)
	if err != nil {
		return nil, fmt.Errorf("reading database owners: %w", err)
	}
	owners := map[string]string{}
	for _, line := range rs {
		if db, owner, ok := strings.Cut(line, "|"); ok {
			owners[db] = owner
		}
	}
	return owners, nil
}

// dumpGlobals runs the target image's pg_dumpall --roles-only and writes the
// filtered SQL to path. Roles are fog's: an app role cannot create roles or
// grant membership, so the only thing an app lands here is its own
// ALTER ROLE … SET of a GUC, which is inert to replay. The bootstrap
// superuser's own CREATE/ALTER lines are filtered (initdb already made it).
func (u *Upgrader) dumpGlobals(ctx context.Context, image, path string) error {
	return u.pgDumpToFile(ctx, image, path, true, []string{"pg_dumpall", "-h", ContainerName, "-U", "postgres", "--roles-only"})
}

// dumpDatabaseCustom dumps one database's schema and data in custom format,
// so the restore can replay it with pg_restore as the database's owner (a
// non-superuser), stripping extension entries and pre-creating them as admin.
func (u *Upgrader) dumpDatabaseCustom(ctx context.Context, image, db, path string) error {
	if !pgIdent(db) {
		return fmt.Errorf("invalid database name %q", db)
	}
	return u.pgDumpToFile(ctx, image, path, false, []string{"pg_dump", "-h", ContainerName, "-U", "postgres", "--lock-wait-timeout=120s", "-Fc", db})
}

// pgDumpToFile runs a pg_dump/pg_dumpall in the target image over the fog
// network and writes its stdout to path. The superuser password travels in
// an env file, never argv. filter removes the bootstrap superuser's role
// lines (globals only; a custom-format dump is binary and never filtered).
func (u *Upgrader) pgDumpToFile(ctx context.Context, image, path string, filter bool, argv []string) error {
	m := u.PG
	pw, err := m.superuserPassword()
	if err != nil {
		return err
	}
	envPath := filepath.Join(filepath.Dir(path), ".env")
	if err := os.WriteFile(envPath, []byte("PGPASSWORD="+pw+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(envPath)
	ctx, cancel := context.WithTimeout(ctx, upgradeDumpTimeout)
	defer cancel()
	// Named, so a cancel or timeout can stop it: killing this client does
	// not stop the container, which would go on dumping (and holding the
	// old cluster) on its own. Its output is the dump itself, never logs.
	name := "fog-pg-dump-" + randx.Hex(6)
	defer execx.Command(context.WithoutCancel(ctx), "docker", "rm", "-f", "-v", name).Run()
	cmd := execx.Command(ctx, "docker", append([]string{"run", "--rm", "--name", name, "--log-driver", "none", "--network", m.Network, "--env-file", envPath, image}, argv...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	var writeErr error
	if filter {
		writeErr = filterDump(f, stdout)
	} else {
		_, writeErr = io.Copy(f, stdout)
	}
	closeErr := f.Close()
	waitErr := cmd.Wait()
	if waitErr != nil {
		return execx.Explain(ctx, upgradeDumpTimeout, argv[0], execx.WithOutput(stderr.String(), waitErr))
	}
	if writeErr != nil {
		return fmt.Errorf("writing the dump: %w", writeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if fsx.DirSize(ctx, path) == 0 {
		return fmt.Errorf("%s produced an empty dump", argv[0])
	}
	return nil
}

// extensionNames is the sorted extension names from an inventory's map, for
// the admin to pre-create before a non-superuser restore.
func extensionNames(exts map[string]string) []string {
	names := make([]string, 0, len(exts))
	for name := range exts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// superuserRoleLine matches the two statements pg_dumpall emits for the
// bootstrap superuser. The fresh cluster already has that role, created by
// initdb with the same password from .env, and CREATE ROLE on an existing
// role is the one error a strict restore of a pg_dumpall would hit.
var superuserRoleLine = regexp.MustCompile(`^(CREATE|ALTER) ROLE postgres\b`)

func filterDump(w io.Writer, r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<20)
	bw := bufio.NewWriterSize(w, 1<<20)
	for {
		line, err := br.ReadString('\n')
		if line != "" && !superuserRoleLine.MatchString(line) {
			if _, werr := bw.WriteString(line); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return bw.Flush()
}

// restoreGlobals feeds the roles-only dump to psql in the new container
// under ON_ERROR_STOP: the first error aborts, and the caller rolls back.
// This runs as the admin, which is sound — the dump holds only role SQL,
// which an app cannot inject into. Each database's contents are restored
// separately, as their owner, never here.
func (u *Upgrader) restoreGlobals(ctx context.Context, path string) error {
	return u.restoreGlobalsInto(ctx, ContainerName, path)
}

// restoreGlobalsInto replays the roles-only dump into container's cluster.
func (u *Upgrader) restoreGlobalsInto(ctx context.Context, container, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, upgradeRestoreTimeout)
	defer cancel()
	cmd := execx.Command(ctx, "docker", "exec", "-i", container,
		"psql", "-v", "ON_ERROR_STOP=1", "-q", "-U", "postgres", "-d", "postgres", "-f", "-")
	cmd.Stdin = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return execx.Explain(ctx, upgradeRestoreTimeout, "psql restore", execx.WithOutput(msg, err))
	}
	return nil
}

// inventory is the shape of a cluster that must survive the move exactly:
// every role, every database, every table with its row count, and every
// extension by name. Extension versions may legitimately rise (a new
// major ships newer contrib modules), so they are not part of the compare.
type inventory struct {
	roles []string
	dbs   map[string]dbInventory
}

type dbInventory struct {
	rows       map[string]int64
	extensions map[string]string
	// extSchemas: where each extension lives, carried and checked per schema.
	extSchemas map[string]string
}

func (inv inventory) tables() int {
	n := 0
	for _, d := range inv.dbs {
		n += len(d.rows)
	}
	return n
}

func (inv inventory) rows() int64 {
	var n int64
	for _, d := range inv.dbs {
		for _, c := range d.rows {
			n += c
		}
	}
	return n
}

func (u *Upgrader) inventory(ctx context.Context) (inventory, error) {
	m := u.PG
	ctx, cancel := context.WithTimeout(ctx, upgradeQueryTimeout)
	defer cancel()
	inv := inventory{dbs: map[string]dbInventory{}}
	roles, err := m.Roles(ctx)
	if err != nil {
		return inv, err
	}
	inv.roles = roles
	dbs, err := m.Databases(ctx)
	if err != nil {
		return inv, err
	}
	for _, db := range dbs {
		rows, err := m.TableRowCounts(ctx, db)
		if err != nil {
			return inv, err
		}
		exts, err := m.Extensions(ctx, db)
		if err != nil {
			return inv, err
		}
		schemas, err := m.ExtensionSchemas(ctx, db)
		if err != nil {
			return inv, err
		}
		inv.dbs[db] = dbInventory{rows: rows, extensions: exts, extSchemas: schemas}
	}
	return inv, nil
}

// compare reports the first way `other` fails to contain everything in
// inv, or "" when it matches. Pure, so the rule is testable.
func (inv inventory) compare(other inventory) string {
	if a, b := strings.Join(inv.roles, ","), strings.Join(other.roles, ","); a != b {
		return fmt.Sprintf("roles differ: had [%s], now [%s]", a, b)
	}
	names := make([]string, 0, len(inv.dbs))
	for db := range inv.dbs {
		names = append(names, db)
	}
	sort.Strings(names)
	for _, db := range names {
		want := inv.dbs[db]
		got, ok := other.dbs[db]
		if !ok {
			return "database " + db + " is missing"
		}
		if len(want.rows) != len(got.rows) {
			return fmt.Sprintf("%s has %d tables, expected %d", db, len(got.rows), len(want.rows))
		}
		tables := make([]string, 0, len(want.rows))
		for t := range want.rows {
			tables = append(tables, t)
		}
		sort.Strings(tables)
		for _, t := range tables {
			n, ok := got.rows[t]
			if !ok {
				return fmt.Sprintf("%s.%s is missing", db, t)
			}
			if n != want.rows[t] {
				return fmt.Sprintf("%s.%s has %d rows, expected %d", db, t, n, want.rows[t])
			}
		}
		for ext := range want.extensions {
			if _, ok := got.extensions[ext]; !ok {
				return fmt.Sprintf("%s is missing extension %s", db, ext)
			}
		}
		for ext, schema := range want.extSchemas {
			if got.extSchemas != nil && got.extSchemas[ext] != schema {
				return fmt.Sprintf("%s has extension %s in schema %q, expected %q", db, ext, got.extSchemas[ext], schema)
			}
		}
	}
	for db := range other.dbs {
		if _, ok := inv.dbs[db]; !ok {
			return "unexpected database " + db
		}
	}
	return ""
}

// Retired lists the data directories earlier upgrades set aside, for the
// status endpoint and the janitor.
func (m *Manager) Retired() []RetiredData {
	var out []RetiredData
	ents, _ := os.ReadDir(m.dir())
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "data.pg") {
			p := filepath.Join(m.dir(), e.Name())
			out = append(out, RetiredData{Path: p, SizeBytes: fsx.DirSize(context.Background(), p)})
		}
	}
	return out
}

type RetiredData struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

// ClusterStatus is what the settings card shows.
type ClusterStatus struct {
	Major            int           `json:"major"`
	TargetMajor      int           `json:"targetMajor"`
	Layout           Layout        `json:"layout"`
	Fresh            bool          `json:"fresh"`
	UpgradeAvailable bool          `json:"upgradeAvailable"`
	DataBytes        int64         `json:"dataBytes"`
	Error            string        `json:"error,omitempty"`
	Retired          []RetiredData `json:"retired"`
}

func (m *Manager) ClusterStatus() ClusterStatus {
	cs := ClusterStatus{TargetMajor: TargetMajor, Retired: m.Retired()}
	if cs.Retired == nil {
		cs.Retired = []RetiredData{}
	}
	st, err := m.Inspect()
	if err != nil {
		cs.Error = err.Error()
		return cs
	}
	cs.Major, cs.Layout, cs.Fresh = st.Major, st.Layout, st.Fresh
	cs.UpgradeAvailable = !st.Fresh && st.Major < TargetMajor
	if !st.Fresh {
		cs.DataBytes = fsx.DirSize(context.Background(), m.dataDir())
	}
	return cs
}
