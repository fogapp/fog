package catalog

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const goodApp = `id: web
title: Web
description: A web server.
category: Utilities
icon: '🌐'
logo: logo.svg
screenshots:
  - src: shot-1.webp
    alt: The front page
spec:
  source:
    type: image
    image: nginx:1
  routes:
    - port: 80
options:
  - key: tz
    label: Timezone
    target: env:TZ
    default: Etc/UTC
`

func catalogFS(apps map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"catalog.yaml": {Data: []byte("format: fog-catalog/1\nname: test\ntitle: Test\nfeatured: [web]\n")},
	}
	for id, y := range apps {
		fsys["apps/"+id+"/app.yaml"] = &fstest.MapFile{Data: []byte(y)}
		fsys["apps/"+id+"/logo.svg"] = &fstest.MapFile{Data: []byte("<svg/>")}
		fsys["apps/"+id+"/shot-1.webp"] = &fstest.MapFile{Data: []byte("RIFF")}
	}
	return fsys
}

// A catalog is read entry by entry: what's wrong with one entry is said
// about that entry, and the others still load.
func TestLoadReportsBadEntries(t *testing.T) {
	bad := map[string]string{
		"misspelt":   strings.Replace(strings.Replace(goodApp, "id: web", "id: misspelt", 1), "category:", "catgory:", 1),
		"elsewhere":  goodApp, // id says web, folder says elsewhere
		"nologo":     strings.Replace(strings.Replace(goodApp, "id: web", "id: nologo", 1), "logo: logo.svg", "logo: missing.svg", 1),
		"escape":     strings.Replace(strings.Replace(goodApp, "id: web", "id: escape", 1), "logo: logo.svg", "logo: ../web/logo.svg", 1),
		"badtarget":  strings.Replace(strings.Replace(goodApp, "id: web", "id: badtarget", 1), "target: env:TZ", "target: file:/etc", 1),
		"selftrust":  strings.Replace(goodApp, "id: web", "id: selftrust\ntrusted: true", 1),
		"noroute":    strings.Replace(strings.Replace(goodApp, "id: web", "id: noroute", 1), "  routes:\n    - port: 80\n", "", 1),
		"homefolder": strings.Replace(strings.Replace(goodApp, "id: web", "id: homefolder", 1), "  routes:", "  volumes:\n    - name: data\n      mount: /data\n      files: Apps/Web/data\n  routes:", 1),
	}
	all := map[string]string{"web": goodApp}
	for k, v := range bad {
		all[k] = v
	}
	c, err := Load(catalogFS(all), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 1 || c.Entries[0].ID != "web" {
		t.Fatalf("entries = %+v, want only web", c.Entries)
	}
	got := map[string]string{}
	for _, p := range c.Problems {
		got[p.App] = p.Error
	}
	for id := range bad {
		if got[id] == "" {
			t.Errorf("%s: no problem reported", id)
		}
	}
	web := c.Entries[0]
	if web.Logo != "/api/catalog/assets/test/web/logo.svg" || web.Screenshots[0].Src != "/api/catalog/assets/test/web/shot-1.webp" {
		t.Errorf("artwork not served by fog: %q %+v", web.Logo, web.Screenshots)
	}
	if web.Catalog != "test" || web.Trusted {
		t.Errorf("catalog %q trusted %v", web.Catalog, web.Trusted)
	}
}

func TestLoadRefusesAnotherFormat(t *testing.T) {
	fsys := fstest.MapFS{"catalog.yaml": {Data: []byte("format: fog-catalog/2\nname: x\n")}}
	if _, err := Load(fsys, "x"); err == nil || !strings.Contains(err.Error(), "fog-catalog/1") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Load(fstest.MapFS{}, "x"); err == nil {
		t.Fatal("a directory with no catalog.yaml loaded")
	}
}

// An id belongs to the first catalog that has it, and a companion that is
// in no catalog, or can't install with a click, is dropped from its parent.
func TestMerge(t *testing.T) {
	parent := strings.Replace(goodApp, "id: web", "id: parent\ncompanions:\n  - id: web\n    blurb: Serves it.\n  - id: ghost\n    blurb: Nowhere.\n  - id: asks\n    blurb: Needs an answer.", 1)
	asks := strings.Replace(goodApp, "id: web", "id: asks", 1) + "  - key: token\n    label: Token\n    target: env:TOKEN\n"
	a, _ := Load(catalogFS(map[string]string{"web": goodApp, "parent": parent}), "a")
	b, _ := Load(catalogFS(map[string]string{"web": strings.Replace(goodApp, "title: Web", "title: Other web", 1), "asks": asks}), "b")
	entries := Merge([]*Catalog{a, b})
	if len(entries) != 3 {
		t.Fatalf("got %d entries", len(entries))
	}
	if web := find(t, entries, "web"); web.Title != "Web" || web.Catalog != "a" {
		t.Errorf("web came from %s (%s); the first catalog wins", web.Catalog, web.Title)
	}
	if p := find(t, entries, "parent"); len(p.Companions) != 1 || p.Companions[0].ID != "web" {
		t.Errorf("companions = %+v, want only web", p.Companions)
	}
	if len(a.Problems) != 2 || len(b.Problems) != 1 {
		t.Errorf("problems: a %+v, b %+v", a.Problems, b.Problems)
	}
}

func TestParseRemotes(t *testing.T) {
	rs, err := ParseRemotes("")
	if err != nil || len(rs) != len(DefaultRemotes) || !Trusted(rs[0]) {
		t.Fatalf("default: %+v %v", rs, err)
	}
	rs, err = ParseRemotes(`[{"name":"mine","url":"https://example.com/me/apps.git"},{"name":"lan","url":"git@gitea.lan:me/apps.git","branch":"dev"}]`)
	if err != nil || rs[0].Branch != "main" || rs[1].Branch != "dev" || Trusted(rs[0]) {
		t.Fatalf("%+v %v", rs, err)
	}
	// A list saved before fog's catalogs moved reads at their new address,
	// still trusted; others keep theirs.
	rs, err = ParseRemotes(`[{"name":"fog-apps","url":"https://github.com/fogapp/fog-apps.git"},{"name":"fog-catalog","url":"https://github.com/fogapp/fog-catalog.git"},{"name":"beta","url":"ssh://gitea@gitea.lan/me/beta.git"}]`)
	if err != nil || !slices.Equal(rs[:2], DefaultRemotes) || rs[2].URL != "ssh://gitea@gitea.lan/me/beta.git" {
		t.Fatalf("moved: %+v %v", rs, err)
	}
	// fog's own name at someone else's address is not fog's own.
	if Trusted(Remote{Name: "fog-catalog", URL: "https://example.com/fog-catalog.git"}) {
		t.Error("trusted by name alone")
	}
	for _, bad := range []string{
		`{"name":"x"}`,
		`[{"name":"Bad Name","url":"https://example.com/x.git"}]`,
		`[{"name":"a","url":"https://example.com/x.git"},{"name":"a","url":"https://example.com/y.git"}]`,
		`[{"name":"a","url":"http://example.com/x.git"}]`,
		`[{"name":"a","url":"https://user:pw@example.com/x.git"}]`,
		`[{"name":"a","url":"https://example.com/x.git","branch":"-x"}]`,
	} {
		if ValidRemotes(bad) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A catalog repository is cloned, read, refetched as it changes, and
// removed with its clone when it leaves the list. One that isn't fog's own
// installs held for review.
func TestRegistrySyncs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS(filepath.Join("testdata", "catalogs", "fog-catalog"))); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "catalog")

	reg := &Registry{Dir: t.TempDir(), Seed: fstest.MapFS{}}
	reg.SetRemotes([]Remote{{Name: "mine", URL: repo, Branch: "main"}})
	if len(reg.Entries()) != 0 || reg.Statuses()[0].Error == "" {
		t.Fatalf("read before any fetch: %+v", reg.Statuses())
	}
	reg.Sync(context.Background())
	st := reg.Statuses()[0]
	if st.From != "clone" || st.Commit == "" || st.Error != "" || st.Apps == 0 || st.Trusted {
		t.Fatalf("status %+v", st)
	}
	jf := find(t, reg.Entries(), "jellyfin")
	if jf.Trusted || jf.Catalog != "mine" {
		t.Errorf("jellyfin from someone's catalog: trusted %v, catalog %q", jf.Trusted, jf.Catalog)
	}
	f, typ, err := reg.Asset("mine", "jellyfin", strings.TrimPrefix(jf.Logo, "/api/catalog/assets/mine/jellyfin/"))
	if err != nil || typ == "" {
		t.Fatalf("asset: %v", err)
	}
	f.Close()

	// The repository changes; the next fetch sees it.
	os.RemoveAll(filepath.Join(repo, "apps", "ntfy"))
	git(t, repo, "commit", "-qam", "drop ntfy")
	reg.Sync(context.Background())
	for _, e := range reg.Entries() {
		if e.ID == "ntfy" {
			t.Fatal("ntfy still in the store after it left the catalog")
		}
	}

	reg.SetRemotes(nil)
	reg.Sync(context.Background())
	if _, err := os.Stat(filepath.Join(reg.Dir, "builds", "mine")); !os.IsNotExist(err) {
		t.Errorf("clone kept after the catalog left the list: %v", err)
	}
}

