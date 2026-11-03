-- Compute nodes (docs/compute-contract.md): machines paired with fog
-- that lend it their hardware. token_digest is the node's credential for
-- /compute/v1, kept as a digest only. inventory is the node's own report
-- of its machine (JSON compute.Inventory); budget, when set, is the
-- operator's override of its slots (JSON compute.Slots).
CREATE TABLE compute_nodes (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    token_digest  TEXT NOT NULL UNIQUE,
    inventory     TEXT NOT NULL DEFAULT '{}',
    budget        TEXT NOT NULL DEFAULT '',
    paused        INTEGER NOT NULL DEFAULT 0,
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL DEFAULT ''
);

-- One-time codes that pair a machine: shown once in Settings › Compute and
-- spent by fog-node join. Digests only.
CREATE TABLE compute_join_codes (
    digest      TEXT PRIMARY KEY,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
