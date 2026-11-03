package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// release.sh's preflight, run for real against a scratch repo: it must
// refuse to send the git credential over http, to sign a tree with
// untracked files in it, and to use a signing key anyone else can read —
// each before it builds anything. A stub `go` marks "got past the checks".

func preflightRepo(t *testing.T) (repo, key string) {
	t.Helper()
	skipShell(t)
	repo = t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../scripts/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "scripts/release.sh"), b, 0o755)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("/bin/\n"), 0o644)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	key = filepath.Join(t.TempDir(), "key.pem")
	os.WriteFile(key, []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600)
	return repo, key
}

func runRelease(t *testing.T, repo string, env ...string) (string, error) {
	t.Helper()
	stubs := t.TempDir()
	writeStub(t, stubs, "go", "#!/bin/sh\necho 'STUB GO: past the preflight' >&2\nexit 42\n")
	cmd := exec.Command("sh", filepath.Join(repo, "scripts/release.sh"), "v1.2.3")
	cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseRefusesCredentialsOverHTTP(t *testing.T) {
	repo, key := preflightRepo(t)
	out, err := runRelease(t, repo, "FOG_GIT_HOST=http://gitea.lan", "FOG_SIGNING_KEY="+key)
	if err == nil || !strings.Contains(out, "plain http") || !strings.Contains(out, "git config fog.insecureCredentialHost gitea.lan") {
		t.Fatalf("http credential not refused (err %v):\n%s", err, out)
	}
	// The old blanket switch no longer does anything.
	if out, err := runRelease(t, repo, "FOG_GIT_HOST=http://gitea.lan", "FOG_ALLOW_INSECURE_CREDENTIALS=1", "FOG_SIGNING_KEY="+key); err == nil || strings.Contains(out, "STUB GO") {
		t.Fatalf("FOG_ALLOW_INSECURE_CREDENTIALS still lets credentials over http:\n%s", out)
	}

	// The repo names its one allowed http host: exactly that host passes.
	cmd := exec.Command("git", "config", "fog.insecureCredentialHost", "gitea.lan")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v\n%s", err, out)
	}
	out, _ = runRelease(t, repo, "FOG_GIT_HOST=http://gitea.lan", "FOG_SIGNING_KEY="+key)
	if !strings.Contains(out, "STUB GO") {
		t.Fatalf("the configured host was refused:\n%s", out)
	}
	for _, other := range []string{"http://evil.lan", "http://gitea.lan:3000", "http://gitea.lan.evil.com", "http://GITEA.lan"} {
		out, err := runRelease(t, repo, "FOG_GIT_HOST="+other, "FOG_SIGNING_KEY="+key)
		if err == nil || strings.Contains(out, "STUB GO") || !strings.Contains(out, "allows that only for 'gitea.lan'") {
			t.Errorf("%s: credential would go over http to a host the repo didn't name (err %v):\n%s", other, err, out)
		}
	}
	out, _ = runRelease(t, repo, "FOG_GIT_HOST=https://git.example.com", "FOG_SIGNING_KEY="+key)
	if !strings.Contains(out, "STUB GO") {
		t.Fatalf("an https host was refused:\n%s", out)
	}
}

// The attack: a stray untracked .go file would compile into a signed
// release.
func TestReleaseRefusesUntrackedFiles(t *testing.T) {
	repo, key := preflightRepo(t)
	os.MkdirAll(filepath.Join(repo, "internal/x"), 0o755)
	os.WriteFile(filepath.Join(repo, "internal/x/backdoor.go"), []byte("package x\n"), 0o644)
	out, err := runRelease(t, repo, "FOG_GIT_HOST=https://git.example.com", "FOG_SIGNING_KEY="+key)
	if err == nil || !strings.Contains(out, "isn't clean") || !strings.Contains(out, "backdoor.go") || strings.Contains(out, "STUB GO") {
		t.Fatalf("untracked file not refused (err %v):\n%s", err, out)
	}
	// Ignored build output is fine.
	os.Remove(filepath.Join(repo, "internal/x/backdoor.go"))
	os.MkdirAll(filepath.Join(repo, "bin"), 0o755)
	os.WriteFile(filepath.Join(repo, "bin/fogd-linux-amd64"), []byte("x"), 0o644)
	if out, _ := runRelease(t, repo, "FOG_GIT_HOST=https://git.example.com", "FOG_SIGNING_KEY="+key); !strings.Contains(out, "STUB GO") {
		t.Fatalf("ignored files blocked the release:\n%s", out)
	}
}

func TestReleaseRefusesAnExposedSigningKey(t *testing.T) {
	repo, key := preflightRepo(t)
	os.Chmod(key, 0o644)
	out, err := runRelease(t, repo, "FOG_GIT_HOST=https://git.example.com", "FOG_SIGNING_KEY="+key)
	if err == nil || !strings.Contains(out, "mode 0600") || strings.Contains(out, "STUB GO") {
		t.Fatalf("a world-readable key was used (err %v):\n%s", err, out)
	}
	os.Chmod(key, 0o600)
	link := filepath.Join(t.TempDir(), "link.pem")
	os.Symlink(key, link)
	if out, err := runRelease(t, repo, "FOG_GIT_HOST=https://git.example.com", "FOG_SIGNING_KEY="+link); err == nil || !strings.Contains(out, "mode 0600") {
		t.Fatalf("a symlinked key was used (err %v):\n%s", err, out)
	}
}

