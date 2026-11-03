-- A replayed authorization code revokes everything it minted (RFC 6749
-- §4.1.2). A visitor's access tokens did not say which code they came from,
-- so a replay revoked a person's tokens and never a visitor's. Tokens from
-- before this are minutes from expiry, so '' is enough for them.
ALTER TABLE visitor_access_tokens ADD COLUMN code_digest TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_visitor_access_tokens_code ON visitor_access_tokens(code_digest);
