-- How a session was handed out (docs/signin-contract.md "Sessions and
-- logout"), so how it may be used is decided by what fog did, never by the
-- name a request presents it under:
--   host   set as __Host-fog_session on an https setup: only fog's own host
--          can hold it, so it is not pinned to an address;
--   plain  set as a plain host-only fog_session (an http setup, a bare
--          address) or otherwise reachable by a browser: every port of the
--          address can see it, so it works only from the address it was
--          signed in from;
--   device a native chat client's bearer token, proven by its device key:
--          never accepted as a cookie, and not pinned, so the client can
--          move between networks.
-- Sessions from before this are 'plain': pinned, which costs at most a
-- sign-in after moving networks.
ALTER TABLE people_sessions ADD COLUMN issued_as TEXT NOT NULL DEFAULT 'plain';
-- A native chat client's session was only ever its bearer token.
UPDATE people_sessions SET issued_as = 'device' WHERE device_id IS NOT NULL AND client_kind = 'native';
