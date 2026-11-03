package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The store offers the household's own apps only once this fog knows where
// they come from: with no base, /api/catalog leaves them out and counts
// them; with one, they are back with their repositories filled in.
func TestCatalogFollowsTheSourceSetting(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)
	type entry struct {
		ID   string `json:"id"`
		Spec struct {
			Source struct {
				Type string `json:"type"`
				Repo string `json:"repo"`
			} `json:"source"`
		} `json:"spec"`
	}
	read := func() (map[string]entry, int) {
		t.Helper()
		w := do(t, h, "GET", "/api/catalog", "", withCookie(c))
		if w.Code != http.StatusOK {
			t.Fatalf("catalog: %d %s", w.Code, w.Body)
		}
		var body struct {
			Entries   []entry `json:"entries"`
			Unsourced int     `json:"unsourced"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]entry{}
		for _, e := range body.Entries {
			out[e.ID] = e
		}
		return out, body.Unsourced
	}

	got, unsourced := read()
	if _, ok := got["manor"]; ok || unsourced == 0 {
		t.Fatalf("no base: manor shown=%v, unsourced=%d", ok, unsourced)
	}
	if _, ok := got["sonarr"]; !ok {
		t.Fatal("image apps must stay")
	}
	for id, e := range got {
		if e.Spec.Source.Type == "git" && e.Spec.Source.Repo == "" {
			t.Errorf("%s offered with no repository", id)
		}
	}

	for _, bad := range []string{`{"catalog_source_base":"https://github.com/you/"}`, `{"catalog_source_overrides":"{\"fit\":\"nope\"}"}`} {
		if w := do(t, h, "PUT", "/api/settings", bad, withCookie(c)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "app source") {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	body := `{"catalog_source_base":"ssh://git@git.example.com/you/","catalog_source_overrides":"{\"fit\":\"ssh://git@github.com/me/fit.git\"}"}`
	if w := do(t, h, "PUT", "/api/settings", body, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("set source: %d %s", w.Code, w.Body)
	}
	got, unsourced = read()
	if unsourced != 0 {
		t.Errorf("unsourced = %d with a base", unsourced)
	}
	if r := got["manor"].Spec.Source.Repo; r != "ssh://git@git.example.com/you/manor.git" {
		t.Errorf("manor repo %q", r)
	}
	if r := got["fit-collector"].Spec.Source.Repo; r != "ssh://git@github.com/me/fit.git" {
		t.Errorf("fit-collector repo %q", r)
	}
}
