-- What the operator approved an app to do beyond fog's own storage and
-- ports: a host path, a host port fog answers on, raw sockets. A spec's
-- own acknowledgeHost* fields are what it asks for; a deploy needs these
-- records, made by an operator in fog's UI or API, or by confirming a
-- review — so neither an imported manifest nor a rollback to a revision
-- nobody approved carries an approval of its own (internal/apps/approvals.go).
-- They go with the app: a later app of the same name inherits none.
CREATE TABLE app_approvals (
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    value      TEXT NOT NULL,
    actor      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (app_id, kind, value)
);

-- The revision an operator confirmed an imported app's review at: no
-- earlier one deploys without being confirmed again.
ALTER TABLE apps ADD COLUMN reviewed_rev INTEGER NOT NULL DEFAULT 0;
