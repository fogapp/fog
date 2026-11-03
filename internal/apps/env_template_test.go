package apps

import (
	"io"
	"strings"
	"testing"
)

// A domainless deploy must not bake a half-expanded hostname into the app.
// Manor's MANOR_WEBAUTHN_ORIGINS became "https://" this way, and the app
// honoured it as its allowed-origin list: every passkey ceremony failed
// origin verification, on a hostname the proxy was serving correctly.
func TestRenderEnvTemplatesDropsFqdnVarsWithoutDomain(t *testing.T) {
	env := map[string]string{
		"MANOR_WEBAUTHN_RP_ID":   "{{fqdn}}",
		"MANOR_WEBAUTHN_ORIGINS": "https://{{fqdn}}",
		"GIT_SHA":                "{{commit}}",
		"JOB_CONCURRENCY":        "1",
	}
	var log strings.Builder
	renderEnvTemplates(env, "abc123", "", &log)

	for _, k := range []string{"MANOR_WEBAUTHN_RP_ID", "MANOR_WEBAUTHN_ORIGINS"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s should be dropped without a domain, got %q", k, v)
		}
		if !strings.Contains(log.String(), k) {
			t.Errorf("deploy log should say %s was skipped, got %q", k, log.String())
		}
	}
	if env["GIT_SHA"] != "abc123" || env["JOB_CONCURRENCY"] != "1" {
		t.Errorf("non-hostname vars must be untouched, got %+v", env)
	}
}

func TestRenderEnvTemplatesExpandsWithDomain(t *testing.T) {
	env := map[string]string{
		"MANOR_WEBAUTHN_RP_ID":   "{{fqdn}}",
		"MANOR_WEBAUTHN_ORIGINS": "https://{{fqdn}}",
		"GIT_SHA":                "{{commit}}",
	}
	renderEnvTemplates(env, "abc123", "manor.jen.is", io.Discard)

	want := map[string]string{
		"MANOR_WEBAUTHN_RP_ID":   "manor.jen.is",
		"MANOR_WEBAUTHN_ORIGINS": "https://manor.jen.is",
		"GIT_SHA":                "abc123",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}
