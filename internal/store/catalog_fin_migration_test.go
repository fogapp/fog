package store

import (
	"path/filepath"
	"testing"
)

// 0091: Manor is now Fin. A store setting that names "manor" learns "fin"
// too, so a fog that saved its visible set or Manor's clone URL still shows
// Fin and knows where to clone it; settings without Manor are untouched.
func TestCatalogFinMigration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name               string
		enabled, overrides string // "" = setting absent
		wantEnabled        string
		wantOverrides      string
	}{
		{
			name:          "manor in both settings",
			enabled:       `["manor","hub"]`,
			overrides:     `{"manor":"ssh://git@git.example.com/me/manor.git"}`,
			wantEnabled:   `["manor","hub","fin"]`,
			wantOverrides: `{"manor":"ssh://git@git.example.com/me/manor.git","fin":"ssh://git@git.example.com/me/manor.git"}`,
		},
		{
			name:          "fin already there",
			enabled:       `["manor","fin"]`,
			overrides:     `{"manor":"ssh://a/manor.git","fin":"ssh://a/fin.git"}`,
			wantEnabled:   `["manor","fin"]`,
			wantOverrides: `{"manor":"ssh://a/manor.git","fin":"ssh://a/fin.git"}`,
		},
		{
			name:          "no manor",
			enabled:       `["hub"]`,
			overrides:     `{"hub":"ssh://a/hub.git"}`,
			wantEnabled:   `["hub"]`,
			wantOverrides: `{"hub":"ssh://a/hub.git"}`,
		},
		{
			name: "neither setting saved",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fog.db")
			db := openAtMigration(t, path, "0091")
			for key, value := range map[string]string{"catalog_enabled": c.enabled, "catalog_source_overrides": c.overrides} {
				if value == "" {
					continue
				}
				if _, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, key, value, now()); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()

			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got, _ := s.GetSetting("catalog_enabled"); got != c.wantEnabled {
				t.Errorf("catalog_enabled = %q, want %q", got, c.wantEnabled)
			}
			if got, _ := s.GetSetting("catalog_source_overrides"); got != c.wantOverrides {
				t.Errorf("catalog_source_overrides = %q, want %q", got, c.wantOverrides)
			}
		})
	}
}
