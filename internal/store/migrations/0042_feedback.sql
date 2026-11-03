-- Feedback: what a person says when something surprised them or they wished
-- it were different, written from the bug button in the header. The words
-- are theirs; the dossier is what fog knew at that moment — the page, the
-- browser's recent trail, fogd's recent logs, open errors and issues, and
-- the app on screen — so the report still means something a week later.
--
-- A row is local (peer_id NULL, written here) or relayed by a paired peer
-- holding a feedback.ingest grant. uid is minted by the fog the person
-- wrote it on, so a peer's retry lands on the row it already made: unique
-- per origin, with NULL folded to 0 because SQLite's UNIQUE treats NULLs
-- as distinct.
CREATE TABLE feedback (
    id          INTEGER PRIMARY KEY,
    uid         TEXT NOT NULL,
    peer_id     INTEGER REFERENCES fed_peers(id) ON DELETE CASCADE,
    author      TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL,
    page        TEXT NOT NULL DEFAULT '',
    fog_version TEXT NOT NULL DEFAULT '',
    dossier     TEXT NOT NULL DEFAULT '',        -- JSON; '' when the sender sent the note alone
    status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at  TEXT NOT NULL,                   -- when it was written, on the origin's clock
    received_at TEXT NOT NULL,
    closed_at   TEXT
);
CREATE UNIQUE INDEX idx_feedback_origin_uid ON feedback (ifnull(peer_id, 0), uid);
CREATE INDEX idx_feedback_status ON feedback (status, id);

-- One row per peer a local report is sent to. It is the outbox and the
-- receipt at once: queued rows are what the forwarder drains, and a
-- delivered row is the proof the report page shows. The mode is fixed at
-- the moment of writing — the consent that was in force when the person
-- pressed Send, not whatever the setting says by the time a retry lands.
CREATE TABLE feedback_deliveries (
    feedback_id     INTEGER NOT NULL REFERENCES feedback(id) ON DELETE CASCADE,
    peer_id         INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    mode            TEXT NOT NULL,                        -- note | full
    state           TEXT NOT NULL DEFAULT 'queued',       -- queued | delivered | refused
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    next_attempt_at TEXT NOT NULL,
    delivered_at    TEXT,
    PRIMARY KEY (feedback_id, peer_id)
);
CREATE INDEX idx_feedback_deliveries_due ON feedback_deliveries (state, peer_id, next_attempt_at);
