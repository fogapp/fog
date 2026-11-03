package files

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// The app volumes are read once per operation, not once per item: reading
// them lists every app and parses every spec. Creating never reads them.
func TestMountsReadOncePerOperation(t *testing.T) {
	m := newManager(t)
	for _, rel := range []string{"a/1.txt", "a/2.txt", "a/3.txt", "b/x.txt", "media/movies/m.mkv"} {
		write(t, m, rel, "x")
	}
	reads := 0
	m.MountedPaths = func() []MountedPath {
		reads++
		return []MountedPath{{Path: filepath.Join(m.Root(), "media", "movies"), App: "jellyfin"}}
	}
	count := func(what string, want int, fn func() error) {
		t.Helper()
		reads = 0
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if reads != want {
			t.Errorf("%s read the mounts %d times, want %d", what, reads, want)
		}
	}
	count("move of three", 1, func() error {
		_, err := m.Move([]string{"a/1.txt", "a/2.txt", "a/3.txt"}, "b", ConflictAsk)
		return err
	})
	count("trash of three", 1, func() error {
		_, err := m.Trash([]string{"b/1.txt", "b/2.txt", "b/3.txt"})
		return err
	})
	count("rename over WebDAV", 1, func() error { return m.renameAny("b/x.txt", "a/x.txt") })
	count("listing", 1, func() error { _, err := m.List("", ListOpts{}); return err })
	count("mkdir", 0, func() error { _, err := m.Mkdir("c"); return err })
	count("upload", 0, func() error { _, err := m.BeginUpload("c/new.txt", 1, "", "", ConflictAsk); return err })
}

// AppMounts reads the newest spec of every app: host and Files volumes,
// app and sidecar alike, resolved against the live root; managed volumes
// and apps with no readable spec contribute nothing.
func TestAppMounts(t *testing.T) {
	m := newManager(t)
	mk := func(name, spec string) {
		t.Helper()
		a, err := m.Store.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		if spec != "" {
			if _, err := m.Store.CreateRevision(a.ID, spec, "", "test"); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("jellyfin", `{"volumes":[{"name":"config","mount":"/config"},{"files":"Content/Movies","mount":"/movies","readOnly":true}],
		"aux":[{"name":"side","volumes":[{"host":"/srv/pool","mount":"/pool"}]}]}`)
	mk("bare", "")
	mk("corrupt", `{not-json`)

	got := m.AppMounts()
	want := []MountedPath{
		{Path: filepath.Join(m.Root(), "Content", "Movies"), App: "jellyfin", Mount: "/movies", ReadOnly: true},
		{Path: "/srv/pool", App: "jellyfin", Mount: "/pool"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AppMounts = %+v, want %+v", got, want)
	}
}

// Creating the folder a volume points at, before it exists, is allowed:
// it takes nothing from the app. Taking it away again is refused.
func TestCreatingAMountedFolderIsAllowed(t *testing.T) {
	m := newManager(t)
	m.MountedPaths = func() []MountedPath {
		return []MountedPath{{Path: filepath.Join(m.Root(), "media", "movies"), App: "jellyfin"}}
	}
	if err := m.EnsureDir(""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Mkdir("media"); err != nil {
		t.Fatalf("mkdir above a volume not yet created: %v", err)
	}
	if _, err := m.Mkdir("media/movies"); err != nil {
		t.Fatalf("mkdir of the volume's folder: %v", err)
	}
	if _, err := m.Rename("media", "films"); !errors.Is(err, ErrMounted) {
		t.Errorf("rename of a mounted parent: %v", err)
	}
	if err := m.renameAny("media/movies", "elsewhere"); !errors.Is(err, ErrMounted) {
		t.Errorf("WebDAV move of a mounted folder: %v", err)
	}
	if got := m.AppsMounting("media"); len(got) != 1 || got[0] != "jellyfin" {
		t.Errorf("AppsMounting(media) = %v", got)
	}
	if got := m.AppsMounting("media/movies/inside"); len(got) != 0 {
		t.Errorf("AppsMounting(inside the volume) = %v", got)
	}
}
