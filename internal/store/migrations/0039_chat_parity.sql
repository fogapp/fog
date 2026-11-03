-- Chat, Slack parity (docs/chat-contract.md Phase 4): per-person preferences,
-- Web Push subscriptions, room webhooks, the fog bot's own plaintext store,
-- and the MCP token flag that lets an assistant post.

ALTER TABLE people ADD COLUMN prefs TEXT NOT NULL DEFAULT '{}';

CREATE TABLE chat_push_subs (
    id          INTEGER PRIMARY KEY,
    device_id   INTEGER NOT NULL REFERENCES people_devices(id) ON DELETE CASCADE,
    endpoint    TEXT NOT NULL UNIQUE,
    p256dh      TEXT NOT NULL,
    auth        TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    failures    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_chat_push_subs_device ON chat_push_subs(device_id);

-- Incoming webhooks post as the fog bot into one room. Digest stored,
-- plaintext shown once.
CREATE TABLE chat_webhooks (
    id            INTEGER PRIMARY KEY,
    room_id       TEXT NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    token_digest  TEXT NOT NULL UNIQUE,
    created_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_used_at  TEXT
);
CREATE INDEX idx_chat_webhooks_room ON chat_webhooks(room_id);

-- What the fog bot has read in rooms people put it in — the one place fogd
-- holds message text, by those people's choice, for webhooks, apps and MCP.
CREATE TABLE chat_bot_messages (
    room_id      TEXT NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    id           TEXT NOT NULL,
    sender_addr  TEXT NOT NULL,
    at           TEXT NOT NULL,
    text         TEXT NOT NULL,
    PRIMARY KEY (room_id, seq)
);

ALTER TABLE mcp_tokens ADD COLUMN can_post_chat INTEGER NOT NULL DEFAULT 0;
