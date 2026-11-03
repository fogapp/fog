package selfupdate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/publish-public.sh, dry runs only, against scratch repos: a private
// clone with an origin and a tag, and a separate-history public repo. It
// must commit under the public identity this clone names (and refuse
// without one), and its scan must catch a secret inside a binary file, a
// rename to .env, and a large stray binary.

type publicFixture struct {
	t      *testing.T
	repo   string
	public string
}

func newPublicFixture(t *testing.T) *publicFixture {
	t.Helper()
	skipShell(t)
	root := t.TempDir()
	f := &publicFixture{t: t, repo: filepath.Join(root, "private"), public: filepath.Join(root, "public.git")}
	os.MkdirAll(filepath.Join(f.repo, "scripts"), 0o755)
	b, err := os.ReadFile("../../scripts/publish-public.sh")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.repo, "scripts/publish-public.sh"), b, 0o755)
	// The GitHub release step has its own tests; here it only says yes.
	os.WriteFile(filepath.Join(f.repo, "scripts/publish-github-release"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("fog\n"), 0o644)

	f.git(root, "init", "-q", "--bare", "origin.git")
	f.git(root, "init", "-q", "--bare", "public.git")
	f.git(f.repo, "init", "-q", "-b", "main")
	f.git(f.repo, "config", "user.name", "Private Person")
	f.git(f.repo, "config", "user.email", "private@home.example")
	f.git(f.repo, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	f.commit("v1.0.0 tree")
	f.git(f.repo, "tag", "v1.0.0")
	f.git(f.repo, "push", "-q", "origin", "main", "--tags")

	// The public repo: its own history, made by the first publish.
	seed := filepath.Join(root, "seed")
	f.git(root, "init", "-q", "-b", "main", "seed")
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("fog\n"), 0o644)
	f.git(seed, "add", "-A")
	f.git(seed, "-c", "user.name=fog", "-c", "user.email=fog@example.com", "commit", "-q", "-m", "Initial public release")
	f.git(seed, "push", "-q", f.public, "main")
	return f
}

func (f *publicFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit records the working tree as the next release.
func (f *publicFixture) commit(msg string) {
	f.git(f.repo, "add", "-A")
	f.git(f.repo, "commit", "-q", "-m", msg)
}

// release tags HEAD as v1.0.1 and pushes it, as /ship leaves things.
func (f *publicFixture) release() {
	f.git(f.repo, "tag", "-f", "v1.0.1")
	f.git(f.repo, "push", "-q", "-f", "origin", "main", "--tags")
}

func (f *publicFixture) identity() {
	f.git(f.repo, "config", "fog.publicAuthorName", "fog project")
	f.git(f.repo, "config", "fog.publicAuthorEmail", "releases@fog.example")
}

func (f *publicFixture) run(args ...string) (string, error) {
	f.t.Helper()
	key := filepath.Join(f.t.TempDir(), "key")
	os.WriteFile(key, []byte("k"), 0o600)
	cmd := exec.Command("sh", append([]string{filepath.Join(f.repo, "scripts/publish-public.sh")}, args...)...)
	cmd.Env = append(os.Environ(), "FOG_PUBLIC_URL="+f.public, "FOG_PUBLIC_KEY="+key)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestPublishPublicUsesThePublicIdentity(t *testing.T) {
	f := newPublicFixture(t)
	os.WriteFile(filepath.Join(f.repo, "main.go"), []byte("package main\n"), 0o644)
	f.commit("a change")
	f.release()

	out, err := f.run()
	if err == nil || !strings.Contains(out, "git config fog.publicAuthorName") || strings.Contains(out, "dry run ok") {
		t.Fatalf("published with no public identity set (err %v):\n%s", err, out)
	}

	f.identity()
	out, err = f.run()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	sha := strings.TrimSuffix(strings.Fields(out[strings.LastIndex(out, "would publish v1.0.1 as ")+len("would publish v1.0.1 as "):])[0], ",")
	obj := f.git(f.repo, "cat-file", "-p", sha)
	for _, want := range []string{"author fog project <releases@fog.example>", "committer fog project <releases@fog.example>", "+0000"} {
		if !strings.Contains(obj, want) {
			t.Errorf("sync commit lacks %q:\n%s", want, obj)
		}
	}
	if strings.Contains(obj, "Private Person") || strings.Contains(obj, "private@home.example") {
		t.Errorf("the personal identity reached the public commit:\n%s", obj)
	}
}

func TestPublishPublicScanCatchesWhatItMissed(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(f *publicFixture)
	}{
		{"a key inside a binary file", "secret scan hit", func(f *publicFixture) {
			blob := append([]byte{0, 1, 2, 0}, []byte("AKIA"+strings.Repeat("Q", 16))...)
			os.WriteFile(filepath.Join(f.repo, "asset.bin"), append(blob, 0, 0), 0o644)
		}},
		{"a rename to .env", "secret scan hit", func(f *publicFixture) {
			f.git(f.repo, "mv", "README.md", ".env")
		}},
		{"a large stray binary", "binary files over", func(f *publicFixture) {
			os.WriteFile(filepath.Join(f.repo, "fog-locker"), append([]byte{0x7f, 'E', 'L', 'F', 0}, bytes.Repeat([]byte{0}, 2<<20)...), 0o755)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicFixture(t)
			f.identity()
			tc.change(f)
			f.commit("change")
			f.release()
			out, err := f.run()
			if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, "dry run ok") {
				t.Fatalf("not refused (err %v):\n%s", err, out)
			}
		})
	}
	// A large font is a web asset, and passes.
	f := newPublicFixture(t)
	f.identity()
	os.MkdirAll(filepath.Join(f.repo, "assets"), 0o755)
	os.WriteFile(filepath.Join(f.repo, "assets/Inter.woff2"), append([]byte{'w', 'O', 'F', '2', 0}, bytes.Repeat([]byte{1, 0}, 1<<20)...), 0o644)
	f.commit("font")
	f.release()
	if out, err := f.run(); err != nil || !strings.Contains(out, "dry run ok") {
		t.Fatalf("an allowlisted font was refused (err %v):\n%s", err, out)
	}
}
