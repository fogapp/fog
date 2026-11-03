// Database checks: the app side of the shared Postgres. The motivating
// case for the whole subsystem lives here — a pg_dump older than the
// server it dumps refuses to run, which silently kills every pre-update
// backup and aborts every update.
package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/store"
)

// checkPGDumpSkew probes each delegated app's image for its pg_dump major.
// Primary source is a live exec — it works even when every backup fails,
// which is exactly when this check matters most. The newest artifact's
// manifest (pg.dump_tool) is the fallback when exec isn't possible.
func (sc *Scanner) checkPGDumpSkew(ctx context.Context, sn *snapshot) report {
	// A failed container listing left every main nil, which would silently
	// swap the live probe for stale manifest evidence — skip the sweep. No
	// known server major is the same kind of missing evidence.
	if !sn.dockerOK || sn.pgMajor == 0 {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil || a.deploying {
			continue
		}
		if a.spec.Needs == nil || a.spec.Needs.Postgres == nil {
			continue
		}
		if !a.spec.DelegatedBackups() {
			continue
		}
		// An app the operator stopped makes no backups — a delegated
		// backup is an HTTP conversation with a running app — so whatever
		// its pg_dump is, nothing is failing. Evaluated clean: an issue
		// filed while it ran resolves, and files again the first sweep
		// after it is running with the skew still there. (Wanted running
		// but not: no evidence, leave the issue alone; container-drift
		// owns that case.)
		if a.app.DesiredState != "running" {
			rep.evaluated = append(rep.evaluated, a.app.ID)
			continue
		}
		major, ok := 0, false
		if sc.ExecInApp != nil && a.main != nil && a.main.State == "running" {
			if out, err := sc.ExecInApp(ctx, a.app.Name, 10*time.Second, "pg_dump", "--version"); err == nil {
				major, ok = pg.ParseMajor(out)
			}
		}
		if !ok && a.lastBackup != nil && a.lastBackup.Manifest != "" {
			var m struct {
				PG struct {
					DumpTool string `json:"dump_tool"`
				} `json:"pg"`
			}
			if json.Unmarshal([]byte(a.lastBackup.Manifest), &m) == nil {
				major, ok = pg.ParseMajor(m.PG.DumpTool)
			}
		}
		if !ok {
			continue // no evidence either way — leave existing issues alone
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		if f, found := pgDumpSkewFinding(a.app, major, sn.pgMajor); found {
			rep.findings = append(rep.findings, f)
		}
	}
	return rep
}

// pgDumpSkewFinding is the direction rule. A client older than the server
// is the outage: pg_dump refuses outright. A client newer than the server
// dumps fine but is still wrong — its pg_restore emits settings the older
// server rejects (exit 1 on an otherwise complete restore), and its archives
// are unreadable to the server's own tools — so it files as a warning that
// clears when the fleet is on one major.
func pgDumpSkewFinding(app store.App, client, server int) (store.IssueFinding, bool) {
	switch {
	case client == server:
		return store.IssueFinding{}, false
	case client < server:
		return store.IssueFinding{
			Kind: "pg-dump-skew", AppID: app.ID, AppName: app.Name, Severity: "critical",
			Title:  fmt.Sprintf("%s's pg_dump is v%d but fog's Postgres is v%d", app.Name, client, server),
			Detail: "pg_dump refuses to dump a server newer than itself, so every backup this app produces fails — and with pre-update backups enabled, every update aborts with it.",
			Fix:    fmt.Sprintf("Rebuild the app's image with postgresql-client-%d (Debian's default postgresql-client package is older; use the PGDG apt repo).", server),
		}, true
	default:
		return store.IssueFinding{
			Kind: "pg-dump-skew", AppID: app.ID, AppName: app.Name, Severity: "warning",
			Title:  fmt.Sprintf("%s's pg_dump is v%d, newer than fog's Postgres v%d", app.Name, client, server),
			Detail: "Backups work, but the app's own pg_restore emits settings a Postgres " + fmt.Sprint(server) + " server rejects, so a restore reports failure after copying every row — and fog's own tools cannot read the archive. Expected only while the fleet moves majors.",
			Fix:    fmt.Sprintf("Upgrade fog's Postgres to %d (Settings → Storage → Database), or pin the app's image to postgresql-client-%d.", client, server),
			FixURL: "/settings/storage/database",
		}, true
	}
}

// checkPGUpgradeAvailable is the standing prompt while the data directory
// is behind the major this fog ships: nothing is broken, the move is one
// button, and the finding resolves itself on the sweep after it.
func (sc *Scanner) checkPGUpgradeAvailable(_ context.Context, sn *snapshot) report {
	if sn.pgMajor == 0 {
		return report{}
	}
	rep := report{evaluated: []int64{0}}
	if sn.pgMajor < pg.TargetMajor {
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "postgres-upgrade-available", Subject: "cluster", Severity: "warning",
			Title:  fmt.Sprintf("fog's Postgres is v%d; this fog runs v%d", sn.pgMajor, pg.TargetMajor),
			Detail: fmt.Sprintf("The shared cluster's data is Postgres %d and keeps running as it is. Fog now ships Postgres %d, and app images are pinned to its client tools; until the cluster moves, every app's pg_dump is newer than the server it dumps.", sn.pgMajor, pg.TargetMajor),
			Fix:    "Settings → Storage → Database → Upgrade. Apps stop for the few minutes it takes; the old data directory is kept until you reclaim it.",
			FixURL: "/settings/storage/database",
		})
	}
	return rep
}

