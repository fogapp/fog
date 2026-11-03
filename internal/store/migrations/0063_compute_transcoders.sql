-- fog-compute/1 (docs/compute-contract.md): what a git app's fog.yaml
-- declares about compute, recorded at each deploy as JSON ('' is none).
ALTER TABLE apps ADD COLUMN compute TEXT NOT NULL DEFAULT '';

-- The app's own name for each pool it was introduced to as a transcoder
-- (POST /fog/compute/transcoders), so fog polls the one it made.
CREATE TABLE compute_transcoders (
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    pool        TEXT NOT NULL,
    remote_id   TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, pool)
);
