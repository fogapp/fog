// Package pg manages Fog's shared Postgres: one container whose artifacts
// live under <datadir>/postgres/, plus idempotent per-app provisioning
// (role, databases, extensions) via psql in the container.
package pg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

const (
	ContainerName = "fog-postgres"
	superuserKey  = "pg_superuser_password" // system secret
)

// Images maps a Postgres major to the image fog runs for it. The cluster
// runs whatever major its data directory already is (see Inspect), so a
// fogd release that raises TargetMajor changes nothing on an existing
// install until the operator runs the upgrade (upgrade.go) — pointing a
// new major's image at an old major's data would not crash-loop, it would
// initdb an empty cluster beside the real one and every app would happily
// migrate against nothing.
//
// pgvector's image rather than plain postgres, so apps can declare
// extensions: [vector]. Without it such an app fails provisioning
// outright — the extension simply isn't on disk to create.
//
// The -trixie suffix matters and is not cosmetic. pgvector's default tag
// is still Debian 12 (bookworm, glibc 2.36) while postgres has moved to
// Debian 13 (trixie, glibc 2.41). Swapping an existing install onto a
// bookworm image makes every database report a collation version mismatch:
// text indexes were built under a different glibc and may now sort
// differently, which corrupts them quietly — as wrong query results rather
// than as errors. Every entry here is a trixie build so the glibc under
// every collation stays the same across majors too; verified by
// round-tripping a database between pg16-trixie and pg18-trixie
// (datcollversion 2.41 on both sides, no reindex).
//
// Deliberately a table in code and not a setting: the Caddy image is
// configurable because swapping it is harmless, whereas a Postgres image
// is bound to its data directory's major.
var Images = map[int]string{
	16: "pgvector/pgvector:pg16-trixie",
	18: "pgvector/pgvector:pg18-trixie",
}

// TargetMajor is the major fog wants every install on — what a fresh
// install gets, and what the upgrade moves an existing one to. The issues
// scanner compares app-side client tooling (pg_dump) against the major
// actually running, not this: a client older than the server refuses to
// dump at all, which silently kills that app's backups.
const TargetMajor = 18

// What a fresh install gets. Variables only so the integration test can
// start a cluster on the old major and layout and run the upgrade over it.
var (
	freshMajor  = TargetMajor
	freshLayout = LayoutNested
)

// Layout is where a major keeps its files inside the data directory.
//
// Legacy is what postgres images up to 17 do — PGDATA is the mounted
// directory itself. Nested mirrors postgres:18's own convention: the
// mounted directory holds one subdirectory per major (<major>/docker),
// so two majors can sit side by side during an upgrade and pg_upgrade
// stays possible later. In the nested layout PGDATA is set explicitly in
// the compose file, so the data's place never depends on an image default.
type Layout string

const (
	LayoutLegacy Layout = "legacy" // data/PG_VERSION; PGDATA=/var/lib/postgresql/data
	LayoutNested Layout = "nested" // data/<major>/docker/PG_VERSION; PGDATA=/var/lib/postgresql/<major>/docker
)

// State is what the data directory says about the cluster.
type State struct {
	Major  int    // the major the on-disk cluster is (TargetMajor when fresh)
	Layout Layout // where its files live
	Fresh  bool   // no cluster on disk yet — the next Ensure runs initdb
}

// ensureTimeout bounds bringing the shared Postgres up (`--wait` blocks on the
// health check, and a container stuck restarting would block forever).
const ensureTimeout = 5 * time.Minute

type Manager struct {
	DataDir string
	Store   *store.Store
	Vault   *vault.Vault
	Log     *slog.Logger
	Network string
	// SealedNetwork is a second network the cluster joins, so sealed apps
	// can reach it; "" for none.
	SealedNetwork string
	// Docker is the shared Engine API client. Nil makes one on first use.
	Docker *dockerx.Client

	// mu guards the shared cluster. Provision and the major upgrade hold it
	// exclusively: two apps deploying at once would otherwise race to write
	// the same compose.yaml and run `compose up` in the same directory, and
	// their CREATE ROLE / CREATE DATABASE statements would interleave; and an
	// upgrade swaps the cluster out from under everyone. Dumps, restores and
	// drops share it — they touch one database each, but must never run
	// against a cluster mid-upgrade or mid-bring-up.
	mu         sync.RWMutex
	dockerOnce sync.Once
	// provMu serialises Provision and Deprovision among themselves. They
	// take mu only shared, like restores: a restore replays an app's dump,
	// which the app can make slow, and must not hold up every other app's
	// deploy. Only the major upgrade takes mu exclusively.
	provMu sync.Mutex
	// restoring names the fog_rst_/fog_old_ databases restores in flight
	// are using, so the leftover sweep (run by Provision, now concurrent
	// with restores) never drops one out from under its restore.
	restMu    sync.Mutex
	restoring map[string]bool

	// container, when set, is the container the database commands run in
	// instead of the cluster's own: the upgrade's trial-restore cluster.
	container string
}

// ctr is the container database commands run in.
func (m *Manager) ctr() string {
	if m.container != "" {
		return m.container
	}
	return ContainerName
}

func (m *Manager) docker() *dockerx.Client {
	m.dockerOnce.Do(func() {
		if m.Docker == nil {
			m.Docker = dockerx.New()
		}
	})
	return m.Docker
}

func (m *Manager) dir() string     { return filepath.Join(m.DataDir, "postgres") }
func (m *Manager) dataDir() string { return filepath.Join(m.dir(), "data") }

// Inspect reads the data directory: which major the cluster on disk is and
// how it is laid out. A directory with no cluster in it is a fresh install
// and gets TargetMajor in the nested layout; a cluster of a major fog has
// no image for is an error, because starting anything against it is how
// data gets lost.
func (m *Manager) Inspect() (State, error) {
	return inspectData(m.dataDir())
}

func inspectData(dataDir string) (State, error) {
	if major, ok := readPGVersion(filepath.Join(dataDir, "PG_VERSION")); ok {
		if _, known := Images[major]; !known {
			return State{}, fmt.Errorf("the Postgres data directory is major %d, which this fog has no image for", major)
		}
		return State{Major: major, Layout: LayoutLegacy}, nil
	}
	ents, err := os.ReadDir(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return State{}, err
	}
	found := 0
	var majors []int
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if major, ok := readPGVersion(filepath.Join(dataDir, e.Name(), "docker", "PG_VERSION")); ok {
			if major != n {
				return State{}, fmt.Errorf("data/%s holds a Postgres %d cluster", e.Name(), major)
			}
			majors = append(majors, major)
			found++
		}
	}
	switch found {
	case 0:
		// Anything else in the directory is not a cluster fog knows how to
		// run, and initdb would refuse a non-empty PGDATA anyway.
		if len(ents) > 0 {
			return State{}, fmt.Errorf("the Postgres data directory %s is not empty but holds no recognizable cluster", dataDir)
		}
		return State{Major: freshMajor, Layout: freshLayout, Fresh: true}, nil
	case 1:
		if _, known := Images[majors[0]]; !known {
			return State{}, fmt.Errorf("the Postgres data directory is major %d, which this fog has no image for", majors[0])
		}
		return State{Major: majors[0], Layout: LayoutNested}, nil
	default:
		return State{}, fmt.Errorf("the Postgres data directory holds %d clusters (%v); an upgrade was interrupted — see docs/postgres-upgrade.md", found, majors)
	}
}

func readPGVersion(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return n, err == nil
}

