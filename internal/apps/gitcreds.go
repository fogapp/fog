// Per-app git credentials — deploy keypair and webhook token — plus the
// connection test that tells an operator why a repository is unreachable.
package apps

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// The per-app secrets fog mints for itself. The leading underscore keeps
// them out of the operator's secret list.
const (
	SecretDeployKey    = "_deploy_key"     // the git deploy key's private half
	SecretDeployKeyPub = "_deploy_key_pub" // its public half, pasted into the git host
	SecretWebhookToken = "_webhook_token"  // authenticates the git host's push hook
	SecretFogAPIToken  = "_fog_api_token"  // FOG_API_TOKEN, fog ↔ app
	SecretErrorsKey    = "_errors_key"     // the Flare DSN's ingest key
)

// DeployKey exposes an app's git deploy key to sibling packages (the update
// checker needs it for ls-remote).
func (e *Engine) DeployKey(appID int64) (string, error) {
	return e.appSecret(appID, SecretDeployKey)
}

func (e *Engine) appSecret(appID int64, key string) (string, error) {
	return e.secrets().Get(appID, key)
}

func (e *Engine) secrets() vault.Secrets {
	return vault.Secrets{Store: e.Store, Vault: e.Vault}
}

// ensureGitCredentials generates the app's deploy key and webhook token once.
// The public key (SecretDeployKeyPub) is shown in the UI to paste into the git
// host; the webhook token authenticates push notifications. Serialized by a
// mutex: concurrent first calls (e.g. parallel API reads) must never
// interleave two generations, or the stored private and public halves can
// come from different keypairs. vault.Secrets.GetOrMint can't stand in: it
// mints one value under a lock of its own, and RegenerateDeployKey's two
// deletes must not interleave with a mint either.
func (e *Engine) ensureGitCredentials(appID int64) error {
	e.deployKeyMu.Lock()
	defer e.deployKeyMu.Unlock()
	if _, err := e.Store.GetSecret(appID, SecretDeployKey); err == store.ErrNotFound {
		priv, pub, err := build.GenerateDeployKey()
		if err != nil {
			return err
		}
		for k, v := range map[string]string{SecretDeployKey: priv, SecretDeployKeyPub: pub} {
			if err := e.secrets().Put(appID, k, v); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	_, err := e.ensureTokenSecret(appID, SecretWebhookToken)
	return err
}

// GitInfo returns what the operator needs to wire up their git host.
type GitInfo struct {
	DeployPublicKey string `json:"deployPublicKey"`
	WebhookPath     string `json:"webhookPath"`
}

func (e *Engine) GitInfo(appID int64, appName string) (*GitInfo, error) {
	if err := e.ensureGitCredentials(appID); err != nil {
		return nil, err
	}
	pub, err := e.appSecret(appID, SecretDeployKeyPub)
	if err != nil {
		return nil, err
	}
	tok, err := e.appSecret(appID, SecretWebhookToken)
	if err != nil {
		return nil, err
	}
	return &GitInfo{DeployPublicKey: pub, WebhookPath: "/hooks/" + appName + "/" + tok}, nil
}

// TestResult is the setup flow's connection check: a friendly verdict plus
// the raw error and the presented key's fingerprint, so a failure is
// debuggable without SSH access to the server.
type TestResult struct {
	OK          bool   `json:"ok"`
	Message     string `json:"message"`
	Detail      string `json:"detail,omitempty"`      // raw git/ssh output on failure
	Fingerprint string `json:"fingerprint,omitempty"` // SHA256 of the deploy key Fog presents
	RepoURL     string `json:"repoUrl,omitempty"`     // exactly what was tested
	// Server's-eye network view — catches split-DNS traps where the same
	// hostname reaches different machines from different clients.
	ResolvedIPs []string `json:"resolvedIps,omitempty"`
	SSHHostKey  string   `json:"sshHostKey,omitempty"` // fingerprint of whoever answered SSH
}

// sshHostFromRepo extracts host and port from an ssh:// or scp-style URL.
func sshHostFromRepo(repo string) (host, port string) {
	if u, err := url.Parse(repo); err == nil && u.Scheme == "ssh" && u.Host != "" {
		h, p, err := net.SplitHostPort(u.Host)
		if err != nil {
			return u.Host, "22"
		}
		return h, p
	}
	// scp-style: user@host:path
	if at := strings.Index(repo, "@"); at >= 0 {
		rest := repo[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return rest[:colon], "22"
		}
	}
	return "", ""
}

// probeSSHHost resolves the git host and fingerprints whichever sshd answers.
// The lookup runs under its own deadline within ctx: a hanging resolver must
// fail the probe, not pin the TestConnection request past its budget (the
// SSH dial below already carries a timeout of its own).
func probeSSHHost(ctx context.Context, host, port string) (ips []string, hostKeyFP string) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if addrs, err := (&net.Resolver{}).LookupHost(rctx, host); err == nil {
		ips = addrs
	}
	var fp string
	cfg := &gossh.ClientConfig{
		User:    "fog-probe",
		Timeout: 5 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
			fp = gossh.FingerprintSHA256(key)
			return nil
		},
	}
	// Auth fails after key exchange — we only want the host key.
	if c, err := gossh.Dial("tcp", net.JoinHostPort(host, port), cfg); err == nil {
		c.Close()
	}
	return ips, fp
}

