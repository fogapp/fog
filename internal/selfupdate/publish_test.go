package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// scripts/publish-github-release puts the signed release directory on
// GitHub as the release strangers install from. Run against a scratch repo
// with gh stubbed: it must refuse to act without gh logged in as the public
// account (and say how), refuse a directory that isn't this version's
// signed, relative release, and create — or, on a re-run, update — exactly
// one release with every file.

type publishFixture struct {
	repo, stubs, log string
}

func newPublishFixture(t *testing.T) *publishFixture { return newPublishFixtureAt(t, "v1.4.0") }

func newPublishFixtureAt(t *testing.T, version string) *publishFixture {
	return newPublishFixtureWith(t, version, "#!/bin/sh\nexit 0\n")
}

func newPublishFixtureWith(t *testing.T, version, fogd string) *publishFixture {
	t.Helper()
	skipShell(t)
	f := &publishFixture{repo: t.TempDir(), stubs: t.TempDir()}
	f.log = filepath.Join(f.stubs, "gh.log")
	os.MkdirAll(filepath.Join(f.repo, "scripts"), 0o755)
	b, err := os.ReadFile("../../scripts/publish-github-release")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.repo, "scripts/publish-github-release"), b, 0o755)
	b, _ = os.ReadFile("../../scripts/check-release-binaries")
	os.WriteFile(filepath.Join(f.repo, "scripts/check-release-binaries"), b, 0o755)
	// The release directory release.sh builds: relative, signed, summed.
	dir := releaseDir(t, newSigner(t), version, fogd, false)
	rel := filepath.Join(f.repo, "bin/release")
	os.MkdirAll(rel, 0o755)
	entries, _ := os.ReadDir(dir)
	var sums []string
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		os.WriteFile(filepath.Join(rel, e.Name()), b, 0o644)
		h := sha256.Sum256(b)
		sums = append(sums, hex.EncodeToString(h[:])+"  "+e.Name())
	}
	sort.Strings(sums)
	os.WriteFile(filepath.Join(rel, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644)

	writeStub(t, f.stubs, "gh", fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1 $2" in
  "api user") [ -n "${STUB_GH_USER:-}" ] && echo "$STUB_GH_USER" || exit 1 ;;
  "release view")
    case "$3" in
      --*) [ -n "${STUB_GH_LATEST:-}" ] && echo "$STUB_GH_LATEST" || exit 1 ;; # no tag: GitHub's latest
      *) case "$*" in
           *--json*) ls bin/release ;;
           *) [ "${STUB_GH_EXISTS:-0}" = 1 ] || exit 1 ;;
         esac ;;
    esac ;;
esac
`, f.log))
	// signtool, as the script calls it through go run: verify is stubbed;
	// compare is the real one, run in this repo, so "latest" is decided by
	// the updater's own order.
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	repo, _ := filepath.Abs("../..")
	writeStub(t, f.stubs, "go", fmt.Sprintf(`#!/bin/sh
case "$*" in
  *" compare "*) cd %q && exec %q "$@" ;;
  "version -m "*) exec %q "$@" ;;
