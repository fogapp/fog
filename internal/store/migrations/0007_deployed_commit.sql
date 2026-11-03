-- The commit a git app is actually running, recorded on each successful
-- deploy. Lets the update checker compare against the remote branch head.
ALTER TABLE apps ADD COLUMN deployed_commit TEXT NOT NULL DEFAULT '';
