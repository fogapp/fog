// The fog API token — the per-app shared secret behind the fog-backup/1
// contract (docs/backup-contract.md). Fog injects it as FOG_API_TOKEN; the
// app authenticates fog's /fog/* calls with it. Same shape as the webhook
// token, pointed the other way: fog → app instead of git host → fog.
package apps

import (
	"context"
	"fmt"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// EnsureFogAPIToken returns the app's fog API token, generating and storing
// it on first use; concurrent first calls can't mint two tokens. The digest
// row is ensured on every call — it's what lets the token authenticate the app to the
// /app/v1 surface, and tokens minted before that surface existed gain their
// row on the app's next deploy.
func (e *Engine) EnsureFogAPIToken(appID int64) (string, error) {
	tok, err := e.ensureTokenSecret(appID, SecretFogAPIToken)
	if err != nil {
		return "", err
	}
	if err := e.Store.SetAppAPITokenDigest(appID, auth.DigestToken(tok)); err != nil {
		return "", err
	}
	return tok, nil
}

// ensureErrorsKey returns the app's Flare ingest key — the "public key" in
// its FLARE_DSN. Deliberately a separate secret from _fog_api_token: DSN
// keys are semi-public by design (a browser SDK ships one to every visitor,
// and it lands in every stack-trace-reporting config), while the fog API
// token authorizes delegated backups. This key authorizes exactly one thing:
// reporting errors for this one app.
func (e *Engine) ensureErrorsKey(appID int64) (string, error) {
	return e.ensureTokenSecret(appID, SecretErrorsKey)
}

// ensureTokenSecret returns the app's random hex token under key, minting
// it on first use. GetOrMint serialises mints, so concurrent first calls
// can't store two different tokens.
func (e *Engine) ensureTokenSecret(appID int64, key string) (string, error) {
	return e.secrets().GetOrMint(appID, key, func() (string, error) { return randx.Hex(24), nil })
}

// AppBaseURL resolves the running main container's address on its own
// network — the same route the health prober takes — for fog→app API calls.
func (e *Engine) AppBaseURL(ctx context.Context, name string, spec *AppSpec) (string, error) {
	port := spec.HealthPort()
	if port == 0 {
		return "", fmt.Errorf("%s has no health or route port to reach it on", name)
	}
	cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name)
	if err != nil {
		return "", err
	}
	return baseURLIn(dockerx.MainContainers(cs), name, port)
}

// baseURLIn is AppBaseURL over a container listing already in hand.
func baseURLIn(mains map[string]dockerx.ContainerSummary, name string, port int) (string, error) {
	if ip := mainIPIn(mains, name); ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, port), nil
	}
	return "", fmt.Errorf("%s has no running container fog can reach", name)
}

// AppEndpoint is how fog reaches one app's /fog/* contract surface: its
// base URL on the fog network and the FOG_API_TOKEN it checks. Err says why
// the app can't be reached right now.
type AppEndpoint struct {
	Base, Token string
	Err         error
}

// AppEndpoints resolves several apps at once, in list's order, from one
// spec query and one container listing: a fan-out to every contracted app
// asks Docker once rather than once per app.
func (e *Engine) AppEndpoints(ctx context.Context, list []store.App) []AppEndpoint {
	out := make([]AppEndpoint, len(list))
	fail := func(err error) []AppEndpoint {
		for i := range out {
			out[i].Err = err
		}
		return out
	}
	if len(list) == 0 {
		return out
	}
	specs, err := LatestSpecs(e.Store)
	if err != nil {
		return fail(err)
	}
	// One app asks Docker about that app alone, as AppBaseURL does.
	label := "fog.app"
	if len(list) == 1 {
		label += "=" + list[0].Name
	}
	cs, err := e.Docker.ListByLabel(ctx, label)
	if err != nil {
		return fail(err)
	}
	mains := dockerx.MainContainers(cs)
	for i, a := range list {
		spec := specs[a.ID]
		if spec == nil {
			out[i].Err = fmt.Errorf("%s has no revision to reach it by", a.Name)
			continue
		}
		port := spec.HealthPort()
		if port == 0 {
			out[i].Err = fmt.Errorf("%s has no health or route port to reach it on", a.Name)
			continue
		}
		if out[i].Base, out[i].Err = baseURLIn(mains, a.Name, port); out[i].Err != nil {
			continue
		}
		out[i].Token, out[i].Err = e.EnsureFogAPIToken(a.ID)
	}
	return out
}
