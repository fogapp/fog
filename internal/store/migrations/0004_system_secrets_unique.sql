-- System secrets (app_id NULL) were never updatable: SQLite treats NULLs as
-- distinct in UNIQUE constraints, so upserts inserted duplicates and reads
-- returned the oldest row. Keep the newest value per key, then enforce
-- uniqueness with a partial index that upserts can target.

DELETE FROM secrets
WHERE app_id IS NULL
  AND id NOT IN (
    SELECT MAX(id) FROM secrets WHERE app_id IS NULL GROUP BY key
  );

CREATE UNIQUE INDEX idx_secrets_system ON secrets(key) WHERE app_id IS NULL;
