// Backup checks: configurations under which backups quietly don't happen,
// don't get verified, or wouldn't restore to the state the operator thinks.
package issues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// checkBackupUnscheduled: backups enabled but no hour resolves — no
// snapshots run, and (worse) the staleness dead-man's switch returns early
// for exactly this case, so nothing ever alarms about it.
func checkBackupUnscheduled(_ context.Context, sn *snapshot) report {
	globalOK := false
	if h, err := strconv.Atoi(sn.backupHourGlobal); err == nil && h >= 0 && h <= 23 {
		globalOK = true
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		b := a.spec.Backup
		if b == nil || !b.Enabled || b.HourUTC != nil || globalOK {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "backup-unscheduled", AppID: a.app.ID, AppName: a.app.Name, Severity: "warning",
			Title:  a.app.Name + " has backups enabled but unscheduled",
			Detail: "Backups are enabled for this app, but neither the app nor the global settings define a backup hour — no scheduled snapshots run, and the staleness alarm never engages.",
			Fix:    "Set an hour on the app's Backups → Schedule tab, or a default backup hour in Settings → Storage → Backups.",
			FixURL: "/settings/storage/backups#policy",
		})
	}
	return rep
}

// checkBackupUnverifiedMode: legacy volume snapshots are never
// test-restored, so their first real test is the disaster itself. Filed
// only for git-source apps — the operator's own code, which can adopt the
// contract (or already has, and only the spec is stale). An image app
// (paperless, homepage) would need upstream support the operator cannot
// add, so volume snapshots are simply the right mode for it; an issue
// whose only resolution is acknowledging it is a notification, not a
// configuration problem. Image apps stay in the evaluated set so an issue
// filed for one under the old rule resolves on the next sweep.
func checkBackupUnverifiedMode(_ context.Context, sn *snapshot) report {
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		if !a.spec.BackupsEnabled() || a.spec.DelegatedBackups() || a.spec.Source.Type != "git" {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "backup-unverified-mode", AppID: a.app.ID, AppName: a.app.Name, Severity: "info",
			Title:  a.app.Name + " uses legacy volume backups",
			Detail: "Volume-mode snapshots are taken but never restore-verified; only fog-backup/1 artifacts get periodic test restores.",
			Fix:    "If this app implements the fog-backup/1 contract (docs/backup-contract.md), switch its backup mode to delegated on the Backups → Schedule tab. If it doesn't yet, adopting the contract is a change to the app's own code.",
			FixURL: "/apps/" + a.app.Name + "/drive?tab=schedule",
		})
	}
	return rep
}

// checkBackupSecretsDrift compares the newest artifact's secrets
// fingerprints against the vault's current values. A mismatch means
// restoring that artifact would come up under different keys than it was
// taken with — data encrypted with the old ones becomes unreadable, and
// the discrepancy is invisible until a restore is already failing.
func (sc *Scanner) checkBackupSecretsDrift(_ context.Context, sn *snapshot) report {
	if sc.SecretValue == nil {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil || a.lastBackup == nil || a.lastBackup.Manifest == "" {
			continue
		}
		var m struct {
			SecretsFingerprint map[string]string `json:"secrets_fingerprint"`
		}
		if json.Unmarshal([]byte(a.lastBackup.Manifest), &m) != nil || len(m.SecretsFingerprint) == 0 {
			continue
		}
		var drifted []string
		failed := false
		for key, want := range m.SecretsFingerprint {
			val, err := sc.SecretValue(a.app.ID, key)
			if err != nil {
				// A vault read failing is not evidence of drift; leave the
				// app out of evaluated so nothing resolves on it either.
				sc.Log.Warn("issues: secrets drift", "app", a.app.Name, "key", key, "err", err)
				failed = true
				break
			}
			if secretFingerprint(val) != want {
				drifted = append(drifted, key)
			}
		}
		if failed {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		if len(drifted) == 0 {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "backup-secrets-drift", AppID: a.app.ID, AppName: a.app.Name, Severity: "warning",
			Title:  fmt.Sprintf("%s's newest backup was taken under different secrets", a.app.Name),
			Detail: "These restore-critical secrets have changed since the newest backup: " + strings.Join(drifted, ", ") + ". Restoring it would refuse at preflight (or worse, decrypt nothing).",
			Fix:    "Take a fresh backup — or, if the change was accidental, restore the previous secret values first.",
		})
	}
	return rep
}

// secretFingerprint mirrors the contract's recipe (backup-contract.md):
// "sha256:" + first 12 hex chars of the SHA-256 of the value.
func secretFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}
