package files

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openTestIndex(t *testing.T, paths ...string) *Index {
	t.Helper()
	ix, err := OpenIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	for _, p := range paths {
		parent, name := Split(p)
		if _, err := ix.db.Exec(`INSERT INTO nodes (parent, name, kind, size, mtime, mode, inode) VALUES (?, ?, 'file', 0, 0, 0, 0)`, parent, name); err != nil {
			t.Fatal(err)
		}
	}
	return ix
}

func indexPaths(t *testing.T, ix *Index) []string {
	t.Helper()
	rows, err := ix.db.Query(`SELECT parent, name FROM nodes WHERE name <> ''`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var parent, name string
		rows.Scan(&parent, &name)
		out = append(out, Join(parent, name))
	}
	slices.Sort(out)
	return out
}

// A subtree is exactly the paths that start with "dir/": not a sibling
// whose name merely shares the prefix, not a differently-cased twin, and
// LIKE's wildcards in a name are just characters.
func TestSubtreeMatchesOnlyItsOwnPaths(t *testing.T) {
	ix := openTestIndex(t,
		"Photos/a.jpg", "Photos/2024/b.jpg",
		"photos/keep.jpg", "photos/sub/keep.jpg", "Photos2/keep.jpg", "Photos-old/keep.jpg",
		"a_b/x", "axb/keep", "100%/x", "1000/keep",
	)
	for _, dir := range []string{"Photos", "a_b", "100%"} {
		tx, err := ix.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := deleteSubtree(tx, dir); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	}
	want := []string{"1000/keep", "Photos-old/keep.jpg", "Photos2/keep.jpg", "axb/keep", "photos/keep.jpg", "photos/sub/keep.jpg"}
	if got := indexPaths(t, ix); !slices.Equal(got, want) {
		t.Errorf("after delete = %v, want %v", got, want)
	}
}

func TestMoveSubtreeLeavesLookalikesAlone(t *testing.T) {
	ix := openTestIndex(t, "a/x", "a/b/y", "ab/z", "A/w", "A/q/w")
	if err := ix.moveSubtree("a", "c"); err != nil {
		t.Fatal(err)
	}
	want := []string{"A/q/w", "A/w", "ab/z", "c/b/y", "c/x"}
	if got := indexPaths(t, ix); !slices.Equal(got, want) {
		t.Errorf("after move = %v, want %v", got, want)
	}
}

// The range form is the point: it lets SQLite search the parent index
// instead of scanning every row.
func TestSubtreeUsesTheParentIndex(t *testing.T) {
	ix := openTestIndex(t)
	w, args := subtree("parent", "a/b")
	rows, err := ix.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM nodes WHERE `+w, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "; ")
	if strings.Contains(joined, "SCAN nodes") || !strings.Contains(joined, "USING") {
		t.Errorf("plan = %s, want an index search", joined)
	}
}

// A listing hands its folder to each annotator together with the entries:
// one call, so the queries an annotator makes run once per listing.
func TestListAnnotatesEntriesAndFolderInOnePass(t *testing.T) {
	m := newManager(t)
	write(t, m, "docs/a.txt", "a")
	write(t, m, "docs/b.txt", "b")
	calls := 0
	m.Annotators = append(m.Annotators, func(nodes []Node) {
		calls++
		for i := range nodes {
			nodes[i].KeptBy = "seen"
		}
	})
	l, err := m.List("docs", ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("annotator ran %d times, want 1", calls)
	}
	if l.Dir.Path != "docs" || l.Dir.KeptBy != "seen" {
		t.Errorf("folder = %+v, want it annotated", l.Dir)
	}
	if len(l.Entries) != 2 || l.Entries[0].KeptBy != "seen" || l.Entries[1].KeptBy != "seen" {
		t.Errorf("entries = %+v, want both annotated", l.Entries)
	}
}
