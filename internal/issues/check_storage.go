// Storage checks: paths that look fine to the filesystem but land data
// somewhere other than where the operator believes it lives.
package issues

import (
	"context"
	"fmt"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/store"
)

// canWriteAs is the test seam for the owner probe: the real one forks a
// child as another uid, which needs root and a real filesystem.
var canWriteAs = fsx.CanWriteAs

// checkVolumeOffShare: an app's host-path volume sits under a NAS share's
// mountpoint while the share is not mounted — the container binds the bare
// directory and quietly writes to local disk. Deploys guard against this
// (EnsureMounts), but a share dropping while the app runs does not redeploy
// anything.
func checkVolumeOffShare(_ context.Context, sn *snapshot) report {
	// Unreadable share statuses are not "everything mounted" — sync nothing
	// so open critical issues survive instead of resolve/recur flapping.
	if !sn.nasOK {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		for _, p := range a.spec.HostPaths(sn.filesRoot) {
			if sh := unmountedShareFor(p, sn.shares); sh != nil {
				rep.findings = append(rep.findings, store.IssueFinding{
					Kind: "volume-off-share", AppID: a.app.ID, AppName: a.app.Name, Subject: p, Severity: "critical",
					Title:  fmt.Sprintf("%s is writing past an unmounted share", a.app.Name),
					Detail: fmt.Sprintf("%s lives under the NAS share %q (%s), which is not mounted — anything the app writes there lands on the local disk and disappears from view once the share mounts again.", p, sh.Name, sh.MountPoint),
					Fix:    "Remount the share in Settings → Storage → Disks & shares, then restart the app.",
				})
			}
		}
	}
	// The backup directory has the same failure mode, host-scoped.
	rep.evaluated = append(rep.evaluated, 0)
	if sn.backupDir != "" {
		if sh := unmountedShareFor(sn.backupDir, sn.shares); sh != nil {
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "volume-off-share", Subject: "backup-dir", Severity: "critical",
				Title:  "The backup directory is on an unmounted share",
				Detail: fmt.Sprintf("Backups go to %s under the NAS share %q, which is not mounted — new snapshots are landing on the local disk.", sn.backupDir, sh.Name),
				Fix:    "Remount the share in Settings → Storage → Disks & shares.",
			})
		}
	}
	return rep
}

// checkShareUnavailable: an enabled share is not mounted, or is mounted but
// its server no longer answers (a hard mount whose NAS moved or went away).
// The mount reconcile keeps retrying in the background; this check is what
// makes the failure visible instead of a Debug line — a server carried to a
// new network should announce which of its shares didn't survive the trip.
func checkShareUnavailable(_ context.Context, sn *snapshot) report {
	// Same rule as checkVolumeOffShare: no statuses, no verdicts.
	if !sn.nasOK {
		return report{}
	}
	var rep report
	rep.evaluated = append(rep.evaluated, 0)
	for _, sh := range sn.shares {
		if !sh.Enabled || (sh.Mounted && !sh.Stale) {
			continue
		}
		state := "is not mounted"
		if sh.Stale {
			state = "is mounted but not responding"
		}
		detail := fmt.Sprintf("The NAS share %q (%s on %s) %s.", sh.Name, sh.Remote, sh.Server, state)
		if sh.LastError != "" {
			detail += " Last error: " + sh.LastError
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "share-unavailable", Subject: sh.Name, Severity: "warning",
			Title:  fmt.Sprintf("NAS share %q is unavailable", sh.Name),
			Detail: detail,
			Fix:    "Fog keeps retrying the mount. If the NAS is at a new address, edit the share in Settings → Storage → Disks & shares; if it is gone for good, remove the share.",
		})
	}
	return rep
}

func unmountedShareFor(path string, shares []nas.ShareStatus) *nas.ShareStatus {
	for i := range shares {
		mp := shares[i].MountPoint
		if mp == "" || shares[i].Mounted {
			continue
		}
		if files.IsUnder(path, mp) {
			return &shares[i]
		}
	}
	return nil
}

// checkFilesOwnerCannotWrite: fog creates an app's folder in Files and chowns
// it to files_owner, so the container — which writes as that uid, never as
// root — can fill it. On a share that does not honour the uid the chown is a
// formality: an NFS export that squashes every identity but root, a DSM share
// whose ACL names accounts rather than numbers. fogd is root, so everything
// fog does itself keeps working, while every app fails inside its own folder,
// each in its own vocabulary and none of them naming the cause. Only a write
// as that uid can tell them apart, so this makes one.
//
// The probe is per folder, not on the Files root: fog chowns what it creates
// under Apps, and a root of its own (a fresh install's /var/lib/fog/files is
// root:root) is no promise to anyone.
func (sc *Scanner) checkFilesOwnerCannotWrite(ctx context.Context, sn *snapshot) report {
	if sc.FilesOwner == nil || sn.filesRoot == "" || !sn.nasOK {
		return report{}
	}
	uid, gid, ok := sc.FilesOwner()
	if !ok {
		// files_owner is blank, or fogd is not root: fog leaves ownership
		// alone and promises nothing about it.
		return report{}
	}

	// Gather first, probe once per distinct folder: several apps commonly
	// share one library, and this runs against a NAS.
	type target struct {
		app   store.App
		paths []string
	}
	var targets []target
	folders := map[string]bool{}
	for _, a := range sn.apps {
		if a.spec == nil || a.deploying {
			continue
		}
		t := target{app: a.app}
		for _, v := range a.spec.AllVolumes() {
			p := v.HostPath(sn.filesRoot)
			if v.Files == "" || p == "" {
				continue
			}
			// An unmounted share is already its own issue, and the folder
			// under a dead mountpoint is not the one the app would use.
			if unmountedShareFor(p, sn.shares) != nil {
				continue
			}
			t.paths = append(t.paths, p)
			folders[p] = true
		}
		if len(t.paths) > 0 {
			targets = append(targets, t)
		}
	}

	refused := map[string]string{}
	for p := range folders {
		writable, why, err := canWriteAs(ctx, p, uid, gid)
		if err != nil {
			// The probe could not run: no verdict for anyone, rather than a
			// verdict drawn from a broken instrument.
			sc.Log.Warn("files owner probe", "folder", p, "err", err)
			return report{}
		}
		if !writable {
			refused[p] = why
		}
	}

	var rep report
	for _, t := range targets {
		rep.evaluated = append(rep.evaluated, t.app.ID)
		for _, p := range t.paths {
			why, bad := refused[p]
			if !bad {
				continue
			}
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "files-not-app-writable", AppID: t.app.ID, AppName: t.app.Name, Subject: p, Severity: "critical",
				Title: fmt.Sprintf("%s cannot write to its folder in Drive", t.app.Name),
				Detail: fmt.Sprintf("Fog gives %s the folder %s owned by %d:%d, because that is the identity its container writes as — but creating a file there as %d:%d is refused (%s). Fog runs as root, so the folder browses and fills normally from fog's own side; %s fails the moment it saves anything.",
					t.app.Name, p, uid, gid, uid, gid, why, t.app.Name),
				Fix:    fmt.Sprintf("The share decides this, not fog. On the NAS, let this server's writes land as an account with write access — on Synology, Control Panel → Shared Folder → NFS Permissions → Squash → \"Map all users to admin\" — or grant %d:%d write access to the share. If the apps should run as a different uid, set that as the Drive owner and redeploy them.", uid, gid),
				FixURL: "/settings/storage/disks",
			})
		}
	}
	return rep
}
