// Package build turns a git ref into a local docker image: fetch (with a
// per-app deploy key), checkout, docker build, stamp the commit. Build logs
// stream into the deploy record.
package build

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/signin"
)

type Builder struct {
	DataDir string // builds live under <DataDir>/builds/<app>/
	// ensureBuilder stands in for the buildx builder check in tests.
	ensureBuilder func(ctx context.Context, app string) error
}

func (b *Builder) appDir(app string) string { return filepath.Join(b.DataDir, "builds", app) }

// Remove deletes the app's build workspace — the checkout and the on-disk
// copy of the deploy key. No-op for apps that never built.
func (b *Builder) Remove(app string) error { return os.RemoveAll(b.appDir(app)) }

// GenerateDeployKey returns (privatePEM, publicOpenSSH). The private key is
// stored in the vault as an app secret; the public half is pasted into
// Gitea/GitHub as a read-only deploy key.
func GenerateDeployKey() (string, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "fog deploy key")
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " fog-deploy", nil
}

// Fetch clones or updates the repo and checks out the branch head.
// Returns the resolved commit sha.
func (b *Builder) Fetch(ctx context.Context, app, repo, branch, deployKeyPEM string, log io.Writer) (string, error) {
	return b.fetch(ctx, app, repo, branch, "", deployKeyPEM, log)
}

// FetchCommit brings the repo up to date the same way Fetch does, then
// checks out one specific commit instead of the branch head — the rollback
// path. The clone is a full one, so a commit that is behind the head is
// already in the history the fetch brought down; a sha that is not (a force
// push dropped it, or it belongs to another repo) fails here, with the sha
// in the message, rather than silently building the head instead.
func (b *Builder) FetchCommit(ctx context.Context, app, repo, branch, commit, deployKeyPEM string, log io.Writer) (string, error) {
	if commit == "" {
		return "", fmt.Errorf("no commit given to check out")
	}
	return b.fetch(ctx, app, repo, branch, commit, deployKeyPEM, log)
}

func (b *Builder) fetch(ctx context.Context, app, repo, branch, commit, deployKeyPEM string, log io.Writer) (string, error) {
	if branch == "" {
		branch = "main"
	}
	// Every argument below that came from a spec is checked before git sees
	// it — a branch never starts with "-" (ValidateBranch), a commit is hex,
	// "origin/…" can't be an option — and every git call ends its options
	// or its revisions explicitly with "--": a branch "-u…" or
	// "--upload-pack=…" must be a refused name, never an option. (Not
	// --end-of-options: git 2.43, Ubuntu 24.04's, mishandles it in checkout
	// and reset.)
	if err := ValidateBranch(branch); err != nil {
		return "", err
	}
	if err := validateRepoArg(repo); err != nil {
		return "", err
	}
	// A password in the URL is no use to ssh (the deploy key is), and git
	// writes the URL into .git/config — inside the build context, where the
	// Dockerfile's COPY . and the image could carry it. It never reaches git.
	repo = withoutPassword(repo)
	if commit != "" {
		if err := validateCommit(commit); err != nil {
			return "", err
		}
	}
	dir := b.appDir(app)
	repoDir := filepath.Join(dir, "repo")
	// The clone runs *in* this directory, and until now it existed only as a
	// side effect of writing a deploy key beside it — so a repo that needs
	// no key (a public https one) failed with "git clone failed" and an
	// empty log, because git was never started at all.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	env, err := b.sshEnv(app, deployKeyPEM)
	if err != nil {
		return "", err
	}

	// git's own output names the remote; a credential in its URL must not
	// reach the deploy log.
	red := newRedactor(log, repoSecrets(repo)...)
	defer red.Flush()
	log = red
	run := func(args []string, workdir string) error {
		cmd := execx.Command(ctx, "git", args...)
		cmd.Dir = workdir
		cmd.Env = env
		cmd.Stdout = red
		cmd.Stderr = red
		return cmd.Run()
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		// No .git means either a fresh workspace or the wreckage of a clone
		// that died partway. Clear it either way: git refuses to clone into a
		// non-empty directory, so leftovers would make every future attempt
		// fail with "already exists and is not empty" — an app permanently
		// stuck with no way out through the UI.
		os.RemoveAll(repoDir)
		fmt.Fprintf(log, "$ git clone --branch %s %s\n", branch, repo)
		if err := run([]string{"clone", "--branch", branch, "--", repo, repoDir}, dir); err != nil {
			os.RemoveAll(repoDir)
			return "", fmt.Errorf("git clone failed")
		}
	} else {
		// A checkout made before kept the URL it was cloned with: the
		// remote is set, every time, to the URL without a password, before
		// anything is fetched from it.
		if err := run([]string{"remote", "set-url", "--", "origin", repo}, repoDir); err != nil {
			return "", fmt.Errorf("git remote set-url failed")
		}
		fmt.Fprintf(log, "$ git fetch origin %s\n", branch)
		if err := run([]string{"fetch", "--", "origin", branch}, repoDir); err != nil {
			return "", fmt.Errorf("git fetch failed")
		}
		if err := run([]string{"checkout", "--force", branch, "--"}, repoDir); err != nil {
			return "", fmt.Errorf("git checkout failed")
		}
		if err := run([]string{"reset", "--hard", "origin/" + branch, "--"}, repoDir); err != nil {
			return "", fmt.Errorf("git reset failed")
		}
	}

	if commit != "" {
		fmt.Fprintf(log, "$ git checkout --force %s\n", commit)
		if err := run([]string{"checkout", "--force", commit, "--"}, repoDir); err != nil {
			return "", fmt.Errorf("commit %.12s is not in %s — it may have been removed by a force push", commit, repo)
		}
	}

	out, err := execx.Output(ctx, "git", "-C", repoDir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("git rev-parse failed: %w", err)
	}
	return out, nil
}

