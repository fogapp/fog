package store

import "testing"

// The list is the app's distinct versions, newest first: three redeploys of
// one image are one place to go back to, not three rows crowding out the
// version the operator is actually looking for.
func TestListAppVersionsCollapsesRedeploys(t *testing.T) {
	s := testStore(t)
	app, err := s.CreateApp("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"sha256:a", "sha256:b", "sha256:a", "sha256:c"} {
		if err := s.RecordAppVersion(app.ID, "image", ref, "img@"+ref, 3, "matthew"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAppVersions(app.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sha256:c", "sha256:a", "sha256:b"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, ref := range want {
		if got[i].Ref != ref {
			t.Errorf("row %d: got %q, want %q", i, got[i].Ref, ref)
		}
	}
	// sha256:a ran twice, and its row must say when it last ran — the time
	// that decides where it sits in the list.
	a := got[1]
	if a.Deploys != 2 {
		t.Errorf("sha256:a deploys = %d, want 2", a.Deploys)
	}
	if a.FirstAt == "" || a.CreatedAt < a.FirstAt {
		t.Errorf("sha256:a first ran %q, last ran %q — the last run must not precede the first", a.FirstAt, a.CreatedAt)
	}
}

// Current is the newest recorded version, and only this app's.
func TestCurrentAppVersionIsPerApp(t *testing.T) {
	s := testStore(t)
	one, _ := s.CreateApp("one")
	two, _ := s.CreateApp("two")
	if err := s.RecordAppVersion(one.ID, "git", "aaa", "fog/one:aaa", 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAppVersion(two.ID, "git", "bbb", "fog/two:bbb", 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAppVersion(one.ID, "git", "ccc", "fog/one:ccc", 2, "matthew"); err != nil {
		t.Fatal(err)
	}
	cur, err := s.CurrentAppVersion(one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Ref != "ccc" {
		t.Errorf("current = %q, want ccc", cur.Ref)
	}
	if _, err := s.AppVersionByRef(one.ID, "bbb"); err != ErrNotFound {
		t.Errorf("another app's version resolved on this one: %v", err)
	}
}

// An app with no recorded deploy has no current version — and must say so
// rather than answering with an empty ref that reads as a real one.
func TestCurrentAppVersionUnknown(t *testing.T) {
	s := testStore(t)
	app, _ := s.CreateApp("fresh")
	if _, err := s.CurrentAppVersion(app.ID); err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
	// A deploy fog couldn't name records nothing: a blank row would sit in
	// the list as a rollback target that cannot be taken.
	if err := s.RecordAppVersion(app.ID, "image", "", "img:latest", 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CurrentAppVersion(app.ID); err != ErrNotFound {
		t.Errorf("an unnameable version was recorded anyway: %v", err)
	}
}

// Versions belong to their app: deleting it takes them with it.
func TestAppVersionsCascade(t *testing.T) {
	s := testStore(t)
	app, _ := s.CreateApp("doomed")
	if err := s.RecordAppVersion(app.ID, "image", "sha256:a", "img@sha256:a", 1, "matthew"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListAppVersions(app.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("versions outlived their app: %+v", got)
	}
}
