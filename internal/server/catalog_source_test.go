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
	t.Parallel()

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
	if _, ok := got["fin"]; ok || unsourced == 0 {
		t.Fatalf("no base: fin shown=%v, unsourced=%d", ok, unsourced)
	}
	if _, ok := got["sonarr"]; !ok {
		t.Fatal("image apps must stay")
	}
	for id, e := range got {
		if e.Spec.Source.Type == "git" && e.Spec.Source.Repo == "" {
			t.Errorf("%s offered with no repository", id)
		}
	}

	for _, bad := range []string{`{"catalog_source_base":"http://github.com/you/"}`, `{"catalog_source_overrides":"{\"fit\":\"nope\"}"}`} {
		if w := do(t, h, "PUT", "/api/settings", bad, withCookie(c)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "app source") {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	body := `{"catalog_source_base":"ssh://git@git.example.com/you/","catalog_source_overrides":"{\"journal\":\"ssh://git@github.com/me/journal.git\"}"}`
	if w := do(t, h, "PUT", "/api/settings", body, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("set source: %d %s", w.Code, w.Body)
	}
	got, unsourced = read()
	if unsourced != 0 {
		t.Errorf("unsourced = %d with a base", unsourced)
	}
	if r := got["fin"].Spec.Source.Repo; r != "ssh://git@git.example.com/you/fin.git" {
		t.Errorf("fin repo %q", r)
	}
	if r := got["journal-collector"].Spec.Source.Repo; r != "ssh://git@github.com/me/journal.git" {
		t.Errorf("journal-collector repo %q", r)
	}
}

// The list of catalogs is a setting the API checks before it is stored,
// and what each catalog is and how it stands comes back with the store.
// Someone else's catalog is never trusted.
func TestCatalogList(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	c := setup(t, h)
	var body struct {
		Entries []struct {
			ID      string `json:"id"`
			Catalog string `json:"catalog"`
			Trusted bool   `json:"trusted"`
		} `json:"entries"`
		Enabled  []string `json:"enabled"`
		Catalogs []struct {
			Name    string `json:"name"`
			Trusted bool   `json:"trusted"`
			From    string `json:"from"`
			Apps    int    `json:"apps"`
		} `json:"catalogs"`
		CatalogKey string `json:"catalogKey"`
	}
	w := do(t, h, "GET", "/api/catalog", "", withCookie(c))
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Catalogs) != 2 || body.Catalogs[0].Name != "fog-apps" || !body.Catalogs[0].Trusted || body.Catalogs[1].From != "seed" {
		t.Fatalf("catalogs = %+v", body.Catalogs)
	}
	if !strings.HasPrefix(body.CatalogKey, "ssh-ed25519 ") {
		t.Errorf("catalog key %q", body.CatalogKey)
	}
	for _, e := range body.Entries {
		if e.Catalog == "" || !e.Trusted {
			t.Errorf("%s: catalog %q trusted %v", e.ID, e.Catalog, e.Trusted)
		}
	}
	if len(body.Enabled) == 0 {
		t.Error("nothing featured out of the box")
	}

	for _, bad := range []string{`[{"name":"x","url":"http://example.com/x.git"}]`, `[{"name":"Bad","url":"https://example.com/x.git"}]`, `{}`} {
		raw, _ := json.Marshal(map[string]string{"catalog_sources": bad})
		if w := do(t, h, "PUT", "/api/settings", string(raw), withCookie(c)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "catalogs") {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	raw, _ := json.Marshal(map[string]string{"catalog_sources": `[{"name":"fog-catalog","url":"https://example.com/not-fogs.git"}]`})
	if w := do(t, h, "PUT", "/api/settings", string(raw), withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("save list: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/catalog", "", withCookie(c))
	body.Catalogs = nil
	body.Entries = nil
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Catalogs) != 1 || body.Catalogs[0].Trusted || body.Catalogs[0].From != "" || len(body.Entries) != 0 {
		t.Errorf("an impostor under fog's name: %+v, %d entries", body.Catalogs, len(body.Entries))
	}
}

// Artwork comes from the catalog it shipped with, sandboxed, and nothing
// outside an entry's own folder is served.
func TestCatalogAssets(t *testing.T) {
	t.Parallel()

	_, h := newTestServer(t)
	c := setup(t, h)
	w := do(t, h, "GET", "/api/catalog/assets/fog-catalog/immich/logo.svg", "", withCookie(c))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("logo: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{
		"/api/catalog/assets/fog-catalog/immich/app.yaml",
		"/api/catalog/assets/fog-catalog/..%2F..%2Fcatalog.yaml/logo.svg",
		"/api/catalog/assets/fog-catalog/immich/..%2F..%2Fcatalog.yaml",
		"/api/catalog/assets/nope/immich/logo.svg",
	} {
		if w := do(t, h, "GET", p, "", withCookie(c)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := do(t, h, "GET", "/api/catalog/assets/fog-catalog/immich/logo.svg", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", w.Code)
	}
}
