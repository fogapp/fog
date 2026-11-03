-- The browser a cookie session was set in (docs/signin-contract.md
-- "Sessions and logout"): the digest of the fog_browser cookie fog set beside
-- it. Where the session cookie is a plain fog_session — an http setup, a
-- bare address — anything sharing the host can plant a session cookie of
-- its own beside fog's. When a request carries more than one session that
-- would count, only one bound to a fog_browser cookie the browser carries
-- does. Sessions from before this have none, and never win such a tie.
ALTER TABLE people_sessions ADD COLUMN browser_digest TEXT NOT NULL DEFAULT '';
