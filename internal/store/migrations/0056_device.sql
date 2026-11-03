-- Device authorization (RFC 8628, docs/signin-contract.md): an app
-- signs someone in on a screen with no keyboard worth using — Stream on the
-- television — by showing a short code the person approves on their phone
-- at fog.<domain>/device. The client is still the app server.
ALTER TABLE oidc_clients ADD COLUMN device_grant INTEGER NOT NULL DEFAULT 0;

CREATE TABLE oidc_device_codes (
    device_digest  TEXT PRIMARY KEY,               -- the device_code, as a digest
    user_code      TEXT NOT NULL UNIQUE,           -- XXXX-XXXX, shown on the screen
    client_id      TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    device_name    TEXT NOT NULL DEFAULT '',       -- "Living Room TV", as the app names it
    scope          TEXT NOT NULL,                  -- JSON array
    status         TEXT NOT NULL DEFAULT 'pending',-- pending | approved | denied | used
    session_id     INTEGER REFERENCES people_sessions(id) ON DELETE CASCADE,
    grant          TEXT NOT NULL DEFAULT '',       -- the decision, JSON, once approved
    expires_at     TEXT NOT NULL,
    last_poll_at   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_oidc_device_codes_expires ON oidc_device_codes(expires_at);
