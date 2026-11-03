-- Flare forwarding moves onto federation rails (docs/federation-contract.md):
-- the outbox becomes per-peer and per-kind. The pre-federation
-- forwarding pieces — the singleton flare_forward_* settings, the vault
-- token, and the old flare_outbox — are dropped outright; no deployment
-- ever configured them. Multiple forwarding destinations fall out of the
-- schema change for free.

CREATE TABLE fed_outbox (
    id              INTEGER PRIMARY KEY,
    peer_id         INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL,                -- 'flare' today
    app_name        TEXT NOT NULL DEFAULT '',
    payload         TEXT NOT NULL,                -- scrubbed per this peer's mode before it ever lands here
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    created_at      TEXT NOT NULL
);
CREATE INDEX idx_fed_outbox_due ON fed_outbox (peer_id, kind, next_attempt_at);

DROP TABLE flare_outbox;
DELETE FROM settings WHERE key IN ('flare_forward_url', 'flare_forward_mode');
DELETE FROM secrets WHERE app_id IS NULL AND key = '_flare_forward_token';
