-- What apps asked of compute (Settings › Compute › Activity): each inference
-- request and each video run, from the moment fog took it to its outcome.
-- App and machine are kept by name, so the history outlives either.
CREATE TABLE compute_activity (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          TEXT NOT NULL,
    ended_at    TEXT NOT NULL DEFAULT '',
    app         TEXT NOT NULL,
    pool        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT '',
    what        TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL,
    status      INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    ttft_ms     INTEGER NOT NULL DEFAULT 0,
    tokens_in   INTEGER NOT NULL DEFAULT 0,
    tokens_out  INTEGER NOT NULL DEFAULT 0,
    -- The server reported no usage; tokens_out counts streamed chunks.
    tokens_estimated INTEGER NOT NULL DEFAULT 0,
    message     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX compute_activity_at ON compute_activity (at);
CREATE INDEX compute_activity_running ON compute_activity (outcome) WHERE outcome = 'running';