// sshEnv prepares the git environment shared by Fetch and LsRemote: the
// deploy key for SSH, and no password prompts for https.
func (b *Builder) sshEnv(app, deployKeyPEM string) ([]string, error) {
	// An https:// repository that turns out to be private asks for a
	// password; with no terminal to ask on, git must fail at once rather
	// than wait for one.
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if deployKeyPEM == "" {
		return env, nil
	}
	dir := b.appDir(app)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "deploy_key")
	if err := os.WriteFile(keyPath, []byte(deployKeyPEM), 0o600); err != nil {
		return nil, err
	}
	// known_hosts lives in Fog's data dir — the service sandbox makes /root
	// read-only, so ssh's default path can't record accepted host keys.
	knownHosts := filepath.Join(b.DataDir, "builds", "known_hosts")
	return append(env, "GIT_SSH_COMMAND=ssh -i "+keyPath+
		" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10"+
		" -o UserKnownHostsFile="+knownHosts+" -o BatchMode=yes"), nil
}

// LsRemote verifies the repository is reachable with the deploy key and, when
// a branch is given, that the branch exists. Fast (~seconds) — used by the
// setup flow's Test Connection and as a deploy preflight.
func (b *Builder) LsRemote(ctx context.Context, app, repo, branch, deployKeyPEM string) error {
	_, err := b.RemoteHead(ctx, app, repo, branch, deployKeyPEM)
	return err
}

// RemoteHead returns the commit sha at the tip of the branch on the remote,
// without fetching. This is how the update checker notices new pushes.
func (b *Builder) RemoteHead(ctx context.Context, app, repo, branch, deployKeyPEM string) (string, error) {
	if err := validateRepoArg(repo); err != nil {
		return "", err
	}
	if branch != "" {
		if err := ValidateBranch(branch); err != nil {
			return "", err
		}
	}
	env, err := b.sshEnv(app, deployKeyPEM)
	if err != nil {
		return "", err
	}
	args := []string{"ls-remote", "--", repo}
	if branch != "" {
		args = append(args, "refs/heads/"+branch)
	}
	cmd := execx.Command(ctx, "git", args...)
	cmd.Env = env
	body, err := execx.Run(cmd)
	if err != nil {
		// The error carries git's stderr, which names the remote.
		if secrets := repoSecrets(repo); len(secrets) > 0 {
			return "", errors.New(redactRepo(err.Error(), repo))
		}
		return "", err
	}
	if branch != "" && body == "" {
		return "", fmt.Errorf("branch %s not found on the remote", branch)
	}
	if f := strings.Fields(body); len(f) > 0 {
		return f[0], nil
	}
	return "", nil
}

// CheckDockerfile confirms the checked-out repo has a Dockerfile at the
// configured path.
func (b *Builder) CheckDockerfile(app, dockerfile string) error {
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	// Resolved inside the checkout: the repo is the app author's, and a
	// Dockerfile that is a symlink to a file of the host's would have
	// docker read that file into the build (and its log).
	root, err := os.OpenRoot(filepath.Join(b.appDir(app), "repo"))
	if err != nil {
		return fmt.Errorf("no Dockerfile at %s in the repository", dockerfile)
	}
	defer root.Close()
	if st, err := root.Stat(filepath.FromSlash(dockerfile)); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("no Dockerfile at %s in the repository", dockerfile)
	}
	return nil
}

