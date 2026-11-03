package build

import (
	"fmt"
	"strings"
)

// ValidateBranch checks a branch name the way `git check-ref-format --branch`
// does, and then some: a branch reaches git's command line as an argument,
// so a name git would read as an option (a leading "-") or one that is not a
// ref at all is refused before any git runs.
//
// The rules (git-check-ref-format(1)), applied to refs/heads/<branch>: no
// component starts with "." or ends with ".lock"; no ".."; no control
// characters, space, "~", "^", ":", "?", "*", "[" or "\"; no leading,
// trailing or doubled "/"; no trailing "."; no "@{"; not "@". HEAD is not a
// branch either.
func ValidateBranch(branch string) error {
	bad := func(why string) error { return fmt.Errorf("branch %q is not a valid git branch name: %s", branch, why) }
	switch {
	case branch == "":
		return bad("it is empty")
	case len(branch) > 255:
		return bad("it is longer than 255 bytes")
	case strings.HasPrefix(branch, "-"):
		return bad("it starts with \"-\"")
	case branch == "@" || branch == "HEAD":
		return bad("it names no branch")
	case strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.Contains(branch, "//"):
		return bad("it has a leading, trailing or doubled \"/\"")
	case strings.HasSuffix(branch, "."):
		return bad("it ends with \".\"")
	case strings.Contains(branch, ".."):
		return bad("it contains \"..\"")
	case strings.Contains(branch, "@{"):
		return bad("it contains \"@{\"")
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return bad(fmt.Sprintf("it contains %q", r))
		}
	}
	for _, c := range strings.Split(branch, "/") {
		if strings.HasPrefix(c, ".") {
			return bad("a component starts with \".\"")
		}
		if strings.HasSuffix(c, ".lock") {
			return bad("a component ends with \".lock\"")
		}
	}
	return nil
}

// validateCommit checks a commit to check out is a hex object name, so it
// can never reach git as an option or a revision expression.
func validateCommit(commit string) error {
	if len(commit) < 7 || len(commit) > 64 {
		return fmt.Errorf("%q is not a commit sha", commit)
	}
	for _, r := range commit {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return fmt.Errorf("%q is not a commit sha", commit)
		}
	}
	return nil
}

// validateRepoArg refuses a repository that git would read as an option. The
// spec's own check (apps.validateGitRepo) is stricter; this one guards the
// command line whatever reaches it.
func validateRepoArg(repo string) error {
	if repo == "" || strings.HasPrefix(repo, "-") {
		return fmt.Errorf("repository %q is not a repository URL", repo)
	}
	return nil
}
