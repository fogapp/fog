package apps

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/build"
)

const glassRepoYAML = `
gettingStarted: hi
integrations:
  offers:
    - id: telemetry
      title: Telemetry
      env:
        GLASS_URL: "{{url}}"
      secrets: [GLASS_API_KEY]
    - id: read
      title: Read access
      consent: true
      env: { GLASS_READ_URL: "{{url}}" }
      secrets: [GLASS_READ_TOKEN]
  wants:
    - offer: hub/calendar
      as: { HUB_TOKEN: GLASS_HUB_TOKEN }
`

// A repo's fog.yaml, as the build reads it, becomes the declaration fog
// records at deploy.
func TestDeclaredFromRepoManifest(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "builds", "glass", "repo")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "fog.yaml"), []byte(glassRepoYAML), 0o644)
	m := (&build.Builder{DataDir: dir}).ReadRepoManifest("glass")

	d, err := DeclaredFrom(m.Integrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Offers) != 2 || !d.Offers[1].Consent || d.Offers[0].Env["GLASS_URL"] != URLTemplate ||
		!slices.Equal(d.Offers[1].Secrets, []string{"GLASS_READ_TOKEN"}) {
		t.Fatalf("offers: %+v", d.Offers)
	}
	if len(d.Wants) != 1 || d.Wants[0].As["HUB_TOKEN"] != "GLASS_HUB_TOKEN" {
		t.Fatalf("wants: %+v", d.Wants)
	}
	if again := ParseDeclared(d.JSON()); again == nil || len(again.Offers) != 2 {
		t.Fatalf("round trip: %+v", again)
	}

	if d, err := DeclaredFrom(nil); d != nil || err != nil || d.JSON() != "" {
		t.Fatalf("no block: %v %v", d, err)
	}
	for name, raw := range map[string]map[string]any{
		"misspelt field": {"offers": []any{map[string]any{"id": "x", "title": "X", "secret": []any{"K"}}}},
		"bad offer":      {"offers": []any{map[string]any{"id": "x", "title": "X"}}},
		"bad want":       {"wants": []any{map[string]any{"offer": "glass"}}},
	} {
		if _, err := DeclaredFrom(raw); err == nil || !strings.HasPrefix(err.Error(), "integrations: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The spec is the operator's: its connection's address and any offer or
// want it makes under the same name win over the repo's.
func TestEffectiveConnectionAndWants(t *testing.T) {
	d := &Declared{
		Offers: []Offer{{ID: "telemetry", Title: "From the repo", Secrets: []string{"K"}}, {ID: "read", Title: "Read", Secrets: []string{"R"}}},
		Wants:  []Want{{Offer: "hub/calendar"}, {Offer: "glass/read", As: map[string]string{"R": "FROM_REPO"}}},
	}
	bare := &AppSpec{Routes: []Route{{Port: 3000}}}
	c := EffectiveConnection(bare, d)
	if c == nil || c.Port != 3000 || len(c.Offers) != 2 || c.ConnectionURL("glass") != "http://glass:3000" {
		t.Fatalf("no connection in the spec: %+v", c)
	}
	if bare.Connection != nil {
		t.Fatal("the spec must not be changed")
	}

	edited := &AppSpec{Connection: &Connection{Port: 80, Offers: []Offer{{ID: "telemetry", Title: "Operator's", Secrets: []string{"K"}}}},
		Wants: []Want{{Offer: "glass/read", As: map[string]string{"R": "MINE"}}}}
	c = EffectiveConnection(edited, d)
	if c.Port != 80 || len(c.Offers) != 2 || c.Offer("telemetry").Title != "Operator's" || len(edited.Connection.Offers) != 1 {
		t.Fatalf("spec offers win, spec untouched: %+v", c)
	}
	w := EffectiveWants(edited, d)
	if len(w) != 2 || FindWant(w, "glass", "read").Name("R") != "MINE" || FindWant(w, "hub", "calendar") == nil {
		t.Fatalf("wants: %+v", w)
	}

	d.Wants = append(d.Wants, Want{App: "sabnzbd"}, Want{App: "glass", Capabilities: []string{"from the repo"}})
	edited.Wants = append(edited.Wants, Want{App: "glass"})
	w = EffectiveWants(edited, d)
	if len(w) != 4 || w[1].App != "glass" || len(w[1].Capabilities) != 0 || w[3].Provider() != "sabnzbd" || w[3].OfferID() != "" {
		t.Fatalf("app wants: %+v", w)
	}

	if EffectiveConnection(edited, nil) != edited.Connection || EffectiveConnection(bare, nil) != nil {
		t.Fatal("nothing declared: the spec's connection as it is")
	}
}
