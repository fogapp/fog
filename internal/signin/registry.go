// Package signin provisions apps as clients of fog's OpenID provider
// (docs/signin-contract.md): from what an app declares in its fog.yaml
// to a registered client, a secret in the vault, and the environment the
// app's fog-kit reads. Nothing is typed by anyone.
package signin

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// Contract is the app-side contract name.
const Contract = "fog-signin/1"

// secretName is the client secret's vault name, app-scoped.
const secretName = "_oidc_client_secret"

// Manifest is the signin block of an app's fog.yaml.
type Manifest struct {
	Contract string            `yaml:"contract" json:"contract"`
	Redirect string            `yaml:"redirect" json:"redirect"`
	Logout   string            `yaml:"logout" json:"logout,omitempty"`
	SCIM     string            `yaml:"scim" json:"scim,omitempty"`
	Roles    []string          `yaml:"roles" json:"roles,omitempty"`
	Default  map[string]string `yaml:"default" json:"default,omitempty"`
	ACR      string            `yaml:"acr" json:"acr,omitempty"` // "phr" or ""
	Email    bool              `yaml:"email" json:"email,omitempty"`
	// Redirects are further redirect URIs: a path on the app's host, or an
	// absolute URI for a native app's custom scheme (Immich's
	// app.immich:///oauth-callback). Matched exactly, like Redirect.
	Redirects []string `yaml:"redirects" json:"redirects,omitempty"`
	// PAR, when explicitly false, lets a client send its authorization
	// request on the front channel: third-party apps whose OIDC library
	// cannot push one (RFC 9126). fog-kit always pushes.
	PAR *bool `yaml:"par" json:"par,omitempty"`
	// Device lets the app sign people in on a screen by code (RFC 8628):
	// Stream on the television.
	Device   bool `yaml:"device" json:"device,omitempty"`
	Visitors *struct {
		Role string `yaml:"role" json:"role"`
	} `yaml:"visitors" json:"visitors,omitempty"`
}

// Declared reports whether the manifest asks for fog sign-in at all.
func (m Manifest) Declared() bool { return strings.TrimSpace(m.Contract) == Contract }

// Validate checks a declared manifest. Problems are the app's to fix, so the
// message names the field.
func (m Manifest) Validate() error {
	if !strings.HasPrefix(m.Redirect, "/") {
		return errors.New("signin.redirect must be a path on the app's host, like /auth/oidc/callback")
	}
	for _, r := range m.Redirects {
		if !strings.HasPrefix(r, "/") && !customScheme(r) {
			return fmt.Errorf("signin.redirects: %q must be a path on the app's host or a native app's custom-scheme URI", r)
		}
	}
	for name, p := range map[string]string{"logout": m.Logout, "scim": m.SCIM} {
		if p != "" && !strings.HasPrefix(p, "/") {
			return fmt.Errorf("signin.%s must be a path on the app's host", name)
		}
	}
	for fogRole, appRole := range m.Default {
		if fogRole != identity.RoleOperator && fogRole != identity.RoleMember {
			return fmt.Errorf("signin.default maps %q, which is not a fog role that signs in (operator, member)", fogRole)
		}
		if len(m.Roles) > 0 && !slices.Contains(m.Roles, appRole) {
			return fmt.Errorf("signin.default maps %s to %q, which is not in signin.roles", fogRole, appRole)
		}
	}
	if m.ACR != "" && m.ACR != "phr" {
		return errors.New(`signin.acr must be "phr" (require a passkey) or absent`)
	}
	if m.Visitors != nil {
		switch {
		case len(m.Roles) == 0:
			return errors.New("signin.visitors needs signin.roles: a visitor's role must be one the app knows")
		case !slices.Contains(m.Roles, m.Visitors.Role):
			return errors.New("signin.visitors.role is not in signin.roles")
		case !identity.VisitorRoleAllowed(m.Roles, m.Visitors.Role, m.Visitors.Role):
			return fmt.Errorf("signin.visitors.role cannot be %q: a visitor is never the app's most privileged role", m.Visitors.Role)
		}
	}
	return nil
}

// Registry registers clients.
type Registry struct {
	Store *store.Store
	Vault *vault.Vault
}

func (r *Registry) secrets() vault.Secrets { return vault.Secrets{Store: r.Store, Vault: r.Vault} }

// Registration is what an app's deploy gets back: the variables to inject.
type Registration struct {
	Env map[string]string
}

