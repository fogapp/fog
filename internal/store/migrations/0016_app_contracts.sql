-- Apps declare fog contracts (fog-federation/1 today) in their repo's
-- fog.yaml; the build records them here so fogd knows which apps accept
-- introductions — and refuses them for apps that never opted in
-- (docs/federation-contract.md).

ALTER TABLE apps ADD COLUMN contracts TEXT NOT NULL DEFAULT '[]';
