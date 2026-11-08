package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// Two sets of entries for tests. The fixture (testdata/catalogs) is a few
// of fog's own entries, trimmed, for tests of what fog does with an entry:
// they always run. fog's own catalogs are where the real entries live —
// separate repositories — so tests of what those entries say run against
// checkouts of them beside fog (or under $FOG_CATALOGS), and skip when
// there are none.

func loadMerged(t testing.TB, root string, extra ...string) []Entry {
	t.Helper()
	names := []string{}
	for _, rm := range DefaultRemotes {
		names = append(names, rm.Name)
	}
	names = append(names, extra...)
	var cats []*Catalog
	for _, name := range names {
		c, err := Load(os.DirFS(filepath.Join(root, name)), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cats = append(cats, c)
	}
	entries := Merge(cats)
	for _, c := range cats {
		for _, p := range c.Problems {
			t.Errorf("%s: %s: %s", c.Name, p.App, p.Error)
		}
	}
	return entries
}

func fixtureEntries(t testing.TB) []Entry {
	return loadMerged(t, filepath.Join("testdata", "catalogs"))
}

// ownCatalogsRoot is the directory holding checkouts of fog-apps and
// fog-catalog, or "" when there are none.
func ownCatalogsRoot() string {
	root := os.Getenv("FOG_CATALOGS")
	if root == "" {
		root = filepath.Join("..", "..", "..") // the fog checkout's parent
	}
	for _, rm := range DefaultRemotes {
		if _, err := os.Stat(filepath.Join(root, rm.Name, "catalog.yaml")); err != nil {
			return ""
		}
	}
	return root
}

func ownEntries(t testing.TB) []Entry {
	t.Helper()
	root := ownCatalogsRoot()
	if root == "" {
		t.Skip("no checkouts of fog-apps and fog-catalog beside fog (set FOG_CATALOGS)")
	}
	return loadMerged(t, root, extraCatalogs(root)...)
}

// extraCatalogs names the other catalog checkouts beside fog's own: an
// operator's private catalogs, which aren't defaults but whose entries are
// tested with fog's where a checkout of them is at hand. A directory is a
// catalog when it holds a catalog.yaml.
func extraCatalogs(root string) []string {
	own := map[string]bool{}
	for _, rm := range DefaultRemotes {
		own[rm.Name] = true
	}
	dirs, _ := os.ReadDir(root)
	var extra []string
	for _, d := range dirs {
		if !d.IsDir() || own[d.Name()] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, d.Name(), "catalog.yaml")); err == nil {
			extra = append(extra, d.Name())
		}
	}
	return extra
}

func find(t testing.TB, entries []Entry, id string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no entry %s", id)
	return Entry{}
}

// byID is a fixture entry; ownByID one of the real catalogs'. An app that
// lives only in a private catalog skips its tests where none beside fog's
// own has it.
func byID(t testing.TB, id string) Entry { return find(t, fixtureEntries(t), id) }

func ownByID(t testing.TB, id string) Entry {
	t.Helper()
	entries := ownEntries(t)
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Skipf("%s isn't in fog's own catalogs, or in a private catalog checked out beside them", id)
	return Entry{}
}