// RepoManifest is app-owned metadata read from fog.yaml in the repo root —
// the app describes itself instead of Fog's catalog having to know.
type RepoManifest struct {
	GettingStarted string `yaml:"gettingStarted"`
	// Connect declares that fog may connect a paired household's app to
	// this one (docs/connect-contract.md). An app that never opts in is
	// never connected to — fog carries no per-app federation knowledge.
	Connect struct {
		Contract string `yaml:"contract"` // e.g. "fog-connect/1"
		// HostChange is the app's own account of what a change of its host or
		// domain costs its federation — the identity-permanence fact fog
		// surfaces so an operator sees the blast radius before they move it.
		// Free text; empty means the app hasn't said (fog shows that honestly
		// rather than guessing "nothing").
		HostChange string `yaml:"hostChange"`
	} `yaml:"connect"`
	// Files declares that the app wants to hear when a Files folder it
	// mounts changes underneath it (docs/files-contract.md).
	Files struct {
		Contract string `yaml:"contract"` // e.g. "fog-files/1"
	} `yaml:"files"`
	// Signin declares household sign-in (docs/signin-contract.md): fog
	// registers the app as an OIDC client and hands it the configuration.
	Signin signin.Manifest `yaml:"signin"`
	// Integrations declares what the app offers other apps and which of
	// theirs it wants (docs/integrations-contract.md). Left as decoded:
	// apps.DeclaredFrom gives it its shape, and fog records it at each
	// deploy so the installed app keeps up with its repo.
	Integrations map[string]any `yaml:"integrations"`
	// Compute declares that the app hands work to fog's transcoders
	// (docs/compute-contract.md). Recorded as JSON at each deploy.
	Compute ComputeManifest `yaml:"compute"`
	// Widgets declares that the app says what it has to show on a start page
	// (docs/widgets-contract.md).
	Widgets struct {
		Contract string `yaml:"contract"` // "fog-widgets/1"
	} `yaml:"widgets"`
	// InferenceRouter declares that the app orders the machines for each
	// inference request (docs/compute-contract.md).
	InferenceRouter struct {
		Contract string `yaml:"contract"` // "fog-inference-router/1"
	} `yaml:"inferenceRouter"`
}

// ComputeManifest is fog.yaml's compute block.
type ComputeManifest struct {
	Contract string `yaml:"contract" json:"contract"` // "fog-compute/1"
	Video    *struct {
		Classes []string `yaml:"classes" json:"classes,omitempty"`
		Blurb   string   `yaml:"blurb" json:"blurb,omitempty"`
	} `yaml:"video" json:"video,omitempty"`
	Inference *struct {
		Blurb string `yaml:"blurb" json:"blurb,omitempty"`
		// Locality "household" keeps the app's requests on machines whose
		// model server is the machine itself.
		Locality string `yaml:"locality" json:"locality,omitempty"`
	} `yaml:"inference" json:"inference,omitempty"`
}

// LocalityHousehold is the one locality a manifest can name.
const LocalityHousehold = "household"

// UnknownLocality is a locality fog doesn't know, or "". fog holds such an app
// to the household anyway; the deploy log says so.
func (c ComputeManifest) UnknownLocality() string {
	if c.Inference == nil || c.Inference.Locality == LocalityHousehold {
		return ""
	}
	return c.Inference.Locality
}

