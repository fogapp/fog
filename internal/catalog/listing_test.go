package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every entry is a full listing: the store page has no "description
// pending" state, so a new entry without copy is caught here, not on the
// screen.
func TestListingsComplete(t *testing.T) {
	ids := map[string]bool{}
	for _, e := range Entries() {
		ids[e.ID] = true
		if e.About == "" || len(e.Highlights) < 3 || e.Logo == "" || e.Developer == "" || e.License == "" {
			t.Errorf("%s: incomplete listing (about %d chars, %d highlights, logo %q, developer %q, license %q)",
				e.ID, len(e.About), len(e.Highlights), e.Logo, e.Developer, e.License)
		}
		if !strings.HasPrefix(e.Logo, "/store/"+e.ID+"/") {
			t.Errorf("%s: logo %q is not under the entry's own store folder", e.ID, e.Logo)
		}
		for _, s := range e.Screenshots {
			if !strings.HasPrefix(s.Src, "/store/"+e.ID+"/") || s.Alt == "" {
				t.Errorf("%s: screenshot %+v needs a src under /store/%s/ and an alt", e.ID, s, e.ID)
			}
		}
		for _, u := range []string{e.Website, e.Docs} {
			if u != "" && !strings.HasPrefix(u, "https://") {
				t.Errorf("%s: %q is not an https url", e.ID, u)
			}
		}
	}
	for id := range listings {
		if !ids[id] {
			t.Errorf("listings names %q, which is not a catalog entry", id)
		}
	}
}

// The artwork a listing names has to exist in the UI's static files, or
// the store shows a broken image. Checked against the checkout when the
// test runs inside one; a build elsewhere skips it.
func TestListingArtworkExists(t *testing.T) {
	public := filepath.Join("..", "..", "frontend", "public")
	if _, err := os.Stat(filepath.Join(public, "store")); err != nil {
		t.Skip("frontend/public/store not present")
	}
	for _, e := range Entries() {
		paths := []string{e.Logo}
		for _, s := range e.Screenshots {
			paths = append(paths, s.Src)
		}
		for _, p := range paths {
			if _, err := os.Stat(filepath.Join(public, filepath.FromSlash(p))); err != nil {
				t.Errorf("%s: %s is missing from frontend/public", e.ID, p)
			}
		}
	}
}
