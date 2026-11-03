-- A person is named in the audit log by their bare handle.
--
-- Most rows a person caused have always carried just the handle, but the
-- people and chat verbs wrote "person:<handle>", so one person read as two
-- actors in the activity views. Those verbs now write the bare handle too;
-- the rows they already wrote are brought in line here.
UPDATE audit_log SET actor = substr(actor, 8) WHERE actor LIKE 'person:%';
