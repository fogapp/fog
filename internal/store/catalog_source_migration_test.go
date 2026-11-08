package store

import (
	"path/filepath"
	"testing"
)

// 0088: a fog that installed the store's first-party apps from a base
// written into fog keeps that base as its setting, read back off the apps'
// latest revisions; one that installed none is left without it.
func TestCatalogSourceBaseMigration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		repos map[string][]string // app -> repo of each revision, oldest first
		want  string
	}{
		{
			name: "first-party installs set the base",
			repos: map[string][]string{
				"manor":  {"ssh://git@git.example.com/you/manor.git"},
				"home":   {"ssh://git@old.example.com/you/hub.git", "ssh://git@git.example.com/you/hub.git"},
				"fit2":   {"ssh://git@git.example.com/you/fit.git"},
				"mine":   {"ssh://git@github.com/someone/blog.git"},
				"sonarr": {""},
			},
			want: "ssh://git@git.example.com/you/",
		},
		{
			name:  "only the latest revision counts",
			repos: map[string][]string{"hub": {"ssh://git@git.example.com/you/hub.git", "ssh://git@github.com/me/hub.git"}},
			want:  "ssh://git@github.com/me/",
		},
		{
			name:  "no first-party app, no base",
			repos: map[string][]string{"blog": {"ssh://git@github.com/someone/blog.git"}, "xhub": {"ssh://git@h/me/xhub.git"}},
			want:  "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fog.db")
			db := openAtMigration(t, path, "0088")
			id := 0
			for app, revs := range c.repos {
				id++
				if _, err := db.Exec(`INSERT INTO apps (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`, id, app, now(), now()); err != nil {
					t.Fatal(err)
				}
				for i, repo := range revs {
					spec := `{"source":{"type":"image","image":"x"}}`
					if repo != "" {
						spec = `{"source":{"type":"git","repo":"` + repo + `"}}`
					}
					if _, err := db.Exec(`INSERT INTO app_revisions (app_id, rev, spec, actor, created_at) VALUES (?, ?, ?, 'test', ?)`,
						id, i+1, spec, now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			db.Close()

			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got, _ := s.GetSetting("catalog_source_base"); got != c.want {
				t.Fatalf("base = %q, want %q", got, c.want)
			}
		})
	}
}
