package store

import (
	"reflect"
	"testing"
)

func TestAllPersonPagesMatchesPersonPages(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	hub, err := s.CreateApp("hub")
	if err != nil {
		t.Fatal(err)
	}
	blog, err := s.CreateApp("blog")
	if err != nil {
		t.Fatal(err)
	}
	const ann, bob = "person:i1/ANN", "person:i2/BOB"
	for _, p := range []struct {
		app  int64
		addr string
	}{{hub.ID, ann}, {blog.ID, ann}, {hub.ID, bob}} {
		if err := s.PutPersonPage(p.app, p.addr, "https://x/"+p.addr, "Open"); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.AllPersonPages()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("AllPersonPages has %d addresses, want 2: %+v", len(all), all)
	}
	for _, addr := range []string{ann, bob} {
		one, err := s.PersonPages(addr)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(all[addr], one) {
			t.Errorf("%s: AllPersonPages %+v, PersonPages %+v", addr, all[addr], one)
		}
	}
	if got := all[ann]; len(got) != 2 || got[0].App != "blog" || got[1].App != "hub" {
		t.Errorf("pages for %s not ordered by app name: %+v", ann, got)
	}
}
