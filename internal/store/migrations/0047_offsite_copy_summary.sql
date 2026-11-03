-- A Files-snapshot copy carries the few facts the locker index lists.
--
-- The nightly index names every Files snapshot in the locker with its
-- stamp, creation time, file count and size. Those were read out of doc,
-- which for a Files snapshot is the whole per-file manifest, so writing
-- the index unmarshalled every manifest on every sync to keep four
-- fields. summary holds just those four, as the index's own JSON; '' for
-- the other kinds. Existing rows are filled from their manifests here.
ALTER TABLE offsite_copies ADD COLUMN summary TEXT NOT NULL DEFAULT '';
UPDATE offsite_copies SET summary = json_object(
        'stamp', COALESCE(json_extract(doc, '$.stamp'), ''),
        'created_at', COALESCE(json_extract(doc, '$.created_at'), ''),
        'files', COALESCE(json_extract(doc, '$.count'), 0),
        'bytes', COALESCE(json_extract(doc, '$.bytes'), 0))
    WHERE kind = 'files-snapshot' AND json_valid(doc);
