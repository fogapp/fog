-- The store's first-party apps (Manor, Hub, Newtab…) used to clone from one
-- base URL written into fog itself. Where they come from is now this fog's
-- setting, catalog_source_base (internal/catalog/source.go), and a fog with
-- none set leaves them out of the store.
--
-- A fog that already installed one keeps everything as it was: the base is
-- read back off its installed apps. An app whose repository ends in
-- "/<name>.git" for one of the first-party repository names gives the part
-- before the name; the base the most apps share wins. Installed apps keep
-- their stored URLs either way; this only fills the setting, and only when
-- it is unset.
INSERT OR IGNORE INTO settings (key, value, updated_at)
SELECT 'catalog_source_base', base, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM (
    SELECT substr(repo, 1, length(repo) - length(n.column1) - 4) AS base, COUNT(*) AS uses
    FROM (
        SELECT json_extract(r.spec, '$.source.repo') AS repo
        FROM app_revisions AS r
        JOIN (SELECT app_id, MAX(rev) AS rev FROM app_revisions GROUP BY app_id) AS latest
            ON latest.app_id = r.app_id AND latest.rev = r.rev
        JOIN apps AS a ON a.id = r.app_id
        WHERE json_extract(r.spec, '$.source.type') = 'git'
    ) AS installed
    JOIN (VALUES ('manor'), ('hub'), ('office'), ('feed'), ('hook'), ('stream'), ('once'),
                 ('hud'), ('glass'), ('newtab'), ('signet'), ('fit')) AS n
        ON substr(installed.repo, -(length(n.column1) + 5)) = '/' || n.column1 || '.git'
    GROUP BY base
    ORDER BY uses DESC, base
    LIMIT 1
);