// pgdata is the in-container PGDATA for a major in a layout ("" = leave
// the image default), and mount is the host bind that puts data/ under it.
//
// Legacy leaves PGDATA to the image: every pre-18 image defaults to the
// mounted directory itself, and setting it explicitly would change the
// compose file of every existing install, which recreates the running
// container on the next deploy for no gain. Nested always says where the
// data is, because that is the layout whose default moved.
func pgdata(major int, layout Layout) (pgdata, mount string) {
	if layout == LayoutLegacy {
		return "", "./data:/var/lib/postgresql/data"
	}
	return fmt.Sprintf("/var/lib/postgresql/%d/docker", major), "./data:/var/lib/postgresql"
}

// serverCommand is the postgres invocation, with the settings a shared
// cluster behind several container-restarting apps needs and the image
// defaults get wrong:
//
//   - keepalives: a container that is stopped or replaced usually drops
//     its sessions without a FIN, and the server keeps them as idle
//     backends until TCP keepalive notices — two hours on Linux by
//     default. A few deploys in an evening and the dead crowd out the
//     living ("remaining connection slots are reserved for ... SUPERUSER",
//     seen on feed). 60/10/3 reaps a dead client in about ninety seconds.
//   - max_connections: each Rails app is puma plus a worker supervisor
//     with several processes, every one holding a pool per database
//     (primary, cache, queue), and a blue-green swap runs two of the app
//     for a while. 200 was sized for three such apps; five (feed, hook,
//     hud, glass, stream — stream alone runs two puma workers of twelve
//     threads) ran it out on 2026-10-19 and 20 during overlapping deploys.
//     Deploys of these apps now switch over one at a time (apps.switchGate),
//     and 400 leaves headroom for one swap on top of everyone's steady
//     state. An idle backend costs a few MB; PgBouncer would save them, but
//     only in transaction mode, which breaks what these apps rely on —
//     per-session SETs (feed's statement_timeout), session advisory locks
//     (Rails migrations, feed's restore), LISTEN/NOTIFY.
//
// Changing this recreates the container on the next Ensure — a restart
// of the cluster during the next app deploy, once.
var serverCommand = []string{
	"postgres",
	"-c", "max_connections=400",
	"-c", "tcp_keepalives_idle=60",
	"-c", "tcp_keepalives_interval=10",
	"-c", "tcp_keepalives_count=3",
}

// ParseMajor reads the major out of a Postgres tool's --version line
// ("pg_dump (PostgreSQL) 18.6 (Debian ...)"), or a manifest's dump_tool.
func ParseMajor(s string) (int, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

var versionRe = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)`)

// composeDoc renders the compose file for a major in a layout — pure, so
// the layout rules are testable without docker.
func composeDoc(network, sealed string, major int, layout Layout) ([]byte, error) {
	image, ok := Images[major]
	if !ok {
		return nil, fmt.Errorf("no Postgres image for major %d", major)
	}
	data, mount := pgdata(major, layout)
	attached := map[string]any{network: map[string]any{"aliases": []string{ContainerName}}}
	declared := map[string]any{network: map[string]any{"external": true}}
	if sealed != "" {
		attached[sealed] = map[string]any{"aliases": []string{ContainerName}}
		declared[sealed] = map[string]any{"external": true}
	}
	service := map[string]any{
		"image":          image,
		"container_name": ContainerName,
		"restart":        "unless-stopped",
		"command":        serverCommand,
		"env_file":       []string{".env"},
		"volumes":        []string{mount},
		"networks":       attached,
		"labels":         map[string]string{"fog.managed": "true", "fog.system": "postgres"},
		"healthcheck": map[string]any{
			"test":     []string{"CMD-SHELL", "pg_isready -U postgres"},
			"interval": "10s",
			"timeout":  "5s",
			"retries":  5,
		},
	}
	if data != "" {
		service["environment"] = map[string]string{"PGDATA": data}
	}
	doc := map[string]any{
		"name":     "fog-postgres",
		"services": map[string]any{"postgres": service},
		"networks": declared,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	header := "# Generated by Fog. Recovery without Fog: docker compose up -d\n"
	return append([]byte(header), out...), nil
}

// prepareDataDir creates the data directory with the mode its layout
// needs. Legacy: the directory is PGDATA itself, the image's entrypoint
// chowns it to postgres, 0700 is right. Nested: the directory is the
// parent of PGDATA and stays owned by root, and the entrypoint re-runs
// its directory setup after dropping to the postgres user — which cannot
// so much as stat PGDATA through a root-owned 0700 parent and dies with
// "mkdir: cannot create directory /var/lib/postgresql: Permission denied".
// Seen on a Proxmox LXC; Docker Desktop hides it by remapping bind-mount
// ownership. 0755 on a directory whose only entry is the 0700 postgres-
// owned PGDATA exposes nothing.
func prepareDataDir(dataDir string, layout Layout) error {
	mode := os.FileMode(0o700)
	if layout == LayoutNested {
		mode = 0o755
	}
	if err := os.MkdirAll(dataDir, mode); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory alone; the mode is the point.
	return os.Chmod(dataDir, mode)
}

func (m *Manager) writeCompose(major int, layout Layout) error {
	doc, err := composeDoc(m.Network, m.SealedNetwork, major, layout)
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(m.dir(), "compose.yaml"), doc, 0o644)
}

// ensureNetworks creates what the compose file declares external, which is
// never left to an app deploy having got there first.
func (m *Manager) ensureNetworks(ctx context.Context) error {
	if err := m.docker().EnsureNetwork(ctx, m.Network); err != nil {
		return err
	}
	if m.SealedNetwork == "" {
		return nil
	}
	_, err := m.docker().EnsureInternalNetwork(ctx, m.SealedNetwork)
	return err
}

func (m *Manager) superuserPassword() (string, error) {
	return m.vaultSecret(0, superuserKey)
}

// vaultSecret returns the password stored under key, minting one only when
// none exists. Any other failure is returned rather than read as "missing":
// rotating a password Postgres still holds would lock its owner out.
func (m *Manager) vaultSecret(appID int64, key string) (string, error) {
	return vault.Secrets{Store: m.Store, Vault: m.Vault}.GetOrMint(appID, key,
		func() (string, error) { return randx.Hex(24), nil })
}

func (m *Manager) ensureLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ensureTimeout)
	defer cancel()
	pw, err := m.superuserPassword()
	if err != nil {
		return err
	}
	dir := m.dir()
	st, err := m.Inspect()
	if err != nil {
		return err
	}
	if err := prepareDataDir(m.dataDir(), st.Layout); err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, ".env"), []byte("POSTGRES_PASSWORD="+pw+"\n"), 0o600); err != nil {
		return err
	}
	if err := m.writeCompose(st.Major, st.Layout); err != nil {
		return err
	}
	if err := m.ensureNetworks(ctx); err != nil {
		return err
	}
	up := execx.Command(ctx, "docker", "compose", "up", "-d", "--wait")
	up.Dir = dir
	if _, err := execx.Run(up); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("the managed Postgres did not become healthy within %s", ensureTimeout)
		}
		return fmt.Errorf("postgres up: %w", err)
	}
	return nil
}

// Provision idempotently creates the app's role, databases, and extensions,
// and returns connection details for env injection.
type Conn struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string // first database
}

func (m *Manager) Provision(ctx context.Context, appName string, appID int64, databases, extensions []string) (*Conn, error) {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := m.ensureLocked(ctx); err != nil {
		return nil, err
	}
	// A restore leftover no restore in flight is using is a leak: sweep it.
	m.sweepLeakedLocked(ctx)
	// Per-app role password, generated once and vault-stored.
	rolePw, err := m.vaultSecret(appID, "_pg_password")
	if err != nil {
		return nil, err
	}

	role := RoleName(appName)
	// Identifiers are interpolated into SQL below, so validate them here even
	// though spec validation already has. Deprovision and RestoreDatabase both
	// check; Provision not checking made the guarantee depend on every future
	// caller remembering, which is exactly how it eventually stops holding.
	// Conn.DBName is databases[0], so an empty list is a caller bug caught
	// here rather than a panic below.
	if len(databases) == 0 {
		return nil, fmt.Errorf("app %s declares no databases", appName)
	}
	if !pgIdent(role) {
		return nil, fmt.Errorf("invalid role name %q", role)
	}
	for _, db := range databases {
		if !pgIdent(db) {
			return nil, fmt.Errorf("invalid database name %q", db)
		}
	}
	for _, ext := range extensions {
		if !pgExtIdent(ext) {
			return nil, fmt.Errorf("invalid extension name %q", ext)
		}
	}
	// Role. A new one is created NOLOGIN: the app cannot connect to anything
	// until its databases and extensions are in place, so nothing it does
	// can race fog's setup. LOGIN and the password come last, below.
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE %s NOLOGIN; END IF; END $$;`,
		role, role)); err != nil {
		return nil, fmt.Errorf("role %s: %w", role, err)
	}
	for _, db := range databases {
		// A database name is the app's claim, and an app must not be connected
		// to a database another app owns by naming it. If it already exists it
		// has to be this app's, otherwise the app would squat a neighbour's
		// data (and deleting or restoring this app would then reach it).
		owner, exists, err := m.databaseOwner(ctx, db)
		if err != nil {
			return nil, err
		}
		if exists {
			if owner != role {
				return nil, fmt.Errorf("database %s already exists and is owned by %q, not %s — another app has claimed this name", db, owner, role)
			}
		} else if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`CREATE DATABASE %s OWNER %s;`, db, role)); err != nil {
			return nil, fmt.Errorf("create database %s: %w", db, err)
		}
		// Other apps' roles must not even connect: a database's default PUBLIC
		// CONNECT would let any role reach it. A new database is also sealed
		// from its own owner until fog has installed its extensions, so the
		// app cannot plant anything there first.
		seal := fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC;`, db)
		if !exists {
			seal += fmt.Sprintf(` REVOKE CONNECT ON DATABASE %s FROM %s;`, db, role)
		}
		if _, err := m.psql(ctx, "postgres", seal); err != nil {
			return nil, fmt.Errorf("securing database %s: %w", db, err)
		}
		if err := m.installExtensions(ctx, db, publicTargets(extensions), !exists); err != nil {
			return nil, fmt.Errorf("extensions on %s: %w", db, err)
		}
		if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s;`, db, role)); err != nil {
			return nil, fmt.Errorf("opening database %s to %s: %w", db, role, err)
		}
	}
	// Ready: the role may log in now (password updated in case it drifted).
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`ALTER ROLE %s WITH LOGIN PASSWORD '%s';`, role, rolePw)); err != nil {
		return nil, fmt.Errorf("role %s: %w", role, err)
	}
	return &Conn{Host: ContainerName, Port: "5432", User: role, Password: rolePw, DBName: databases[0]}, nil
}

