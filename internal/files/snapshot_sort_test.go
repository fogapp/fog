package files

import (
	"slices"
	"testing"
)

// Equal keys must come out in one order whichever way the listing is
// sorted: a comparator that calls each of two ties "less" than the other
// leaves them wherever the sort happens to, and pages of a snapshot
// listing then repeat or skip entries.
func TestSortNodesTiesAreDeterministic(t *testing.T) {
	names := func(ns []Node) []string {
		out := make([]string, len(ns))
		for i, n := range ns {
			out[i] = n.Name
		}
		return out
	}
	for _, desc := range []bool{false, true} {
		var want []string
		for _, order := range [][]string{{"c", "a", "B", "d"}, {"d", "B", "a", "c"}, {"a", "d", "c", "B"}} {
			var ns []Node
			for _, name := range order {
				mtime := int64(100)
				if name == "d" {
					mtime = 200
				}
				ns = append(ns, Node{Name: name, Kind: "file", mtime: mtime})
			}
			ns = append(ns, Node{Name: "dir", Kind: "dir"})
			sortNodes(ns, SortMtime, desc)
			got := names(ns)
			if want == nil {
				want = got
			} else if !slices.Equal(got, want) {
				t.Fatalf("desc=%v: order depends on input: %v vs %v", desc, got, want)
			}
		}
		exp := []string{"dir", "a", "B", "c", "d"}
		if desc {
			exp = []string{"dir", "d", "a", "B", "c"}
		}
		if !slices.Equal(want, exp) {
			t.Errorf("desc=%v: got %v, want %v", desc, want, exp)
		}
	}
}
