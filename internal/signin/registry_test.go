package signin

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

func testRegistry(t *testing.T) (*Registry, *store.App) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp("hook")
	if err != nil {
		t.Fatal(err)
	}
	return &Registry{Store: st, Vault: vlt}, app
}

const manifestYAML = `
contract: fog-signin/1
redirect: /auth/oidc/callback
logout: /auth/oidc/backchannel
scim: /scim/v2
roles: [admin, member, guest]
default:
  operator: admin
  member: member
acr: phr
`

func TestRegisterFromManifest(t *testing.T) {
	r, app := testRegistry(t)
	var m Manifest
	if err := yaml.Unmarshal([]byte(manifestYAML), &m); err != nil {
		t.Fatal(err)
	}
	reg, err := r.Register(app, "https://hook.home.test", "https://fog.home.test", "http://host.docker.internal:8080", m)
	if err != nil {
		t.Fatal(err)
	}
	env := reg.Env
	if env["OIDC_ISSUER"] != "https://fog.home.test" || env["OIDC_CLIENT_ID"] != "hook" || env["OIDC_CLIENT_SECRET"] == "" ||
		env["FOG_OIDC_INTERNAL_URL"] != "http://host.docker.internal:8080" {
		t.Fatalf("env = %v", env)
	}
	c, err := r.Store.OIDCClient("hook")
	if err != nil {
		t.Fatal(err)
	}
	if c.SecretDigest != auth.DigestToken(env["OIDC_CLIENT_SECRET"]) {
		t.Fatal("the stored digest is not the injected secret's")
	}
	if c.RedirectURIs[0] != "https://hook.home.test/auth/oidc/callback" || c.BackchannelURI != "https://hook.home.test/auth/oidc/backchannel" ||
		c.SCIMBaseURL != "https://hook.home.test/scim/v2" || !c.RequirePAR || c.RequiredACR != "urn:fog:acr:phr" || c.RoleMap["operator"] != "admin" {
		t.Fatalf("client = %+v", c)
	}

	// Redeploying keeps the secret; a moved host moves the URIs.
	again, err := r.Register(app, "https://books.home.test", "https://fog.home.test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	if again.Env["OIDC_CLIENT_SECRET"] != env["OIDC_CLIENT_SECRET"] {
		t.Fatal("a redeploy changed the client secret")
	}
	c, _ = r.Store.OIDCClient("hook")
	if c.RedirectURIs[0] != "https://books.home.test/auth/oidc/callback" {
		t.Fatalf("redirect after a host move = %v", c.RedirectURIs)
	}
}

