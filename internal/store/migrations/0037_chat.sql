-- Chat: sealed rooms (docs/chat-contract.md). Every message body is MLS
-- ciphertext; the hub stores, orders, and delivers bytes it cannot read, and
-- keeps the one thing it must know — who is in the room — from the public
-- half of the MLS group.

CREATE TABLE chat_rooms (
    id                TEXT PRIMARY KEY,                 -- ULID
    group_id          BLOB NOT NULL UNIQUE,             -- the MLS group_id
    hub_instance      TEXT NOT NULL DEFAULT '',         -- '' = hubbed here
    kind              TEXT NOT NULL,                    -- channel | group | dm
    label             TEXT NOT NULL DEFAULT '',         -- what the room chose to reveal for listing
    topic_label       TEXT NOT NULL DEFAULT '',
    policy            TEXT NOT NULL,                    -- JSON
    workspace_visible INTEGER NOT NULL DEFAULT 0,       -- channel: listed to the household
    epoch             INTEGER NOT NULL DEFAULT 0,       -- current MLS epoch
    seq               INTEGER NOT NULL DEFAULT 0,       -- last assigned sequence number
    tree              TEXT NOT NULL DEFAULT '',         -- JSON: the public leaf list (mlsview.Tree)
    group_info        BLOB,                             -- latest GroupInfo for external joins
    group_info_epoch  INTEGER NOT NULL DEFAULT -1,
    dm_key            TEXT NOT NULL DEFAULT '',         -- canonical participant set for DMs
    replica_status    TEXT NOT NULL DEFAULT 'live',     -- follower: live | stale | archived
    created_by        TEXT NOT NULL,                    -- person address
    created_at        TEXT NOT NULL,
    archived_at       TEXT
);
CREATE UNIQUE INDEX idx_chat_rooms_dm ON chat_rooms(dm_key) WHERE dm_key <> '';

-- Who is in a room, at what role, and since when. A person is a participant
-- while any of their devices holds a leaf; left_seq marks the commit that
-- removed their last one.
CREATE TABLE chat_participants (
    room_id        TEXT NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    person_addr    TEXT NOT NULL,                       -- fog:<instance>/<handle>
    home_instance  TEXT NOT NULL,
    role_index     INTEGER NOT NULL DEFAULT 2,
    joined_seq     INTEGER NOT NULL,
    left_seq       INTEGER,
    notify         TEXT NOT NULL DEFAULT 'all',         -- all | mentions | none (the person's own setting)
    PRIMARY KEY (room_id, person_addr)
);

-- The log. payload is the MLSMessage (commit or private message) exactly as
-- submitted, or JSON for system events; hints is the sender-declared,
-- policy-bounded metadata beside the ciphertext.
CREATE TABLE chat_events (
    room_id        TEXT NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    id             TEXT NOT NULL,                       -- client-minted ULID
    epoch          INTEGER NOT NULL,
    kind           TEXT NOT NULL,                       -- mls_private | mls_commit | mls_proposal | system
    sender_addr    TEXT NOT NULL DEFAULT '',
    sender_device  TEXT NOT NULL DEFAULT '',            -- device key, base64url
    sent_at        TEXT NOT NULL DEFAULT '',
    hub_at         TEXT NOT NULL,
    payload        BLOB NOT NULL,
    hints          TEXT NOT NULL DEFAULT '',
    expires_at     TEXT,
    PRIMARY KEY (room_id, seq)
);
CREATE UNIQUE INDEX idx_chat_events_id ON chat_events(room_id, id);
CREATE INDEX idx_chat_events_expires ON chat_events(expires_at) WHERE expires_at IS NOT NULL;

-- A Welcome waiting for a newly added person's devices. Membership is
-- recorded at the commit; this is only how the device gets the keys.
CREATE TABLE chat_invites (
    id             INTEGER PRIMARY KEY,
    room_id        TEXT NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    person_addr    TEXT NOT NULL,
    device_key     TEXT NOT NULL DEFAULT '',            -- '' = any of the person's devices
    invited_by     TEXT NOT NULL,
    seq            INTEGER NOT NULL,                    -- the commit that added them
    welcome        BLOB NOT NULL,
    created_at     TEXT NOT NULL,
    accepted_at    TEXT
);
CREATE INDEX idx_chat_invites_person ON chat_invites(person_addr, accepted_at);

-- Encrypted attachments, content-addressed. The bytes live on disk under
-- <data>/chat/blobs; this is the index.
CREATE TABLE chat_blobs (
    sha256         TEXT PRIMARY KEY,
    room_id        TEXT NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    size           INTEGER NOT NULL,
    uploader_addr  TEXT NOT NULL,
    stored_at      TEXT NOT NULL,
    event_id       TEXT NOT NULL DEFAULT ''             -- set once a message references it
);
CREATE INDEX idx_chat_blobs_room ON chat_blobs(room_id);

-- A person's own unread state: the highest seq they have read per room.
CREATE TABLE chat_read_marks (
    person_id      INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    room_id        TEXT NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    read_seq       INTEGER NOT NULL DEFAULT 0,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (person_id, room_id)
);

-- What the fog bot has already mirrored into #fog.
CREATE TABLE chat_bot_seen (
    kind           TEXT NOT NULL,
    key            TEXT NOT NULL,
    seen_at        TEXT NOT NULL,
    PRIMARY KEY (kind, key)
);
