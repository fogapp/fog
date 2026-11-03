package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/compute"
)

// The deploy keypair must be internally consistent (private half derives the
// stored public half) and in a format the real ssh client accepts — a silent
// mismatch here produces auth failures that look like git-host misconfig.
func TestReadRepoManifest(t *testing.T) {
	dir := t.TempDir()
	b := &Builder{DataDir: dir}
	repo := filepath.Join(dir, "builds", "myapp", "repo")
	os.MkdirAll(repo, 0o755)

	// Missing file → empty manifest, no error.
	if m := b.ReadRepoManifest("myapp"); m.GettingStarted != "" {
		t.Fatalf("expected empty, got %q", m.GettingStarted)
	}
	os.WriteFile(filepath.Join(repo, "fog.yaml"), []byte("gettingStarted: |\n  First login: admin / hunter2\n"), 0o644)
	if m := b.ReadRepoManifest("myapp"); !strings.Contains(m.GettingStarted, "admin / hunter2") {
		t.Fatalf("got %q", m.GettingStarted)
	}
	// Broken YAML degrades to empty rather than failing the deploy.
	os.WriteFile(filepath.Join(repo, "fog.yaml"), []byte(":\n:::not yaml"), 0o644)
	if m := b.ReadRepoManifest("myapp"); m.GettingStarted != "" {
		t.Fatalf("expected empty on bad yaml, got %q", m.GettingStarted)
	}
}

func TestGenerateDeployKey(t *testing.T) {
	priv, pub, err := GenerateDeployKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil {
		t.Fatalf("private key doesn't parse: %v", err)
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		t.Fatalf("public key doesn't parse: %v", err)
	}
	if ssh.FingerprintSHA256(signer.PublicKey()) != ssh.FingerprintSHA256(pk) {
		t.Fatal("private and public halves are different keys")
	}

	// ssh-keygen -y is the authoritative parser for what ssh(1) will load.
	if _, err := exec.LookPath("ssh-keygen"); err == nil {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "key")
		if err := os.WriteFile(keyPath, []byte(priv), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("ssh-keygen", "-y", "-f", keyPath).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen rejects the private key: %s", out)
		}
		derived := strings.Fields(string(out))
		stored := strings.Fields(pub)
		if len(derived) < 2 || len(stored) < 2 || derived[1] != stored[1] {
			t.Fatalf("ssh-keygen derives a different public key\nderived: %s\nstored:  %s", out, pub)
		}
	}
}

func TestManifestFederationFacts(t *testing.T) {
	var m RepoManifest
	src := "connect:\n  contract: fog-connect/1\n  hostChange: \"Peers must re-pair; follows break.\"\nfiles:\n  contract: fog-files/1\n"
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	if m.Connect.HostChange != "Peers must re-pair; follows break." {
		t.Fatalf("hostChange: %q", m.Connect.HostChange)
	}
	if got := m.Contracts(); len(got) != 2 {
		t.Fatalf("contracts: %v", got)
	}
	if got := m.UnknownContracts(); len(got) != 0 {
		t.Fatalf("known contracts should not be flagged: %v", got)
	}

	var bad RepoManifest
	if err := yaml.Unmarshal([]byte("connect:\n  contract: fog-bogus/9\n"), &bad); err != nil {
		t.Fatal(err)
	}
	if got := bad.UnknownContracts(); len(got) != 1 || got[0] != "fog-bogus/9" {
		t.Fatalf("unknown contract not flagged: %v", got)
	}
}

