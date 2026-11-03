-- MCP access goes from one token per box to one named token per client, so
-- a laptop's Claude Code and an OpenClaw instance hold different
-- credentials: either can be revoked without cutting off the other, and the
-- audit log can say which one acted. Digest only, same discipline as
-- sessions — the plaintext is shown once at creation and never stored.

CREATE TABLE mcp_tokens (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    digest       TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    last_used_at TEXT
);

-- Names identify a client in the token list and in the audit log, so they
-- have to be distinct to mean anything.
CREATE UNIQUE INDEX idx_mcp_tokens_name ON mcp_tokens (name);

-- Carry the settings-held token forward under a name, so a client connected
-- before the upgrade keeps working; the old rows are dead weight after.
INSERT INTO mcp_tokens (name, digest, created_at)
SELECT 'default', value,
       COALESCE(NULLIF((SELECT value FROM settings WHERE key = 'mcp_token_created_at'), ''),
                strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
FROM settings WHERE key = 'mcp_token_digest' AND value != '';

DELETE FROM settings WHERE key IN ('mcp_token_digest', 'mcp_token_created_at');
