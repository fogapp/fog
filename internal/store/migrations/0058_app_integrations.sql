-- Integrations (docs/integrations-contract.md): the operator connected an
-- app to an offer another app makes, and the provider minted a credential
-- for it alone. remote_id is the provider's name for that credential, to
-- read its status and revoke it. env and secrets are the names fog set in
-- the consumer's spec, so disconnecting removes exactly those. The
-- provider is by name: removing it leaves the row, which the tab shows.
CREATE TABLE app_integrations (
    id          INTEGER PRIMARY KEY,
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    offer       TEXT NOT NULL,
    remote_id   TEXT NOT NULL,
    env         TEXT NOT NULL DEFAULT '[]',  -- JSON array of names
    secrets     TEXT NOT NULL DEFAULT '[]',  -- JSON array of names
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    UNIQUE (app_id, provider, offer)
);