// JSON is the block as fog records it; "" when the app declares none.
func (c ComputeManifest) JSON() string {
	if strings.TrimSpace(c.Contract) == "" {
		return ""
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(raw)
}

// KnownContracts is every fog contract fogd itself implements. A manifest that
// names a contract outside this set has almost certainly typoed it or targets
// a fogd newer than this one — either way the app won't get the behaviour it
// asked for, so fog flags it rather than silently ignoring it.
var KnownContracts = map[string]bool{
	"fog-connect/1": true,
	"fog-files/1":   true,
	"fog-backup/1":  true,
	"fog-backup/2":  true,
	"fog-compute/1": true,
	WidgetsContract: true,
	RouterContract:  true,
	signin.Contract: true,
}

// RouterContract is what an app declares to order the machines for each
// inference request; compute.RouterContract is the same name.
const RouterContract = "fog-inference-router/1"

// WidgetsContract is what an app declares to show on a start page.
const WidgetsContract = "fog-widgets/1"

// Contracts lists the fog contracts the manifest declares. Never nil — a
// manifest that declares a hostChange but no contract still marshals as [], so
// a consumer's length check never meets a JSON null.
func (m RepoManifest) Contracts() []string {
	out := []string{}
	if c := strings.TrimSpace(m.Connect.Contract); c != "" {
		out = append(out, c)
	}
	if c := strings.TrimSpace(m.Files.Contract); c != "" {
		out = append(out, c)
	}
	if c := strings.TrimSpace(m.Signin.Contract); c != "" {
		out = append(out, c)
	}
	if c := strings.TrimSpace(m.Compute.Contract); c != "" {
		out = append(out, c)
	}
	if c := strings.TrimSpace(m.Widgets.Contract); c != "" {
		out = append(out, c)
	}
	if c := strings.TrimSpace(m.InferenceRouter.Contract); c != "" {
		out = append(out, c)
	}
	return out
}

// UnknownContracts lists declared contracts fogd does not recognise — the
// validation Project J surfaces. Empty for a clean manifest.
func (m RepoManifest) UnknownContracts() []string {
	var out []string
	for _, c := range m.Contracts() {
		if !KnownContracts[c] {
			out = append(out, c)
		}
	}
	return out
}

// ReadRepoManifest reads fog.yaml (or .fog.yaml) from the checked-out repo.
// A missing file is an empty manifest, not an error.
func (b *Builder) ReadRepoManifest(app string) RepoManifest {
	var m RepoManifest
	// Read inside the checkout, a regular file of bounded size: the repo is
	// the app author's, and fog.yaml as a symlink would otherwise have fogd
	// read whatever host file it names into the app's notes.
	root, err := os.OpenRoot(filepath.Join(b.appDir(app), "repo"))
	if err != nil {
		return m
	}
	defer root.Close()
	for _, name := range []string{"fog.yaml", "fog.yml", ".fog.yaml"} {
		raw, err := readRepoFile(root, name, maxManifestBytes)
		if err != nil {
			continue
		}
		if err := yaml.Unmarshal(raw, &m); err == nil {
			return m
		}
	}
	return m
}

// maxManifestBytes bounds fog.yaml: a manifest is a page of YAML.
const maxManifestBytes = 1 << 20

// readRepoFile reads a regular file inside root, refusing one larger than max.
func readRepoFile(root *os.Root, name string, max int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, fmt.Errorf("%s is not a manifest", name)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// ImageRepo is the local repository an app's builds are tagged into.
func ImageRepo(app string) string { return "fog/" + app }

// imageRef is the local tag for an app build.
func imageRef(app, sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return ImageRepo(app) + ":" + short
}

// Build runs docker build in the checked-out repo.
func (b *Builder) Build(ctx context.Context, app, sha, dockerfile string, buildArgs map[string]string, log io.Writer) (string, error) {
	repoDir := filepath.Join(b.appDir(app), "repo")
	// Checked here as well as before the build is reached: -f is read by
	// docker from the host, and must be a file inside the checkout.
	if err := b.CheckDockerfile(app, dockerfile); err != nil {
		return "", err
	}
	ref := imageRef(app, sha)
	// Each app builds on a builder of its own (isolate.go): a RUN
	// --mount=type=cache is the builder's, shared by every build on it, and
	// one repo's build could leave in it what another's then ran.
	prefix, release := b.isolate(ctx, app, log)
	defer release()
	args := append(prefix, "-t", ref)
	if dockerfile != "" {
		args = append(args, "-f", dockerfile)
	}
	// Build-arg values are often credentials (a package registry token), and
	// the deploy log is read by the app page, notifications and MCP: the
	// command is echoed with each value redacted, and so is docker's output.
	shown := append([]string{}, args...)
	var secrets []string
	for k, v := range buildArgs {
		v = strings.ReplaceAll(v, "{{commit}}", sha)
		args = append(args, "--build-arg", k+"="+v)
		if v == sha || v == "" {
			shown = append(shown, "--build-arg", k+"="+v)
			continue
		}
		shown = append(shown, "--build-arg", k+"="+Redacted)
		secrets = append(secrets, v)
	}
	args = append(args, ".")
	shown = append(shown, ".")
	fmt.Fprintf(log, "$ docker %s\n", strings.Join(shown, " "))
	red := newRedactor(log, secrets...)
	defer red.Flush()
	cmd := execx.Command(ctx, "docker", args...)
	cmd.Dir = repoDir
	cmd.Stdout = red
	cmd.Stderr = red
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build failed")
	}
	return ref, nil
}
