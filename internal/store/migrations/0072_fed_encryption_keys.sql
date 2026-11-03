-- Sealed bodies. Every fed-plane body is encrypted (HPKE, RFC 9180) to the
-- receiving fog's encryption key, which its identity key signs. This
-- instance's public half sits beside its identity key (the seed is
-- vault-encrypted as _fed_encryption_key); each peer's is learned at pairing,
-- or for a pairing older than this from a signed exchange, and then pinned.
ALTER TABLE fed_identity ADD COLUMN enc_public_key TEXT NOT NULL DEFAULT '';
ALTER TABLE fed_peers ADD COLUMN enc_public_key TEXT NOT NULL DEFAULT '';

-- A peer's encryption key, once known, never changes — the rule its identity
-- key follows (migration 0070). Learning it the first time is the only write.
CREATE TRIGGER fed_peers_enc_key_immutable
BEFORE UPDATE OF enc_public_key ON fed_peers
WHEN OLD.enc_public_key <> '' AND NEW.enc_public_key IS NOT OLD.enc_public_key
BEGIN
    SELECT RAISE(ABORT, 'a paired peer''s encryption key never changes');
END;
