package catalog

import (
	"testing"
)

// Every entry is a full listing: the store page has no "description
// pending" state, so a new entry without copy is caught here, not on the
// screen.
func TestListingsComplete(t *testing.T) {
	for _, e := range ownEntries(t) {
		if e.About == "" || len(e.Highlights) < 3 || e.Logo == "" || e.Developer == "" || e.License == "" {
			t.Errorf("%s: incomplete listing (about %d chars, %d highlights, logo %q, developer %q, license %q)",
				e.ID, len(e.About), len(e.Highlights), e.Logo, e.Developer, e.License)
		}
	}
}
