package apps

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// An image app that declares fog sign-in in its spec gets a client, and its
// {{oidc.*}} env values are filled in; without a domain they are left out
// rather than handed to the image as literal placeholders.
func TestRegisterSigninFillsOIDCTemplates(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := st.CreateApp("paperless")
	e := &Engine{Store: st, Signin: &signin.Registry{Store: st, Vault: vlt}}
	no := false
	spec := &AppSpec{Source: Source{Type: "image", Image: "x"},
		Signin: &signin.Manifest{Contract: signin.Contract, Redirect: "/accounts/oidc/fog/login/callback/", PAR: &no}}
	tmpl := `{"client_id":"{{oidc.client_id}}","secret":"{{oidc.client_secret}}","server_url":"{{oidc.issuer}}"}`

	env := map[string]string{"PROVIDERS": tmpl, "OTHER": "kept"}
	if err := e.registerSignin(app, spec, "", "", env, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, ok := env["PROVIDERS"]; ok || env["OTHER"] != "kept" {
		t.Fatalf("without a domain: %v", env)
	}

	env = map[string]string{"PROVIDERS": tmpl}
	if err := e.registerSignin(app, spec, "paperless.home.test", "home.test", env, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := env["PROVIDERS"]
	if strings.Contains(got, "{{") || !strings.Contains(got, `"server_url":"https://fog.home.test"`) ||
		!strings.Contains(got, `"client_id":"paperless"`) || !strings.Contains(got, env["OIDC_CLIENT_SECRET"]) {
		t.Fatalf("filled = %s", got)
	}
	c, err := st.OIDCClient("paperless")
	if err != nil || c.RequirePAR || c.RedirectURIs[0] != "https://paperless.home.test/accounts/oidc/fog/login/callback/" {
		t.Fatalf("client = %+v (%v)", c, err)
	}
}