// TestConnection verifies a git app can reach its repository with its deploy
// key — the setup flow's fast feedback before the first deploy.
func (e *Engine) TestConnection(ctx context.Context, name string) TestResult {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return TestResult{Message: "no such app"}
	}
	spec, err := LatestSpec(e.Store, app.ID)
	if err != nil {
		return TestResult{Message: err.Error()}
	}
	if spec.Source.Type != "git" {
		return TestResult{Message: "not a git app"}
	}
	res := TestResult{RepoURL: spec.Source.Repo}
	key, _ := e.appSecret(app.ID, SecretDeployKey)
	// Fingerprint the PRIVATE key — that's what ssh actually presents. If it
	// disagrees with the stored public key (the one the operator pasted into
	// the git host), no amount of key-adding will ever work; say so.
	if signer, err := gossh.ParsePrivateKey([]byte(key)); err == nil {
		res.Fingerprint = gossh.FingerprintSHA256(signer.PublicKey())
		if pub, err := e.appSecret(app.ID, SecretDeployKeyPub); err == nil {
			if pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(pub)); err == nil {
				if gossh.FingerprintSHA256(pk) != res.Fingerprint {
					res.Message = "Fog's key files are out of sync — the key shown above is NOT the one being presented. " +
						"Click 'Regenerate key', then add the newly shown key to the repository."
					res.Detail = "stored public key fingerprint: " + gossh.FingerprintSHA256(pk) +
						"\npresented private key fingerprint: " + res.Fingerprint
					return res
				}
			}
		}
	}
	branch := spec.Source.BranchOrDefault()
	tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := e.Builder.LsRemote(tctx, app.Name, spec.Source.Repo, branch, key); err != nil {
		res.Detail = err.Error()
		if d := Diagnose(err.Error()); d != nil {
			res.Message = d.Title + " — " + d.Fix
		} else {
			res.Message = err.Error()
		}
		if host, port := sshHostFromRepo(spec.Source.Repo); host != "" {
			// The request ctx, not tctx: when LsRemote burned the whole 20s
			// budget timing out, the probe is the diagnostic for exactly that
			// case and must still get its bounded chance to run.
			res.ResolvedIPs, res.SSHHostKey = probeSSHHost(ctx, host, port)
			if len(res.ResolvedIPs) > 1 {
				res.Message += " NOTE: " + host + " resolves to multiple addresses on this server — SSH may be reaching a different machine than your git host; try the git server's IP directly in the repository URL."
			}
		}
		return res
	}
	res.OK = true
	res.Message = "Connected — repository and branch look good."
	return res
}

// CheckWebhookToken constant-time-ish validates a webhook token.
func (e *Engine) CheckWebhookToken(appID int64, token string) bool {
	want, err := e.appSecret(appID, SecretWebhookToken)
	return err == nil && want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

// mergedEnv combines plain env with decrypted secrets.
func (e *Engine) mergedEnv(appID int64, spec *AppSpec) (map[string]string, error) {
	env, _, err := e.mergedEnvMissing(appID, spec, false)
	return env, err
}

// mergedEnvMissing is mergedEnv that, when missingOK, sets a secret the
// vault no longer has to "" and names it rather than failing: a start
// renders the revision that is running, whose secrets an integration's
// disconnect, a deleted provider or an operator's save may have taken
// away since — the app runs without them, and an issue says so.
func (e *Engine) mergedEnvMissing(appID int64, spec *AppSpec, missingOK bool) (map[string]string, []string, error) {
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	var missing []string
	for _, k := range spec.Secrets {
		v, err := e.appSecret(appID, k)
		if missingOK && errors.Is(err, store.ErrNotFound) {
			env[k] = ""
			missing = append(missing, k)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", k, err)
		}
		env[k] = v
	}
	return env, missing, nil
}
