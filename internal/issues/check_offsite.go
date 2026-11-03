// Offsite checks: backups that never leave the house, and targets whose
// bucket or key still lets a compromised box destroy the history. Both
// are configuration states — a target card shows them, and the Inbox
// keeps them visible between failures with the fix a click away.
package issues

import (
	"context"

	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/store"
)

// checkOffsiteUnconfigured: scheduled backups exist but no active offsite
// target does — every copy is on the same premises as the original.
// Host scope. An info, not a warning: a NAS in the house is a choice; the
// issue says what the choice costs.
func checkOffsiteUnconfigured(_ context.Context, sn *snapshot) report {
	rep := report{evaluated: []int64{0}}
	scheduled := sn.backupHourGlobal != ""
	for _, a := range sn.apps {
		if a.spec.BackupsEnabled() && a.spec.Backup.HourUTC != nil {
			scheduled = true
		}
	}
	if !scheduled {
		return rep
	}
	for _, t := range sn.offsiteTargets {
		if t.Role == "active" {
			return rep
		}
	}
	rep.findings = append(rep.findings, store.IssueFinding{
		Kind: "offsite-unconfigured", Subject: "offsite", Severity: "info",
		Title:  "Backups never leave the house",
		Detail: "Nightly backups run, but no offsite target is configured: a fire, a flood, a burglary, or ransomware that reaches the NAS takes the server and its backups together.",
		Fix:    "Add an offsite target on Settings → Storage → Offsite — a Backblaze B2 bucket costs cents a month for this fleet, and the recovery kit lands there too.",
		FixURL: "/settings/storage/offsite",
	})
	return rep
}

// checkOffsiteGrade: an active target below grade A. The probe already
// said what raises it; this keeps the answer in the Inbox until it is
// done, per target.
func checkOffsiteGrade(_ context.Context, sn *snapshot) report {
	rep := report{evaluated: []int64{0}}
	for _, t := range sn.offsiteTargets {
		if t.Role != "active" || t.Grade == "" || t.Grade == "A" {
			continue
		}
		fix := "Re-check the target on Settings → Storage → Offsite for the one change that raises it."
		if p := offsite.ParseProbe(t.Probe); p != nil && p.Fix != "" {
			fix = p.Fix
		}
		severity := "warning"
		detail := "The key fog holds for this bucket can permanently delete what it wrote, so a compromised server — or a stolen key — could erase the offsite history along with the local copies."
		switch t.Grade {
		case "B":
			severity = "info"
			detail = "The key cannot destroy a version, but the bucket has no Object Lock: someone with the account login could still remove the history. Object Lock makes it immutable for the retention window."
		case "D":
			detail = "The bucket keeps no versions: a delete is a delete, and fog's tidy-up — or an attacker's — is final."
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "offsite-grade", Subject: t.Name, Severity: severity,
			Title:  "Offsite target " + t.Name + " is " + offsite.GradeLabel(t.Grade) + " (grade " + t.Grade + "), not immutable",
			Detail: detail,
			Fix:    fix,
			FixURL: "/settings/storage/offsite",
		})
	}
	return rep
}