esac
[ "${STUB_SIG_OK:-1}" = 1 ]
`, repo, realGo, realGo))
	return f
}

func (f *publishFixture) run(t *testing.T, env []string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(f.repo, "scripts/publish-github-release")}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+f.stubs+":"+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	log, _ := os.ReadFile(f.log)
	os.Remove(f.log)
	return string(out), string(log), err
}

func TestPublishGitHubReleaseNeedsTheFogappAccount(t *testing.T) {
	f := newPublishFixture(t)
	for _, who := range []string{"", "someone-else"} {
		out, log, err := f.run(t, []string{"STUB_GH_USER=" + who}, "v1.4.0", "--publish", "abc123")
		if err == nil || !strings.Contains(out, "gh auth login --hostname github.com --git-protocol ssh --web") || !strings.Contains(out, "'fogapp'") {
			t.Errorf("gh as %q: not refused with instructions (err %v):\n%s", who, err, out)
		}
		if strings.Contains(log, "release create") || strings.Contains(log, "release upload") {
			t.Errorf("gh as %q: published anyway:\n%s", who, log)
		}
	}
}

func TestPublishGitHubReleaseCreatesThenUpdates(t *testing.T) {
	f := newPublishFixture(t)
	env := []string{"STUB_GH_USER=fogapp"}
	out, log, err := f.run(t, env, "v1.4.0", "--check")
	if err != nil || strings.Contains(log, "release ") {
		t.Fatalf("--check: %v (gh log %q)\n%s", err, log, out)
	}

	out, log, err = f.run(t, env, "v1.4.0", "--publish", "abc123")
	if err != nil {
		t.Fatalf("--publish: %v\n%s", err, out)
	}
	if !strings.Contains(log, "release create v1.4.0 --repo fogapp/fog --title v1.4.0") || !strings.Contains(log, "--target abc123") {
		t.Errorf("no release created at the sync commit:\n%s", log)
	}
	for _, f := range []string{"latest.json", "latest.json.sig", "install.sh", "fogd.service", "fogd-rollback", "SHA256SUMS", "fogd-linux-amd64", "fogd-linux-arm64", "flash.sh"} {
		if !strings.Contains(log, "bin/release/"+f) {
			t.Errorf("%s not uploaded:\n%s", f, log)
		}
	}

	// A re-run replaces assets on the existing release; never a second one.
	out, log, err = f.run(t, append(env, "STUB_GH_EXISTS=1"), "v1.4.0", "--publish", "abc123")
	if err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
	if strings.Contains(log, "release create") || !strings.Contains(log, "release upload v1.4.0 --repo fogapp/fog --clobber") {
		t.Errorf("re-run didn't update in place:\n%s", log)
	}
}

func TestPublishGitHubReleaseRefusesAWrongDirectory(t *testing.T) {
	env := []string{"STUB_GH_USER=fogapp"}
	t.Run("another version", func(t *testing.T) {
		f := newPublishFixture(t)
		if out, _, err := f.run(t, env, "v1.5.0", "--check"); err == nil || !strings.Contains(out, "holds v1.4.0") {
			t.Errorf("err %v:\n%s", err, out)
		}
	})
	t.Run("not signed by the pinned key", func(t *testing.T) {
		f := newPublishFixture(t)
		if out, _, err := f.run(t, append(env, "STUB_SIG_OK=0"), "v1.4.0", "--check"); err == nil || !strings.Contains(out, "isn't signed") {
			t.Errorf("err %v:\n%s", err, out)
		}
	})
	t.Run("absolute URLs", func(t *testing.T) {
		f := newPublishFixture(t)
		p := filepath.Join(f.repo, "bin/release/latest.json")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.Replace(string(b), `"url": "`, `"url": "http://gitea.lan/x/`, 1)), 0o644)
		if out, _, err := f.run(t, env, "v1.4.0", "--check"); err == nil || !strings.Contains(out, "absolute URLs") {
			t.Errorf("err %v:\n%s", err, out)
		}
	})
	// A file swapped in bin/release/, with SHA256SUMS rewritten to match:
	// the signed manifest's digest is what decides, and it refuses.
	t.Run("a file changed, SHA256SUMS rewritten to match", func(t *testing.T) {
		f := newPublishFixture(t)
		rel := filepath.Join(f.repo, "bin/release")
		evil := "#!/bin/sh\necho owned\n"
		os.WriteFile(filepath.Join(rel, "install.sh"), []byte(evil), 0o644)
		sums, _ := os.ReadFile(filepath.Join(rel, "SHA256SUMS"))
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
			if strings.HasSuffix(l, "  install.sh") {
				l = sum(evil) + "  install.sh"
			}
			lines = append(lines, l)
		}
		os.WriteFile(filepath.Join(rel, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		out, log, err := f.run(t, env, "v1.4.0", "--publish", "abc123")
		if err == nil || !strings.Contains(out, "install.sh isn't the file the signed manifest names") || strings.Contains(log, "release create") {
			t.Errorf("err %v:\n%s", err, out)
		}
	})
	t.Run("a file the manifest doesn't name", func(t *testing.T) {
		f := newPublishFixture(t)
		os.WriteFile(filepath.Join(f.repo, "bin/release/extra.sh"), []byte("#!/bin/sh\n"), 0o644)
		out, log, err := f.run(t, env, "v1.4.0", "--publish", "abc123")
		if err == nil || !strings.Contains(out, "extra.sh isn't named by the signed manifest") || strings.Contains(log, "release create") {
			t.Errorf("err %v:\n%s", err, out)
		}
	})
	t.Run("SHA256SUMS disagrees with the manifest", func(t *testing.T) {
		f := newPublishFixture(t)
		p := filepath.Join(f.repo, "bin/release/SHA256SUMS")
		sums, _ := os.ReadFile(p)
		bad := strings.Replace(string(sums), "  fogd.service", "  fogd-rollback", 1)
		os.WriteFile(p, []byte(bad), 0o644)
		if out, _, err := f.run(t, env, "v1.4.0", "--check"); err == nil || !strings.Contains(out, "SHA256SUMS") {
			t.Errorf("err %v:\n%s", err, out)
		}
	})
}

