// Compute checks: a machine paired with fog whose service never reached
// it, and one stuck behind fog's release of fog-node.
// Pairing runs from the owner's terminal and succeeds there; the service it
// installs is what has to keep reaching fog, and when it can't, nothing in
// Settings › Compute fails loudly — the machine just never comes online. The
// Inbox says so, and says how to find out why on the machine itself.
package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// computeConnectGrace is how long after pairing a machine's service has to
// first poll before fog says it hasn't: the installer starts it at once,
// and on a Mac the owner may be answering the Local Network prompt.
var computeConnectGrace = 2 * time.Minute

// checkComputeNeverConnected: a machine paired more than computeConnectGrace
// ago whose service has never polled. Host scope, one issue per machine; it
// resolves once the machine connects or is removed.
func (sc *Scanner) checkComputeNeverConnected(_ context.Context, _ *snapshot) report {
	rep := report{}
	nodes, err := sc.Store.ListComputeNodes()
	if err != nil {
		return rep
	}
	rep.evaluated = []int64{0}
	now := time.Now()
	for _, n := range nodes {
		if n.ConnectedAt != "" {
			continue
		}
		paired, err := time.Parse(time.RFC3339, n.CreatedAt)
		if err != nil || now.Sub(paired) < computeConnectGrace {
			continue
		}
		var inv struct {
			OS string `json:"os"`
		}
		_ = json.Unmarshal([]byte(n.Inventory), &inv)
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "compute-never-connected", Subject: n.ID, Severity: "warning",
			Title: fmt.Sprintf("%s was paired but has never connected", n.Name),
			Detail: fmt.Sprintf("%s paired with fog %s, but the fog-node service it installed hasn't reached fog since, so no app can use it. "+
				"Pairing ran from a terminal on %s; the service runs on its own, and something is stopping it — its log on %s says what.",
				n.Name, paired.Local().Format("Jan 2 at 15:04"), n.Name, n.Name),
			Fix:    computeConnectFix(n.Name, inv.OS),
			FixURL: "/settings/server/compute",
		})
	}
	return rep
}

// computeConnectFix is how to read fog-node's log on the machine, and what
// each thing it may say means.
func computeConnectFix(name, os string) string {
	log := computeLog(os)
	var service, start string
	switch os {
	case "darwin":
		service = "launchctl print gui/$(id -u)/lan.fog.node"
		start = "~/Library/Application\\ Support/fog-node/bin/fog-node install"
	default:
		service = "systemctl status fog-node   (or systemctl --user status fog-node)"
		start = "fog-node install"
	}
	lines := []string{
		fmt.Sprintf("On %s, read fog-node's log: %s", name, log),
		"• \"no route to host\": macOS hasn't allowed fog-node on the local network. Turn it on in System Settings › Privacy & Security › Local Network; fog-node picks it up by itself within a minute.",
		"• \"connection refused\", \"timeout\" or \"no such host\": the machine can't reach the address it paired with (fog-node status shows it). Use an address of fog's that works from that machine, and pair again.",
		"• \"fog no longer knows this machine\": it was removed here. Pair it again with a new code.",
		fmt.Sprintf("• No log, or nothing new in it: the service isn't running. Check with %s, and start it with %s.", service, start),
	}
	if os != "darwin" {
		lines = append(lines[:1], lines[2:]...)
	}
	return strings.Join(lines, "\n")
}

// computeLog is how to follow fog-node's log on a machine of that OS.
func computeLog(os string) string {
	if os == "darwin" {
		return "tail -f ~/Library/Application\\ Support/fog-node/fog-node.log"
	}
	return "journalctl -u fog-node -f   (or journalctl --user -u fog-node -f, if it was installed without root)"
}

// checkComputeOutOfDate: a machine online but stuck on another fog-node
// than fog's release — its update failed, or it hasn't taken it in
// compute.UpdateGrace. fog updates machines itself, so one left behind is
// one nobody would otherwise think to go and look at. Host scope, one
// issue per machine; it resolves once the machine runs fog's release, or
// goes offline (which Settings › Compute already says).
func (sc *Scanner) checkComputeOutOfDate(_ context.Context, _ *snapshot) report {
	rep := report{}
	if sc.ComputeStuck == nil {
		return rep
	}
	rep.evaluated = []int64{0}
	for _, m := range sc.ComputeStuck() {
		b := m.Behind
		why := fmt.Sprintf("fog offered it %s and it hasn't updated, or said why (a fog-node before v0.14.14 doesn't).", b.Want)
		if b.Error != "" {
			why = fmt.Sprintf("Updating to %s failed: %s.", b.Want, strings.TrimSuffix(b.Error, "."))
		}
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "compute-out-of-date", Subject: m.ID, Severity: "warning",
			Title: fmt.Sprintf("%s runs fog-node %s, not fog's %s", m.Name, m.Inventory.Version, b.Want),
			Detail: fmt.Sprintf("fog keeps the machines it borrows on its own release of fog-node, and %s is behind. %s "+
				"Until it updates, it runs work with the older fog-node, which may not do everything fog asks of it.", m.Name, why),
			Fix:    computeUpdateFix(m.Name, m.Inventory.OS, b.Error),
			FixURL: "/settings/server/compute",
		})
	}
	return rep
}

// computeUpdateFix is what to do about a machine stuck behind, by why.
func computeUpdateFix(name, os, why string) string {
	lines := []string{fmt.Sprintf("Press Update now on %s in Settings › Compute: it tries again at once.", name)}
	switch e := strings.ToLower(why); {
	case strings.Contains(e, "permission denied"), strings.Contains(e, "read-only"), strings.Contains(e, "operation not permitted"):
		lines = append(lines, "• fog-node can't replace its own binary. Make the folder it's installed in writable by the account its service runs as — or remove the machine here and pair it again, which installs it where it can.")
	case strings.HasPrefix(e, "fog couldn't get"):
		lines = append(lines, "• fog itself couldn't fetch that build of fog-node from its release server. Check this fog can reach it (Settings › Server › Updates), then press Update now.")
	case strings.Contains(e, "checksum"), strings.Contains(e, "download"), strings.Contains(e, "eof"), strings.Contains(e, "timeout"):
		lines = append(lines, "• The download was cut short or garbled on the way; Update now fetches it again.")
	case strings.Contains(e, "no binary of its own"):
		lines = append(lines, fmt.Sprintf("• fog-node on %s isn't running as the installed service, so it can't update itself. Run fog-node install on it.", name))
	case strings.Contains(e, "no space"):
		lines = append(lines, fmt.Sprintf("• %s's disk is full; free some space, then press Update now.", name))
	}
	lines = append(lines, fmt.Sprintf("If it's still behind, read fog-node's log on %s, where \"update failed\" says why: %s", name, computeLog(os)))
	return strings.Join(lines, "\n")
}

// SweepCompute runs the compute checks alone, so a machine that never
// connected, or is stuck behind, reaches the Inbox within minutes rather
// than at the next full sweep.
func (sc *Scanner) SweepCompute(ctx context.Context) {
	for kind, check := range map[string]func(context.Context, *snapshot) report{
		"compute-never-connected": sc.checkComputeNeverConnected,
		"compute-out-of-date":     sc.checkComputeOutOfDate,
	} {
		rep := check(ctx, nil)
		if len(rep.evaluated) == 0 {
			continue
		}
		res, err := sc.Store.SyncIssues(kind, rep.evaluated, rep.findings)
		if err != nil {
			sc.Log.Warn("issues: compute sync", "kind", kind, "err", err)
			continue
		}
		sc.report(res)
	}
}