// TestRotationEndsTheOldSecretOnceTheAppHasTheNew is the rotation finding:
// the old secret used to live until the next rotation, so a leak took two
// rotations — and two before a deploy locked the running app out. Now a
// rotation never touches what the app runs with, and the old secret ends
// SecretOverlap after the app first uses the new one.
func TestRotationEndsTheOldSecretOnceTheAppHasTheNew(t *testing.T) {
	r, app := testRegistry(t)
	m := Manifest{Contract: Contract, Redirect: "/cb"}
	reg, err := r.Register(app, "https://hook.home.test", "https://fog.home.test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	running := reg.Env["OIDC_CLIENT_SECRET"]
	accepts := func(secret string, at time.Time) bool {
		t.Helper()
		c, err := r.Store.OIDCClient("hook")
		if err != nil {
			t.Fatal(err)
		}
		ok, err := r.Authenticate(c, secret, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	now := time.Now()

	// Two rotations before any deploy: the running app is not locked out.
	if err := r.Rotate(app); err != nil {
		t.Fatal(err)
	}
	if err := r.Rotate(app); err != nil {
		t.Fatal(err)
	}
	if !accepts(running, now) {
		t.Fatal("two rotations before a deploy locked the running app out")
	}

	// The deploy carries the newest secret.
	next, err := r.Register(app, "https://hook.home.test", "https://fog.home.test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	fresh := next.Env["OIDC_CLIENT_SECRET"]
	if fresh == running {
		t.Fatal("the deploy did not carry the rotated secret")
	}
	if !accepts(running, now) {
		t.Fatal("the old secret stopped before the app used the new one")
	}
	// The app's first use of the new secret makes it the one; the old is
	// honoured only for the overlap of a blue-green swap.
	if !accepts(fresh, now) {
		t.Fatal("the new secret was refused")
	}
	if !accepts(running, now.Add(SecretOverlap/2)) {
		t.Fatal("the old secret ended inside the swap's overlap")
	}
	if accepts(running, now.Add(SecretOverlap+time.Second)) {
		t.Fatal("the old secret outlived the app's switch to the new one")
	}
	if !accepts(fresh, now.Add(time.Hour)) {
		t.Fatal("the new secret stopped working")
	}

	if err := r.Unregister(app); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.OIDCClient("hook"); err == nil {
		t.Fatal("unregistered client still there")
	}
}

// TestRevokeNowEndsTheLeakedSecretAtOnce: after a leak, one act ends the old
// secret — and the waiting one — immediately, along with access tokens
// minted for the client.
func TestRevokeNowEndsTheLeakedSecretAtOnce(t *testing.T) {
	r, app := testRegistry(t)
	m := Manifest{Contract: Contract, Redirect: "/cb"}
	reg, err := r.Register(app, "https://hook.home.test", "https://fog.home.test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	leaked := reg.Env["OIDC_CLIENT_SECRET"]
	if err := r.Rotate(app); err != nil {
		t.Fatal(err)
	}
	staged, _ := r.secrets().Get(app.ID, secretName)
	if err := r.RevokeNow(app); err != nil {
		t.Fatal(err)
	}
	c, _ := r.Store.OIDCClient("hook")
	for name, secret := range map[string]string{"leaked": leaked, "staged": staged} {
		if ok, _ := r.Authenticate(c, secret, time.Now()); ok {
			t.Errorf("the %s secret still works after revoking now", name)
		}
	}
	again, err := r.Register(app, "https://hook.home.test", "https://fog.home.test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.Authenticate(c, again.Env["OIDC_CLIENT_SECRET"], time.Now()); !ok {
		t.Fatal("the redeploy's secret is refused")
	}
}

// The SCIM bearer the app is handed is the one the provisioner presents,
// and unregistering takes it with the client instead of orphaning it.
func TestSCIMTokenSharedAndRemovedOnUnregister(t *testing.T) {
	r, app := testRegistry(t)
	m := Manifest{Contract: Contract, Redirect: "/cb", SCIM: "/scim/v2"}
	reg, err := r.Register(app, "https://hook.home.test", "https://fog.home.test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provisioner{Store: r.Store, Vault: r.Vault}
	tok, err := p.Token(app)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Env["SCIM_TOKEN"] == "" || reg.Env["SCIM_TOKEN"] != tok {
		t.Fatalf("SCIM_TOKEN %q is not the provisioner's %q", reg.Env["SCIM_TOKEN"], tok)
	}
	if err := r.Unregister(app); err != nil {
		t.Fatal(err)
	}
	keys, err := r.Store.SecretKeys(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("unregister left secrets behind: %v", keys)
	}
	again, err := r.Register(app, "https://hook.home.test", "https://fog.home.test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	if again.Env["SCIM_TOKEN"] == tok {
		t.Fatal("re-registering reused the old SCIM token")
	}
}

// A provisioning pass reads the instance name once, however many apps take
// provisioning.
func TestProvisionerLoadsSharedStateOncePerPass(t *testing.T) {
	r, app := testRegistry(t)
	other, err := r.Store.CreateApp("books")
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Contract: Contract, Redirect: "/cb", SCIM: "/scim/v2"}
	for _, a := range []*store.App{app, other} {
		if _, err := r.Register(a, "https://"+a.Name+".home.test", "https://fog.home.test", "", m); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	p := &Provisioner{Store: r.Store, Vault: r.Vault, Log: slog.Default(),
		Instance: func() (string, error) { calls++; return "fog.home.test", nil }}
	p.Run(context.Background())
	if calls != 1 {
		t.Fatalf("Instance read %d times for two clients, want 1", calls)
	}
}

func TestManifestValidation(t *testing.T) {
	for name, c := range map[string]struct {
		m    Manifest
		want string
	}{
		"relative redirect":  {Manifest{Contract: Contract, Redirect: "https://evil.test/cb"}, "signin.redirect"},
		"guest mapped":       {Manifest{Contract: Contract, Redirect: "/cb", Default: map[string]string{"guest": "admin"}}, "not a fog role"},
		"role outside roles": {Manifest{Contract: Contract, Redirect: "/cb", Roles: []string{"admin"}, Default: map[string]string{"member": "owner"}}, "not in signin.roles"},
		"unknown acr":        {Manifest{Contract: Contract, Redirect: "/cb", ACR: "mfa"}, "signin.acr"},
		"absolute logout":    {Manifest{Contract: Contract, Redirect: "/cb", Logout: "http://x/"}, "signin.logout"},
	} {
		err := c.m.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !(Manifest{Contract: " fog-signin/1 "}).Declared() || (Manifest{Contract: "fog-signin/2"}).Declared() {
		t.Error("Declared")
	}
}

func TestRedirectsAndNativeSchemes(t *testing.T) {
	r, app := testRegistry(t)
	no := false
	m := Manifest{Contract: Contract, Redirect: "/auth/login", Redirects: []string{"/user-settings", "app.immich:///oauth-callback"}, PAR: &no}
	if _, err := r.Register(app, "https://photos.home.test", "https://fog.home.test", "", m); err != nil {
		t.Fatal(err)
	}
	c, _ := r.Store.OIDCClient("hook")
	want := []string{"https://photos.home.test/auth/login", "https://photos.home.test/user-settings", "app.immich:///oauth-callback"}
	if strings.Join(c.RedirectURIs, " ") != strings.Join(want, " ") || c.RequirePAR {
		t.Fatalf("client = %+v", c)
	}
	for _, bad := range []string{"https://evil.test/cb", "http://x/cb", "javascript:alert(1)", "myapp:/cb"} {
		if err := (Manifest{Contract: Contract, Redirect: "/cb", Redirects: []string{bad}}).Validate(); err == nil {
			t.Errorf("accepted redirect %q", bad)
		}
	}
}
