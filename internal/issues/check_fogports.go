package issues

import (
	"context"
	"fmt"
	"strings"

	"gitea.lan/matthew/fog/internal/fogports"
	"gitea.lan/matthew/fog/internal/store"
)

// checkFogPortTaken: an app whose configuration publishes a host port fog
// holds (fogports) — saved before fog refused them. It no longer deploys
// or starts, and fog stops it if it runs (apps.ReclaimFogPorts): at boot
// Docker starts it before fogd, and it would answer in fog's place.
func (sc *Scanner) checkFogPortTaken(_ context.Context, sn *snapshot) report {
	fogd := 0
	if sc.FogdPort != nil {
		fogd = sc.FogdPort()
	}
	var rep report
	for _, a := range sn.apps {
		if a.spec == nil {
			continue
		}
		rep.evaluated = append(rep.evaluated, a.app.ID)
		var held, owners []string
		for _, p := range a.spec.Ports {
			hp := p.EffectiveHostPort()
			if owner := fogports.Owner(hp, fogd); owner != "" {
				held = append(held, fmt.Sprintf("%d/%s", hp, p.EffectiveProtocol()))
				owners = append(owners, owner)
			}
		}
		if len(held) == 0 {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "fog-port-taken", AppID: a.app.ID, AppName: a.app.Name, Severity: "critical",
			Title: fmt.Sprintf("%s publishes %s of fog's own", a.app.Name, plural(held, "a port", "ports")),
			Detail: fmt.Sprintf("%s publishes host %s, which %s. Docker starts apps before fog at boot, so it would take the port first and answer in fog's place. fog no longer deploys or starts it, and stops it if it runs.",
				a.app.Name, strings.Join(held, ", "), strings.Join(dedupe(owners), " and ")+" "+plural(dedupe(owners), "holds", "hold")),
			Fix:    "Publish it on other host ports, then deploy it.",
			FixURL: "/apps/" + a.app.Name + "/config",
		})
	}
	return rep
}

func dedupe(xs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// checkAppNotReapplied: an app fog could not bring up under the rules as
// they stand (apps.ReapplyRules, and every start): a revision they refuse
// — a bind no longer approved, a port of fog's — is stopped; one whose
// start failed for another reason may still run as it was last rendered,
// with whatever that render allowed.
func (sc *Scanner) checkAppNotReapplied(_ context.Context, sn *snapshot) report {
	refused := map[string]string{}
	if sc.RulesRefused != nil {
		refused = sc.RulesRefused()
	}
	var rep report
	for _, a := range sn.apps {
		rep.evaluated = append(rep.evaluated, a.app.ID)
		why, ok := refused[a.app.Name]
		if !ok {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "app-not-reapplied", AppID: a.app.ID, AppName: a.app.Name, Severity: "critical",
			Title:  a.app.Name + " couldn't be brought up under fog's current rules",
			Detail: "fog renders every app afresh when it starts, so the protections fog has now apply to it. " + a.app.Name + "'s didn't go through: " + why + ". If the rules refused it, it is stopped; otherwise it may still be running as it was last deployed.",
			Fix:    "Fix what the message names (approve it in the app's settings, or change its configuration), then start or deploy it.",
			FixURL: "/apps/" + a.app.Name + "/config",
		})
	}
	return rep
}

// checkAppSecretMissing: an app started without secrets its running
// revision lists — removed since by a disconnect, a deleted provider or an
// edit. It runs with them empty rather than not at all; deploying its
// latest revision settles which it should have.
func (sc *Scanner) checkAppSecretMissing(_ context.Context, sn *snapshot) report {
	missing := map[string][]string{}
	if sc.SecretsMissing != nil {
		missing = sc.SecretsMissing()
	}
	var rep report
	for _, a := range sn.apps {
		rep.evaluated = append(rep.evaluated, a.app.ID)
		names := missing[a.app.Name]
		if len(names) == 0 {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "app-secret-missing", AppID: a.app.ID, AppName: a.app.Name, Severity: "warning",
			Title:  a.app.Name + " is running without " + strings.Join(names, ", "),
			Detail: "The revision " + a.app.Name + " runs lists " + strings.Join(names, ", ") + ", which fog no longer holds — removed by a disconnect, a deleted app it was connected to, or an edit. fog started it with them empty rather than not at all.",
			Fix:    "Deploy its latest revision, or set the secrets again.",
			FixURL: "/apps/" + a.app.Name + "/config",
		})
	}
	return rep
}

// checkAppWaitingForStorage: an app left stopped because its storage
// isn't there yet — a NAS share not mounted after a power cut. fog starts
// it the moment the share mounts; this only says why it isn't running.
func (sc *Scanner) checkAppWaitingForStorage(_ context.Context, sn *snapshot) report {
	waiting := map[string]string{}
	if sc.Waiting != nil {
		waiting = sc.Waiting()
	}
	var rep report
	for _, a := range sn.apps {
		rep.evaluated = append(rep.evaluated, a.app.ID)
		why, ok := waiting[a.app.Name]
		if !ok {
			continue
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "app-waiting-for-storage", AppID: a.app.ID, AppName: a.app.Name, Severity: "info",
			Title:  a.app.Name + " is waiting for its storage",
			Detail: a.app.Name + " binds a folder that isn't there yet: " + why + ". fog keeps it stopped rather than start it on an empty folder, and starts it as soon as the share mounts.",
			Fix:    "Nothing, if the NAS is on its way up. Otherwise check the share under Settings › Storage › Disks & shares.",
			FixURL: "/settings/storage/disks",
		})
	}
	return rep
}