// Register makes app a client of the provider at issuer, its URIs on host
// (scheme://host), and returns the environment its fog-kit reads. The
// secret is minted once and kept; re-registering on every deploy changes
// only the URIs and the declared vocabulary.
func (r *Registry) Register(app *store.App, origin, issuer, internalURL string, m Manifest) (*Registration, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("signin: app origin %q is not a URL", origin)
	}
	secrets := r.secrets()
	secret, err := secrets.GetOrMint(app.ID, secretName, func() (string, error) { return randx.Token(), nil })
	if err != nil {
		return nil, err
	}
	c := store.OIDCClient{
		ClientID:       app.Name,
		AppID:          app.ID,
		SecretDigest:   auth.DigestToken(secret),
		RedirectURIs:   redirectURIs(origin, m),
		PostLogoutURIs: []string{origin + "/"},
		Scopes:         append([]string{}, oidc.DefaultScopes...),
		RequirePAR:     m.PAR == nil || *m.PAR,
		DeviceGrant:    m.Device,
		Roles:          m.Roles,
		RoleMap:        m.Default,
	}
	if m.Email {
		c.Scopes = append(c.Scopes, oidc.ScopeEmail)
	}
	if m.Logout != "" {
		c.BackchannelURI = origin + m.Logout
	}
	if m.SCIM != "" {
		c.SCIMBaseURL = origin + m.SCIM
	}
	if m.ACR == "phr" {
		c.RequiredACR = identity.ACRPhishingResistant
	}
	before, err := r.Store.OIDCClient(c.ClientID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	// PutOIDCClient sets the digest only when the row is new: the vault
	// value is the same secret unless someone rotated it, and rotation is
	// its own act.
	if err := r.Store.PutOIDCClient(c); err != nil {
		return nil, err
	}
	visitorRole := ""
	if m.Visitors != nil {
		visitorRole = m.Visitors.Role
	}
	if err := r.Store.SetClientVisitorRole(c.ClientID, visitorRole); err != nil {
		return nil, err
	}
	// What the app now declares may take something from people already
	// signed in to it: a role renamed or remapped, visitors capped lower or
	// no longer hosted. Those sessions end, as any narrowing's do.
	if before != nil {
		if _, err := LogoutReduced(r.Store, app.ID, before, &c); err != nil {
			return nil, err
		}
	}
	if err := r.capVisitorGrants(c.ClientID, c.Roles, visitorRole); err != nil {
		return nil, err
	}
	env := map[string]string{
		"OIDC_ISSUER":        issuer,
		"OIDC_CLIENT_ID":     app.Name,
		"OIDC_CLIENT_SECRET": secret,
	}
	if internalURL != "" {
		env["FOG_OIDC_INTERNAL_URL"] = internalURL
	}
	if m.SCIM != "" {
		// The bearer fog's provisioning presents; the name is the generic
		// one any provisioning client would be configured with.
		tok, err := scimToken(secrets, app.ID)
		if err != nil {
			return nil, err
		}
		env["SCIM_TOKEN"] = tok
	}
	return &Registration{Env: env}, nil
}

// SecretOverlap is how long a replaced client secret is still honoured
// once the app has shown it holds the new one: the old containers of a
// blue-green swap finishing what they started.
const SecretOverlap = 10 * time.Minute

// Rotate mints a new secret for an app's client, for its next deploy to
// carry. The running app's secret keeps working until the app first
// authenticates with the new one (Promote), and SecretOverlap after; a
// second rotation before a deploy replaces only the waiting secret, so the
// running app is never locked out.
func (r *Registry) Rotate(app *store.App) error {
	secret, digest := auth.NewToken()
	if err := r.secrets().Put(app.ID, secretName, secret); err != nil {
		return err
	}
	return r.Store.StageOIDCClientSecret(app.Name, digest)
}

// RevokeNow is the rotation for a leaked secret: a new one works from this
// moment and no other does, and whatever was minted for the client under
// the old — requests in flight, codes, access tokens — is gone. The app
// cannot sign anyone in until a deploy hands it the new secret; the caller
// redeploys it.
func (r *Registry) RevokeNow(app *store.App) error {
	secret, digest := auth.NewToken()
	if err := r.secrets().Put(app.ID, secretName, secret); err != nil {
		return err
	}
	return r.Store.ReplaceOIDCClientSecret(app.Name, digest)
}

// Authenticate checks a presented client secret against what the client
// may use now: the running secret, the waiting one (whose first use
// promotes it, retiring the running one after SecretOverlap), or the one a
// promotion replaced, until its overlap ends. Each comparison is of
// digests, in constant time.
func (r *Registry) Authenticate(c *store.OIDCClient, secret string, now time.Time) (bool, error) {
	d := []byte(auth.DigestToken(secret))
	eq := func(digest string) bool {
		return digest != "" && subtle.ConstantTimeCompare(d, []byte(digest)) == 1
	}
	cur, next := eq(c.SecretDigest), eq(c.NextSecretDigest)
	prev := eq(c.PrevSecretDigest) && now.Before(c.PrevSecretUntil)
	if next {
		if err := r.Store.PromoteOIDCClientSecret(c.ClientID, c.NextSecretDigest, SecretOverlap); err != nil {
			return false, err
		}
	}
	return cur || next || prev, nil
}

// capVisitorGrants withdraws visitor grants the app's declaration no longer
// allows — visitors no longer hosted, or a grant above the new cap — so the
// visitors holding them are signed out (PutVisitorGrants), not merely
// refused their next sign-in.
func (r *Registry) capVisitorGrants(clientID string, roles []string, visitorRole string) error {
	grants, err := r.Store.VisitorGrants(clientID)
	if err != nil {
		return err
	}
	kept := []store.VisitorGrant{}
	for _, g := range grants {
		if visitorRole != "" && identity.VisitorRoleAllowed(roles, visitorRole, g.Role) {
			kept = append(kept, g)
		}
	}
	if len(kept) == len(grants) {
		return nil
	}
	return r.Store.PutVisitorGrants(clientID, kept)
}

// Unregister removes an app's client and the credentials minted for it:
// the app stopped declaring the contract.
func (r *Registry) Unregister(app *store.App) error {
	if err := r.Store.DeleteOIDCClient(app.Name); err != nil {
		return err
	}
	if err := r.Store.DeleteSecret(app.ID, secretName); err != nil {
		return err
	}
	return r.Store.DeleteSecret(app.ID, scimSecretName)
}

func redirectURIs(origin string, m Manifest) []string {
	out := []string{origin + m.Redirect}
	for _, r := range m.Redirects {
		if strings.HasPrefix(r, "/") {
			r = origin + r
		}
		out = append(out, r)
	}
	return out
}

// customScheme accepts a native app's redirect: scheme:/… with a scheme
// that is not http(s) — never another web origin.
func customScheme(r string) bool {
	u, err := url.Parse(r)
	return err == nil && u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" && strings.Contains(u.Scheme, ".")
}
