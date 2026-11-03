-- Issues: findings from fog's configuration scanner (internal/issues).
-- One row per distinct checkable fact, keyed (kind, app, subject). Each
-- sweep re-evaluates every check: a finding that reproduces bumps last_seen;
-- one that stops reproducing auto-resolves (rows kept as history); a
-- resolved fact that reproduces reopens with its acknowledgement cleared.
-- Acknowledging silences an open issue until its details materially change
-- (details_hash covers severity|title|detail|fix — not data, which is
-- cosmetic payload for the UI and must not break an ack).
CREATE TABLE issues (
    id               INTEGER PRIMARY KEY,
    kind             TEXT NOT NULL,                   -- check id, e.g. 'pg-dump-skew'
    app_id           INTEGER REFERENCES apps(id) ON DELETE CASCADE,  -- NULL = host scope
    app_name         TEXT NOT NULL DEFAULT '',        -- denormalized for display
    subject          TEXT NOT NULL DEFAULT '',        -- disambiguator within (kind, app)
    severity         TEXT NOT NULL DEFAULT 'warning', -- critical | warning | info
    title            TEXT NOT NULL,
    detail           TEXT NOT NULL DEFAULT '',
    fix              TEXT NOT NULL DEFAULT '',        -- actionable remediation hint
    data             TEXT NOT NULL DEFAULT '',        -- structured JSON for the UI
    status           TEXT NOT NULL DEFAULT 'open',    -- open | acked | resolved
    details_hash     TEXT NOT NULL,
    first_seen       TEXT NOT NULL,
    last_seen        TEXT NOT NULL,
    resolved_at      TEXT,
    acked_at         TEXT,
    last_notified_at TEXT
);

-- app_id is NULL for host-scope issues, and UNIQUE treats NULLs as
-- distinct — so identity is enforced over COALESCE(app_id, 0). Every
-- lookup must use the same expression or duplicates appear.
CREATE UNIQUE INDEX idx_issues_identity ON issues (kind, COALESCE(app_id, 0), subject);
CREATE INDEX idx_issues_triage ON issues (status, severity, last_seen DESC);
