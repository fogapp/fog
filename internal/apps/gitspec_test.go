package apps

import "testing"

// The attack at save: a spec whose branch or repository git (or ssh) would
// read as an option. Refused before it is stored, so no deploy, update
// check or Test Connection ever hands it to git.
func TestValidateRefusesGitOptionInjection(t *testing.T) {
	git := func(repo, branch string) *AppSpec {
		return &AppSpec{Source: Source{Type: "git", Repo: repo, Branch: branch}}
	}
	for _, s := range []*AppSpec{
		git("ssh://git@host/o/r.git", "--upload-pack=touch /tmp/x"),
		git("ssh://git@host/o/r.git", "-b"),
		git("ssh://git@host/o/r.git", "a..b"),
		git("ssh://git@host/o/r.git", "x.lock"),
		git("ssh://git@host/o/r.git", "a b"),
		git("ssh://git@-oProxyCommand=x/o/r.git", "main"),
		git("ssh://-oProxyCommand=x@host/o/r.git", "main"),
		git("-x@host:o/r.git", "main"),
		git("git@-x:o/r.git", "main"),
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("accepted repo %q branch %q", s.Source.Repo, s.Source.Branch)
		}
	}
	for _, s := range []*AppSpec{
		git("ssh://git@host/o/r.git", ""),
		git("ssh://git@host/o/r.git", "feature/x"),
		git("git@host:o/r.git", "release-1.2"),
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("refused repo %q branch %q: %v", s.Source.Repo, s.Source.Branch, err)
		}
	}
}

func TestValidateRefusesDockerfileOutOfTheRepo(t *testing.T) {
	for _, d := range []string{"/etc/shadow", "../x", "a/../../x", "..", "./Dockerfile", "a//b", "-f"} {
		s := &AppSpec{Source: Source{Type: "git", Repo: "ssh://git@h/o/r.git", Dockerfile: d}}
		if err := s.Validate(); err == nil {
			t.Errorf("dockerfile %q accepted", d)
		}
	}
	for _, d := range []string{"Dockerfile", "docker/Dockerfile.prod"} {
		s := &AppSpec{Source: Source{Type: "git", Repo: "ssh://git@h/o/r.git", Dockerfile: d}}
		if err := s.Validate(); err != nil {
			t.Errorf("dockerfile %q refused: %v", d, err)
		}
	}
}
