package filesfed

import (
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
)

// filesfed names its own mode/scope constants (Go wants compile-time constants
// for its switch statements), but they must stay the shared consent-grammar
// vocabulary — the same terms hub uses and the conformance vectors lock. This
// asserts filesfed hasn't drifted from fed.Lens; a renamed or added value here
// without the same change to the shared vocabulary fails CI.
func TestFilesfedModeScopeMatchSharedVocabulary(t *testing.T) {
	assertSameSet(t, "mode", []string{ModeRead, ModeContribute}, lensValues(fed.LensModes))
	assertSameSet(t, "scope", []string{ScopeAll, ScopePicked, ScopeFiltered}, lensValues(fed.LensScopes))
	// And every value filesfed uses is a valid shared term.
	for _, m := range []string{ModeRead, ModeContribute} {
		if !fed.LensValid("mode", m) {
			t.Errorf("filesfed mode %q is not a shared lens term", m)
		}
	}
	for _, s := range []string{ScopeAll, ScopePicked, ScopeFiltered} {
		if !fed.LensValid("scope", s) {
			t.Errorf("filesfed scope %q is not a shared lens term", s)
		}
	}
}

// lensValues is the wire values of one shared axis.
func lensValues(terms []fed.LensTerm) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = t.Value
	}
	return out
}

func assertSameSet(t *testing.T, axis string, a, b []string) {
	t.Helper()
	set := func(xs []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		t.Fatalf("%s: filesfed has %d values, shared vocab has %d", axis, len(sa), len(sb))
	}
	for x := range sa {
		if !sb[x] {
			t.Errorf("%s: filesfed value %q is not in the shared vocabulary", axis, x)
		}
	}
}