// extTarget is one extension to install and the schema it belongs in.
type extTarget struct {
	Name   string
	Schema string
}

// publicTargets is a spec's declared extensions, each into public.
func publicTargets(names []string) []extTarget {
	out := make([]extTarget, 0, len(names))
	for _, n := range names {
		out = append(out, extTarget{Name: n, Schema: "public"})
	}
	return out
}

// withDeclared is the dump's extensions plus each declared one the dump does
// not name, into public as provisioning installs it. The dump's own schema
// for an extension wins.
func withDeclared(exts []extTarget, declared []string) []extTarget {
	have := map[string]bool{}
	for _, x := range exts {
		have[x.Name] = true
	}
	out := append([]extTarget(nil), exts...)
	for _, d := range publicTargets(declared) {
		if !have[d.Name] {
			have[d.Name] = true
			out = append(out, d)
		}
	}
	return out
}

// installExtensions creates extensions in db as the superuser, never letting
// an install script run with a search_path the app can steer. Postgres runs a
// script with search_path set to the schema it installs into, and an app owns
// its database's schemas — so objects it planted there could be resolved by
// the script and run as the superuser. The connection itself is pinned
// (psqlSafe) and:
//
//   - an extension already present is left alone;
//   - one whose control file fixes it in pg_catalog installs there (no app
//     can write pg_catalog);
//   - a relocatable one — always, fresh database or not — installs into a
//     brand-new schema only the superuser can write and is then moved into
//     its schema with ALTER EXTENSION … SET SCHEMA, which runs no script;
//   - one that cannot be moved installs directly only while the database is
//     sealed (no role but the admin may connect, so nothing can have been
//     planted); otherwise it is refused, with the reason.
func (m *Manager) installExtensions(ctx context.Context, db string, exts []extTarget, sealed bool) error {
	for _, x := range exts {
		ext := x.Name
		if !pgExtIdent(ext) {
			return fmt.Errorf("invalid extension name %q", ext)
		}
		schema := x.Schema
		if schema == "" {
			schema = "public"
		}
		have, err := m.psqlRowsSafe(ctx, db, fmt.Sprintf(
			`SELECT 1 FROM pg_catalog.pg_extension WHERE extname OPERATOR(pg_catalog.=) '%s';`, ext))
		if err != nil {
			return err
		}
		if len(have) > 0 {
			continue
		}
		reloc, fixed, _, err := m.extensionControl(ctx, db, ext)
		if err != nil {
			return err
		}
		// Where the extension's control file does not fix its schema, fog
		// puts it where the dump (or spec) says — never into a system
		// schema: objects there are resolved ahead of everything else, for
		// every role in the database.
		if fixed == "" && systemSchema(schema) {
			return fmt.Errorf("extension %s: refusing to install it into system schema %s", ext, schema)
		}
		var sql string
		switch {
		case fixed == "pg_catalog":
			sql = fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s;", quoteIdent(ext))
		case reloc && fixed == "":
			staging := "fog_ext_" + randx.Hex(8)
			sql = fmt.Sprintf("CREATE SCHEMA %s; CREATE EXTENSION %s SCHEMA %s; ALTER EXTENSION %s SET SCHEMA %s; DROP SCHEMA %s;",
				staging, quoteIdent(ext), staging, quoteIdent(ext), quoteIdent(schema), staging)
			if _, err := m.psqlSafe(ctx, db, sql); err != nil {
				m.psqlSafe(context.WithoutCancel(ctx), db, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE;", staging))
				return fmt.Errorf("creating extension %s in schema %s: %w", ext, schema, err)
			}
			continue
		case !sealed:
			return fmt.Errorf("extension %s cannot be moved once installed, and fog will not run its install script inside a database the app can already reach; it can only be added when the database is first created", ext)
		case fixed != "":
			sql = fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s;", quoteIdent(ext))
		default:
			sql = fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s SCHEMA %s;", quoteIdent(ext), quoteIdent(schema))
		}
		if _, err := m.psqlSafe(ctx, db, sql); err != nil {
			return fmt.Errorf("creating extension %s: %w", ext, err)
		}
	}
	return nil
}

// extensionControl reads an available extension's control facts at its
// default version: relocatable, a schema its control file fixes it in, and
// whether a database owner may create it (trusted).
func (m *Manager) extensionControl(ctx context.Context, db, ext string) (relocatable bool, fixed string, trusted bool, err error) {
	info, err := m.psqlRowsSafe(ctx, db, fmt.Sprintf(
		`SELECT e.relocatable::text || '|' || e.trusted::text || '|' || coalesce(e.schema::text, '')
		   FROM pg_catalog.pg_available_extension_versions e
		   JOIN pg_catalog.pg_available_extensions a
		     ON a.name OPERATOR(pg_catalog.=) e.name AND a.default_version OPERATOR(pg_catalog.=) e.version
		  WHERE e.name OPERATOR(pg_catalog.=) '%s';`, ext))
	if err != nil {
		return false, "", false, err
	}
	if len(info) != 1 {
		return false, "", false, fmt.Errorf("extension %s is not available on this cluster", ext)
	}
	f := strings.SplitN(info[0], "|", 3)
	if len(f) != 3 {
		return false, "", false, fmt.Errorf("extension %s: unreadable control facts %q", ext, info[0])
	}
	return f[0] == "true", f[2], f[1] == "true", nil
}

// systemSchema is a schema Postgres itself owns: pg_catalog, pg_toast, any
// other pg_ name, and information_schema.
func systemSchema(s string) bool {
	return s == "information_schema" || strings.HasPrefix(s, "pg_")
}

// quoteIdent renders a name as a quoted SQL identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// databaseOwner reports a database's owning role and whether it exists. The
// query is a pinned catalog read (postgres db, but uniform): db is already a
// validated identifier, interpolated into a literal.
func (m *Manager) databaseOwner(ctx context.Context, db string) (owner string, exists bool, err error) {
	rs, err := m.psqlRowsSafe(ctx, "postgres", fmt.Sprintf(
		`SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname OPERATOR(pg_catalog.=) '%s';`, db))
	if err != nil {
		return "", false, fmt.Errorf("reading owner of %s: %w", db, err)
	}
	if len(rs) == 0 {
		return "", false, nil
	}
	return rs[0], true, nil
}

// Deprovision drops the app's databases and role. Database contents are
// unrecreatable user data, so this only runs on delete-with-data.
func (m *Manager) Deprovision(ctx context.Context, appName string, databases []string) error {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Nothing to drop if the shared Postgres was never brought up.
	if err := execx.Command(ctx, "docker", "inspect", ContainerName).Run(); err != nil {
		return nil
	}
	role := RoleName(appName)
	if !pgIdent(role) {
		return fmt.Errorf("invalid role name %q", role)
	}
	var errs []string
	for _, db := range databases {
		if !pgIdent(db) {
			errs = append(errs, fmt.Sprintf("invalid database name %q", db))
			continue
		}
		// Only ever drop a database this app owns. An app that named a
		// neighbour's database must not take it down when it is deleted.
		owner, exists, err := m.databaseOwner(ctx, db)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if !exists {
			continue
		}
		if owner != role {
			errs = append(errs, fmt.Sprintf("refusing to drop database %s: it is owned by %q, not %s", db, owner, role))
			continue
		}
		if err := m.dropDatabase(ctx, db); err != nil {
			errs = append(errs, fmt.Sprintf("drop database %s: %v", db, err))
		}
	}
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`DROP ROLE IF EXISTS %s;`, role)); err != nil {
		errs = append(errs, fmt.Sprintf("drop role %s: %v", role, err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// psql runs sql against db and returns psql's trimmed output — on failure
// too, alongside an error whose text is what psql printed.
//
// The SQL travels over stdin, never argv: Provision's ALTER ROLE carries the
// role's plaintext password, and anything in argv is visible in `ps` on both
// host and container for the command's duration. pg_restore below reads its
// dump the same way.
func (m *Manager) psql(ctx context.Context, db, sql string, flags ...string) (string, error) {
	return m.psqlEnv(ctx, nil, db, sql, flags...)
}

// safeSearchPath pins a connection's search_path to the catalog (and the
// session's own temp schema) through the startup packet, which an app's
// ALTER DATABASE/ROLE SET cannot override. Every superuser query fog runs
// inside a database an app can write goes through psqlSafe with this, so a
// planted view, operator or function in `public` can never be resolved in
// place of a catalog one and run with fog's rights.
const safeSearchPath = "PGOPTIONS=-c search_path=pg_catalog,pg_temp"

// psqlEnv is psql with extra container environment (PGOPTIONS). The SQL
// travels over stdin, never argv: Provision's ALTER ROLE carries the role's
// plaintext password, and anything in argv is visible in `ps` on both host
// and container for the command's duration. pg_restore below reads its dump
// the same way.
func (m *Manager) psqlEnv(ctx context.Context, env []string, db, sql string, flags ...string) (string, error) {
	args := []string{"exec", "-i"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, m.ctr(), "psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", db)
	args = append(args, flags...)
	cmd := execx.Command(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(sql)
	return execx.Run(cmd)
}

// psqlSafe runs a superuser query against db with search_path pinned to the
// catalog — for reads into a database an app controls, immune to search_path,
// operator and function hijacks. Extension installs use it too, always
// naming their schema (installExtensions).
func (m *Manager) psqlSafe(ctx context.Context, db, sql string, flags ...string) (string, error) {
	return m.psqlEnv(ctx, []string{safeSearchPath}, db, sql, flags...)
}

// psqlRowsSafe is psqlRows with the search_path pinned (psqlSafe).
func (m *Manager) psqlRowsSafe(ctx context.Context, db, sql string) ([]string, error) {
	out, err := m.psqlSafe(ctx, db, sql, "-A", "-t")
	if err != nil {
		return nil, err
	}
	return rows(out), nil
}

// psqlRows runs a query and returns its non-empty result rows. -A -t
// (unaligned, tuples-only) drops psql's table framing — header, dashes,
// "(n rows)" — so each line is exactly one row, columns joined by "|".
// Parsing the framed output instead means guessing which line is the
// value. On failure the error is psql's own message.
func (m *Manager) psqlRows(ctx context.Context, db, sql string) ([]string, error) {
	out, err := m.psql(ctx, db, sql, "-A", "-t")
	if err != nil {
		return nil, err
	}
	return rows(out), nil
}

func rows(out string) []string {
	var rs []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rs = append(rs, line)
		}
	}
	return rs
}

// terminateSessions ends every session on db (and so whatever a session
// was running, and every lock it held). The caller has validated db.
func (m *Manager) terminateSessions(ctx context.Context, db string) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	m.psql(ctx, "postgres", fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';`, db))
}

// dropDatabase ends every session on db, which DROP DATABASE would
// otherwise refuse over, then drops it. The caller has validated db.
func (m *Manager) dropDatabase(ctx context.Context, db string) error {
	m.terminateSessions(ctx, db)
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`DROP DATABASE IF EXISTS %s;`, db)); err != nil {
		return err
	}
	return nil
}

