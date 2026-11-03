-- A Fix that names a place in the UI ("Settings → Advanced") should be a
-- click, not a scavenger hunt. fix_url carries an in-app path alongside the
-- fix text; the Issues page renders it as a link. Display payload like
-- data: it stays out of the ack hash, so a link change never breaks an ack.
ALTER TABLE issues ADD COLUMN fix_url TEXT NOT NULL DEFAULT '';
