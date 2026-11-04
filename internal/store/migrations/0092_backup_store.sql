-- Incremental backups (docs/store-contract.md): each backup row learns the
-- store snapshot that holds the same bytes, when that snapshot was last
-- read back whole and matched the backup, and the error if it did not.
ALTER TABLE backups ADD COLUMN store_snapshot TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN store_verified_at TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN store_error TEXT NOT NULL DEFAULT '';
