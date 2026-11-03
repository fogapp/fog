package apps

import (
	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/store"
)

// Deploy logs are read through these, never straight from the store: a log
// written before fog redacted at the source (internal/build/redact.go) can
// carry a build-arg value or a repository credential, and every reader —
// the app page, the deploy status poll, MCP's fog_app_get, a feedback
// report — gets it redacted with the values every revision of the app ever
// named.

// deployLogSecrets are the build-arg values and repository credentials any
// revision of the app has named.
func (e *Engine) deployLogSecrets(appID int64) []string {
	revs, err := e.Store.ListRevisionSpecs(appID, 10000)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range revs {
		spec, err := ParseSpec(r.Spec)
		if err != nil {
			continue
		}
		out = append(out, build.LogSecrets(spec.Source.BuildArgs, spec.Source.Repo)...)
	}
	return out
}

// DeploySummaries is store.ListDeploySummaries with every log redacted.
func (e *Engine) DeploySummaries(appID int64, limit, tailBytes int) ([]store.Deploy, error) {
	ds, err := e.Store.ListDeploySummaries(appID, limit, tailBytes)
	if err != nil || len(ds) == 0 {
		return ds, err
	}
	secrets := e.deployLogSecrets(appID)
	for i := range ds {
		ds[i].Log = build.RedactLog(ds[i].Log, secrets)
	}
	return ds, nil
}

// DeployLog is one deploy's whole log, redacted.
func (e *Engine) DeployLog(appID, deployID int64) (string, error) {
	log, err := e.Store.DeployLog(deployID)
	if err != nil {
		return "", err
	}
	return build.RedactLog(log, e.deployLogSecrets(appID)), nil
}
