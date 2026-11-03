-- SCIM provisioning (docs/signin-contract.md): fog, as a SCIM client,
-- keeps each app that takes provisioning in step with who may use it. This
-- is what fog last told an app about a person — the app's id for them and a
-- digest of the attributes sent — so a reconcile sends only what changed,
-- and converges from any state, including a lost row (it asks the app by
-- externalId).
CREATE TABLE scim_state (
    client_id   TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    person_id   INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    scim_id     TEXT NOT NULL,
    sent        TEXT NOT NULL,          -- digest of the last attributes the app accepted
    active      INTEGER NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (client_id, person_id)
);