// RoleName is the Postgres role Provision creates for an app.
func RoleName(appName string) string { return strings.ReplaceAll(appName, "-", "_") }

// VerifyRole is the legacy fixed scratch role name. New verifies mint a fresh
// ephemeral role (ScratchRole) instead; this remains only for direct callers
// and tests that pass a role by name.
const VerifyRole = "fog_restore_scratch"

// ScratchRole mints a fresh, random-named, non-superuser login role for one
// restore of untrusted dump bytes, and returns it with a drop func. Random
// and short-lived so a dump replayed as it cannot set a password on a role an
// attacker knows the name of and could then log in as — by the time any
// ALTER ROLE in the dump runs, the name is unknown and the role is about to
// be dropped. It carries no password, so it can never log in over the network
// (the cluster's host auth is scram, which needs one); only fog's local
// unix-socket restore uses it.
func (m *Manager) ScratchRole(ctx context.Context) (role string, drop func(), err error) {
	name := "fog_scratch_" + randx.Hex(16)
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(
		`CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;`, name)); err != nil {
		return "", nil, fmt.Errorf("creating a scratch role: %w", err)
	}
	drop = func() {
		dctx := context.WithoutCancel(ctx)
		// Cut any session the dump may have opened on it, then drop it.
		m.psql(dctx, "postgres", fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = '%s';`, name))
		m.psql(dctx, "postgres", fmt.Sprintf(`DROP ROLE IF EXISTS %s;`, name))
	}
	return name, drop, nil
}

// RestoreDatabase restores db from a pg_dump custom-format file whose bytes
// are UNTRUSTED — an app's own database, or an app-supplied delegated dump
// (db/primary.dump) a verify is about to test-restore.
//
// Never as the superuser. The dump's SQL is replayed with only role's
// privileges: role is a non-superuser login role, forced NOSUPERUSER/
// NOCREATEROLE/NOCREATEDB; pg_restore connects AS it (a local trust
// connection — no password) with --no-owner --no-privileges, so every object
// comes out owned by role. A doctored dump therefore cannot `COPY … TO/FROM
// PROGRAM`, create C/untrusted-language functions, or touch any other app's
// database. For a real app restore role is the app's own role; for a verify
// scratch it is a fresh ScratchRole.
//
// Never by dropping the live database first. The dump is restored into a
// brand-new database, and only once that has fully succeeded is it swapped
// in: in one transaction the live database is renamed aside and the new one
// takes its name, and the old one is dropped only after that commits. A
// restore that fails at any point leaves the live database exactly as it was.
//
// Extensions are recreated as the dump uses them — each in the schema the
// dump names, and app-created ones (an owner may create trusted extensions)
// included — by the admin, before the role replays the rest. Only extensions
// fog permits an app to have are recreated: trusted ones (a database owner
// could create them anyway) and those in allowed (the app's spec, or what the
// live cluster already holds). Anything else is refused up front, by name,
// before any database is created. Every install goes through installExtensions,
// so no script runs with an app-steerable search_path.
func (m *Manager) RestoreDatabase(ctx context.Context, db, role, dumpPath string, allowed []string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.restoreDatabaseLocked(ctx, db, role, dumpPath, allowed)
}

// restoreDatabaseLocked is RestoreDatabase without taking m.mu — for callers
// that already hold it exclusively (the major upgrade), since sync.RWMutex is
// not reentrant.
func (m *Manager) restoreDatabaseLocked(ctx context.Context, db, role, dumpPath string, allowed []string) error {
	// An app's dump is the app's code: a function it planted can loop
	// forever once it sees it is being restored (current_database() is a
	// fog_rst_ name). So a restore is bounded by things the app cannot
	// inflate: a fixed cap on its whole run, and a stall detector that
	// ends it once nothing has moved for RestoreStall (watchRestore). Past
	// either, its sessions are terminated, the temporary database dropped,
	// and the live one left as it was.
	ctx, cancel := context.WithTimeoutCause(ctx, RestoreCap, errRestoreCap)
	defer cancel()
	err := m.restoreDatabaseBounded(ctx, db, role, dumpPath, allowed)
	if err != nil && ctx.Err() != nil {
		if cause := context.Cause(ctx); errors.Is(cause, errRestoreCap) || errors.Is(cause, errRestoreStalled) {
			return fmt.Errorf("restoring %s was stopped: %v — an app's dump can be built to hang a restore: %w", db, cause, err)
		}
	}
	if err != nil && errors.Is(err, errRestoreStalled) {
		return fmt.Errorf("restoring %s was stopped: %w — an app's dump can be built to hang a restore", db, err)
	}
	return err
}

// RestoreCap bounds one restore's (or dump's) whole run, and RestoreStall
// how long a restore may go without progress (watchRestore). Neither grows
// with what the app hands over. Vars so tests can shorten them.
var (
	RestoreCap   = 6 * time.Hour
	RestoreStall = 20 * time.Minute
)

var (
	errRestoreCap     = errors.New("it ran past the time any restore is given")
	errRestoreStalled = errors.New("it made no progress for too long")
)

func (m *Manager) restoreDatabaseBounded(ctx context.Context, db, role, dumpPath string, allowed []string) error {
	if !pgIdent(db) {
		return fmt.Errorf("invalid database name %q", db)
	}
	if !pgIdent(role) {
		return fmt.Errorf("invalid role name %q", role)
	}
	if role == "postgres" {
		// The whole point is to never replay untrusted dump bytes as the
		// superuser; being asked to is a caller bug, refused loudly.
		return errors.New("refusing to restore a dump as the superuser role")
	}
	for _, ext := range allowed {
		if !pgExtIdent(ext) {
			return fmt.Errorf("invalid extension name %q", ext)
		}
	}
	// Never restore over a database that belongs to another app: a squatter's
	// backup must not replace a neighbour's database.
	if owner, exists, err := m.databaseOwner(ctx, db); err != nil {
		return err
	} else if exists && owner != role {
		return fmt.Errorf("refusing to restore over database %s: it is owned by %q, not %s", db, owner, role)
	}
	// Read and vet the dump before creating anything: its table of contents,
	// the schemas it creates and the extensions it uses, each checked against
	// what fog permits. Nothing exists yet that a refusal would leave behind.
	toc, err := m.pgRestoreList(ctx, dumpPath)
	if err != nil {
		return err
	}
	schemas, exts, err := m.dumpSchemasAndExtensions(ctx, dumpPath, toc)
	if err != nil {
		return err
	}
	if err := m.checkExtensionsAllowed(ctx, exts, allowed); err != nil {
		return err
	}
	// The spec's own extensions go in too, as provisioning puts them in the
	// live database: an app that dumps without its extensions (pg_dump
	// --exclude-extension, so its non-owner role can restore the dump
	// itself) relies on them being there already.
	exts = withDeclared(exts, allowed)
	// A non-superuser login role owns the restore. The ALTER forces the safe
	// attributes even on a role that predates this: an app is never a
	// superuser, and the restore must not become one by accident.
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE %s LOGIN; END IF; END $$; ALTER ROLE %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;`,
		role, role, role)); err != nil {
		return fmt.Errorf("restore role %s: %w", role, err)
	}

	// Restore into a new database, sealed from every role (its owner
	// included) while fog creates its schemas and extensions.
	// It is born refusing every connection (ALLOW_CONNECTIONS false), so
	// there is no moment between CREATE and REVOKE in which its owner could
	// get in; connections are allowed again only once CONNECT is revoked.
	tmp := "fog_rst_" + randx.Hex(8)
	defer m.track(tmp)()
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`CREATE DATABASE %s OWNER %s ALLOW_CONNECTIONS false;`, tmp, role)); err != nil {
		return fmt.Errorf("creating a database to restore %s into: %w", db, err)
	}
	swapped := false
	defer func() {
		if !swapped {
			// Detached, and bounded on its own: this is also the cleanup
			// after a deadline, which must still end the restore's sessions
			// (dropDatabase terminates them) and release their locks.
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer ccancel()
			m.dropDatabase(cctx, tmp)
		}
	}()
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC; REVOKE CONNECT ON DATABASE %s FROM %s; ALTER DATABASE %s ALLOW_CONNECTIONS true;`, tmp, tmp, role, tmp)); err != nil {
		return fmt.Errorf("sealing the restore database: %w", err)
	}
	if len(schemas) > 0 {
		var b strings.Builder
		for _, sc := range schemas {
			fmt.Fprintf(&b, "CREATE SCHEMA %s AUTHORIZATION %s;\n", quoteIdent(sc), role)
		}
		if _, err := m.psqlSafe(ctx, tmp, b.String()); err != nil {
			return fmt.Errorf("recreating the dump's schemas: %w", err)
		}
	}
	if err := m.installExtensions(ctx, tmp, exts, true); err != nil {
		return fmt.Errorf("recreating the dump's extensions: %w", err)
	}
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s;`, tmp, role)); err != nil {
		return fmt.Errorf("opening the restore database to %s: %w", role, err)
	}
	if err := m.restoreDumpAsRole(ctx, tmp, role, dumpPath, filterRecreatedEntries(toc)); err != nil {
		return err
	}
	if err := m.swapDatabase(ctx, db, tmp); err != nil {
		return err
	}
	swapped = true
	return nil
}

// swapDatabase puts tmp in db's place. With no db yet, a rename. Otherwise,
// in one transaction, db is renamed aside and tmp takes its name — a failure
// rolls both back and leaves db as it was — and the old database is dropped
// only after that commits.
func (m *Manager) swapDatabase(ctx context.Context, db, tmp string) error {
	_, exists, err := m.databaseOwner(ctx, db)
	if err != nil {
		return err
	}
	terminate := func(name string) {
		m.psql(ctx, "postgres", fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';`, name))
	}
	terminate(tmp)
	if !exists {
		if _, err := m.psql(ctx, "postgres", fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s;`, tmp, db)); err != nil {
			return fmt.Errorf("putting the restored %s in place: %w", db, err)
		}
		return nil
	}
	old := "fog_old_" + randx.Hex(8)
	defer m.track(old)()
	terminate(db)
	// The replaced database is closed to every connection in the same
	// transaction that renames it aside: should the drop below fail, or fog
	// stop before it runs, it lingers sealed until the next Provision sweeps it.
	if _, err := m.psql(ctx, "postgres", fmt.Sprintf(
		`BEGIN; ALTER DATABASE %s RENAME TO %s; ALTER DATABASE %s ALLOW_CONNECTIONS false; ALTER DATABASE %s RENAME TO %s; COMMIT;`, db, old, old, tmp, db)); err != nil {
		return fmt.Errorf("swapping the restored %s in (the live database is unchanged): %w", db, err)
	}
	if err := m.dropDatabase(ctx, old); err != nil && m.Log != nil {
		m.Log.Warn("restore: the replaced database could not be dropped", "db", db, "kept as", old, "err", err)
	}
	return nil
}

// leakedRe is the name of a database a restore makes and drops: the
// temporary one restored into, and the live one renamed aside by the swap.
const leakedRe = `^fog_(rst|old)_[0-9a-f]{16}$`

var leakedName = regexp.MustCompile(leakedRe)

// sweepLeakedLocked drops every restore leftover — a fog_rst_ or fog_old_
// database a crash, a failed drop or a killed fogd left behind — sealing
// each against connections first. One a restore in flight is using
// (restoring) is left alone.
// track marks name as in use by a restore until the returned func runs.
func (m *Manager) track(name string) func() {
	m.restMu.Lock()
	if m.restoring == nil {
		m.restoring = map[string]bool{}
	}
	m.restoring[name] = true
	m.restMu.Unlock()
	return func() {
		m.restMu.Lock()
		delete(m.restoring, name)
		m.restMu.Unlock()
	}
}

func (m *Manager) inFlight(name string) bool {
	m.restMu.Lock()
	defer m.restMu.Unlock()
	return m.restoring[name]
}

func (m *Manager) sweepLeakedLocked(ctx context.Context) {
	names, err := m.psqlRows(ctx, "postgres", fmt.Sprintf(
		`SELECT datname FROM pg_catalog.pg_database WHERE datname ~ '%s';`, leakedRe))
	if err != nil {
		return
	}
	for _, name := range names {
		if !leakedName.MatchString(name) || m.inFlight(name) {
			continue
		}
		m.psql(ctx, "postgres", fmt.Sprintf(`ALTER DATABASE %s ALLOW_CONNECTIONS false;`, name))
		if err := m.dropDatabase(ctx, name); err != nil && m.Log != nil {
			m.Log.Warn("postgres: a leftover restore database could not be dropped", "db", name, "err", err)
		} else if m.Log != nil {
			m.Log.Info("postgres: dropped a leftover restore database", "db", name)
		}
	}
}

// restoreDumpAsRole replays an untrusted custom-format dump into db as a
// non-superuser role, restoring only the entries in list. The dump streams
// over stdin (never argv, never a bind the app could race); the restore list
// is staged into the container as its own object.
func (m *Manager) restoreDumpAsRole(ctx context.Context, db, role, dumpPath, list string) error {
	listPath, cleanup, err := m.stageList(ctx, list)
	if err != nil {
		return err
	}
	defer cleanup()
	f, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	in := &countingReader{r: f}
	go m.watchRestore(ctx, stop, db, in)
	cmd := execx.Command(ctx, "docker", "exec", "-i", m.ctr(),
		"pg_restore", "-U", role, "-d", db, "--no-owner", "--no-privileges", "-L", listPath)
	cmd.Stdin = in
	if out, err := execx.Run(cmd); err != nil {
		if errors.Is(context.Cause(ctx), errRestoreStalled) {
			m.terminateSessions(context.WithoutCancel(ctx), db)
			return fmt.Errorf("pg_restore %s: %w", db, errRestoreStalled)
		}
		if ctx.Err() != nil {
			// Killing the client stops nothing on the server: a function
			// looping in a backend runs on. End every session on db now.
			m.terminateSessions(context.WithoutCancel(ctx), db)
			return fmt.Errorf("pg_restore %s: %w", db, ctx.Err())
		}
		return fmt.Errorf("pg_restore %s: %s", db, strings.TrimSpace(out))
	}
	return nil
}

// countingReader counts what pg_restore has read of its dump.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// watchRestore ends a restore that has stopped moving: nothing more of
// the dump read, and on the server no new statement started, no COPY row
// added, no index block built — for RestoreStall. An honest restore of
// any size keeps moving; a function looping forever does not.
func (m *Manager) watchRestore(ctx context.Context, stop context.CancelCauseFunc, db string, in *countingReader) {
	tick := min(RestoreStall/4, 30*time.Second)
	last, since := "", time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
		qctx, cancel := context.WithTimeout(ctx, tick)
		fp, _ := m.psql(qctx, "postgres", fmt.Sprintf(`SELECT coalesce(string_agg(a.pid::text || ':' || coalesce(a.query_start::text, '') || ':' ||
  coalesce(c.tuples_processed::text, '') || ':' || coalesce((i.blocks_done + i.tuples_done)::text, ''), ',' ORDER BY a.pid), '')
  FROM pg_catalog.pg_stat_activity a
  LEFT JOIN pg_catalog.pg_stat_progress_copy c ON c.pid = a.pid
  LEFT JOIN pg_catalog.pg_stat_progress_create_index i ON i.pid = a.pid
 WHERE a.datname = '%s';`, db), "-A", "-t")
		cancel()
		fp = fmt.Sprintf("%d|%s", in.n.Load(), strings.TrimSpace(fp))
		if fp != last {
			last, since = fp, time.Now()
			continue
		}
		if time.Since(since) >= RestoreStall {
			stop(errRestoreStalled)
			return
		}
	}
}

// stageList writes a pg_restore list into the container (pg_restore -L takes
// a path; the dump itself comes over stdin).
func (m *Manager) stageList(ctx context.Context, list string) (string, func(), error) {
	listPath := fmt.Sprintf("/tmp/fog-restore-%s.list", randx.Hex(8))
	write := execx.Command(ctx, "docker", "exec", "-i", m.ctr(), "sh", "-c", "cat > "+listPath)
	write.Stdin = strings.NewReader(list)
	if out, err := execx.Run(write); err != nil {
		return "", nil, fmt.Errorf("staging the restore list: %s", strings.TrimSpace(out))
	}
	return listPath, func() {
		execx.Command(context.WithoutCancel(ctx), "docker", "exec", m.ctr(), "rm", "-f", listPath).Run()
	}, nil
}

// pgRestoreList reads a custom-format dump's table of contents (pg_restore
// -l), the dump streaming over stdin. A failure here (a truncated archive, a
// dump from a newer pg_dump than this cluster can read) is the error the
// restore would hit, and it comes before any database is created — though
// with the restore going into a new database and swapping in only on
// success, the live one is never at risk either way.
func (m *Manager) pgRestoreList(ctx context.Context, dumpPath string) (string, error) {
	f, err := os.Open(dumpPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd := execx.Command(ctx, "docker", "exec", "-i", m.ctr(), "pg_restore", "-l")
	cmd.Stdin = f
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("reading the dump's contents: %s", msg)
		}
		return "", fmt.Errorf("reading the dump's contents: %w", err)
	}
	return stdout.String(), nil
}

// tocDesc is a pg_restore -l line's entry description ("TABLE", "SCHEMA",
// "EXTENSION", "COMMENT - EXTENSION" …), "" for a comment or blank line.
// Lines read "<id>; <catOid> <oid> <DESC…> <schema> <name> <owner>".
func tocDesc(line string) string {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, ";") {
		return ""
	}
	_, rest, ok := strings.Cut(line, ";")
	if !ok {
		return ""
	}
	f := strings.Fields(rest)
	if len(f) < 3 {
		return ""
	}
	if f[2] == "COMMENT" && len(f) >= 5 && f[3] == "-" {
		return "COMMENT - " + f[4]
	}
	return f[2]
}

// filterRecreatedEntries drops the entries fog recreated as the admin — the
// dump's schemas and extensions, and the extensions' comments — from the list
// the role restores. Comment lines and blanks are kept as they are.
func filterRecreatedEntries(list string) string {
	var b strings.Builder
	for _, line := range strings.Split(list, "\n") {
		switch tocDesc(line) {
		case "SCHEMA", "EXTENSION", "COMMENT - EXTENSION":
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// The SQL pg_dump writes for a schema and an extension. The archive's text is
// the app's (a doctored dump can say anything), so it is matched strictly and
// every name re-quoted by fog before it reaches SQL; anything else among
// these entries is refused.
var (
	sqlIdent          = `("(?:[^"\x00]|"")+"|[a-z_][a-z0-9_$]*)`
	createSchemaRe    = regexp.MustCompile(`^CREATE SCHEMA ` + sqlIdent + `;$`)
	createExtensionRe = regexp.MustCompile(`^CREATE EXTENSION IF NOT EXISTS ` + sqlIdent + ` WITH SCHEMA ` + sqlIdent + `;$`)
)

