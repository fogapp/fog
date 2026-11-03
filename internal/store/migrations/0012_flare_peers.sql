-- fog:fk-off  (this migration rebuilds error_groups; see store.go migrate)
--
-- Flare forwarding (clients/flare/README.md): a friend's Fog instance can
-- relay its fleet's errors here. Three pieces:
--
--   peers        — who may submit; token digests only, session-style.
--   flare_outbox — the sender side's store-and-forward queue (scrubbed
--                  payloads, bounded, retried with backoff).
--   error_groups — rebuilt so a group belongs to EITHER a local app OR a
--                  peer. A peer may run apps this instance doesn't, so peer
--                  groups carry the app *name*, not an apps(id) reference.

CREATE TABLE peers (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    token_digest TEXT NOT NULL UNIQUE,   -- SHA-256 of the bearer token; plaintext shown once
    created_at   TEXT NOT NULL,
    last_seen_at TEXT
);

CREATE TABLE flare_outbox (
    id              INTEGER PRIMARY KEY,
    app_name        TEXT NOT NULL,
    payload         TEXT NOT NULL,       -- event JSON, already scrubbed per the forwarding mode
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    created_at      TEXT NOT NULL
);
CREATE INDEX idx_flare_outbox_due ON flare_outbox (next_attempt_at);

-- Rebuild: app_id becomes nullable, peer_id and app_name arrive. The CHECK
-- makes "exactly one owner" a schema fact, not a convention.
CREATE TABLE error_groups_new (
    id            INTEGER PRIMARY KEY,
    app_id        INTEGER REFERENCES apps(id) ON DELETE CASCADE,
    peer_id       INTEGER REFERENCES peers(id) ON DELETE CASCADE,
    app_name      TEXT NOT NULL DEFAULT '',
    fingerprint   TEXT NOT NULL,
    error_class   TEXT NOT NULL,
    title         TEXT NOT NULL,
    culprit       TEXT NOT NULL DEFAULT '',
    level         TEXT NOT NULL DEFAULT 'error',
    status        TEXT NOT NULL DEFAULT 'open',
    events_count  INTEGER NOT NULL DEFAULT 0,
    unhandled     INTEGER NOT NULL DEFAULT 0,
    first_seen    TEXT NOT NULL,
    last_seen     TEXT NOT NULL,
    first_release TEXT NOT NULL DEFAULT '',
    last_release  TEXT NOT NULL DEFAULT '',
    resolved_at        TEXT,
    resolved_release   TEXT,
    ignored_until      TEXT,
    ignore_until_count INTEGER,
    last_notified_at   TEXT,
    CHECK ((app_id IS NULL) <> (peer_id IS NULL))
);
INSERT INTO error_groups_new (id, app_id, peer_id, app_name, fingerprint, error_class, title,
        culprit, level, status, events_count, unhandled, first_seen, last_seen, first_release,
        last_release, resolved_at, resolved_release, ignored_until, ignore_until_count, last_notified_at)
    SELECT g.id, g.app_id, NULL, COALESCE((SELECT a.name FROM apps a WHERE a.id = g.app_id), ''),
        g.fingerprint, g.error_class, g.title, g.culprit, g.level, g.status, g.events_count,
        g.unhandled, g.first_seen, g.last_seen, g.first_release, g.last_release, g.resolved_at,
        g.resolved_release, g.ignored_until, g.ignore_until_count, g.last_notified_at
    FROM error_groups g;
DROP TABLE error_groups;
ALTER TABLE error_groups_new RENAME TO error_groups;

CREATE INDEX idx_error_groups_triage ON error_groups (app_id, status, last_seen DESC);
-- Uniqueness needs two partial indexes: SQLite treats NULLs as distinct
-- inside UNIQUE constraints, so one constraint spanning a nullable column
-- would happily store duplicates.
CREATE UNIQUE INDEX idx_error_groups_local ON error_groups (app_id, fingerprint) WHERE peer_id IS NULL;
CREATE UNIQUE INDEX idx_error_groups_peer ON error_groups (peer_id, app_name, fingerprint) WHERE peer_id IS NOT NULL;
