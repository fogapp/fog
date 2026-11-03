// Wiring between apps (docs/files-contract.md, "Wiring"): an app that
// uses another must see the folders that one shares, and a container must
// still see the folders it was started with.
package issues

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// checkAppUnwired files one issue per app and provider whose shared
// folders the app cannot see — Hook using SABnzbd without a view of its
// downloads. Installing either side wires it; this catches what drifted
// afterwards (a folder moved to a host path, a volume edited away).
func checkAppUnwired(_ context.Context, sn *snapshot) report {
	byName := map[string]*apps.AppSpec{}
	for _, a := range sn.apps {
		if a.spec != nil {
			byName[a.app.Name] = a.spec
		}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		for _, u := range a.spec.Uses {
			p := byName[u]
			if u == apps.UsesEvents || u == a.app.Name || p == nil {
				continue
			}
			unseen := apps.UnseenShares(p, a.spec, sn.filesRoot)
			if len(unseen) == 0 {
				continue
			}
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "app-unwired", AppID: a.app.ID, AppName: a.app.Name, Subject: u, Severity: "warning",
				Title: fmt.Sprintf("%s cannot see %s's %s", a.app.Name, u, plural(unseen, "folder", "folders")),
				Detail: fmt.Sprintf("%s uses %s, which shares %s with the apps that use it. %s has no volume covering %s, so paths %s reports point nowhere inside %s.",
					a.app.Name, u, strings.Join(unseen, ", "), a.app.Name, plural(unseen, "it", "them"), u, a.app.Name),
				Fix:    fmt.Sprintf("Mount them: fog adds a volume to %s that follows each folder, and redeploys it.", a.app.Name),
				FixURL: "/apps/" + a.app.Name + "/config",
			})
		}
	}
	return rep
}

func plural(xs []string, one, many string) string {
	if len(xs) == 1 {
		return one
	}
	return many
}

// staleMountProbe bounds the in-container listing.
const staleMountProbe = 15 * time.Second

// checkMountStale lists each running app's shared folders from inside its
// container. A network share whose folder was replaced under a running
// container leaves it holding a dead handle ("Stale file handle"): the
// host sees the folder, the app does not, until the container restarts.
func (sc *Scanner) checkMountStale(ctx context.Context, sn *snapshot) report {
	if sc.ExecInApp == nil || !sn.dockerOK {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil || a.deploying || a.main == nil || a.main.State != "running" {
			continue
		}
		var mounts []string
		for _, v := range a.spec.Volumes {
			if !v.Managed() {
				mounts = append(mounts, v.Mount)
			}
		}
		if len(mounts) == 0 {
			continue
		}
		out, err := sc.ExecInApp(ctx, a.app.Name, staleMountProbe, append([]string{"ls", "-d"}, mounts...)...)
		if err != nil {
			// A failed listing prints why; anything but a dead handle (no ls
			// in the image, the container going away) is no verdict.
			if out = err.Error(); !strings.Contains(out, "Stale file handle") {
				continue
			}
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		var stale []string
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, "Stale file handle") {
				continue
			}
			for _, m := range mounts {
				if strings.Contains(line, "'"+m+"'") || strings.Contains(line, " "+m+":") {
					stale = append(stale, m)
				}
			}
		}
		if len(stale) == 0 {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "app-mount-stale", AppID: a.app.ID, AppName: a.app.Name, Subject: strings.Join(stale, ","), Severity: "critical",
			Title:  fmt.Sprintf("%s lost sight of %s", a.app.Name, strings.Join(stale, ", ")),
			Detail: fmt.Sprintf("The folder behind %s was replaced on the share while %s was running, and the container still holds the old one: every read there fails with \"Stale file handle\" though the folder is fine on the host.", strings.Join(stale, ", "), a.app.Name),
			Fix:    fmt.Sprintf("Restart %s — it binds the folder afresh.", a.app.Name),
			FixURL: "/apps/" + a.app.Name,
		})
	}
	return rep
}