// A symlink in somebody's catalog doesn't reach outside it: the entry that
// names one is refused, and the file is never served.
func TestRegistryStaysInsideTheClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	secret := filepath.Join(t.TempDir(), "secret.svg")
	os.WriteFile(secret, []byte("<svg>secret</svg>"), 0o644)
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "catalog.yaml"), []byte("format: fog-catalog/1\nname: x\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, "apps", "web"), 0o755)
	os.WriteFile(filepath.Join(repo, "apps", "web", "app.yaml"), []byte(goodApp), 0o644)
	os.WriteFile(filepath.Join(repo, "apps", "web", "shot-1.webp"), []byte("RIFF"), 0o644)
	if err := os.Symlink(secret, filepath.Join(repo, "apps", "web", "logo.svg")); err != nil {
		t.Skip(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "catalog")

	reg := &Registry{Dir: t.TempDir(), Seed: fstest.MapFS{}}
	reg.SetRemotes([]Remote{{Name: "x", URL: repo, Branch: "main"}})
	reg.Sync(context.Background())
	if n := len(reg.Entries()); n != 0 {
		t.Fatalf("an entry whose logo leaves the clone loaded (%d entries)", n)
	}
	if f, _, err := reg.Asset("x", "web", "logo.svg"); err == nil {
		b, _ := io.ReadAll(f)
		t.Fatalf("served %q from outside the clone", b)
	}
}

// fog's own catalogs are read from the copy a release ships with until
// they are first fetched; nobody else's catalog is, even under fog's names.
func TestRegistrySeed(t *testing.T) {
	seed := os.DirFS(filepath.Join("testdata", "catalogs"))
	reg := &Registry{Dir: t.TempDir(), Seed: seed}
	reg.SetRemotes(DefaultRemotes)
	for _, st := range reg.Statuses() {
		if st.From != "seed" || !st.Trusted || st.Apps == 0 {
			t.Errorf("%s: %+v", st.Name, st)
		}
	}
	if e := find(t, reg.Entries(), "planner"); !e.Trusted {
		t.Error("planner from fog's own catalog isn't trusted")
	}
	if len(reg.Featured()) == 0 {
		t.Error("nothing featured")
	}
	reg.SetRemotes([]Remote{{Name: "fog-catalog", URL: "https://example.com/impostor.git", Branch: "main"}})
	if n := len(reg.Entries()); n != 0 {
		t.Errorf("someone else's catalog read from the seed: %d entries", n)
	}
}
