package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// openAtMigration builds a database with only the migrations strictly below
// `stop` applied — the state a real instance was in before upgrading.
func openAtMigration(t *testing.T, path, stop string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(OFF)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if name >= stop {
			break
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, now()); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// A released-0012 instance (old peers table, forwarding settings, queued
// outbox rows, stray peer-owned groups) upgrades cleanly: the legacy pieces
// are dropped wholesale, and local error history survives the rebuild.
func TestPreFederationStateIsDroppedCleanly(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "fog.db")
	db := openAtMigration(t, path, "0013")
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", q, err)
		}
	}
	// 0012-era state: a token peer, one of its groups, a local app + group,
	// forwarding settings, a queued outbox row, the vault token secret.
	seed(`INSERT INTO peers (name, token_digest, created_at) VALUES ('dave', 'digest-d', ?)`, now())
	seed(`INSERT INTO apps (name, desired_state, created_at, updated_at) VALUES ('manor', 'running', ?, ?)`, now(), now())
	seed(`INSERT INTO error_groups (app_id, peer_id, app_name, fingerprint, error_class, title, first_seen, last_seen)
		VALUES (NULL, 1, 'manor', 'fp-peer', 'Boom', 'peer boom', ?, ?)`, now(), now())
	seed(`INSERT INTO error_groups (app_id, peer_id, app_name, fingerprint, error_class, title, first_seen, last_seen)
		VALUES (1, NULL, 'manor', 'fp-local', 'Boom', 'local boom', ?, ?)`, now(), now())
	// Both groups carry stored events: the peer group's must go with it (the
	// rebuild runs with foreign keys off, so no cascade covers for it), the
	// local group's must survive.
	seed(`INSERT INTO error_events (group_id, event_id, received_at, payload) VALUES (1, 'ev-peer', ?, '{}')`, now())
	seed(`INSERT INTO error_events (group_id, event_id, received_at, payload) VALUES (2, 'ev-local', ?, '{}')`, now())
	seed(`INSERT INTO settings (key, value, updated_at) VALUES ('flare_forward_url', 'https://fog.friend.example', ?)`, now())
	seed(`INSERT INTO settings (key, value, updated_at) VALUES ('flare_forward_mode', 'redacted', ?)`, now())
	seed(`INSERT INTO flare_outbox (app_name, payload, next_attempt_at, created_at) VALUES ('manor', '{"queued":1}', ?, ?)`, now(), now())
	seed(`INSERT INTO secrets (app_id, key, ciphertext, updated_at) VALUES (NULL, '_flare_forward_token', 'ct', ?)`, now())
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// No peers came over — federation starts empty, by pairing only.
	if peers, _ := s.ListFedPeers(); len(peers) != 0 {
		t.Fatalf("legacy peers survived: %+v", peers)
	}
	// The stray peer group is gone; the local group is intact.
	groups, err := s.ListOpenErrorGroupsAll(10)
	if err != nil || len(groups) != 1 || groups[0].Fingerprint != "fp-local" || groups[0].AppID != 1 {
		t.Fatalf("groups after migration: %v %+v", err, groups)
	}
	// Settings and the token secret are gone.
	if v, _ := s.GetSetting("flare_forward_url"); v != "" {
		t.Fatal("forwarding settings survived")
	}
	if _, err := s.GetSecret(0, "_flare_forward_token"); err != ErrNotFound {
		t.Fatalf("forwarding token survived: %v", err)
	}
	// The old tables are gone outright.
	for _, table := range []string{"peers", "flare_outbox"} {
		var n int
		err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil || n != 0 {
			t.Fatalf("table %s still exists", table)
		}
	}
	// The dropped peer group took its events with it; the local group kept
	// its own; nothing dangles.
	var events []string
	rows, err := s.DB().Query(`SELECT event_id FROM error_events ORDER BY event_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		events = append(events, id)
	}
	if len(events) != 1 || events[0] != "ev-local" {
		t.Fatalf("events after migration: %v", events)
	}
	var orphans int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM error_events e
		WHERE NOT EXISTS (SELECT 1 FROM error_groups g WHERE g.id = e.group_id)`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d orphaned error_events after migration", orphans)
	}
}

// An instance that upgraded through the original 0013 — which dropped peer
// groups with foreign keys off and left their events behind — gets the
// orphans swept by 0021.
func TestOrphanedErrorEventsAreSwept(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "fog.db")
	db := openAtMigration(t, path, "0021")
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", q, err)
		}
	}
	seed(`INSERT INTO apps (name, desired_state, created_at, updated_at) VALUES ('manor', 'running', ?, ?)`, now(), now())
	seed(`INSERT INTO error_groups (app_id, peer_id, app_name, fingerprint, error_class, title, first_seen, last_seen)
		VALUES (1, NULL, 'manor', 'fp-local', 'Boom', 'local boom', ?, ?)`, now(), now())
	seed(`INSERT INTO error_events (group_id, event_id, received_at, payload) VALUES (1, 'ev-local', ?, '{}')`, now())
	// The orphan the original 0013 left: its group row no longer exists.
	seed(`INSERT INTO error_events (group_id, event_id, received_at, payload) VALUES (999, 'ev-orphan', ?, '{}')`, now())
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var got string
	if err := s.DB().QueryRow(`SELECT group_concat(event_id) FROM error_events`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ev-local" {
		t.Fatalf("events after sweep: %q", got)
	}
}
