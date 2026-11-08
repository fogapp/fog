package catalog

// fog-catalog/1: a catalog is a directory — a git repository, in practice —
// that fog reads its store from. docs/catalog-contract.md is the format for
// people writing one; this file is the reader, and the rules here are the
// ones that document states.
//
//	catalog.yaml           Manifest
//	apps/<id>/app.yaml     one Entry
//	apps/<id>/<artwork>    the logo and screenshots its listing names
//
// A catalog is read entry by entry: one that is malformed is left out and
// reported as a Problem, and the rest of the catalog still loads. Only a
// catalog.yaml fog can't read refuses the whole thing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/apps"
)

// Format is the version a catalog.yaml declares.
const Format = "fog-catalog/1"

const (
	maxManifestBytes = 1 << 20
	maxArtworkBytes  = 8 << 20
)

// Manifest is a catalog's catalog.yaml.
type Manifest struct {
	Format      string `json:"format"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Featured are the entries a fog shows in its store out of the box,
	// until the operator chooses for themselves.
	Featured []string `json:"featured,omitempty"`
	// Order is display order within a category; entries it doesn't name
	// follow those it does, by title.
	Order []string `json:"order,omitempty"`
}

// Problem is something in a catalog fog could not use, said so the
// catalog's author can fix it. App is empty for one about the catalog as a
// whole.
type Problem struct {
	App   string `json:"app,omitempty"`
	Error string `json:"error"`
}

// Catalog is one loaded catalog.
type Catalog struct {
	Manifest
	Entries  []Entry
	Problems []Problem
	// FS is the catalog's directory, which its artwork is served from.
	FS fs.FS `json:"-"`
}

// artworkTypes are the files a listing may name, by extension.
var artworkTypes = map[string]string{
	".svg":  "image/svg+xml",
	".webp": "image/webp",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
}

// ArtworkType is the content type of an artwork file name, or "" for one a
// catalog may not serve.
func ArtworkType(name string) string { return artworkTypes[strings.ToLower(path.Ext(name))] }

// AssetURL is where fog serves a catalog's artwork file.
func AssetURL(catalog, id, file string) string {
	return "/api/catalog/assets/" + catalog + "/" + id + "/" + file
}

// Load reads a catalog from fsys. name is what this fog calls it (the
// source's name), which artwork URLs are made under.
func Load(fsys fs.FS, name string) (*Catalog, error) {
	raw, err := readLimited(fsys, "catalog.yaml", maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("catalog.yaml: %w", err)
	}
	var m Manifest
	if err := decodeStrict(raw, &m); err != nil {
		return nil, fmt.Errorf("catalog.yaml: %w", err)
	}
	if m.Format != Format {
		return nil, fmt.Errorf("catalog.yaml: format is %q; this fog reads %s", m.Format, Format)
	}
	// The catalog is known by the name this fog gives it, whatever its own
	// file calls it: two catalogs may well both call themselves "apps".
	m.Name = name
	if m.Title == "" {
		m.Title = name
	}
	c := &Catalog{Manifest: m, FS: fsys}
	dirs, err := fs.ReadDir(fsys, "apps")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("apps: %w", err)
	}
	ids := map[string]bool{}
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		e, err := loadEntry(fsys, name, d.Name())
		if err != nil {
			c.Problems = append(c.Problems, Problem{App: d.Name(), Error: err.Error()})
			continue
		}
		ids[e.ID] = true
		c.Entries = append(c.Entries, e)
	}
	for _, list := range []struct {
		field string
		ids   []string
	}{{"featured", m.Featured}, {"order", m.Order}} {
		for _, id := range list.ids {
			if !ids[id] {
				c.Problems = append(c.Problems, Problem{Error: fmt.Sprintf("catalog.yaml %s names %q, which is not an app here", list.field, id)})
			}
		}
	}
	sortEntries(c.Entries, m.Order)
	return c, nil
}

func sortEntries(entries []Entry, order []string) {
	rank := map[string]int{}
	for i, id := range order {
		rank[id] = i + 1
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := rank[entries[i].ID], rank[entries[j].ID]
		switch {
		case ri != 0 && rj != 0:
			return ri < rj
		case ri != 0 || rj != 0:
			return ri != 0
		}
		return strings.ToLower(entries[i].Title) < strings.ToLower(entries[j].Title)
	})
}

func loadEntry(fsys fs.FS, catalog, id string) (Entry, error) {
	var e Entry
	raw, err := readLimited(fsys, "apps/"+id+"/app.yaml", maxManifestBytes)
	if err != nil {
		return e, err
	}
	if err := decodeStrict(raw, &e); err != nil {
		return e, fmt.Errorf("app.yaml: %w", err)
	}
	// What fog fills in per host or per source is not the catalog's to say.
	if e.Blocker != "" || e.Catalog != "" || e.Trusted {
		return e, fmt.Errorf("app.yaml sets blocker, catalog or trusted, which fog fills in itself")
	}
	if e.ID != id {
		return e, fmt.Errorf("id is %q but the folder is apps/%s", e.ID, id)
	}
	if err := validateEntry(e); err != nil {
		return e, err
	}
	// Artwork: a plain file name in the entry's own folder, served by fog.
	art := func(file, what string) (string, error) {
		if file == "" {
			return "", nil
		}
		if strings.ContainsAny(file, `/\`) || strings.HasPrefix(file, ".") {
			return "", fmt.Errorf("%s %q must be a file name in apps/%s", what, file, id)
		}
		if ArtworkType(file) == "" {
			return "", fmt.Errorf("%s %q is not an svg, webp, png or jpeg", what, file)
		}
		st, err := fs.Stat(fsys, "apps/"+id+"/"+file)
		if err != nil {
			return "", fmt.Errorf("%s %q is missing from apps/%s", what, file, id)
		}
		if st.Size() > maxArtworkBytes {
			return "", fmt.Errorf("%s %q is over %d MB", what, file, maxArtworkBytes>>20)
		}
		return AssetURL(catalog, id, file), nil
	}
	if e.Logo, err = art(e.Logo, "logo"); err != nil {
		return e, err
	}
	shots := make([]Screenshot, len(e.Screenshots))
	for i, s := range e.Screenshots {
		if s.Alt == "" {
			return e, fmt.Errorf("screenshot %q has no alt text", s.Src)
		}
		if s.Src, err = art(s.Src, "screenshot"); err != nil {
			return e, err
		}
		shots[i] = s
	}
	e.Screenshots = shots
	e.Catalog = catalog
	return e, nil
}

// validateEntry holds an entry to what a one-click install needs: a name
// and a spec fog accepts, and options that fill what they say they fill.
func validateEntry(e Entry) error {
	if err := apps.ValidateName(e.ID); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if e.Title == "" || e.Description == "" || e.Category == "" || e.Icon == "" {
		return fmt.Errorf("title, description, category and icon are all required")
	}
	if e.FirstParty != "" {
		if e.Spec.Source.Type != "git" {
			return fmt.Errorf("firstParty names a repository, so the source must be git")
		}
		if err := apps.ValidateName(e.FirstParty); err != nil {
			return fmt.Errorf("firstParty: %w", err)
		}
		if e.PublicRepo != "" {
			if err := apps.ValidateGitRepo(e.PublicRepo); err != nil {
				return fmt.Errorf("publicRepo: %w", err)
			}
		}
	}
	// A first-party spec has no repository until a fog resolves one; check
	// it as a fog with a source would see it.
	spec := Resolve([]Entry{e}, Sources{Base: "ssh://git@git.example.com/you/"})[0].Spec
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	if len(spec.Routes) == 0 {
		return fmt.Errorf("spec: no route — the store installs apps people open")
	}
	// A folder a template names is a content library: Content itself (one
	// shared root for a whole stack) or one capitalised level inside it. An
	// app's own data names no folder — it is given Apps/<App>/<volume> when
	// the app is created — so a template that hardcodes one is a mistake.
	for _, v := range spec.AllVolumes() {
		if v.Files == "" || v.Files == apps.FilesContentDir {
			continue
		}
		parts := strings.Split(v.Files, "/")
		if len(parts) != 2 || parts[0] != apps.FilesContentDir || parts[1] == "" || strings.ToUpper(parts[1][:1]) != parts[1][:1] {
			return fmt.Errorf("volume %s lives at %q; want %s or %s/<Library>", v.Name, v.Files, apps.FilesContentDir, apps.FilesContentDir)
		}
	}
	keys := map[string]bool{}
	for _, o := range e.Options {
		if o.Key == "" || keys[o.Key] {
			return fmt.Errorf("option keys must be present and unique (%q)", o.Key)
		}
		keys[o.Key] = true
		switch {
		case strings.HasPrefix(o.Target, "env:") && len(o.Target) > 4:
		case strings.HasPrefix(o.Target, "volume:") && len(o.Target) > 7:
			if !slices.ContainsFunc(spec.AllVolumes(), func(v apps.Volume) bool { return v.Name == o.Target[7:] }) {
				return fmt.Errorf("option %s names no volume %q", o.Key, o.Target[7:])
			}
		case strings.HasPrefix(o.Target, "secret:") && len(o.Target) > 7:
			if !slices.Contains(spec.Secrets, o.Target[7:]) {
				return fmt.Errorf("option %s fills secret %q, which spec.secrets doesn't declare", o.Key, o.Target[7:])
			}
		case o.Target == "repo":
			if spec.Source.Type != "git" {
				return fmt.Errorf("option %s fills the repository of an image app", o.Key)
			}
		default:
			return fmt.Errorf("option %s has target %q; want env:, volume:, secret: or repo", o.Key, o.Target)
		}
		switch o.Generate {
		case "", "alnum32", "hex32", "hex64", "hex128":
		default:
			return fmt.Errorf("option %s: generate %q is not alnum32, hex32, hex64 or hex128", o.Key, o.Generate)
		}
	}
	if e.Requires != nil && e.Requires.CPU != "" && cpuLevel(e.Requires.CPU) == 0 {
		return fmt.Errorf("requires.cpu %q is not x86-64-v2, -v3 or -v4", e.Requires.CPU)
	}
	if f := e.Footprint; f != nil {
		if f.Memory == 0 || f.MemoryPeak < f.Memory || f.Disk == 0 {
			return fmt.Errorf("footprint needs memory, a peak at least as large, and a disk size")
		}
		switch f.CPU {
		case CPULight, CPUModerate, CPUHeavy:
		default:
			return fmt.Errorf("footprint cpu %q is not light, moderate or heavy", f.CPU)
		}
	}
	for _, u := range []string{e.Website, e.Docs, e.ProjectURL} {
		if u != "" && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("%q is not an https URL", u)
		}
	}
	for _, c := range e.Companions {
		if c.ID == e.ID {
			return fmt.Errorf("lists itself as a companion")
		}
		if c.Blurb == "" {
			return fmt.Errorf("companion %q has no blurb", c.ID)
		}
	}
	return nil
}

// decodeStrict reads YAML into v by its JSON field names, refusing keys v
// doesn't have: a misspelt field is an error, not a silently ignored one.
func decodeStrict(raw []byte, v any) error {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	j, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func readLimited(fsys fs.FS, name string, max int64) ([]byte, error) {
	st, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, err
	}
	if st.Size() > max {
		return nil, fmt.Errorf("over %d bytes", max)
	}
	return fs.ReadFile(fsys, name)
}

// Merge makes one store from catalogs in priority order. An id two
// catalogs both use belongs to the first; companions resolve across every
// catalog, and one that can't install with a click is dropped from its
// parent. Problems found here are added to the catalog they belong to.
func Merge(cats []*Catalog) []Entry {
	var out []Entry
	owner := map[string]string{}
	for _, c := range cats {
		for _, e := range c.Entries {
			if first, ok := owner[e.ID]; ok {
				c.Problems = append(c.Problems, Problem{App: e.ID, Error: "the " + first + " catalog already has an app with this id, and it comes first"})
				continue
			}
			owner[e.ID] = c.Name
			out = append(out, e)
		}
	}
	byID := map[string]Entry{}
	for _, e := range out {
		byID[e.ID] = e
	}
	byName := map[string]*Catalog{}
	for _, c := range cats {
		byName[c.Name] = c
	}
	for i, e := range out {
		var kept []Companion
		for _, comp := range e.Companions {
			why := companionProblem(byID, comp)
			if why == "" {
				kept = append(kept, comp)
				continue
			}
			if c := byName[e.Catalog]; c != nil {
				c.Problems = append(c.Problems, Problem{App: e.ID, Error: why})
			}
		}
		out[i].Companions = kept
	}
	return out
}

// companionProblem is why a companion can't be offered, or "". It must be
// an app some catalog has, whose every option fills itself.
func companionProblem(byID map[string]Entry, c Companion) string {
	target, ok := byID[c.ID]
	if !ok {
		return fmt.Sprintf("companion %q is in no catalog this fog reads", c.ID)
	}
	for _, o := range target.Options {
		if strings.HasPrefix(o.Target, "volume:") {
			// The spec's placement is the default; a companion's folder
			// must have one so nothing is asked.
			if !slices.ContainsFunc(target.Spec.AllVolumes(), func(v apps.Volume) bool { return v.Name == o.Target[7:] && !v.Managed() }) {
				return fmt.Sprintf("companion %q asks where its %s folder goes, so it can't install with one click", c.ID, o.Key)
			}
			continue
		}
		if o.Default == "" && o.Generate == "" && o.Key != "tz" && o.Target != "repo" {
			return fmt.Sprintf("companion %q needs an answer for %s, so it can't install with one click", c.ID, o.Key)
		}
	}
	return ""
}
