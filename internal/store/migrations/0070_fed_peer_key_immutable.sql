-- A paired peer's key never changes. Fog has no key rotation: an instance's
-- ID and keypair are minted together and are permanent, so the same instance
-- ID arriving under another key is somebody else claiming that household's
-- name — with every grant it holds here. Pairing used to "refresh" the row's
-- key in that case, which let anyone holding a one-time invite impersonate an
-- already-paired household.
--
-- The code refuses that now (store.RefreshPairedPeer). This trigger is the
-- floor beneath it: no statement, from any caller, can rewrite the key a row
-- was paired under, nor move a row to another instance ID. A household that
-- really reinstalled has a new instance ID and pairs as a new peer; the old
-- row is the operator's to remove.
CREATE TRIGGER fed_peers_key_immutable
BEFORE UPDATE OF public_key, instance_id ON fed_peers
WHEN NEW.public_key IS NOT OLD.public_key OR NEW.instance_id IS NOT OLD.instance_id
BEGIN
    SELECT RAISE(ABORT, 'a paired peer''s instance ID and key never change');
END;
