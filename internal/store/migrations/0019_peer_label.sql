-- A peer's label is a local display name, and only that. It is never sent,
-- never matched against, and never appears in an audit record: `name` stays
-- the slug this instance was paired under (derived from the name the peer
-- advertised) and remains the identity every other subsystem addresses the
-- peer by. Empty label means "show that name", which is what every existing
-- peer gets on this migration.
ALTER TABLE fed_peers ADD COLUMN label TEXT NOT NULL DEFAULT '';
