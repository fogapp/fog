-- An app imported from a manifest is created held: no deploy touches it
-- until an operator has read what it binds, publishes and asks for, and
-- confirmed it (internal/apps/hostaccess.go, docs/disaster-recovery.md).
ALTER TABLE apps ADD COLUMN review_hold INTEGER NOT NULL DEFAULT 0;
