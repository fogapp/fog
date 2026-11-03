-- Household sign-in, the OpenID provider (docs/signin-contract.md): clients
-- provisioned from apps' fog.yaml, the requests and codes in
-- flight, access tokens, and which apps each session signed in to.

-- An optional email per person: sent as email_verified false until fog can
-- confirm one, never used to link an account, never sent to a peer.
ALTER TABLE people ADD COLUMN email TEXT NOT NULL DEFAULT '';
ALTER TABLE people ADD COLUMN email_verified_at TEXT;

-- One client per app that declares fog-signin/1. The secret's plaintext is
-- in the vault (_oidc_client_secret); the row keeps digests, the current and
-- the one before it, so a blue-green swap never meets a window where
-- neither half can sign in.
CREATE TABLE oidc_clients (
    client_id          TEXT PRIMARY KEY,
    app_id             INTEGER NOT NULL UNIQUE REFERENCES apps(id) ON DELETE CASCADE,
    secret_digest      TEXT NOT NULL,
    prev_secret_digest TEXT NOT NULL DEFAULT '',
    redirect_uris      TEXT NOT NULL DEFAULT '[]',
    post_logout_uris   TEXT NOT NULL DEFAULT '[]',
    backchannel_uri    TEXT NOT NULL DEFAULT '',
    scim_base_url      TEXT NOT NULL DEFAULT '',
    scopes             TEXT NOT NULL DEFAULT '[]',
    require_par        INTEGER NOT NULL DEFAULT 0,
    required_acr       TEXT NOT NULL DEFAULT '',
    roles              TEXT NOT NULL DEFAULT '[]',
    role_map           TEXT NOT NULL DEFAULT '{}',
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

-- Pushed authorization requests and authorization codes, both single use,
-- both digests. A code carries the decision it was minted under (role, acr,
-- the session) so the token endpoint signs exactly what authorize decided.
CREATE TABLE oidc_requests (
    digest      TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,                 -- par | code
    client_id   TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    session_id  INTEGER REFERENCES people_sessions(id) ON DELETE CASCADE,
    params      TEXT NOT NULL,                 -- the parsed request, JSON
    grant       TEXT NOT NULL DEFAULT '',      -- code: the decision, JSON
    expires_at  TEXT NOT NULL,
    used_at     TEXT
);
CREATE INDEX idx_oidc_requests_expires ON oidc_requests(expires_at);

CREATE TABLE oidc_access_tokens (
    digest       TEXT PRIMARY KEY,
    client_id    TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    session_id   INTEGER NOT NULL REFERENCES people_sessions(id) ON DELETE CASCADE,
    code_digest  TEXT NOT NULL,                -- a replayed code revokes what it minted
    grant        TEXT NOT NULL,                -- the decision, for userinfo
    expires_at   TEXT NOT NULL
);
CREATE INDEX idx_oidc_access_tokens_code ON oidc_access_tokens(code_digest);
CREATE INDEX idx_oidc_access_tokens_expires ON oidc_access_tokens(expires_at);

-- Who to tell when a session ends: every client the session signed in to.
CREATE TABLE oidc_session_clients (
    session_id  INTEGER NOT NULL REFERENCES people_sessions(id) ON DELETE CASCADE,
    client_id   TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    first_at    TEXT NOT NULL,
    PRIMARY KEY (session_id, client_id)
);

-- Who to tell when a person changes: every client that has seen them.
CREATE TABLE oidc_links (
    client_id   TEXT NOT NULL REFERENCES oidc_clients(client_id) ON DELETE CASCADE,
    person_id   INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    scim_id     TEXT NOT NULL DEFAULT '',
    first_at    TEXT NOT NULL,
    last_at     TEXT NOT NULL,
    PRIMARY KEY (client_id, person_id)
);
