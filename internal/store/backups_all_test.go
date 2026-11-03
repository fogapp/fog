package store

import "testing"

func TestListAllBackupRefsGroupsByAppNewestFirst(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateApp("a")
	b, _ := s.CreateApp("b")
	add := func(appID int64, path string) {
		t.Helper()
		if _, err := s.CreateBackup(appID, "scheduled", path, 1); err != nil {
			t.Fatal(err)
		}
	}
	add(a.ID, "a1")
	add(b.ID, "b1")
	add(a.ID, "a2")
	add(b.ID, "b2")

	all, err := s.ListAllBackupRefs()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, bk := range all {
		got = append(got, bk.Path)
	}
	want := []string{"a2", "a1", "b2", "b1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// Per app it is exactly what ListBackups answers.
	one, _ := s.ListBackups(a.ID, 1000)
	if len(one) != 2 || one[0].ID != all[0].ID || one[1].ID != all[1].ID {
		t.Errorf("ListBackups(a) = %v, disagrees with ListAllBackupRefs", one)
	}
}

func TestLatestBackupsIsEachAppsNewest(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateApp("a")
	b, _ := s.CreateApp("b")
	c, _ := s.CreateApp("c") // no backups
	for _, x := range []struct {
		app  int64
		path string
	}{{a.ID, "a1"}, {b.ID, "b1"}, {a.ID, "a2"}, {b.ID, "b2"}, {a.ID, "a3"}} {
		if _, err := s.CreateArtifactBackup(x.app, "scheduled", x.path, 1, "f", "sum-"+x.path, "manifest-"+x.path); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(got), got)
	}
	if _, ok := got[c.ID]; ok {
		t.Error("an app with no backups has an entry")
	}
	for _, app := range []int64{a.ID, b.ID} {
		one, _ := s.ListBackups(app, 1)
		if got[app] != one[0] {
			t.Errorf("app %d: LatestBackups = %+v, ListBackups(1) = %+v", app, got[app], one[0])
		}
	}
	if got[a.ID].Path != "a3" || got[a.ID].Manifest != "manifest-a3" {
		t.Errorf("a's newest = %+v, want a3 with its manifest", got[a.ID])
	}
}

func TestLatestBackupsOfFormatLooksWithinEachAppsNewest(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateApp("a")
	b, _ := s.CreateApp("b")
	c, _ := s.CreateApp("c")
	add := func(app int64, path, format string) {
		t.Helper()
		if _, err := s.CreateArtifactBackup(app, "scheduled", path, 1, format, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// a: the newest artifact sits behind a newer volume snapshot.
	add(a.ID, "a1", "art")
	add(a.ID, "a2", "art")
	add(a.ID, "a3", "")
	// b: its only artifact is older than a window of two.
	add(b.ID, "b1", "art")
	add(b.ID, "b2", "")
	add(b.ID, "b3", "")
	// c: no artifacts at all.
	add(c.ID, "c1", "")

	got, err := s.LatestBackupsOfFormat("art", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[a.ID].Path != "a2" {
		t.Fatalf("got %+v, want only a's a2", got)
	}
	// Widening the window reaches b's artifact, as scanning more of
	// ListBackups would.
	got, _ = s.LatestBackupsOfFormat("art", 3)
	if len(got) != 2 || got[b.ID].Path != "b1" {
		t.Fatalf("within 3: got %+v, want a2 and b1", got)
	}
}
