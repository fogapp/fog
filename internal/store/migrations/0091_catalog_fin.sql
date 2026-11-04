-- Manor is now Fin (2026-11): the store lists a "fin" entry and no "manor".
-- A fog that saved its own visible set, or pointed Manor at its own clone
-- URL, would otherwise hide Fin or clone it from nowhere, so each setting
-- that names "manor" learns "fin" too. Installed Manor apps keep their
-- stored specs either way; this only touches the two store settings.
UPDATE settings
SET value = json_insert(value, '$[#]', 'fin'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE key = 'catalog_enabled'
  AND json_valid(value)
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE json_each.value = 'manor')
  AND NOT EXISTS (SELECT 1 FROM json_each(settings.value) WHERE json_each.value = 'fin');

UPDATE settings
SET value = json_set(value, '$.fin', json_extract(value, '$.manor')),
    updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE key = 'catalog_source_overrides'
  AND json_valid(value)
  AND json_type(value, '$.manor') = 'text'
  AND json_type(value, '$.fin') IS NULL;
