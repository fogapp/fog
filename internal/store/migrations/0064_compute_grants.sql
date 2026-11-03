-- Which apps may use which pool (Settings › Compute). Every app that
-- declares compute work gets every pool; a row here is the operator
-- switching one off.
CREATE TABLE compute_grants (
    app_id  INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    pool    TEXT NOT NULL,
    off     INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (app_id, pool)
);
