package oidc

// Discovery is the provider's metadata (OIDC Discovery 1.0 §3, RFC 8414),
// describing exactly the fog-signin/1 profile — a client reading it learns what fog
// does, not what OIDC permits. claims_supported is exactly what IDToken and
// UserInfo can issue (TestDiscoveryClaimsAreWhatIsIssued holds it there).
func Discovery(issuer string) map[string]any {
	return map[string]any{
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + "/oauth2/authorize",
		"pushed_authorization_request_endpoint":          issuer + "/oauth2/par",
		"token_endpoint":                                 issuer + "/oauth2/token",
		"userinfo_endpoint":                              issuer + "/oauth2/userinfo",
		"jwks_uri":                                       issuer + "/oauth2/jwks",
		"end_session_endpoint":                           issuer + "/oauth2/end_session",
		"revocation_endpoint":                            issuer + "/oauth2/revoke",
		"device_authorization_endpoint":                  issuer + "/oauth2/device_authorization",
		"scopes_supported":                               knownScopes,
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", DeviceGrantType},
		"subject_types_supported":                        []string{"public"},
		"id_token_signing_alg_values_supported":          []string{"ES256"},
		"token_endpoint_auth_methods_supported":          []string{"client_secret_basic", "client_secret_post"},
		"revocation_endpoint_auth_methods_supported":     []string{"client_secret_basic", "client_secret_post"},
		"code_challenge_methods_supported":               []string{"S256"},
		"prompt_values_supported":                        []string{"none", "login"},
		"acr_values_supported":                           []string{ACRAny, ACRPhishingResistant},
		"claims_supported":                               ClaimsSupported,
		"authorization_response_iss_parameter_supported": true,
		"require_pushed_authorization_requests":          false,
		"backchannel_logout_supported":                   true,
		"backchannel_logout_session_supported":           true,
		"request_parameter_supported":                    false,
		"request_uri_parameter_supported":                false,
		"claims_parameter_supported":                     false,
	}
}

// ClaimsSupported is every claim fog may put in an ID token or a userinfo
// answer — and nothing it never sends.
var ClaimsSupported = []string{
	"iss", "sub", "aud", "azp", "exp", "iat", "auth_time", "nonce", "acr", "amr", "sid", "at_hash",
	"preferred_username", "name", "email", "email_verified", "roles", "fog_home", "fog_visitor", ClaimSessionExpires,
}
