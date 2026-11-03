-- Identity-permanence facts an app declares in its fog.yaml (Project J): what
-- a host or domain change costs its federation, and any contracts it named
-- that fog does not recognise. Recorded at build time beside `contracts`,
-- surfaced so an operator sees the blast radius before moving an app.
ALTER TABLE apps ADD COLUMN federation_facts TEXT NOT NULL DEFAULT '';
