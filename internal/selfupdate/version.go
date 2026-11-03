package selfupdate

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// A release version is vMAJOR.MINOR.PATCH, with an optional -prerelease.
// Updates only ever move forward: a manifest naming an older (or the same)
// version is never installed, so a mirror replaying an old, validly signed
// manifest can't walk a server back onto a release with a known bug.

type version struct {
	nums [3]int
	pre  string
}

func parseVersion(s string) (version, bool) {
	var v version
	rest, ok := strings.CutPrefix(s, "v")
	if !ok {
		return v, false
	}
	rest, _, _ = strings.Cut(rest, "+") // build metadata has no precedence
	core, pre, _ := strings.Cut(rest, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		// Semver: numeric identifiers carry no leading zeros.
		if p == "" || len(p) > 9 || strings.TrimLeft(p, "0123456789") != "" || (len(p) > 1 && p[0] == '0') {
			return v, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v.nums[i] = n
	}
	if strings.Contains(rest, "-") {
		if pre == "" {
			return v, false
		}
		for _, id := range strings.Split(pre, ".") {
			if id == "" || strings.Trim(id, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-") != "" {
				return v, false
			}
			if isNumericIdent(id) && len(id) > 1 && id[0] == '0' {
				return v, false
			}
		}
	}
	v.pre = pre
	return v, true
}

// compareVersions orders a and b: -1, 0 or 1, by semver 2.0.0 precedence
// (section 11): the numbers, then a pre-release sorts before its release,
// and two pre-releases compare identifier by identifier — numeric ones as
// numbers (rc.9 < rc.10), numeric before alphanumeric, text in ASCII order,
// and a shorter list before a longer one it prefixes (alpha < alpha.1).
func compareVersions(a, b version) int {
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			if a.nums[i] < b.nums[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	}
	as, bs := strings.Split(a.pre, "."), strings.Split(b.pre, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func isNumericIdent(s string) bool {
	return s != "" && strings.TrimLeft(s, "0123456789") == ""
}

func compareIdent(a, b string) int {
	an, bn := isNumericIdent(a), isNumericIdent(b)
	switch {
	case an && bn:
		// No leading zeros in valid semver, but compare by value anyway:
		// strip them, then the longer number is the larger.
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

// EnvFromUnversioned lets a fogd whose own version isn't a release (a dev
// build, a hand-stamped one) take a release anyway. Without it such a fogd
// has no place in the order, so it can't tell an update from a downgrade
// and takes nothing; with it, set by hand on the unit, its version counts
// as the lowest there is.
const EnvFromUnversioned = "FOG_UPDATE_FROM_UNVERSIONED"

// isNewer reports whether candidate is an upgrade from current.
func isNewer(candidate, current string) bool {
	c, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	cur, ok := parseVersion(current)
	if !ok {
		return os.Getenv(EnvFromUnversioned) == "1"
	}
	return compareVersions(c, cur) > 0
}

// ValidVersion reports whether s is a release version fog orders —
// release.sh checks a new tag with it (signtool version).
func ValidVersion(s string) bool {
	_, ok := parseVersion(s)
	return ok
}

// CompareVersions orders two release versions by semver precedence: -1, 0
// or 1. publish-github-release uses it (signtool compare) to decide what
// GitHub marks latest.
func CompareVersions(a, b string) (int, error) {
	va, ok := parseVersion(a)
	if !ok {
		return 0, fmt.Errorf("%q isn't a release version", a)
	}
	vb, ok := parseVersion(b)
	if !ok {
		return 0, fmt.Errorf("%q isn't a release version", b)
	}
	return compareVersions(va, vb), nil
}
