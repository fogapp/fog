-- Phase 1: apps, config revisions, secrets, deploys.

CREATE TABLE apps (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,          -- dns-safe slug; also the container + network alias
    desired_state TEXT NOT NULL DEFAULT 'running', -- running | stopped
    current_rev   INTEGER NOT NULL DEFAULT 0,      -- revision currently materialized/deployed
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- Every spec change is a new immutable revision; deploys pin a revision so
-- rollback restores code AND config together.
CREATE TABLE app_revisions (
    id         INTEGER PRIMARY KEY,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    rev        INTEGER NOT NULL,                  -- 1,2,3… per app
    spec       TEXT NOT NULL,                     -- AppSpec JSON (no secret values)
    comment    TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (app_id, rev)
);

-- Secret values live outside the spec, encrypted with the vault master key.
-- app_id NULL = system-level secret (e.g. DNS provider token).
CREATE TABLE secrets (
    id         INTEGER PRIMARY KEY,
    app_id     INTEGER REFERENCES apps(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    ciphertext TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (app_id, key)
);

CREATE TABLE deploys (
    id          INTEGER PRIMARY KEY,
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    revision_id INTEGER NOT NULL REFERENCES app_revisions(id),
    status      TEXT NOT NULL,                    -- running | succeeded | failed
    log         TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT
);
CREATE INDEX idx_deploys_app ON deploys(app_id, id);
