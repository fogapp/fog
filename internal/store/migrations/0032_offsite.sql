-- Offsite backups (docs/locker-contract.md): the object-storage targets a
-- locker is written to, and the record of what has been copied where.

CREATE TABLE offsite_targets (
    id             INTEGER PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,       -- "backblaze"
    provider       TEXT NOT NULL DEFAULT 's3', -- preset hint; the wire is always S3
    endpoint       TEXT NOT NULL,              -- https://s3.us-west-004.backblazeb2.com
    region         TEXT NOT NULL DEFAULT '',
    bucket         TEXT NOT NULL,
    prefix         TEXT NOT NULL DEFAULT '',
    path_style     INTEGER NOT NULL DEFAULT 1,
    locker_id      TEXT NOT NULL,
    role           TEXT NOT NULL DEFAULT 'active', -- active | retired
    grade          TEXT NOT NULL DEFAULT '',   -- A | B | C | D from the last probe
    probe          TEXT NOT NULL DEFAULT '',   -- JSON detail of the last probe
    probed_at      TEXT,
    price_per_tb   REAL NOT NULL DEFAULT 6.95,
    max_mbps       INTEGER NOT NULL DEFAULT 0,
    window         TEXT NOT NULL DEFAULT '',   -- "01:00-07:00" UTC, or ''
    last_sync_at   TEXT,
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);

CREATE TABLE offsite_copies (
    id           INTEGER PRIMARY KEY,
    target_id    INTEGER NOT NULL REFERENCES offsite_targets(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,        -- kit | app | files-snapshot | files-blob
    subject      TEXT NOT NULL,        -- backups.id / file_snapshots.id / blob id / kit name
    key          TEXT NOT NULL,        -- object name in the bucket
    bytes        INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL,        -- uploading | stored | hidden
    upload_id    TEXT NOT NULL DEFAULT '',
    nonce        TEXT NOT NULL DEFAULT '',   -- base64 seal nonce, for a resumed upload
    doc          TEXT NOT NULL DEFAULT '',   -- the sealed row/manifest document, in the clear, for the index
    uploaded_at  TEXT,
    verified_at  TEXT,
    hidden_at    TEXT,
    retain_until TEXT,
    UNIQUE (target_id, kind, subject)
);
CREATE INDEX idx_offsite_copies_state ON offsite_copies(target_id, state, kind);
