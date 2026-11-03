-- fog:fk-off  (this migration rebuilds error_groups; see store.go migrate)
--
-- Federation (docs/federation-contract.md): peers stop being flare-only
-- token ACLs and become first-class, mutual, capability-scoped relationships
-- between Fog instances.
--
--   fed_identity     — this instance's own name in the network: a ULID plus
--                      an ed25519 public key (private key in the vault).
--   fed_peers        — one row per federated instance, established by the
--                      pairing handshake. Always identity-keyed: signed
--                      requests are the only peer credential.
--   fed_capabilities — what each peer may do here (direction 'grant') and
--                      what this instance does toward it (direction 'use').
--   fed_invites      — single-use pairing secrets, digest-stored like
--                      sessions, with the grants pre-selected at mint time.
--
-- The pre-federation peers table (0012) is dropped outright — no deployment
-- ever used it — and any stray peer-owned error groups go with it. Local
-- groups survive the rebuild untouched.

CREATE TABLE fed_identity (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    instance_id TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE fed_peers (
    id            INTEGER PRIMARY KEY,
    instance_id   TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL UNIQUE,           -- local alias; editable
    url           TEXT,                           -- NULL = dial-in-only peer
    public_key    TEXT NOT NULL,
    fog_version   TEXT NOT NULL DEFAULT '',
    remote_grants TEXT NOT NULL DEFAULT '[]',     -- JSON, from last heartbeat (display only)
    directory     TEXT NOT NULL DEFAULT '[]',     -- JSON, their exposed apps
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT
);

CREATE TABLE fed_capabilities (
    peer_id    INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    capability TEXT NOT NULL,           -- flare.ingest | apps.directory | apps.introduce
    direction  TEXT NOT NULL,           -- grant (they may, against us) | use (we do, toward them)
    config     TEXT NOT NULL DEFAULT '{}',
    UNIQUE (peer_id, capability, direction)
);

CREATE TABLE fed_invites (
    id            INTEGER PRIMARY KEY,
    secret_digest TEXT NOT NULL UNIQUE,
    grants        TEXT NOT NULL DEFAULT '[]',
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    claimed_by    TEXT                            -- instance_id, once used
);

-- Rebuild error_groups so the peer FK points at fed_peers. Groups owned by
-- old-style peers (none exist in practice) cannot carry over — their owner
-- table is going away — so they're dropped before the copy. Their stored
-- events go first: foreign keys are off here, so ON DELETE CASCADE won't.
DELETE FROM error_events WHERE group_id IN (SELECT id FROM error_groups WHERE peer_id IS NOT NULL);
DELETE FROM error_groups WHERE peer_id IS NOT NULL;

CREATE TABLE error_groups_new (
    id            INTEGER PRIMARY KEY,
    app_id        INTEGER REFERENCES apps(id) ON DELETE CASCADE,
    peer_id       INTEGER REFERENCES fed_peers(id) ON DELETE CASCADE,
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
    SELECT id, app_id, NULL, app_name, fingerprint, error_class, title,
        culprit, level, status, events_count, unhandled, first_seen, last_seen, first_release,
        last_release, resolved_at, resolved_release, ignored_until, ignore_until_count, last_notified_at
    FROM error_groups;
DROP TABLE error_groups;
ALTER TABLE error_groups_new RENAME TO error_groups;

CREATE INDEX idx_error_groups_triage ON error_groups (app_id, status, last_seen DESC);
CREATE UNIQUE INDEX idx_error_groups_local ON error_groups (app_id, fingerprint) WHERE peer_id IS NULL;
CREATE UNIQUE INDEX idx_error_groups_peer ON error_groups (peer_id, app_name, fingerprint) WHERE peer_id IS NOT NULL;

DROP TABLE peers;
