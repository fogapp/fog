-- People: the humans of a household, as distinct from the one operator who
-- administers fog. Chat is the first consumer; the app gate is the second.
-- See docs/chat-contract.md.

CREATE TABLE people (
    id            INTEGER PRIMARY KEY,
    handle        TEXT NOT NULL UNIQUE COLLATE NOCASE,
    display_name  TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'member',   -- operator | member | guest
    status        TEXT NOT NULL DEFAULT 'active',   -- active | suspended
    user_id       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    identity_key  BLOB,                             -- ed25519 public key, set once by the person's first device
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL DEFAULT ''
);

-- How a person proves who they are to fog. Passkeys are the intended primary;
-- a password is the fallback for a box with no domain (WebAuthn needs TLS).
CREATE TABLE people_credentials (
    id            INTEGER PRIMARY KEY,
    person_id     INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,                    -- passkey | password
    data          TEXT NOT NULL,                    -- argon2id hash, or the passkey credential record (JSON)
    label         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    last_used_at  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_people_credentials_person ON people_credentials(person_id);

-- Invite links the operator hands out. Digest stored, plaintext shown once.
CREATE TABLE people_invites (
    id             INTEGER PRIMARY KEY,
    secret_digest  TEXT NOT NULL UNIQUE,
    handle_hint    TEXT NOT NULL DEFAULT '',
    display_hint   TEXT NOT NULL DEFAULT '',
    role           TEXT NOT NULL DEFAULT 'member',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    used_at        TEXT,
    used_by        INTEGER REFERENCES people(id) ON DELETE SET NULL
);

-- A device is one MLS client: its own signature key, attested (or not) by the
-- person's identity key. Fog stores the attestation; it cannot forge one.
CREATE TABLE people_devices (
    id             INTEGER PRIMARY KEY,
    person_id      INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    name           TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL DEFAULT 'web',     -- web | ios | android | desktop | bot
    device_key     BLOB NOT NULL,                   -- ed25519 public key (the MLS signature key)
    attestation    BLOB,                            -- ed25519 signature by identity_key over the device key; NULL = unverified
    push_endpoint  TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    last_seen_at   TEXT NOT NULL DEFAULT '',
    revoked_at     TEXT
);
CREATE UNIQUE INDEX idx_people_devices_key ON people_devices(device_key);
CREATE INDEX idx_people_devices_person ON people_devices(person_id);

-- Person sessions: cookie for browsers, bearer token for native clients.
-- Separate from the operator's sessions table on purpose — a member session
-- can never be mistaken for an administrative one.
CREATE TABLE people_sessions (
    id            INTEGER PRIMARY KEY,
    person_id     INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    device_id     INTEGER REFERENCES people_devices(id) ON DELETE CASCADE,
    token_digest  TEXT NOT NULL UNIQUE,
    client_kind   TEXT NOT NULL DEFAULT 'web',      -- web | native
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    user_agent    TEXT NOT NULL DEFAULT '',
    remote_addr   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_people_sessions_expires ON people_sessions(expires_at);

-- MLS KeyPackages a device published for others to add it with. Single use:
-- consumed_at is stamped when one is handed out.
CREATE TABLE people_key_packages (
    id            INTEGER PRIMARY KEY,
    device_id     INTEGER NOT NULL REFERENCES people_devices(id) ON DELETE CASCADE,
    ciphersuite   INTEGER NOT NULL,
    key_package   BLOB NOT NULL,
    published_at  TEXT NOT NULL,
    consumed_at   TEXT
);
CREATE INDEX idx_people_key_packages_device ON people_key_packages(device_id, consumed_at);

-- The person's client-encrypted backup. Fog stores bytes it cannot open.
CREATE TABLE people_backups (
    person_id     INTEGER PRIMARY KEY REFERENCES people(id) ON DELETE CASCADE,
    blob          BLOB NOT NULL,
    version       INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL
);

-- The operator is person #1. A fresh box has no user yet; setup creates the
-- row then, and the session resolver backfills it for any older box.
INSERT INTO people (handle, display_name, role, status, user_id, created_at)
SELECT username, username, 'operator', 'active', id, created_at FROM users;