// FetchCommit is the git half of a rollback: bring the repo up to date the
// normal way, then land on one older commit rather than the branch head.
// The clone is a full one, so that commit is already in the history the
// fetch brought down.
func TestFetchCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := filepath.Join(t.TempDir(), "origin")
	os.MkdirAll(origin, 0o755)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main", ".")
	os.WriteFile(filepath.Join(origin, "f"), []byte("one"), 0o644)
	git("add", "-A")
	git("commit", "-m", "one")
	first := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(origin, "f"), []byte("two"), 0o644)
	git("add", "-A")
	git("commit", "-m", "two")
	head := git("rev-parse", "HEAD")

	b := &Builder{DataDir: t.TempDir()}
	ctx := t.Context()
	got, err := b.Fetch(ctx, "app", origin, "main", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != head {
		t.Fatalf("Fetch landed on %s, want the head %s", got, head)
	}

	// Back one commit, and the working tree must be that commit's, not the
	// head's — a checkout that silently didn't take would build the very
	// version the operator is rolling away from.
	got, err = b.FetchCommit(ctx, "app", origin, "main", first, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("FetchCommit landed on %s, want %s", got, first)
	}
	content, err := os.ReadFile(filepath.Join(b.appDir("app"), "repo", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one" {
		t.Errorf("working tree holds %q, want the older commit's %q", content, "one")
	}

	// A commit this repo doesn't have fails, naming what it couldn't find,
	// rather than quietly building the head instead.
	if _, err := b.FetchCommit(ctx, "app", origin, "main", "0123456789abcdef0123456789abcdef01234567", "", io.Discard); err == nil {
		t.Error("FetchCommit accepted a commit the repo doesn't have")
	}
	if _, err := b.FetchCommit(ctx, "app", origin, "main", "", "", io.Discard); err == nil {
		t.Error("FetchCommit accepted an empty commit")
	}

	// And a plain Fetch afterwards returns to the branch head: a rollback
	// leaves the checkout detached, and the next ordinary deploy must not
	// inherit it.
	got, err = b.Fetch(ctx, "app", origin, "main", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != head {
		t.Fatalf("after a rollback, Fetch landed on %s, want the head %s", got, head)
	}
}

func TestManifestCompute(t *testing.T) {
	var m RepoManifest
	src := "compute:\n  contract: fog-compute/1\n  video:\n    classes: [interactive, batch]\n    blurb: Live transcodes.\n"
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.Compute.JSON(); got != `{"contract":"fog-compute/1","video":{"classes":["interactive","batch"],"blurb":"Live transcodes."}}` {
		t.Fatalf("recorded as %s", got)
	}
	if got := m.Contracts(); len(got) != 1 || got[0] != "fog-compute/1" || len(m.UnknownContracts()) != 0 {
		t.Fatalf("contracts: %v", got)
	}
	if (RepoManifest{}).Compute.JSON() != "" {
		t.Fatal("no compute block recorded as something")
	}
}

func TestManifestDeclaresWidgets(t *testing.T) {
	var m RepoManifest
	if err := yaml.Unmarshal([]byte("widgets:\n  contract: fog-widgets/1\n"), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.Contracts(); len(got) != 1 || got[0] != WidgetsContract || len(m.UnknownContracts()) != 0 {
		t.Errorf("contracts = %v, unknown %v", got, m.UnknownContracts())
	}
}

func TestManifestDeclaresInferenceLocality(t *testing.T) {
	var m RepoManifest
	src := "compute:\n  contract: fog-compute/1\n  inference:\n    locality: household\n"
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.Compute.JSON(); got != `{"contract":"fog-compute/1","inference":{"locality":"household"}}` {
		t.Errorf("recorded %s", got)
	}
	if got := m.Compute.UnknownLocality(); got != "" {
		t.Errorf("unknown = %q", got)
	}
	m.Compute.Inference.Locality = "houshold"
	if got := m.Compute.UnknownLocality(); got != "houshold" {
		t.Errorf("unknown = %q", got)
	}
	if got := (ComputeManifest{}).UnknownLocality(); got != "" {
		t.Errorf("no inference: %q", got)
	}
}

// The attack: a branch git would read as an option. "--upload-pack=<cmd>"
// on clone or ls-remote runs <cmd> on this machine, so a spec carrying it
// must be refused before git runs — and nothing may land on disk.
func TestFetchRefusesOptionInjectionBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	b := &Builder{DataDir: t.TempDir()}
	ctx := t.Context()
	for _, branch := range []string{
		"--upload-pack=touch " + marker,
		"-u touch " + marker,
		"--template=/tmp",
		"main --upload-pack=x",
		"a..b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`, "a@{1}", "@", "HEAD",
		".hidden", "x/.hidden", "x.lock", "x/", "/x", "x//y", "x.", "a\x00b", "a\nb",
	} {
		if err := ValidateBranch(branch); err == nil {
			t.Errorf("ValidateBranch accepted %q", branch)
		}
		if _, err := b.Fetch(ctx, "app", "/nonexistent/repo", branch, "", io.Discard); err == nil {
			t.Errorf("Fetch accepted branch %q", branch)
		}
		if _, err := b.RemoteHead(ctx, "app", "/nonexistent/repo", branch, ""); err == nil {
			t.Errorf("RemoteHead accepted branch %q", branch)
		}
	}
	// A repository git would read as an option is refused the same way.
	if _, err := b.Fetch(ctx, "app", "--upload-pack=touch "+marker, "main", "", io.Discard); err == nil {
		t.Error("Fetch accepted an option as the repository")
	}
	if _, err := b.RemoteHead(ctx, "app", "--upload-pack=touch "+marker, "main", ""); err == nil {
		t.Error("RemoteHead accepted an option as the repository")
	}
	// And a rollback pin that isn't a sha never reaches checkout.
	for _, pin := range []string{"--orphan=x", "HEAD~1", "main", "-f"} {
		if _, err := b.FetchCommit(ctx, "app", "/nonexistent/repo", "main", pin, "", io.Discard); err == nil || !strings.Contains(err.Error(), "not a commit sha") {
			t.Errorf("FetchCommit pin %q: %v", pin, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran an injected command")
	}
	for _, ok := range []string{"main", "feature/x", "release-1.2", "v1.0", "user/topic_branch", "ünïcode"} {
		if err := ValidateBranch(ok); err != nil {
			t.Errorf("ValidateBranch refused %q: %v", ok, err)
		}
	}
}

// Every name ValidateBranch accepts, git accepts too: the rules are git's,
// not an approximation that would refuse a real branch.
func TestValidateBranchAgreesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, name := range []string{"main", "feature/x", "a.b", "a-b", "a_b", "v1.0", "x/y/z", "a@b", "ünï",
		"a..b", "a b", "-x", ".x", "x.lock", "x/", "a@{", "@", "a~", "a^", "a:", "a?", "a*", "a[", `a\`, "x."} {
		gitOK := exec.Command("git", "check-ref-format", "--branch", name).Run() == nil
		ours := ValidateBranch(name) == nil
		if ours && !gitOK {
			t.Errorf("%q: ValidateBranch accepts it, git does not", name)
		}
	}
}

// The attack: a repo whose fog.yaml or Dockerfile is a symlink to a file of
// the host's. fogd must not read it — into the app's notes, or into a build.
func TestRepoSymlinksOutOfTheCheckoutAreNotFollowed(t *testing.T) {
	b := &Builder{DataDir: t.TempDir()}
	repo := filepath.Join(b.appDir("app"), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.yaml")
	os.WriteFile(secret, []byte("gettingStarted: the host's secret\n"), 0o600)
	if err := os.Symlink(secret, filepath.Join(repo, "fog.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	if got := b.ReadRepoManifest("app").GettingStarted; got != "" {
		t.Fatalf("read the host file through the symlink: %q", got)
	}
	if err := b.CheckDockerfile("app", ""); err == nil {
		t.Fatal("a Dockerfile symlinked out of the repo passed the check")
	}
	if err := b.CheckDockerfile("app", "../../../"+filepath.Base(secret)); err == nil {
		t.Fatal("a Dockerfile path out of the repo passed the check")
	}
	// A symlink inside the repo is still fine.
	os.Remove(filepath.Join(repo, "fog.yaml"))
	os.WriteFile(filepath.Join(repo, "real.yaml"), []byte("gettingStarted: ok\n"), 0o644)
	os.Symlink("real.yaml", filepath.Join(repo, "fog.yaml"))
	if got := b.ReadRepoManifest("app").GettingStarted; got != "ok" {
		t.Fatalf("in-repo symlink: %q", got)
	}
}

// The attack: a build-arg value is a credential (a registry token), and the
// deploy log — the app page, notifications, MCP's fog_app_get — showed it
// in the echoed command and wherever the build printed it. Only the commit
// a value names may show.
func TestBuildLogRedactsBuildArgValues(t *testing.T) {
	bin := t.TempDir()
	// A docker that prints its arguments, split across writes, as a build
	// step echoing its ARGs would.
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s' \"$a\" | cut -c1-5 | tr -d '\\n'; printf '%s ' \"$(printf '%s' \"$a\" | cut -c6-)\"; done; echo\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := &Builder{DataDir: t.TempDir()}
	os.MkdirAll(filepath.Join(b.appDir("app"), "repo"), 0o755)
	os.WriteFile(filepath.Join(b.appDir("app"), "repo", "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	var log bytes.Buffer
	sha := "0123456789abcdef0123456789abcdef01234567"
	if _, err := b.Build(t.Context(), "app", sha, "", map[string]string{"NPM_TOKEN": "npm_s3cretT0ken", "REV": "{{commit}}"}, &log); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "s3cret") {
		t.Fatalf("the token reached the log:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "NPM_TOKEN="+Redacted) || !strings.Contains(log.String(), "REV="+sha) {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestRedactRepoCredentials(t *testing.T) {
	repo := "ssh://git:hunter2pass@git.example.com/o/r.git"
	got := redactRepo("fatal: could not read from "+repo, repo)
	if strings.Contains(got, "hunter2pass") {
		t.Fatalf("%q", got)
	}
}

// The attack: a spec's dockerfile names a host file — absolutely, or by
// climbing out of the checkout. Build must refuse before docker reads it.
func TestBuildRefusesADockerfileOutsideTheRepo(t *testing.T) {
	b := &Builder{DataDir: t.TempDir()}
	os.MkdirAll(filepath.Join(b.appDir("app"), "repo"), 0o755)
	secret := filepath.Join(t.TempDir(), "shadow")
	os.WriteFile(secret, []byte("root:x"), 0o600)
	for _, df := range []string{secret, "../../../" + filepath.Base(secret), "/etc/passwd"} {
		var log bytes.Buffer
		if _, err := b.Build(t.Context(), "app", "0123456789abcdef0123456789abcdef01234567", df, nil, &log); err == nil {
			t.Errorf("built with dockerfile %q", df)
		}
		if strings.Contains(log.String(), "root:x") {
			t.Errorf("the host file reached the log")
		}
	}
}

// Without a builder of the app's own, the shared builder's cache mounts are
// emptied before (and after) the build, one build at a time.
func TestBuildFallbackEmptiesTheSharedCache(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(bin, "log")
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"$@\" >> "+logPath+"\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := &Builder{DataDir: t.TempDir(), ensureBuilder: func(context.Context, string) error { return errors.New("no buildx") }}
	os.MkdirAll(filepath.Join(b.appDir("app"), "repo"), 0o755)
	os.WriteFile(filepath.Join(b.appDir("app"), "repo", "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	if _, err := b.Build(t.Context(), "app", "0123456789abcdef0123456789abcdef01234567", "", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "exec.cachemount") || !strings.HasPrefix(lines[1], "build ") || !strings.Contains(lines[2], "exec.cachemount") {
		t.Fatalf("docker calls:\n%s", raw)
	}
}

// The attack: a repository URL with a password in it, which git writes into
// .git/config — inside the build context, where COPY . puts it in the
// image. The checkout's config never holds it, a legacy one included.
func TestCheckoutConfigHoldsNoRepoPassword(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if got := withoutPassword("ssh://git:hunter2pass@git.example.com/o/r.git"); got != "ssh://git@git.example.com/o/r.git" {
		t.Fatalf("withoutPassword = %q", got)
	}
	origin := filepath.Join(t.TempDir(), "origin")
	os.MkdirAll(origin, 0o755)
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(origin, "init", "--initial-branch=main", ".")
	os.WriteFile(filepath.Join(origin, "f"), []byte("x"), 0o644)
	git(origin, "add", "-A")
	git(origin, "commit", "-m", "one")
	b := &Builder{DataDir: t.TempDir()}
	if _, err := b.Fetch(t.Context(), "app", origin, "main", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	// A checkout cloned by an older fog with the password in its remote.
	repoDir := filepath.Join(b.appDir("app"), "repo")
	git(repoDir, "remote", "set-url", "origin", "ssh://git:hunter2pass@git.example.com/o/r.git")
	if _, err := b.Fetch(t.Context(), "app", origin, "main", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(repoDir, ".git", "config"))
	if strings.Contains(string(cfg), "hunter2pass") {
		t.Fatalf(".git/config keeps the password:\n%s", cfg)
	}
}

// A builder made before builders had networks of their own is made again.
func TestBuilderCurrentNeedsItsOwnNetwork(t *testing.T) {
	old := "Driver Options:\nBuildKit daemon flags: --allow-insecure-entitlement=network.host\n"
	cur := "Driver Options:        network=\"fog-build-a\"\nBuildKit daemon flags: --oci-worker-net bridge --allow-insecure-entitlement=network.host\n"
	if builderCurrent(old, "fog-build-a") || !builderCurrent(cur, "fog-build-a") || builderCurrent(cur, "fog-build-b") {
		t.Fatal("builderCurrent is wrong")
	}
}

func TestRouterContractIsComputes(t *testing.T) {
	if RouterContract != compute.RouterContract {
		t.Fatal("build and compute must name the router contract alike")
	}
}
