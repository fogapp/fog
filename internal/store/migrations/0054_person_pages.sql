-- Where a person lives in an app (docs/signin-contract.md): an app may
-- register a page for a person address — ours or a peer's — and fog chat's
-- profile pane links to it ("Open in Hub").
CREATE TABLE app_person_pages (
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    address     TEXT NOT NULL,
    url         TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, address)
);
CREATE INDEX idx_app_person_pages_address ON app_person_pages(address);
