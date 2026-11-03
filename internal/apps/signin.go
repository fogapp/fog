package apps

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// registerSignin makes an app that declares fog-signin/1 a client of fog's
// provider and puts its configuration in env. The declaration is the spec's
// signin block (image apps: the catalog's Paperless), or else, for a git
// app, its fog.yaml read from the checkout this deploy just built. An app
// that stopped declaring it is unregistered.
//
// Env values that name {{oidc.issuer}}, {{oidc.client_id}} or
// {{oidc.client_secret}} are filled in here — how a third-party image that
// reads its OIDC settings in its own format gets them.
func (e *Engine) registerSignin(app *store.App, spec *AppSpec, fqdn, domain string, env map[string]string, log io.Writer) error {
	var m signin.Manifest
	switch {
	case spec.Signin != nil:
		m = *spec.Signin
	case spec.Source.Type == "git":
		m = e.Builder.ReadRepoManifest(app.Name).Signin
	}
	if !m.Declared() {
		dropOIDCTemplates(env, log)
		if _, err := e.Store.OIDCClientForApp(app.ID); err == nil {
			fmt.Fprintln(log, "the app no longer declares fog-signin/1; removing its sign-in client")
			return e.Signin.Unregister(app)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	}
	if domain == "" || fqdn == "" {
		// The app keeps its own accounts; the Sign-in tab says why.
		fmt.Fprintln(log, "fog-signin/1 declared, but fog has no domain yet: the app keeps its own sign-in")
		dropOIDCTemplates(env, log)
		return nil
	}
	internal := ""
	if e.FogHostAddr != "" {
		internal = "http://" + e.FogHostAddr
	}
	issuer := "https://fog." + domain
	reg, err := e.Signin.Register(app, "https://"+fqdn, issuer, internal, m)
	if err != nil {
		return fmt.Errorf("fog-signin/1: %w", err)
	}
	for k, v := range reg.Env {
		env[k] = v
	}
	r := strings.NewReplacer("{{oidc.issuer}}", issuer, "{{oidc.client_id}}", reg.Env["OIDC_CLIENT_ID"],
		"{{oidc.client_secret}}", reg.Env["OIDC_CLIENT_SECRET"])
	for k, v := range env {
		if strings.Contains(v, "{{oidc.") {
			env[k] = r.Replace(v)
		}
	}
	fmt.Fprintf(log, "registered with fog sign-in as client %q\n", app.Name)
	return nil
}

// dropOIDCTemplates removes settings that only make sense with a client:
// an image handed a literal "{{oidc.client_id}}" would try to use it.
func dropOIDCTemplates(env map[string]string, log io.Writer) {
	for k, v := range env {
		if strings.Contains(v, "{{oidc.") {
			delete(env, k)
			fmt.Fprintf(log, "skipping %s: the app has no sign-in client yet\n", k)
		}
	}
}
