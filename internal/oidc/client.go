package oidc

import (
	"slices"
	"strings"
)

// Client is a registered relying party: always an app server holding a
// secret (§2.4 — fog has no public clients).
type Client struct {
	ID    string
	AppID int64
	// RedirectURIs and PostLogoutRedirectURIs are matched exactly, byte for
	// byte (RFC 9700 §4.1.1): no wildcards, no prefix, no case folding.
	RedirectURIs           []string
	PostLogoutRedirectURIs []string
	// RequirePAR refuses authorization requests that did not arrive through
	// the pushed authorization endpoint (RFC 9126).
	RequirePAR bool
	// Scopes the client may ask for beyond openid.
	Scopes []string
	// ACR is the assurance the app always requires.
	ACR string
	// Roles and RoleMap are the app's vocabulary and fog role → app role.
	Roles   []string
	RoleMap map[string]string
}

// AllowsRedirect reports whether uri is exactly one the client registered.
func (c *Client) AllowsRedirect(uri string) bool { return slices.Contains(c.RedirectURIs, uri) }

// AllowsPostLogoutRedirect reports whether uri is exactly a registered
// post-logout destination.
func (c *Client) AllowsPostLogoutRedirect(uri string) bool {
	return slices.Contains(c.PostLogoutRedirectURIs, uri)
}

// Scopes fog knows. Any other requested scope is refused, not ignored, so a
// client's mistake surfaces at integration rather than as a missing claim.
const (
	ScopeOpenID  = "openid"
	ScopeProfile = "profile"
	ScopeEmail   = "email"
	ScopeRoles   = "roles"
)

var knownScopes = []string{ScopeOpenID, ScopeProfile, ScopeEmail, ScopeRoles}

// DefaultScopes is what a fog-signin/1 client is registered for.
var DefaultScopes = []string{ScopeProfile, ScopeRoles}

// ParseScope validates a requested scope for c: known, registered, and
// including openid.
func ParseScope(raw string, c *Client) ([]string, *Error) {
	var out []string
	for _, s := range strings.Fields(raw) {
		if slices.Contains(out, s) {
			continue
		}
		if !slices.Contains(knownScopes, s) || (s != ScopeOpenID && !slices.Contains(c.Scopes, s)) {
			return nil, errorf(ErrInvalidScope, 400, true, "scope %q is not available to this client", s)
		}
		out = append(out, s)
	}
	if !slices.Contains(out, ScopeOpenID) {
		return nil, errorf(ErrInvalidScope, 400, true, "scope must include openid")
	}
	slices.Sort(out)
	return out, nil
}