// Released binaries carry no build-machine paths and no record of this
// private repo: every go build in release.sh and the Makefile uses
// -trimpath and -buildvcs=false, and a binary built that way passes
// check-release-binaries.
func TestReleaseBuildsTrimPaths(t *testing.T) {
	for _, f := range []string{"../../scripts/release.sh", "../../Makefile"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !strings.Contains(l, "go build") || strings.Contains(l, "bin/signtool") || strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			for _, flag := range []string{"-trimpath", "-buildvcs=false"} {
				if !strings.Contains(l, flag) {
					t.Errorf("%s:%d builds without %s: %s", f, i+1, flag, strings.TrimSpace(l))
				}
			}
		}
	}
}

// goModule is a scratch Go module under git, with one commit: what a build
// stamps vcs.revision from. It answers the module dir and a build function.
func goModule(t *testing.T) (string, func(flags ...string) string) {
	t.Helper()
	skipShell(t)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	b, _ := os.ReadFile("../../scripts/check-release-binaries")
	os.WriteFile(filepath.Join(dir, "scripts/check-release-binaries"), b, 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/stub\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"stub\") }\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "private commit"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir, func(flags ...string) string {
		out := filepath.Join(dir, "bin-out", fmt.Sprintf("fogd-%d", len(flags)))
		args := append([]string{"build"}, flags...)
		args = append(args, "-o", out, ".")
		cmd := exec.Command(goBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, b)
		}
		return out
	}
}

func runCheck(t *testing.T, dir, rel string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(dir, "scripts/check-release-binaries"), rel)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// check-release-binaries refuses a binary that names the private commit
// (vcs.revision, vcs.time) or the build machine, and passes one built as
// release.sh builds.
func TestCheckReleaseBinaries(t *testing.T) {
	dir, build := goModule(t)
	rel := filepath.Join(dir, "rel")
	os.MkdirAll(rel, 0o755)

	stamped := build("-ldflags=-s -w")
	b, _ := os.ReadFile(stamped)
	os.WriteFile(filepath.Join(rel, "fogd-linux-amd64"), b, 0o755)
	os.WriteFile(filepath.Join(rel, "install.sh"), []byte("#!/bin/sh\n"), 0o755) // not Go: skipped
	out, err := runCheck(t, dir, rel)
	if err == nil || !strings.Contains(out, "vcs.revision") || !strings.Contains(out, "contains the local path") {
		t.Fatalf("a vcs-stamped, untrimmed binary passed (err %v):\n%s", err, out)
	}

	clean := build("-trimpath", "-buildvcs=false", "-ldflags=-s -w")
	b, _ = os.ReadFile(clean)
	os.WriteFile(filepath.Join(rel, "fogd-linux-amd64"), b, 0o755)
	out, err = runCheck(t, dir, rel)
	if err != nil || !strings.Contains(out, "1 checked") {
		t.Fatalf("a release-built binary was refused (err %v):\n%s", err, out)
	}
	info, _ := exec.Command("go", "version", "-m", clean).Output()
	if strings.Contains(string(info), "vcs") {
		t.Errorf("-buildvcs=false still stamped:\n%s", info)
	}
}

// release.sh's version pattern accepts exactly what the Go parser accepts
// (minus build metadata, which a tag never carries): a tag it lets through
// is one every fogd can order, and the strings the reviewer found
// (v1.0.0-., v1.0.0-rc..1) are refused before anything runs.
func TestReleaseVersionRuleIsTheGoParsers(t *testing.T) {
	b, err := os.ReadFile("../../scripts/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^VERSION_RE='([^']*)'`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no VERSION_RE in release.sh")
	}
	re := regexp.MustCompile("^(?:" + string(m[1]) + ")$")
	for _, v := range []string{
		"v0.0.0", "v1.2.3", "v10.20.30", "v1.0.0-rc.1", "v1.0.0-0", "v1.0.0-0a", "v1.0.0-x-y.z", "v1.0.0-alpha.beta.11",
		"v1.0.0-.", "v1.0.0-rc..1", "v1.0.0-", "v01.0.0", "v1.0.00", "v1.0.0-rc.01", "v1.0.0-rc_1", "v1.0", "1.0.0",
		"v1.0.0-rc.1;id", "v1234567890.0.0", "v1.0.0+build",
	} {
		goOK := ValidVersion(v) && !strings.Contains(v, "+")
		if got := re.MatchString(v); got != goOK {
			t.Errorf("release.sh's pattern on %q: %v, the Go parser: %v", v, got, goOK)
		}
	}
	repo, key := preflightRepo(t)
	for _, bad := range []string{"v1.0.0-.", "v1.0.0-rc..1"} {
		cmd := exec.Command("sh", filepath.Join(repo, "scripts/release.sh"), bad)
		cmd.Env = append(os.Environ(), "FOG_GIT_HOST=https://git.example.com", "FOG_SIGNING_KEY="+key)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "semver release") {
			t.Errorf("release.sh %s: err %v\n%s", bad, err, out)
		}
	}
}
