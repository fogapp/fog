-- What a git app's repo fog.yaml declares about integrations
-- (docs/integrations-contract.md): the offers it makes and the ones it
-- wants, as JSON, recorded at each deploy beside contracts. '' is none.
ALTER TABLE apps ADD COLUMN integrations TEXT NOT NULL DEFAULT '';
