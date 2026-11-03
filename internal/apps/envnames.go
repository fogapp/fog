package apps

import (
	"fmt"
	"strings"
)

// Names an app's environment and vault must keep for fog, or must not take
// from another app.
//
// "_"-prefixed secrets are fog's own about an app: its FOG_API_TOKEN
// (_fog_api_token), its deploy key, its webhook token. A provider whose
// integration offer named one could have fog store its minted value there —
// and then know the consumer's token, and be it. The variables fog sets at
// deploy are fog's too.
//
// An offer is another app's say in this one's environment, so it is held
// to more: no name that changes what the consumer's runtime loads or where
// it connects — a preload, an interpreter's options, a proxy, PATH.

// fogSetEnv are the variables fog sets in every app at deploy.
var fogSetEnv = map[string]bool{
	"FOG_API_TOKEN": true, "FOG_API_URL": true, "FOG_INFERENCE_URL": true,
	"FOG_INFERENCE_KEY": true, "FLARE_DSN": true,
}

// injectableEnv are names a provider may never set in a consumer (compared
// case-insensitively: http_proxy is read as often as HTTP_PROXY).
var injectableEnv = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
	"NODE_OPTIONS": true, "NODE_PATH": true, "NODE_EXTRA_CA_CERTS": true,
	"PYTHONPATH": true, "PYTHONSTARTUP": true, "PYTHONHOME": true, "PYTHONINSPECT": true,
	"RUBYOPT": true, "RUBYLIB": true, "PERL5OPT": true, "PERL5LIB": true,
	"JAVA_TOOL_OPTIONS": true, "JDK_JAVA_OPTIONS": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true, "FTP_PROXY": true,
	"PATH": true, "BASH_ENV": true, "ENV": true, "SHELLOPTS": true, "PS4": true, "IFS": true,
	"GCONV_PATH": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"CURL_CA_BUNDLE": true, "REQUESTS_CA_BUNDLE": true, "GIT_SSH_COMMAND": true,
}

// checkToolName refuses a name docker compose or the docker CLI would read
// as its own setting. fog no longer hands an app's environment to compose
// (ComposeCmd), but a name that once ran a file of the app's choosing has no
// business in an app's environment at all.
func checkToolName(name string) error {
	u := strings.ToUpper(name)
	if strings.HasPrefix(u, "COMPOSE_") || strings.HasPrefix(u, "DOCKER_") {
		return fmt.Errorf("%s: COMPOSE_ and DOCKER_ names are docker's own settings, not an app's", name)
	}
	return nil
}

// checkFogSecretName refuses a secret name of fog's own ("_"-prefixed).
func checkFogSecretName(name string) error {
	if strings.HasPrefix(name, "_") {
		return fmt.Errorf("%s: names starting with _ are fog's own", name)
	}
	return nil
}

// CheckOfferedName refuses a variable or secret name another app may not
// set in a consumer: fog's own, one fog sets, or one that changes what the
// consumer's runtime runs or where it connects.
func CheckOfferedName(name string) error {
	if err := checkFogSecretName(name); err != nil {
		return err
	}
	if err := checkToolName(name); err != nil {
		return err
	}
	switch u := strings.ToUpper(name); {
	case fogSetEnv[u], strings.HasPrefix(u, "OIDC_"):
		return fmt.Errorf("%s is set by fog", name)
	case injectableEnv[u]:
		return fmt.Errorf("%s changes what the app runs or where it connects; another app may not set it", name)
	}
	return nil
}
