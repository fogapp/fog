-- Visitors (docs/signin-contract.md): a paired household's person
-- signs in to one of this household's apps, authenticated by their own fog.
-- The home decides who (its people.visit grant to us); the host decides
-- what (a visitor grant per app, capped at the app's declared role).
-- Visitors are never people: they live here, apart.

-- The most an app lets a visitor be (its manifest's visitors.role); ''
-- means the app hosts no visitors.
ALTER TABLE oidc_clients ADD COLUMN visitor_role TEXT NOT NULL DEFAULT '';

-- Host side: which peers' people may visit which app, as what.
CREATE TABLE visitor_grants (
    client_id  TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    peer_id    INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    PRIMARY KEY (client_id, peer_id)
);

-- Host side: a visitor signed in here, by their home's word. home_sid is
-- the home fog's session, which its logout names; sid is ours, which the
-- apps were told.
CREATE TABLE visitor_sessions (
    id            INTEGER PRIMARY KEY,
    token_digest  TEXT NOT NULL UNIQUE,
    sid           TEXT NOT NULL UNIQUE,
    peer_id       INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    address       TEXT NOT NULL,
    handle        TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    home_sid      TEXT NOT NULL,
    amr           TEXT NOT NULL DEFAULT '[]',
    auth_time     TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);
CREATE INDEX idx_visitor_sessions_home ON visitor_sessions(peer_id, home_sid);

-- Which apps a visitor session signed in to: whom to tell when it ends.
CREATE TABLE visitor_session_clients (
    visitor_session_id  INTEGER NOT NULL REFERENCES visitor_sessions(id) ON DELETE CASCADE,
    client_id           TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    PRIMARY KEY (visitor_session_id, client_id)
);

-- A visitor session ending — signed out at home, expired, the pairing
-- broken (the cascade from fed_peers lands here too) — tells each app it
-- signed in to, through the same outbox as a person's.
CREATE TRIGGER visitor_sessions_logout BEFORE DELETE ON visitor_sessions
BEGIN
    INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
    SELECT 'logout', vc.client_id, lower(hex(randomblob(16))), json_object('sid', OLD.sid),
           strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM visitor_session_clients vc
    JOIN oidc_clients c ON c.client_id = vc.client_id
    WHERE vc.visitor_session_id = OLD.id AND c.backchannel_uri != '';
END;

-- Both sides: requests and codes of the fog-to-fog flow. On the home side
-- a par/code row is for a peer (peer_id) and a person's session; on the
-- host side a txn row holds what this fog must remember while the browser
-- is away at the home.
CREATE TABLE visit_requests (
    digest      TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,                  -- par | code | txn
    peer_id     INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    session_id  INTEGER REFERENCES people_sessions(id) ON DELETE CASCADE,
    params      TEXT NOT NULL,
    grant       TEXT NOT NULL DEFAULT '',
    expires_at  TEXT NOT NULL,
    used_at     TEXT
);

-- Home side: which of our people's sessions a peer signed in as a visitor,
-- so that session's end is told to the peer.
CREATE TABLE visit_sessions_out (
    session_id  INTEGER NOT NULL REFERENCES people_sessions(id) ON DELETE CASCADE,
    peer_id     INTEGER NOT NULL REFERENCES fed_peers(id) ON DELETE CASCADE,
    PRIMARY KEY (session_id, peer_id)
);

-- And the telling: a live session ending owes each peer that hosted it a
-- visit logout, delivered over the signed federation channel.
CREATE TRIGGER people_sessions_visit_logout BEFORE DELETE ON people_sessions
WHEN OLD.sid IS NOT NULL AND OLD.expires_at > strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
BEGIN
    INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
    SELECT 'visit_logout', 'peer:' || vo.peer_id, lower(hex(randomblob(16))), json_object('sid', OLD.sid),
           strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM visit_sessions_out vo WHERE vo.session_id = OLD.id;
END;

-- Host side: access tokens for visitors (people's live in
-- oidc_access_tokens, whose session is a person's).
CREATE TABLE visitor_access_tokens (
    digest              TEXT PRIMARY KEY,
    client_id           TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    visitor_session_id  INTEGER NOT NULL REFERENCES visitor_sessions(id) ON DELETE CASCADE,
    grant               TEXT NOT NULL,
    expires_at          TEXT NOT NULL
);
