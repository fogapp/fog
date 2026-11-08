package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/apps"
)

// A household app's catalog entry is a copy of the manifest in its own repo
// (<repo>/<id>.fog.yaml). The copy is what a one-click install deploys, so
// when the two drift, a store install quietly differs from the app its
// author ships: newtab once lost its backups that way. Wherever the repos are
// checked out beside fog (or under $FOG_APP_REPOS), each entry must match its
// manifest field for field. Entries whose repo isn't here are skipped.
//
// The repository URL is resolved against the base the manifest names (it is
// a setting on each fog). Two other differences are by design and normalised away: Notes (the manifest
// keeps its own in fog.yaml's gettingStarted), and a secret the entry offers
// as an optional install option. The install pane leaves such a secret
// undeclared when it is left blank, so it never fails a deploy; a manifest
// can't do that, so it may list the secret (an import declares it, and its
// operator deletes it in fog if unused) or leave it out.
func TestFirstPartyEntriesMirrorTheirRepoManifests(t *testing.T) {
	root := os.Getenv("FOG_APP_REPOS")
	if root == "" {
		root = filepath.Join("..", "..", "..") // the fog checkout's parent
	}
	checked := 0
	for _, e := range ownEntries(t) {
		if e.FirstParty == "" {
			continue
		}
		path := filepath.Join(root, e.FirstParty, e.ID+".fog.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		checked++
		want := manifestSpec(t, path, raw)
		// Where the repo is cloned from is this fog's setting, not the
		// entry's: resolve it against the base the manifest's own URL uses.
		base, ok := strings.CutSuffix(want.Source.Repo, e.FirstParty+".git")
		if !ok {
			t.Errorf("%s: manifest repo %q does not name %s.git", e.ID, want.Source.Repo, e.FirstParty)
			continue
		}
		got := Resolve([]Entry{e}, Sources{Base: base})[0].Spec
		got.Notes, want.Notes = "", ""
		// reach is the installing household's approval of who the app calls
		// (apps.AppSpec.Reach); a repo's manifest doesn't approve its own.
		got.Reach = nil
		got.Secrets = withoutOptionalSecrets(got.Secrets, e.Options)
		want.Secrets = withoutOptionalSecrets(want.Secrets, e.Options)
		if g, w := specJSON(t, got), specJSON(t, want); g != w {
			t.Errorf("%s: catalog entry differs from %s\ncatalog:  %s\nmanifest: %s", e.ID, path, g, w)
		}
	}
	if checked == 0 {
		t.Skipf("no household app repo checked out under %s", root)
	}
}

// manifestSpec reads a manifest the way fog's import does: YAML to generic
// JSON, then into the spec.
func manifestSpec(t *testing.T, path string, raw []byte) apps.AppSpec {
	t.Helper()
	var generic map[string]any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	j, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var m struct {
		Spec apps.AppSpec `json:"spec"`
	}
	if err := json.Unmarshal(j, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m.Spec
}

func withoutOptionalSecrets(secrets []string, options []Option) []string {
	optional := map[string]bool{}
	for _, o := range options {
		if key, ok := strings.CutPrefix(o.Target, "secret:"); ok && !o.Required {
			optional[key] = true
		}
	}
	var out []string
	for _, s := range secrets {
		if !optional[s] {
			out = append(out, s)
		}
	}
	return out
}

func specJSON(t *testing.T, s apps.AppSpec) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
