-- Notices: persistent in-app messages that need the operator's attention —
-- things Fog cannot do itself (e.g. revoke a deploy key on the git host).
-- They stay until explicitly dismissed; ntfy pushes are transient, this is
-- the durable inbox.
CREATE TABLE notices (
    id           INTEGER PRIMARY KEY,
    created_at   TEXT NOT NULL,
    kind         TEXT NOT NULL,             -- e.g. 'deploy-key-cleanup'
    app          TEXT NOT NULL DEFAULT '',  -- related app name, if any
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    dismissed_at TEXT
);
CREATE INDEX idx_notices_active ON notices(dismissed_at);
