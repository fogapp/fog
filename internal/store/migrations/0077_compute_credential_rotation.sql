-- A machine rotates its credential right after pairing (docs/compute-
-- contract.md): prev_token_digest is the digest it held before, still
-- taken until the machine is first heard with the new one, so a rotation
-- whose answer was lost isn't a machine locked out.
ALTER TABLE compute_nodes ADD COLUMN prev_token_digest TEXT NOT NULL DEFAULT '';
