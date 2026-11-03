-- Delegated backups (fog-backup/1): artifact identity and verification state.
-- format is '' for legacy volume snapshots, 'fog-backup/1' for delegated
-- artifacts; sha256 is the whole-artifact digest fog verified at download;
-- manifest is the artifact's manifest.json, denormalized for the UI;
-- verified_at is stamped by the test-restore loop.
ALTER TABLE backups ADD COLUMN format TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN manifest TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN verified_at TEXT NOT NULL DEFAULT '';
