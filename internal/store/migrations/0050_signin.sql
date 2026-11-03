-- Household sign-in (docs/signin-contract.md). The pieces every phase
-- stands on: a name for a person that outlives a rename, a session id that
-- can be told to an app, how a session was proven, the keys fog signs with,
-- who may use which app, and the gate's own per-host sessions.

-- The person ULID: the second half of a person address,
-- person:<instance ULID>/<person ULID>. Minted by the store when a person is
-- created; rows from before this migration are filled in when the store
-- opens (SQLite has no ULID function).
ALTER TABLE people ADD COLUMN uid TEXT;
CREATE UNIQUE INDEX idx_people_uid ON people(uid) WHERE uid IS NOT NULL;

-- sid is the public name of a session: carried in tokens and assertions, so
-- an app can be told precisely which sign-in ended. Never a credential.
ALTER TABLE people_sessions ADD COLUMN sid TEXT;
UPDATE people_sessions SET sid = lower(hex(randomblob(16)));
CREATE UNIQUE INDEX idx_people_sessions_sid ON people_sessions(sid) WHERE sid IS NOT NULL;
-- How the session was proven (RFC 8176 values) and when. Sessions from
-- before this migration are unknown, which is what '[]' says.
ALTER TABLE people_sessions ADD COLUMN amr TEXT NOT NULL DEFAULT '[]';
ALTER TABLE people_sessions ADD COLUMN auth_time TEXT NOT NULL DEFAULT '';
UPDATE people_sessions SET auth_time = created_at;

-- The keys fog signs assertions and ID tokens with. The private half of
-- each lives in the vault (system secret _signing_key_<kid>); this row is
-- the public half and its schedule, which internal/signing plans.
CREATE TABLE signing_keys (
    kid           TEXT PRIMARY KEY,
    alg           TEXT NOT NULL,
    public_jwk    TEXT NOT NULL,
    publish_at    TEXT NOT NULL,
    sign_from     TEXT NOT NULL,
    sign_until    TEXT NOT NULL,
    unpublish_at  TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

-- Who may sign in to an app (internal/identity). No row means the default,
-- operator only, so nothing installed before this opens up by surprise.
CREATE TABLE signin_policies (
    app_id      INTEGER PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    audience    TEXT NOT NULL DEFAULT 'operator',   -- operator | household | people
    updated_at  TEXT NOT NULL
);
-- The chosen people when audience = people, and per-person role overrides
-- under any audience ('' = the role map's answer).
CREATE TABLE signin_policy_people (
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    person_id  INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (app_id, person_id)
);

-- The gate's handshake: fog.<domain> mints a single-use code bound to the
-- host it is for; the host's /.fog/gate/callback redeems it for a gate
-- session. Both are digests at rest, and both die with the fog session.
CREATE TABLE gate_codes (
    digest      TEXT PRIMARY KEY,
    host        TEXT NOT NULL,
    session_id  INTEGER NOT NULL REFERENCES people_sessions(id) ON DELETE CASCADE,
    next        TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    used_at     TEXT
);
CREATE TABLE gate_sessions (
    digest      TEXT PRIMARY KEY,
    host        TEXT NOT NULL,
    session_id  INTEGER NOT NULL REFERENCES people_sessions(id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
CREATE INDEX idx_gate_sessions_session ON gate_sessions(session_id);
CREATE INDEX idx_gate_codes_expires ON gate_codes(expires_at);
CREATE INDEX idx_gate_sessions_expires ON gate_sessions(expires_at);
