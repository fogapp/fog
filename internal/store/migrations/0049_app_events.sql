-- The household event stream (docs/events-contract.md): what apps and fogd
-- itself report as having happened, kept a week for consumers that read it
-- with a cursor. seq is the cursor — monotonic, never reused (AUTOINCREMENT),
-- so a consumer that resumes after a prune can tell it missed rows. A
-- publisher's retry of the same event id is absorbed by the unique key.
CREATE TABLE app_events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    source     TEXT NOT NULL,
    id         TEXT NOT NULL,
    type       TEXT NOT NULL,
    subject    TEXT NOT NULL DEFAULT '',
    time       TEXT NOT NULL,
    data       TEXT NOT NULL DEFAULT 'null',
    created_at TEXT NOT NULL,
    UNIQUE (source, id)
);
CREATE INDEX app_events_created ON app_events (created_at);
