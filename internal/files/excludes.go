package files

import (
	"sort"
	"strings"
)

// Exclusions: what the nightly snapshot leaves out. Stored in fog.db by
// path (see migration 0026); read per listing — it is a short list — and
// stamped onto each entry so the page can badge it, with the ancestor
// that caused it when the exclusion is inherited.

// excludeSet answers "is this path excluded, and by which entry?".
type excludeSet struct {
	direct map[string]bool
	sorted []string // for prefix walks
}

func (m *Manager) excludes() excludeSet {
	paths, err := m.Store.ListFileExcludes()
	if err != nil {
		return excludeSet{direct: map[string]bool{}}
	}
	es := excludeSet{direct: make(map[string]bool, len(paths)), sorted: paths}
	for _, p := range paths {
		es.direct[p] = true
	}
	sort.Strings(es.sorted)
	return es
}

// check returns (excluded, by): by is rel itself for a direct exclusion,
// or the nearest excluded ancestor.
func (es excludeSet) check(rel string) (bool, string) {
	if len(es.direct) == 0 {
		return false, ""
	}
	if es.direct[rel] {
		return true, rel
	}
	// Walk ancestors: nearest first.
	p := rel
	for p != "" {
		parent, _ := Split(p)
		if parent != "" && es.direct[parent] {
			return true, parent
		}
		p = parent
	}
	return false, ""
}

// stamp annotates entries in place.
func (es excludeSet) stamp(nodes []Node) {
	if len(es.direct) == 0 {
		return
	}
	for i := range nodes {
		ex, by := es.check(nodes[i].Path)
		if ex {
			nodes[i].Excluded = true
			if by != nodes[i].Path {
				nodes[i].ExcludedBy = by
			}
		}
	}
}

// SetExcluded marks or unmarks one entry. Marking a path inside an
// already-excluded folder is allowed but a no-op in effect; unmarking one
// does not un-exclude the folder (the page says so).
func (m *Manager) SetExcluded(rel string, excluded bool) error {
	if rel == "" {
		return ErrBadName
	}
	if excluded {
		return m.Store.AddFileExclude(rel)
	}
	return m.Store.RemoveFileExclude(rel)
}

// ListExcluded returns every direct exclusion with what is known about it.
func (m *Manager) ListExcluded() ([]Node, error) {
	paths, err := m.Store.ListFileExcludes()
	if err != nil {
		return nil, err
	}
	ix, err := m.Index()
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(paths))
	for _, p := range paths {
		n, err := ix.Get(p)
		if err != nil {
			// Not in the index (renamed away, or never listed): still an
			// exclusion, shown as a bare path.
			n = Node{Path: p, Kind: "other"}
			_, n.Name = Split(p)
		}
		n.Excluded = true
		out = append(out, n)
	}
	return out, nil
}

// followRename keeps exclusions, share links, pins and federation state
// attached to entries fog itself moved.
func (m *Manager) followRename(old, new string) {
	if err := m.Store.RenamePath(old, new); err != nil {
		m.Log.Warn("files: follow rename", "from", old, "to", new, "err", err)
	}
}

// excludedTree reports whether rel or anything above it is excluded,
// for the snapshot walk (which prunes whole subtrees).
// key identifies the set, to tell whether it has changed.
func (es excludeSet) key() string { return strings.Join(es.sorted, "\x00") }

func (es excludeSet) prunes(rel string) bool {
	ex, _ := es.check(rel)
	return ex
}
