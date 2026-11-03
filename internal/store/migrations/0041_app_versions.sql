-- Every version an app has actually run, one row per successful deploy.
--
-- app_updates already records what the *last check* compared — one current
-- ref and one remote ref — which answers "is there something newer" and
-- nothing else. It cannot answer "what was this app running last week", so
-- there was no version to go back to: a bad update could only be escaped by
-- editing the spec by hand.
--
-- ref is what identifies the version to fog's own eyes: the image manifest
-- digest for image apps, the commit sha for git apps. image is the exact
-- reference the deploy ran, and is what a rollback re-renders — pinned by
-- digest for image apps, rebuilt from the commit for git apps. Rows are
-- history: a rollback appends the old version again rather than deleting
-- anything, so the table always reads as what happened.
CREATE TABLE app_versions (
    id         INTEGER PRIMARY KEY,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    source     TEXT NOT NULL,                -- image | git
    ref        TEXT NOT NULL,                -- sha256:… digest, or commit sha
    image      TEXT NOT NULL DEFAULT '',     -- the image reference deployed
    rev        INTEGER NOT NULL DEFAULT 0,   -- config revision it ran under
    actor      TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_app_versions_app ON app_versions(app_id, id);
