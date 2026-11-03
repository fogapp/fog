-- fog:fk-off
-- One login. Fog had two: the operator in `users`/`sessions`, everyone else
-- in `people`/`people_sessions`, each with its own password store, its own
-- cookie, and its own sign-in page. The split cost more than it bought — the
-- operator was already person #1 (0036), `people.role` already said who was
-- an administrator, and the two pages made a household member's first
-- question "which login am I?".
--
-- After this migration `people` is the only identity, `people_credentials`
-- the only password, `people_sessions` the only session, and `role` alone
-- decides what a signed-in person may do.
--
-- Nobody is signed out by the upgrade: live operator sessions are carried
-- across as person sessions, keeping their tokens, so the browser reading
-- this stays signed in.

-- Every operator is a person. 0036 backfilled them and setup has created the
-- row since, so this only catches a box whose person row went missing.
INSERT INTO people (handle, display_name, role, status, user_id, created_at)
SELECT lower(u.username), u.username, 'operator', 'active', u.id, u.created_at
FROM users u
WHERE NOT EXISTS (SELECT 1 FROM people p WHERE p.user_id = u.id);

-- The admin password becomes a person credential. Both sides already store
-- the same argon2id encoding, so the hash moves verbatim — the operator's
-- existing password keeps working, and nobody has to be told a new one.
INSERT INTO people_credentials (person_id, kind, data, label, created_at)
SELECT p.id, 'password', u.password_hash, 'password', u.created_at
FROM people p JOIN users u ON u.id = p.user_id
WHERE NOT EXISTS (
    SELECT 1 FROM people_credentials c WHERE c.person_id = p.id AND c.kind = 'password'
);

-- Live operator sessions keep their tokens and become person sessions. The
-- cookie the browser already holds still resolves after the upgrade.
INSERT INTO people_sessions (person_id, device_id, token_digest, client_kind, created_at, expires_at, last_seen_at, user_agent, remote_addr)
SELECT p.id, NULL, s.token_digest, 'web', s.created_at, s.expires_at, s.last_seen_at, s.user_agent, s.remote_addr
FROM sessions s JOIN people p ON p.user_id = s.user_id;

DROP TABLE sessions;

-- user_id pointed at a table that is about to stop existing.
ALTER TABLE people DROP COLUMN user_id;

DROP TABLE users;
