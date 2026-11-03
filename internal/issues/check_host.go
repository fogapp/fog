// Host checks: fog's own environment, as persistent, acknowledgeable issues.
// The same probes run behind /api/system for boot-time visibility; these
// are the ones that carry a fix and land in the Inbox.
package issues

import (
	"context"
	"os"
	"path/filepath"

	"gitea.lan/matthew/fog/internal/store"
)

// executable is the test seam for locating the fogd binary.
var executable = os.Executable

func (sc *Scanner) checkHostEnv(_ context.Context, sn *snapshot) report {
	// Both findings share one kind and the host scope, so the sweep can't
	// resolve one without the other: if the binary can't be located the
	// self-update half has no answer, and the whole check skips rather
	// than resolving it on missing evidence.
	exe, err := executable()
	if err != nil {
		return report{}
	}
	rep := report{evaluated: []int64{0}}
	if !dirWritable(sn.dataDir) {
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "host-env", Subject: "datadir", Severity: "critical",
			Title:  "Fog's data directory is not writable",
			Detail: sn.dataDir + " is not writable by the fogd process — nothing can be saved or deployed.",
			Fix:    "Fix ownership or permissions on the data directory for the fogd service user.",
		})
	}
	if dir := filepath.Dir(exe); !dirWritable(dir) {
		rep.findings = append(rep.findings, store.IssueFinding{
			Kind: "host-env", Subject: "selfupdate", Severity: "warning",
			Title:  "Self-update can't write next to the fogd binary",
			Detail: dir + " is read-only for the service, so self-update cannot swap the binary.",
			Fix:    "printf '[Service]\\nReadWritePaths=" + dir + "\\n' | sudo tee /etc/systemd/system/fogd.service.d/override.conf && sudo systemctl daemon-reload && sudo systemctl restart fogd",
		})
	}
	return rep
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".fog-writecheck-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// checkDockerMissing: no daemon means no apps, full stop. It used to be a
// banner painted on the dashboard; as an issue it carries the fix link,
// survives a page change, and resolves itself the sweep after an install.
func (sc *Scanner) checkDockerMissing(ctx context.Context, _ *snapshot) report {
	if sc.DockerAvailable == nil {
		return report{}
	}
	rep := report{evaluated: []int64{0}}
	if sc.DockerAvailable(ctx) {
		return rep
	}
	rep.findings = append(rep.findings, store.IssueFinding{
		Kind: "docker-missing", Subject: "docker", Severity: "critical",
		Title:  "Docker isn't available",
		Detail: "Every app is a container. Until Docker is installed and running, nothing can be deployed or started.",
		Fix:    "Install Docker from Settings → Server → Docker, or start the daemon if it is installed and stopped.",
		FixURL: "/settings/server/docker",
	})
	return rep
}
