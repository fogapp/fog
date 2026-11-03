// The check registry and the sweep's reporting tail (push + rollup notice).
// Each check is a pure function over the snapshot (plus the Scanner's probe
// fields) returning findings and the scopes it actually assessed.
package issues

import (
	"context"
	"fmt"

	"gitea.lan/matthew/fog/internal/store"
)

type check struct {
	kind string
	// also lists further kinds the same run files, for a check that sees
	// several problems in one pass. Each kind syncs on its own.
	also     []string
	category string // database | backups | network | storage | runtime | host
	run      func(ctx context.Context, sn *snapshot) report
}

// kinds is every kind the check files.
func (c check) kinds() []string { return append([]string{c.kind}, c.also...) }

// report is one check's output. evaluated lists the scopes (app IDs, 0 for
// host) actually assessed — the auto-resolve boundary.
type report struct {
	findings  []store.IssueFinding
	evaluated []int64
}

// Categories maps check kinds to their display group, for the API. It is
// read off the registry, so a new check can't be missing from it. A nil
// Scanner is enough: the method values in the registry are only listed,
// never called.
var Categories = func() map[string]string {
	m := map[string]string{}
	for _, c := range (*Scanner)(nil).checks() {
		for _, k := range c.kinds() {
			m[k] = c.category
		}
	}
	return m
}()

func (sc *Scanner) checks() []check {
	return []check{
		{kind: "route-host-collision", category: "network", run: checkRouteHostCollision},
		{kind: "spec-unreadable", category: "runtime", run: checkSpecUnreadable},
		{kind: "sealed-wire-refused", category: "network", run: checkSealedWireRefused},
		{kind: "wire-unsupported", category: "network", run: checkWireUnsupported},
		{kind: "wire-unapproved", category: "network", run: checkWireUnapproved},
		{kind: "host-port-collision", category: "network", run: checkHostPortCollision},
		{kind: "fog-port-taken", category: "network", run: sc.checkFogPortTaken},
		{kind: "app-not-reapplied", category: "runtime", run: sc.checkAppNotReapplied},
		{kind: "app-secret-missing", category: "runtime", run: sc.checkAppSecretMissing},
		{kind: "app-waiting-for-storage", category: "storage", run: sc.checkAppWaitingForStorage},
		{kind: "health-unmonitored", category: "network", run: checkHealthUnmonitored},
		{kind: "mdns-collision", category: "network", run: checkMdnsCollision},
		{kind: "docker-subnet-overlap", category: "network", run: checkDockerSubnetOverlap},
		{kind: "tunnel-service", category: "network", run: sc.checkTunnelService},
		{kind: "backup-unscheduled", category: "backups", run: checkBackupUnscheduled},
		{kind: "backup-unverified-mode", category: "backups", run: checkBackupUnverifiedMode},
		{kind: "backup-secrets-drift", category: "backups", run: sc.checkBackupSecretsDrift},
		{kind: "offsite-unconfigured", category: "backups", run: checkOffsiteUnconfigured},
		{kind: "offsite-grade", category: "backups", run: checkOffsiteGrade},
		{kind: "volume-off-share", category: "storage", run: checkVolumeOffShare},
		{kind: "share-unavailable", category: "storage", run: checkShareUnavailable},
		{kind: "files-not-app-writable", category: "storage", run: sc.checkFilesOwnerCannotWrite},
		{kind: "files-sync-failing", also: []string{"files-offer-changed"}, category: "storage", run: checkFilesSync},
		{kind: "app-unwired", category: "storage", run: checkAppUnwired},
		{kind: "app-mount-stale", category: "storage", run: sc.checkMountStale},
		{kind: "container-drift", category: "runtime", run: sc.checkContainerDrift},
		{kind: "stale-domain-env", category: "runtime", run: sc.checkStaleDomainEnv},
		{kind: "host-env", category: "host", run: sc.checkHostEnv},
		{kind: "docker-missing", category: "host", run: sc.checkDockerMissing},
		{kind: "compute-never-connected", category: "host", run: sc.checkComputeNeverConnected},
		{kind: "compute-out-of-date", category: "host", run: sc.checkComputeOutOfDate},
		{kind: "pg-dump-skew", category: "database", run: sc.checkPGDumpSkew},
		{kind: "pg-server-mismatch", category: "database", run: sc.checkPGServerMismatch},
		{kind: "pg-extension-missing", category: "database", run: sc.checkPGExtensionMissing},
		{kind: "postgres-upgrade-available", category: "database", run: sc.checkPGUpgradeAvailable},
	}
}

// report pushes critical transitions. It files no notice: an issue is
// already an Inbox row, and a notice counting those rows was a second copy
// of the Issues tab's own badge — dismissable, but back on the next sweep.
// Counts of Inbox rows are badges, never rows.
func (sc *Scanner) report(total *store.IssueSyncResult) {
	// Push only critical New/Recurred: each fires at most once per issue
	// lifecycle by construction, so no cooldown bookkeeping is needed.
	// Reopened (an ack broken by changed details) stays quiet — pushing
	// would punish acknowledging; the badge resurfaces it.
	var crit []store.Issue
	for _, i := range append(append([]store.Issue{}, total.New...), total.Recurred...) {
		if i.Severity == "critical" {
			crit = append(crit, i)
		}
	}
	if sc.Notify == nil || len(crit) == 0 {
		return
	}
	if len(crit) > 3 {
		sc.Notify("Configuration issues", fmt.Sprintf("%d new critical configuration issues — see the Inbox", len(crit)), false)
		return
	}
	for _, i := range crit {
		sc.Notify("Issue: "+i.Title, i.Detail, false)
	}
}
