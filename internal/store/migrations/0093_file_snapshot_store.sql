-- Drive snapshots move into the incremental store (docs/store-contract.md):
-- a snapshot row names the store snapshot that holds it. Rows written as
-- hardlink trees or a mirror keep an empty value until they are imported.
ALTER TABLE file_snapshots ADD COLUMN store_snapshot TEXT NOT NULL DEFAULT '';
