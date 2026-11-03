-- Flare: error tracking for the fleet (clients/flare/README.md).
--
-- Two-table shape every small tracker converges on: a group row per distinct
-- failure (keyed by fingerprint) carrying the counters and triage state, and
-- an event row per stored occurrence carrying the full payload. Payload
-- storage is capped per group at ingest time; the counters on the group are
-- never capped — the number shown is always the number that happened.
CREATE TABLE error_groups (
    id            INTEGER PRIMARY KEY,
    app_id        INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    fingerprint   TEXT NOT NULL,
    error_class   TEXT NOT NULL,
    title         TEXT NOT NULL,
    culprit       TEXT NOT NULL DEFAULT '',   -- "PostsController#show" / top in-app frame
    level         TEXT NOT NULL DEFAULT 'error',
    status        TEXT NOT NULL DEFAULT 'open',  -- open | resolved | regressed | ignored
    events_count  INTEGER NOT NULL DEFAULT 0,
    unhandled     INTEGER NOT NULL DEFAULT 0,    -- any occurrence was an unrescued crash
    first_seen    TEXT NOT NULL,
    last_seen     TEXT NOT NULL,
    first_release TEXT NOT NULL DEFAULT '',
    last_release  TEXT NOT NULL DEFAULT '',
    -- resolved_release is the app's deployed commit at resolve time. A later
    -- event only regresses the group if it carries a *different* release —
    -- otherwise "resolved" ping-pongs during the deploy containing the fix.
    resolved_at        TEXT,
    resolved_release   TEXT,
    -- Ignore escape hatches: for a duration, or until N total occurrences.
    -- Both NULL with status='ignored' means ignored forever.
    ignored_until      TEXT,
    ignore_until_count INTEGER,
    last_notified_at   TEXT,
    UNIQUE (app_id, fingerprint)
);
CREATE INDEX idx_error_groups_triage ON error_groups (app_id, status, last_seen DESC);

CREATE TABLE error_events (
    id           INTEGER PRIMARY KEY,
    group_id     INTEGER NOT NULL REFERENCES error_groups(id) ON DELETE CASCADE,
    event_id     TEXT NOT NULL,               -- client-minted UUID; dedupes SDK retries
    received_at  TEXT NOT NULL,
    release      TEXT NOT NULL DEFAULT '',
    environment  TEXT NOT NULL DEFAULT '',
    txn          TEXT NOT NULL DEFAULT '',    -- "transaction" is reserved in SQL
    payload      TEXT NOT NULL                -- Sentry-schema event JSON
);
CREATE INDEX idx_error_events_group ON error_events (group_id, id DESC);
-- SDK transports retry on ambiguous failures; the same event must not count
-- twice. Uniqueness is per group, which is enough — a UUID colliding across
-- groups would be two different errors sharing an ID, which the insert-or-
-- ignore treats as the fluke it is.
CREATE UNIQUE INDEX idx_error_events_dedupe ON error_events (group_id, event_id);
