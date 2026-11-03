package catalog

import (
	"testing"
)

// exampleSources stands in for an operator's setting wherever a test needs
// first-party entries installable.
var exampleSources = Sources{Base: "ssh://git@git.example.com/you/"}

func available(entries []Entry) map[string]Entry {
	out := map[string]Entry{}
	for _, e := range entries {
		if Available(e) {
			out[e.ID] = e
		}
	}
	return out
}

func repoDefault(e Entry) string {
	for _, o := range e.Options {
		if o.Target == "repo" {
			return o.Default
		}
	}
	return ""
}

// A fog nobody has told where the household's repositories live offers
// none of them: no stranger sees an app that can't install.
func TestNoSourceHidesFirstPartyEntries(t *testing.T) {
	got := available(Resolve(Entries(), Sources{}))
	for _, e := range Entries() {
		_, shown := got[e.ID]
		if e.FirstParty != "" && shown {
			t.Errorf("%s shown with no source", e.ID)
		}
		if e.FirstParty == "" && !shown {
			t.Errorf("%s (an image app) hidden", e.ID)
		}
	}
	if len(got) == 0 {
		t.Fatal("nothing left in the store")
	}
}

func TestBaseFillsRepositories(t *testing.T) {
	got := available(Resolve(Entries(), exampleSources))
	for id, repo := range map[string]string{
		"manor":         "ssh://git@git.example.com/you/manor.git",
		"fit":           "ssh://git@git.example.com/you/fit.git",
		"fit-collector": "ssh://git@git.example.com/you/fit.git",
	} {
		e, ok := got[id]
		if !ok {
			t.Fatalf("%s hidden with a base set", id)
		}
		if e.Spec.Source.Repo != repo || repoDefault(e) != repo {
			t.Errorf("%s: repo %q, option default %q, want %q", id, e.Spec.Source.Repo, repoDefault(e), repo)
		}
	}
}

// A published app installs with no setting; an override beats both.
func TestPublicRepoAndOverrides(t *testing.T) {
	entries := Entries()
	for i := range entries {
		if entries[i].ID == "newtab" {
			entries[i].PublicRepo = "ssh://git@github.com/fogapp/newtab.git"
		}
	}
	got := available(Resolve(entries, Sources{}))
	if e, ok := got["newtab"]; !ok || e.Spec.Source.Repo != "ssh://git@github.com/fogapp/newtab.git" {
		t.Fatalf("a published entry with no base: %+v", e.Spec.Source)
	}
	if _, ok := got["manor"]; ok {
		t.Fatal("an unpublished entry shown with no base")
	}
	src := ParseSources("ssh://git@git.example.com/you/", `{"newtab":"ssh://git@git.example.com/fork/newtab.git","manor":"git@other:me/manor.git"}`)
	got = available(Resolve(entries, src))
	if r := got["newtab"].Spec.Source.Repo; r != "ssh://git@git.example.com/fork/newtab.git" {
		t.Errorf("override beats the public repo: got %q", r)
	}
	if r := got["manor"].Spec.Source.Repo; r != "git@other:me/manor.git" {
		t.Errorf("override beats the base: got %q", r)
	}
	if r := got["hub"].Spec.Source.Repo; r != "ssh://git@git.example.com/you/hub.git" {
		t.Errorf("base: got %q", r)
	}
}

// Resolving never writes through to the catalog's own entries.
func TestResolveCopies(t *testing.T) {
	entries := Entries()
	Resolve(entries, exampleSources)
	for _, e := range entries {
		if e.FirstParty != "" && (e.Spec.Source.Repo != "" || repoDefault(e) != "") {
			t.Fatalf("%s: Resolve mutated its input", e.ID)
		}
	}
}

func TestSourceSettingsValidate(t *testing.T) {
	for _, ok := range []string{"", "ssh://git@git.example.com/you/", "git@git.example.com:you/", "git@host:"} {
		if err := ValidBase(ok); err != nil {
			t.Errorf("ValidBase(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"ssh://git@git.example.com/you", "https://github.com/you/", "ssh://host/you/", "a b/"} {
		if ValidBase(bad) == nil {
			t.Errorf("ValidBase(%q) passed", bad)
		}
	}
	if err := ValidOverrides(`{"fit":"ssh://git@h/x/fit.git"}`); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`[1]`, `{"fit":"https://h/x/fit"}`, `{"fit":3}`} {
		if ValidOverrides(bad) == nil {
			t.Errorf("ValidOverrides(%s) passed", bad)
		}
	}
}
