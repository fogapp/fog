-- Phase 3: image update tracking and backup records.

CREATE TABLE app_updates (
    app_id        INTEGER PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    local_digest  TEXT NOT NULL DEFAULT '',
    remote_digest TEXT NOT NULL DEFAULT '',
    available     INTEGER NOT NULL DEFAULT 0,
    checked_at    TEXT NOT NULL
);

CREATE TABLE backups (
    id         INTEGER PRIMARY KEY,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,   -- manual | scheduled | pre-update
    path       TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_backups_app ON backups(app_id, id);
