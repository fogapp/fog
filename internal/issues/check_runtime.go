// Runtime checks: the gap between an app's desired state and what docker
// is actually running.
package issues

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// checkContainerDrift: an app that should be running whose main container
// is missing, exited, or looping. The health monitor covers apps with a
// health spec; this covers everything, including the apps without one.
func (sc *Scanner) checkContainerDrift(ctx context.Context, sn *snapshot) report {
	// A failed container listing left every main nil — evidence missing,
	// not containers. Sync nothing; existing issues survive the sweep.
	if sc.Docker == nil || !sn.dockerOK {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		// Mid-deploy state churns by design — skip without resolving.
		if a.spec == nil || a.deploying {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		if a.app.DesiredState != "running" {
			continue
		}
		if a.main == nil {
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "container-drift", AppID: a.app.ID, AppName: a.app.Name, Severity: "warning",
				Title:  a.app.Name + " should be running but has no container",
				Detail: "The app's desired state is running, but no main container exists — the last deploy may have failed after tearing the old one down.",
				Fix:    "Deploy the app again from its page (check the last deploy's log for the failure).",
			})
			continue
		}
		if a.main.State == "running" {
			continue
		}
		f := store.IssueFinding{
			Kind: "container-drift", AppID: a.app.ID, AppName: a.app.Name, Severity: "warning",
			Title:  fmt.Sprintf("%s's main container is %s", a.app.Name, a.main.State),
			Detail: fmt.Sprintf("Desired state is running, but the main container reports %q.", a.main.Status),
			Fix:    "Check the app's logs, then start or redeploy it.",
		}
		// The why matters: OOM kills point at the memory limit, not the app.
		if d, err := sc.Docker.Inspect(ctx, a.main.ID); err == nil {
			if d.State.OOMKilled {
				f.Title = a.app.Name + " was killed by the memory limit"
				f.Detail = fmt.Sprintf("The main container was OOM-killed (exit code %d). It will keep dying at the same workload.", d.State.ExitCode)
				f.Fix = "Raise memoryLimit in the app's spec, or reduce the app's memory appetite."
			} else if d.State.ExitCode != 0 {
				f.Detail = fmt.Sprintf("The main container exited with code %d (%s).", d.State.ExitCode, a.main.Status)
			}
		}
		rep.findings = append(rep.findings, f)
	}
	return rep
}

// checkStaleDomainEnv: an app running with hostname-derived environment from
// before the base domain was set (or from a different one). Env templates are
// expanded once, at deploy time, and setting the domain reconfigures the proxy
// without redeploying anything — so the app keeps serving on the new hostname
// while telling itself it lives somewhere else. Manor's passkeys were the
// first casualty: MANOR_WEBAUTHN_ORIGINS had been baked as "https://" on a
// domainless deploy, and every registration failed origin verification with
// nothing in the UI to suggest why.
func (sc *Scanner) checkStaleDomainEnv(ctx context.Context, sn *snapshot) report {
	if sc.Docker == nil || !sn.dockerOK {
		return report{}
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil || a.deploying || a.main == nil || len(a.spec.Routes) == 0 {
			continue
		}
		templated := map[string]string{}
		for k, v := range a.spec.Env {
			if strings.Contains(v, "{{fqdn}}") {
				templated[k] = v
			}
		}
		if len(templated) == 0 {
			continue
		}
		fqdn := apps.RouteHost(a.app.Name, a.spec.Routes[0], sn.baseDomain)
		if fqdn == "" {
			// No domain to expand to: the deploy drops these variables, and
			// an app with none of them is correct, not stale.
			rep.evaluated = append(rep.evaluated, a.app.ID)
			continue
		}
		d, err := sc.Docker.Inspect(ctx, a.main.ID)
		if err != nil {
			// Evidence missing, not agreement — skip without resolving.
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		running := map[string]string{}
		for _, kv := range d.Config.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				running[k] = v
			}
		}
		var stale []string
		for k, tmpl := range templated {
			if running[k] != strings.ReplaceAll(tmpl, "{{fqdn}}", fqdn) {
				stale = append(stale, k)
			}
		}
		if len(stale) == 0 {
			continue
		}
		sort.Strings(stale)
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "stale-domain-env", AppID: a.app.ID, AppName: a.app.Name, Severity: "warning",
			Title: a.app.Name + " is running with hostname settings from before " + fqdn,
			Detail: fmt.Sprintf(
				"%s expands from the app's hostname, and the container was started before %s was its hostname. "+
					"The app is reachable on the new hostname but still configured for the old one, which breaks "+
					"anything that checks it — logins and passkeys first.",
				strings.Join(stale, ", "), fqdn),
			Fix: "Deploy the app again from its page; env templates expand at deploy time.",
		})
	}
	return rep
}
