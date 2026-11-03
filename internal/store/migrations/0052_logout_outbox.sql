-- Back-channel logout (docs/signin-contract.md): when a session ends or
-- a person is suspended or removed, every app that saw them is told.
--
-- The outbox is generic — durable rows, an idempotency key, retries with
-- backoff — and its first kind is a logout. The rows are written by the
-- database itself, in the same statement that ends the session, so there is
-- no path that ends a session and forgets to say so: a sign-out, a revoked
-- session, a suspension, a deleted person and a cascade all go through
-- these triggers.
CREATE TABLE app_outbox (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,                 -- logout
    target      TEXT NOT NULL,                 -- the OIDC client
    idem_key    TEXT NOT NULL UNIQUE,          -- the logout token's jti
    payload     TEXT NOT NULL,                 -- {"sid": …} or {"uid": …}
    attempts    INTEGER NOT NULL DEFAULT 0,
    next_at     TEXT NOT NULL,
    last_error  TEXT NOT NULL DEFAULT '',
    noticed     INTEGER NOT NULL DEFAULT 0,    -- the Inbox has been told it is stuck
    created_at  TEXT NOT NULL
);
CREATE INDEX idx_app_outbox_due ON app_outbox(next_at);

-- A live session ending: tell each app it signed in to, by sid. BEFORE, so
-- oidc_session_clients is still there to read; an expired session is
-- pruned silently, since every app capped its own session at the same
-- expiry.
CREATE TRIGGER people_sessions_logout BEFORE DELETE ON people_sessions
WHEN OLD.sid IS NOT NULL AND OLD.expires_at > strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
BEGIN
    INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
    SELECT 'logout', sc.client_id, lower(hex(randomblob(16))), json_object('sid', OLD.sid),
           strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM oidc_session_clients sc
    JOIN oidc_clients c ON c.client_id = sc.client_id
    WHERE sc.session_id = OLD.id AND c.backchannel_uri != '';
END;

-- A person suspended: tell every app that has seen them, by subject, so
-- sessions an app holds from any device end too.
CREATE TRIGGER people_suspended_logout AFTER UPDATE OF status ON people
WHEN NEW.status = 'suspended' AND OLD.status != 'suspended' AND NEW.uid IS NOT NULL
BEGIN
    INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
    SELECT 'logout', l.client_id, lower(hex(randomblob(16))), json_object('uid', NEW.uid),
           strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM oidc_links l
    JOIN oidc_clients c ON c.client_id = l.client_id
    WHERE l.person_id = NEW.id AND c.backchannel_uri != '';
END;

-- A person removed: the same, before oidc_links cascades away.
CREATE TRIGGER people_deleted_logout BEFORE DELETE ON people
WHEN OLD.uid IS NOT NULL AND OLD.status != 'suspended'
BEGIN
    INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
    SELECT 'logout', l.client_id, lower(hex(randomblob(16))), json_object('uid', OLD.uid),
           strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM oidc_links l
    JOIN oidc_clients c ON c.client_id = l.client_id
    WHERE l.person_id = OLD.id AND c.backchannel_uri != '';
END;
