package files

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Content and its starter libraries are made once per root: a library the
// operator deletes afterwards stays deleted, rather than reappearing on
// every restart.
func TestEnsureContentSeedsOnce(t *testing.T) {
	m := newManager(t)
	m.EnsureContent()

	got, err := os.ReadDir(filepath.Join(m.Root(), ContentDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := append([]string(nil), contentSeed...)
	sort.Strings(want)
	if len(names) != len(want) {
		t.Fatalf("seeded %v, want %v", names, want)
	}
	for i := range names {
		if names[i] != want[i] {
			t.Fatalf("seeded %v, want %v", names, want)
		}
	}

	if err := os.Remove(filepath.Join(m.Root(), ContentDir, "Movies")); err != nil {
		t.Fatal(err)
	}
	m.EnsureContent()
	if _, err := os.Stat(filepath.Join(m.Root(), ContentDir, "Movies")); !os.IsNotExist(err) {
		t.Fatal("a deleted library came back on the next seed")
	}
}

// The two reserved folders and their children carry roles, so the Drive
// page can say what each one is rather than showing one more folder.
func TestReservedFolderRoles(t *testing.T) {
	m := newManager(t)
	for _, rel := range []string{
		AppsDir + "/Paperless/config", ContentDir + "/Movies/2026", "Holiday",
	} {
		if err := os.MkdirAll(filepath.Join(m.Root(), rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	roles := map[string]string{}
	for _, rel := range []string{"", AppsDir, ContentDir} {
		l, err := m.List(rel, ListOpts{})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range l.Entries {
			roles[n.Path] = n.Role
		}
	}
	want := map[string]string{
		AppsDir:                RoleApps,
		ContentDir:             RoleContent,
		AppsDir + "/Paperless": RoleAppFolder,
		ContentDir + "/Movies": RoleLibrary,
		"Holiday":              "",
	}
	for path, w := range want {
		if roles[path] != w {
			t.Errorf("%s role = %q, want %q", path, roles[path], w)
		}
	}
}
