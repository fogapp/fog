-- The fog API token becomes a lookup credential
-- (docs/federation-contract.md): apps authenticate to the new /app/v1 surface
-- with the same
-- FOG_API_TOKEN the backup contract uses, so fogd needs digest → app
-- resolution. Digests are (re)recorded whenever the token is ensured at
-- deploy, so existing tokens gain a row on their app's next deploy.

CREATE TABLE app_api_tokens (
    app_id     INTEGER PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    digest     TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);