// What GitHub marks latest is what installs read. A pre-release is never
// latest; an older release re-run doesn't take latest from a newer one; a
// newer or first release is latest.
func TestPublishGitHubReleaseDecidesLatest(t *testing.T) {
	cases := []struct {
		name, version, ghLatest, want, notWant string
	}{
		{"a first release", "v1.4.0", "", "--prerelease=false --latest", "--latest=false"},
		{"a newer release", "v1.4.0", "v1.3.9", "--prerelease=false --latest", "--latest=false"},
		{"an older release, re-run", "v1.4.0", "v1.10.0", "--latest=false", "--latest --"},
		{"a pre-release", "v1.5.0-rc.1", "v1.4.0", "--prerelease --latest=false", "--latest --"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublishFixtureAt(t, tc.version)
			for _, exists := range []string{"0", "1"} { // create, then re-run (edit)
				out, log, err := f.run(t, []string{"STUB_GH_USER=fogapp", "STUB_GH_LATEST=" + tc.ghLatest, "STUB_GH_EXISTS=" + exists},
					tc.version, "--publish", "abc123")
				if err != nil {
					t.Fatalf("publish: %v\n%s", err, out)
				}
				// The notes span lines; the flags follow them on the line
				// that names the pre-release state.
				var line string
				for _, l := range strings.Split(log, "\n") {
					if strings.Contains(l, "--prerelease") {
						line = l
					}
				}
				if !strings.Contains(line, tc.want) || strings.Contains(line+" ", tc.notWant+" ") {
					t.Errorf("exists=%s: gh %q, want %q and not %q", exists, line, tc.want, tc.notWant)
				}
			}
		})
	}
}

// The last gate before GitHub: a binary that names the private commit, or
// the build machine, isn't uploaded.
func TestPublishGitHubReleaseRefusesAStampedBinary(t *testing.T) {
	_, build := goModule(t)
	b, _ := os.ReadFile(build("-ldflags=-s -w"))
	f := newPublishFixtureWith(t, "v1.4.0", string(b))
	out, log, err := f.run(t, []string{"STUB_GH_USER=fogapp"}, "v1.4.0", "--publish", "abc123")
	if err == nil || !strings.Contains(out, "version-control info or a local path") || strings.Contains(log, "release create") {
		t.Fatalf("a vcs-stamped binary was published (err %v):\n%s\n%s", err, out, log)
	}
}