// checkPGServerMismatch reads each delegated app's contract descriptor: a
// dead endpoint means the next backup fails, and a reported server major
// that isn't fog's means the app is talking to some other Postgres.
func (sc *Scanner) checkPGServerMismatch(ctx context.Context, sn *snapshot) report {
	// Every main is nil after a failed container listing; nothing below
	// could be evaluated anyway, so say so explicitly.
	if sc.FetchDescriptor == nil || !sn.dockerOK || sn.pgMajor == 0 {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil || a.deploying {
			continue
		}
		if !a.spec.DelegatedBackups() {
			continue
		}
		// A stopped app's endpoint being unreachable is expected, not
		// drift — and nothing about its database can be wrong while it
		// makes no backups, so it evaluates clean and an issue filed while
		// it ran resolves. Wanted running but not: no evidence.
		if a.app.DesiredState != "running" {
			rep.evaluated = append(rep.evaluated, a.app.ID)
			continue
		}
		if a.main == nil || a.main.State != "running" {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		d, err := sc.FetchDescriptor(ctx, &a.app, a.spec)
		if err != nil {
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "pg-server-mismatch", AppID: a.app.ID, AppName: a.app.Name, Subject: "descriptor", Severity: "warning",
				Title:  a.app.Name + "'s backup contract endpoint is unreachable",
				Detail: "GET /fog/backup failed: " + err.Error() + ". The next backup for this app will fail the same way.",
				Fix:    "Check that the app is healthy and that its FOG_API_TOKEN matches (redeploy refreshes the token).",
			})
			continue
		}
		if d.PG.ServerMajor != 0 && d.PG.ServerMajor != sn.pgMajor {
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "pg-server-mismatch", AppID: a.app.ID, AppName: a.app.Name, Severity: "warning",
				Title:  fmt.Sprintf("%s connects to Postgres %d, not fog's v%d", a.app.Name, d.PG.ServerMajor, sn.pgMajor),
				Detail: "The app reports a different Postgres major than fog's managed server — it is likely pointed at a stale or external database.",
				Fix:    "Check the app's database env vars against the fog-injected values.",
			})
		}
	}
	return rep
}

// checkPGExtensionMissing compares what each app's spec declares against
// what is actually installed in its databases — drift a restore or a
// hand-edit can introduce, and which only surfaces as a failing migration.
func (sc *Scanner) checkPGExtensionMissing(ctx context.Context, sn *snapshot) report {
	if sc.PG == nil {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil || a.deploying {
			continue
		}
		need := a.spec.Needs
		if need == nil || need.Postgres == nil || len(need.Postgres.Extensions) == 0 {
			continue
		}
		ok := true
		var findings []store.IssueFinding
		for _, db := range need.Postgres.Databases {
			installed, err := sc.PG.Extensions(ctx, db)
			if err != nil {
				sc.Log.Warn("issues: pg extensions", "app", a.app.Name, "db", db, "err", err)
				ok = false
				break
			}
			var missing []string
			for _, ext := range need.Postgres.Extensions {
				if _, have := installed[ext]; !have {
					missing = append(missing, ext)
				}
			}
			if len(missing) > 0 {
				findings = append(findings, store.IssueFinding{
					Kind: "pg-extension-missing", AppID: a.app.ID, AppName: a.app.Name, Subject: db, Severity: "warning",
					Title:  fmt.Sprintf("%s is missing extensions in %s", a.app.Name, db),
					Detail: fmt.Sprintf("The spec declares %v but %s does not have: %v.", need.Postgres.Extensions, db, missing),
					Fix:    "Redeploy the app so fog re-provisions its databases.",
				})
			}
		}
		if !ok {
			continue // couldn't assess — leave existing issues alone
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		rep.findings = append(rep.findings, findings...)
	}
	return rep
}
