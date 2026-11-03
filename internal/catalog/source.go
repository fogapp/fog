package catalog

import (
	"encoding/json"
	"fmt"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
)

// Where the household's own apps (Entry.FirstParty) are cloned from is not
// the catalog's to know: it is this fog's setting. An entry's repository is,
// first match wins:
//
//  1. the operator's override for that repository (SettingSourceOverrides),
//  2. the entry's published repository (Entry.PublicRepo), for an app that
//     is published,
//  3. the operator's base (SettingSourceBase) + "<repo>.git".
//
// An entry none of these answers has no source on this fog, and the store
// leaves it out: nobody is shown an app that can't be installed.
const (
	// SettingSourceBase is the clone URL prefix of the household's own app
	// repositories, e.g. "ssh://git@git.example.com/you/".
	SettingSourceBase = "catalog_source_base"
	// SettingSourceOverrides is a JSON object of repository name (an
	// entry's FirstParty, e.g. "fit") to its full clone URL.
	SettingSourceOverrides = "catalog_source_overrides"
)

// Sources is the operator's answer to where first-party apps come from.
type Sources struct {
	Base      string
	Overrides map[string]string
}

// ParseSources reads the two settings. A malformed overrides value is
// ignored rather than fatal: the settings API refuses one, so it can only
// come from a hand-edited database.
func ParseSources(base, overrides string) Sources {
	s := Sources{Base: strings.TrimSpace(base)}
	if overrides != "" {
		_ = json.Unmarshal([]byte(overrides), &s.Overrides)
	}
	return s
}

// RepoFor is the clone URL of a first-party repository, or "" when this
// fog has no source for it.
func (s Sources) RepoFor(e Entry) string {
	if e.FirstParty == "" {
		return e.Spec.Source.Repo
	}
	if r := strings.TrimSpace(s.Overrides[e.FirstParty]); r != "" {
		return r
	}
	if e.PublicRepo != "" {
		return e.PublicRepo
	}
	if s.Base != "" {
		return s.Base + e.FirstParty + ".git"
	}
	return ""
}

// Resolve fills each first-party entry's repository (its spec's source and
// the default of its "repo" option) from sources. Entries keep their order;
// one with no source is left with an empty repository, which Available
// reports.
func Resolve(entries []Entry, sources Sources) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		if e.FirstParty != "" {
			repo := sources.RepoFor(e)
			e.Spec.Source.Repo = repo
			opts := make([]Option, len(e.Options))
			copy(opts, e.Options)
			for j := range opts {
				if opts[j].Target == "repo" {
					opts[j].Default = repo
				}
			}
			e.Options = opts
		}
		out[i] = e
	}
	return out
}

// Available reports whether a resolved entry can be installed from this
// fog: everything but a first-party app with no source.
func Available(e Entry) bool {
	return e.FirstParty == "" || e.Spec.Source.Repo != ""
}

// ValidBase checks a base setting: empty, or a prefix that makes a clone
// URL fog accepts once a repository name is appended. It must end in "/"
// or ":" so the name lands as its own path segment.
func ValidBase(v string) error {
	if v == "" {
		return nil
	}
	if !strings.HasSuffix(v, "/") && !strings.HasSuffix(v, ":") {
		return fmt.Errorf("the base must end in \"/\", e.g. ssh://git@git.example.com/you/")
	}
	return apps.ValidateGitRepo(v + "app.git")
}

// ValidOverrides checks an overrides setting: empty, or a JSON object of
// repository name to a clone URL fog accepts.
func ValidOverrides(v string) error {
	if v == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return fmt.Errorf("overrides must be a JSON object of repository name to clone URL")
	}
	for name, repo := range m {
		if err := apps.ValidateGitRepo(repo); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
