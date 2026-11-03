-- Getting-started text supplied by the app's own repo (fog.yaml), refreshed
-- on each successful git deploy. Catalog/spec notes are the fallback.
ALTER TABLE apps ADD COLUMN repo_notes TEXT NOT NULL DEFAULT '';