func unquoteIdent(s string) string {
	if strings.HasPrefix(s, `"`) {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// dumpSchemasAndExtensions reads, from the dump itself, the schemas it
// creates and the extensions it uses with the schema each belongs in, in
// dump order (which respects dependencies).
func (m *Manager) dumpSchemasAndExtensions(ctx context.Context, dumpPath, toc string) (schemas []string, exts []extTarget, err error) {
	pick := schemaExtensionList(toc)
	if pick == "" {
		return nil, nil, nil
	}
	listPath, cleanup, err := m.stageList(ctx, pick)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	f, err := os.Open(dumpPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	cmd := execx.Command(ctx, "docker", "exec", "-i", m.ctr(), "pg_restore", "-L", listPath, "-f", "-")
	cmd.Stdin = f
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("reading the dump's schemas and extensions: %s", strings.TrimSpace(stderr.String()))
	}
	return parseSchemaExtensionSQL(stdout.String())
}

// schemaExtensionList is the part of a pg_restore -l list naming the dump's
// SCHEMA and EXTENSION entries, "" when it has none. pg_restore -L reads
// only each line's leading entry id, so a name with a newline in it (which
// splits its line in two) still picks its entry; the stray half has no id
// and is ignored.
func schemaExtensionList(toc string) string {
	var pick strings.Builder
	for _, line := range strings.Split(toc, "\n") {
		if d := tocDesc(line); d == "SCHEMA" || d == "EXTENSION" {
			pick.WriteString(line + "\n")
		}
	}
	return pick.String()
}

// sqlStatements splits a pg_restore script into statements, comments
// dropped, each trimmed. It understands what can carry a ';' or a newline
// without ending a statement — a "quoted identifier" (a schema an app named
// "a;b" or with a newline in it), a 'string', a $tag$ body — so a name that
// is odd but legal is read as the one name it is, never as two lines.
func sqlStatements(script string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if st := strings.TrimSpace(cur.String()); st != "" {
			out = append(out, st)
		}
		cur.Reset()
	}
	for i := 0; i < len(script); {
		c := script[i]
		switch {
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			for i < len(script) && script[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(script) {
				if script[j] == c {
					if j+1 < len(script) && script[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(script))
			cur.WriteString(script[i:end])
			i = end
		case c == '$':
			tag := dollarTagRe.FindString(script[i:])
			if tag == "" {
				cur.WriteByte(c)
				i++
				continue
			}
			end := strings.Index(script[i+len(tag):], tag)
			if end < 0 {
				cur.WriteString(script[i:])
				i = len(script)
				continue
			}
			stop := i + len(tag) + end + len(tag)
			cur.WriteString(script[i:stop])
			i = stop
		case c == ';':
			cur.WriteByte(c)
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

var dollarTagRe = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*)?\$`)

// parseSchemaExtensionSQL extracts CREATE SCHEMA and CREATE EXTENSION
// statements from pg_restore's script output, strictly, statement by
// statement (sqlStatements): an identifier is matched whole whatever it
// holds, and re-quoted by fog before it reaches SQL.
func parseSchemaExtensionSQL(script string) (schemas []string, exts []extTarget, err error) {
	for _, st := range sqlStatements(script) {
		switch {
		case strings.HasPrefix(st, "CREATE SCHEMA "):
			m := createSchemaRe.FindStringSubmatch(st)
			if m == nil {
				return nil, nil, fmt.Errorf("the dump creates a schema fog cannot read safely: %.120q", st)
			}
			name := unquoteIdent(m[1])
			if name == "public" || systemSchema(name) {
				continue
			}
			schemas = append(schemas, name)
		case strings.HasPrefix(st, "CREATE EXTENSION"):
			m := createExtensionRe.FindStringSubmatch(st)
			if m == nil {
				return nil, nil, fmt.Errorf("the dump creates an extension fog cannot read safely: %.120q", st)
			}
			name := unquoteIdent(m[1])
			if !pgExtIdent(name) {
				return nil, nil, fmt.Errorf("the dump uses an extension with an invalid name %q", name)
			}
			exts = append(exts, extTarget{Name: name, Schema: unquoteIdent(m[2])})
		}
	}
	return schemas, exts, nil
}

// checkExtensionsAllowed refuses, by name and before anything is created, an
// extension fog would not let the app have: one that is neither trusted (a
// database owner could create it itself) nor in allowed (declared by the
// app's spec, or already held by the live cluster), or one the cluster does
// not have at all.
func (m *Manager) checkExtensionsAllowed(ctx context.Context, exts []extTarget, allowed []string) error {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var refused []string
	for _, x := range exts {
		if ok[x.Name] {
			continue
		}
		_, _, trusted, err := m.extensionControl(ctx, "postgres", x.Name)
		if err != nil {
			return fmt.Errorf("the dump needs extension %s: %w", x.Name, err)
		}
		if !trusted {
			refused = append(refused, x.Name)
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("the dump needs extension(s) %s, which this app is not allowed: fog recreates only trusted extensions and those the app's spec declares — declare them in the spec (needs.postgres.extensions) to restore this dump", strings.Join(refused, ", "))
	}
	return nil
}

func pgIdent(s string) bool {
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return s != ""
}

// pgExtIdent is pgIdent plus a hyphen after the first character — extension
// names like uuid-ossp have one, and CREATE EXTENSION quotes them.
func pgExtIdent(s string) bool {
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (i > 0 && (r == '-' || (r >= '0' && r <= '9')))
		if !ok {
			return false
		}
	}
	return s != ""
}

// DumpDatabase streams pg_dump (custom format) for backups.
func (m *Manager) DumpDatabase(ctx context.Context, db string, w *os.File) error {
	// db lands in argv, where a name starting with '-' would parse as a
	// pg_dump flag. Every other entry point validates; same story here.
	if !pgIdent(db) {
		return fmt.Errorf("invalid database name %q", db)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Bounded like a restore: an app holding a lock on its own table must
	// not hold the dump (and the lock above it) forever.
	ctx, cancel := context.WithTimeout(ctx, RestoreCap)
	defer cancel()
	cmd := execx.Command(ctx, "docker", "exec", ContainerName, "pg_dump", "-U", "postgres", "--lock-wait-timeout=120s", "-Fc", db)
	cmd.Stdout = w
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer ccancel()
			m.psql(cctx, "postgres", fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND application_name = 'pg_dump';`, db))
			return fmt.Errorf("pg_dump %s did not finish in time and was stopped: %w", db, ctx.Err())
		}
		return fmt.Errorf("pg_dump %s: %s", db, strings.TrimSpace(errb.String()))
	}
	return nil
}

// DropDatabase removes a database outright (test-restore scratch cleanup).
func (m *Manager) DropDatabase(ctx context.Context, db string) error {
	if !pgIdent(db) {
		return fmt.Errorf("invalid database name %q", db)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := m.dropDatabase(ctx, db); err != nil {
		return fmt.Errorf("drop %s: %w", db, err)
	}
	return nil
}

// Extensions lists the extensions installed in a database with their
// versions — what provisioning actually left behind, as opposed to what a
// spec declares. -A -t (unaligned, tuples-only) sidesteps psql's table
// framing so each line is exactly "name|version".
func (m *Manager) Extensions(ctx context.Context, db string) (map[string]string, error) {
	if !pgIdent(db) {
		return nil, fmt.Errorf("invalid database name %q", db)
	}
	// Superuser read into an app-writable database: pin the search_path and
	// fully qualify, or a planted public.pg_extension view runs as fog.
	rs, err := m.psqlRowsSafe(ctx, db, `SELECT extname, extversion FROM pg_catalog.pg_extension;`)
	if err != nil {
		return nil, fmt.Errorf("extensions %s: %w", db, err)
	}
	exts := map[string]string{}
	for _, line := range rs {
		if name, ver, ok := strings.Cut(line, "|"); ok {
			exts[name] = ver
		}
	}
	return exts, nil
}

// ExtensionSchemas maps each extension installed in db to the schema it lives
// in — the major upgrade's per-schema inventory, so where an app chose to put
// an extension is carried across and checked, not assumed to be public.
func (m *Manager) ExtensionSchemas(ctx context.Context, db string) (map[string]string, error) {
	if !pgIdent(db) {
		return nil, fmt.Errorf("invalid database name %q", db)
	}
	rs, err := m.psqlRowsSafe(ctx, db, `SELECT e.extname || '|' || n.nspname FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) e.extnamespace;`)
	if err != nil {
		return nil, fmt.Errorf("extension schemas %s: %w", db, err)
	}
	out := map[string]string{}
	for _, line := range rs {
		if name, schema, ok := strings.Cut(line, "|"); ok {
			out[name] = schema
		}
	}
	return out, nil
}

// Databases lists the non-template databases besides postgres itself.
func (m *Manager) Databases(ctx context.Context) ([]string, error) {
	dbs, err := m.psqlRows(ctx, "postgres", `SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres';`)
	if err != nil {
		return nil, fmt.Errorf("databases: %w", err)
	}
	return dbs, nil
}

// PublicTableCount answers how many tables a database has — the sanity check
// after a test-restore that a dump actually contained a schema.
func (m *Manager) PublicTableCount(ctx context.Context, db string) (int, error) {
	if !pgIdent(db) {
		return 0, fmt.Errorf("invalid database name %q", db)
	}
	// Superuser read into a scratch database restored from an app dump: pin
	// the search_path, qualify the catalog, and bind the operator to
	// pg_catalog so a planted public.=(name,name) cannot run as fog.
	rs, err := m.psqlRowsSafe(ctx, db, `SELECT pg_catalog.count(*) FROM pg_catalog.pg_tables WHERE schemaname OPERATOR(pg_catalog.=) 'public';`)
	if err != nil {
		return 0, fmt.Errorf("table count: %w", err)
	}
	if len(rs) != 1 {
		return 0, fmt.Errorf("unparseable table count output: %q", rs)
	}
	n, err := strconv.Atoi(rs[0])
	if err != nil {
		return 0, fmt.Errorf("unparseable table count output: %q", rs[0])
	}
	return n, nil
}

// Roles lists the login roles besides the superuser — one per provisioned
// app. The upgrade compares this across the move.
func (m *Manager) Roles(ctx context.Context) ([]string, error) {
	roles, err := m.psqlRows(ctx, "postgres", `SELECT rolname FROM pg_roles WHERE rolname <> 'postgres' AND rolname NOT LIKE 'pg\_%' ORDER BY 1;`)
	if err != nil {
		return nil, fmt.Errorf("roles: %w", err)
	}
	return roles, nil
}

// TableRowCounts counts the rows of every user table in a database, keyed
// "schema.table". Exact counts, not pg_class estimates: this is what the
// upgrade checks a restore against, and an estimate that happens to match
// proves nothing.
func (m *Manager) TableRowCounts(ctx context.Context, db string) (map[string]int64, error) {
	if !pgIdent(db) {
		return nil, fmt.Errorf("invalid database name %q", db)
	}
	// This runs as the superuser against databases apps control, and it calls
	// unqualified functions (format, query_to_xml) and operators whose
	// resolution follows the session search_path — and the dynamic count
	// query runs under it too. The connection pins search_path to the catalog
	// (psqlRowsSafe), which an app's ALTER DATABASE SET cannot override, so
	// every name here resolves to a catalog object, never an app's.
	rs, err := m.psqlRowsSafe(ctx, db, `SELECT table_schema || '.' || table_name,
		        (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', table_schema, table_name), false, true, '')))[1]::text
		   FROM information_schema.tables
		  WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')
		  ORDER BY 1;`)
	if err != nil {
		return nil, fmt.Errorf("row counts %s: %w", db, err)
	}
	counts := map[string]int64{}
	for _, line := range rs {
		name, n, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("row counts %s: unparseable %q", db, line)
		}
		counts[name] = v
	}
	return counts, nil
}
