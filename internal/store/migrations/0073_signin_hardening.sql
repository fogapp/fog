-- Household sign-in, hardened (docs/signin-contract.md): what the first
-- review found the schema could not say.

-- A client secret has three states. secret_digest is what the running app
-- holds; next_secret_digest is a rotation's new secret, waiting in the vault
-- for the next deploy to carry it; prev_secret_digest is the one a deploy
-- just replaced, honoured until prev_secret_until so the old containers of
-- a blue-green swap can finish. A rotation never touches the running
-- secret, so rotating twice before a deploy cannot lock the app out; the
-- old secret ends once the app has shown it holds the new one.
ALTER TABLE oidc_clients ADD COLUMN next_secret_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE oidc_clients ADD COLUMN prev_secret_until TEXT NOT NULL DEFAULT '';
-- Before this, a rotation made the new secret current at once and kept the
-- old as previous, valid until the rotation after. The running app may hold
-- either, so read that as the new model: the old is current until the app
-- uses the new.
UPDATE oidc_clients SET next_secret_digest = secret_digest, secret_digest = prev_secret_digest, prev_secret_digest = ''
WHERE prev_secret_digest != '';

-- The gate's handshake is bound to the browser that began it: the gated
-- host sets a random value in a cookie only its /.fog/gate/ path receives,
-- fog's authorize stores the value's digest with the code, and the
-- callback redeems the code only alongside the cookie. A callback link
-- handed to someone else signs nobody in.
ALTER TABLE gate_codes ADD COLUMN binding TEXT NOT NULL DEFAULT '';

-- A session that runs out ends in the apps too. 0052 and 0057 pruned an
-- expired session silently, trusting every app to cap its own session at
-- the same expiry; now the ID token says when fog's session ends
-- (fog_session_exp) and the logout is sent as well, so an app that keeps
-- its sessions longer still hears.
DROP TRIGGER people_sessions_logout;
CREATE TRIGGER people_sessions_logout BEFORE DELETE ON people_sessions
WHEN OLD.sid IS NOT NULL
BEGIN
    INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
    SELECT 'logout', sc.client_id, lower(hex(randomblob(16))), json_object('sid', OLD.sid),
           strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM oidc_session_clients sc
    JOIN oidc_clients c ON c.client_id = sc.client_id
    WHERE sc.session_id = OLD.id AND c.backchannel_uri != '';
END;

DROP TRIGGER people_sessions_visit_logout;
CREATE TRIGGER people_sessions_visit_logout BEFORE DELETE ON people_sessions
WHEN OLD.sid IS NOT NULL
BEGIN
    INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
    SELECT 'visit_logout', 'peer:' || vo.peer_id, lower(hex(randomblob(16))), json_object('sid', OLD.sid),
           strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM visit_sessions_out vo WHERE vo.session_id = OLD.id;
END;

-- SCIM deprovisioning that outlives the person. Deleting someone cascades
-- scim_state away, so before this the apps that provisioned them never
-- heard active: false. The person's row is gone after the statement; what
-- an app needs to switch them off is kept here until it has.
CREATE TABLE scim_departures (
    client_id     TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    person_uid    TEXT NOT NULL,          -- the address's second half
    scim_id       TEXT NOT NULL DEFAULT '', -- the app's id, when fog knew it
    user_name     TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (client_id, person_uid)
);

-- Written in the statement that deletes the person, so no path can forget:
-- every SCIM app that was told they were active, or that saw them sign in
-- without fog having told it anything yet.
CREATE TRIGGER people_deleted_scim BEFORE DELETE ON people
WHEN OLD.uid IS NOT NULL
BEGIN
    INSERT OR REPLACE INTO scim_departures (client_id, person_uid, scim_id, user_name, display_name, created_at)
    SELECT c.client_id, OLD.uid, COALESCE(st.scim_id, ''), OLD.handle, OLD.display_name, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    FROM oidc_clients c
    LEFT JOIN scim_state st ON st.client_id = c.client_id AND st.person_id = OLD.id
    WHERE c.scim_base_url != ''
      AND (st.active = 1
           OR (st.person_id IS NULL AND EXISTS (SELECT 1 FROM oidc_links l WHERE l.client_id = c.client_id AND l.person_id = OLD.id)));
END;

-- How provisioning to each app is going: since when it has failed, and
-- whether the Inbox has been told. A failure that lasts is the operator's
-- business, not only a log line.
CREATE TABLE scim_health (
    client_id      TEXT PRIMARY KEY REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    failing_since  TEXT NOT NULL,
    last_error     TEXT NOT NULL DEFAULT '',
    noticed        INTEGER NOT NULL DEFAULT 0
);
