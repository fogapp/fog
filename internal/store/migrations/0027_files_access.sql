-- Two ways into Files from outside the browser.
--
-- files_access_tokens: app passwords for the WebDAV mount. One per device
-- or client, digest only at rest (the session/MCP-token discipline), each
-- revocable on its own. The name is a label the operator chose.
--
-- file_shares: public links. A token (digest at rest) names one file or
-- folder; optional expiry and an optional password (argon2id, same helper
-- as the admin password). hits and last_used_at are the "has anyone
-- opened this?" facts the detail pane shows.
CREATE TABLE files_access_tokens (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    digest       TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    last_used_at TEXT
);

CREATE TABLE file_shares (
    id            INTEGER PRIMARY KEY,
    digest        TEXT NOT NULL UNIQUE,
    path          TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    hits          INTEGER NOT NULL DEFAULT 0,
    last_used_at  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX file_shares_path ON file_shares(path);
