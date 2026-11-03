package issues

import "testing"

func TestCategoriesCoverEveryCheck(t *testing.T) {
	checks := (*Scanner)(nil).checks()
	kinds := 0
	for _, c := range checks {
		for _, k := range c.kinds() {
			kinds++
			if c.category == "" || Categories[k] != c.category {
				t.Errorf("%s: category %q", k, Categories[k])
			}
		}
	}
	if len(Categories) != kinds {
		t.Fatalf("%d categories for %d kinds (a kind registered twice?)", len(Categories), kinds)
	}
	if Categories["pg-dump-skew"] != "database" || Categories["files-offer-changed"] != "storage" {
		t.Errorf("unexpected mapping: %v", Categories)
	}
}
