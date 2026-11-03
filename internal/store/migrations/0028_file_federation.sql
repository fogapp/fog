-- Files across peers (docs/files-contract.md). One migration for
-- both halves of the exchange so the schema arrives whole.
--
-- file_fed_shares is the consent row: one folder, one peer, one lens. Rows
-- go with the peer (ON DELETE CASCADE): unpairing ends every share.
-- revision bumps on any lens change and namespaces the peer's manifest, so
-- a narrowed lens scrubs remote copies on their next pull. dirty is the
-- "nudge the peer" flag, set by any change under the folder.
--
-- file_share_excludes are the entries marked "keep on this box": the floor
-- no lens can reach past. Path-keyed like the backup exclusions, and
-- followed by fog's own renames.
--
-- file_fed_subscriptions is the receiving half: one row per (peer, remote
-- share), its local folder, the impact sentence the operator accepted and
-- the one the origin currently states. file_fed_replicas is the provenance
-- of every file the sync landed (or, dehydrated, only knows about); a row
-- here is a cache of someone else's file and is never re-shared.
--
-- file_fed_origins marks files at *this* origin that a peer contributed:
-- theirs to edit, ours to hold.
CREATE TABLE file_fed_shares (
    id              INTEGER PRIMARY KEY,
    peer_id         INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    path            TEXT NOT NULL,
    title           TEXT NOT NULL,
    mode            TEXT NOT NULL DEFAULT 'read',      -- read | contribute
    scope           TEXT NOT NULL DEFAULT 'all',       -- all | picked | filtered
    detail          TEXT NOT NULL DEFAULT 'full',      -- full | previews | listing
    since           TEXT NOT NULL DEFAULT '',          -- RFC3339 or ''
    apps            TEXT NOT NULL DEFAULT 'allowed',   -- allowed | denied
    revision        INTEGER NOT NULL DEFAULT 1,
    dirty           INTEGER NOT NULL DEFAULT 0,
    bytes_served    INTEGER NOT NULL DEFAULT 0,
    last_fetched_at TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (peer_id, path)
);

CREATE TABLE file_fed_share_picks (
    share_id INTEGER NOT NULL REFERENCES file_fed_shares(id) ON DELETE CASCADE,
    rel      TEXT NOT NULL,                             -- inside the share folder
    PRIMARY KEY (share_id, rel)
);

CREATE TABLE file_fed_share_filters (
    share_id INTEGER NOT NULL REFERENCES file_fed_shares(id) ON DELETE CASCADE,
    filter   TEXT NOT NULL,                             -- kind:<category> | glob:<pattern>
    PRIMARY KEY (share_id, filter)
);

CREATE TABLE file_share_excludes (
    path       TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);

CREATE TABLE file_fed_subscriptions (
    id                   INTEGER PRIMARY KEY,
    peer_id              INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    remote_share_id      INTEGER NOT NULL,
    title                TEXT NOT NULL,
    folder               TEXT NOT NULL UNIQUE,          -- root-relative
    mode                 TEXT NOT NULL DEFAULT 'read',
    detail_offered       TEXT NOT NULL DEFAULT 'full',
    detail_wanted        TEXT NOT NULL DEFAULT 'full',  -- full | previews
    apps_allowed         INTEGER NOT NULL DEFAULT 1,
    status               TEXT NOT NULL DEFAULT 'active', -- active | paused | revoked | error | offer_changed
    accepted_impact      TEXT NOT NULL DEFAULT '',
    current_impact       TEXT NOT NULL DEFAULT '',
    revision_seen        INTEGER NOT NULL DEFAULT 0,
    manifest_etag        TEXT NOT NULL DEFAULT '',
    last_synced_at       TEXT NOT NULL DEFAULT '',
    last_error           TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_attempt_at      TEXT NOT NULL DEFAULT '',
    quota_bytes          INTEGER NOT NULL DEFAULT 0,
    backup               INTEGER NOT NULL DEFAULT 0,
    files                INTEGER NOT NULL DEFAULT 0,
    bytes                INTEGER NOT NULL DEFAULT 0,
    created_at           TEXT NOT NULL,
    UNIQUE (peer_id, remote_share_id)
);

CREATE TABLE file_fed_replicas (
    subscription_id INTEGER NOT NULL REFERENCES file_fed_subscriptions(id) ON DELETE CASCADE,
    rel             TEXT NOT NULL,                      -- inside the subscription folder
    kind            TEXT NOT NULL DEFAULT 'file',
    sha256          TEXT NOT NULL DEFAULT '',
    size            INTEGER NOT NULL DEFAULT 0,
    mtime           INTEGER NOT NULL DEFAULT 0,        -- unix nanos
    hydrated        INTEGER NOT NULL DEFAULT 1,
    origin          TEXT NOT NULL DEFAULT '',          -- author instance for contributions; '' = the origin peer
    PRIMARY KEY (subscription_id, rel)
);

CREATE TABLE file_fed_origins (
    path           TEXT PRIMARY KEY,                    -- root-relative, at this origin
    peer_id        INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    share_id       INTEGER NOT NULL REFERENCES file_fed_shares(id) ON DELETE CASCADE,
    instance_id    TEXT NOT NULL,
    sha256         TEXT NOT NULL DEFAULT '',
    size           INTEGER NOT NULL DEFAULT 0,
    contributed_at TEXT NOT NULL
);
