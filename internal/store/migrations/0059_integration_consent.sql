-- An offer a person approves at the provider first (docs/integrations-
-- contract.md, "Consent"): the row waits as pending, found again by the
-- digest of the state fog put in its return address, and becomes
-- connected once fog has claimed the credential. deploy remembers whether
-- the operator asked for a redeploy when they connected.
ALTER TABLE app_integrations ADD COLUMN state TEXT NOT NULL DEFAULT 'connected';  -- pending | connected
ALTER TABLE app_integrations ADD COLUMN return_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE app_integrations ADD COLUMN deploy INTEGER NOT NULL DEFAULT 0;
