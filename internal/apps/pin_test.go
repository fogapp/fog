package apps

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type pinCall struct{ root, rel, target, owner string }

// With pinning, compose binds the pin — a path only fogd writes — never
// the Drive folder or host path an app could swap for a link, and tells
// Docker to refuse a missing pin rather than make an empty folder.
func TestRenderBindsPinsNotPaths(t *testing.T) {
	drive := t.TempDir()
	var calls []pinCall
	var unpinned []string
	m := &Materializer{
		DataDir:   t.TempDir(),
		FilesRoot: func() string { return drive },
		Pin: func(_ context.Context, root, rel, target, owner string) error {
			calls = append(calls, pinCall{root, rel, target, owner})
			return nil
		},
		Unpin: func(_ context.Context, target string, keep ...string) error {
			unpinned = append(unpinned, target+" keep="+strings.Join(keep, ","))
			return nil
		},
	}
	host := filepath.Join(t.TempDir(), "media")
	m.ManagedRoots = func() []string { return []string{filepath.Dir(host)} } // a NAS share
	spec := &AppSpec{
		Source: Source{Type: "image", Image: "x:1"},
		Volumes: []Volume{
			{Name: "movies", Mount: "/data", Files: "Content/Movies"},
			{Name: "nas", Mount: "/nas", Host: host, Owner: "1000:1000", ReadOnly: true},
			{Name: "state", Mount: "/state"}, // managed: nothing above it is the app's
		},
		Aux: []AuxService{{Name: "side", Image: "y", Volumes: []Volume{{Name: "shared", Mount: "/s", Files: "Content/Shared"}}}},
	}
	if err := m.Render("app", spec, nil, "x:1", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []pinCall{
		{drive, filepath.Join("Content", "Movies"), m.PinPath("app", "movies"), ""},
		{"/", strings.TrimPrefix(host, "/"), m.PinPath("app", "nas"), "1000:1000"},
		{drive, filepath.Join("Content", "Shared"), m.PinPath("app", "shared"), ""},
	}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("pins = %v\nwant %v", calls, want)
	}
	if len(unpinned) != 1 || unpinned[0] != filepath.Join(m.BindsDir(), "app")+" keep=movies,nas,shared" {
		t.Fatalf("unpin = %v", unpinned)
	}
	services, _ := readCompose(t, m, "app")
	vols := fmt.Sprint(services["app"]["volumes"])
	for _, bad := range []string{drive, host} {
		if strings.Contains(vols, bad) {
			t.Errorf("compose binds %s itself: %s", bad, vols)
		}
	}
	for _, w := range []string{"source:" + m.PinPath("app", "movies"), "source:" + m.PinPath("app", "nas"), "create_host_path:false", "read_only:true"} {
		if !strings.Contains(vols, w) {
			t.Errorf("compose volumes missing %q: %s", w, vols)
		}
	}
	if !strings.Contains(fmt.Sprint(services["side"]["volumes"]), m.PinPath("app", "shared")) {
		t.Errorf("sidecar binds its path: %v", services["side"]["volumes"])
	}
	if !strings.Contains(vols, filepath.Join(m.DataPath("app"), "state")) {
		t.Errorf("managed volume missing: %s", vols)
	}
}

// A pin that fails — the folder became a link — fails the deploy, and
// compose is never told to bind the path instead.
func TestRenderFailsWhenAPinIsRefused(t *testing.T) {
	m := &Materializer{
		DataDir:   t.TempDir(),
		FilesRoot: func() string { return t.TempDir() },
		Pin: func(context.Context, string, string, string, string) error {
			return fmt.Errorf("Content/Movies passes through a symbolic link")
		},
	}
	spec := &AppSpec{Source: Source{Type: "image", Image: "x:1"}, Volumes: []Volume{{Name: "m", Mount: "/m", Files: "Content/Movies"}}}
	if err := m.Render("app", spec, nil, "x:1", "", io.Discard); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
}

// Where fog can't pin, one app may not hold a writable volume with another
// of its volumes inside it: it could swap the inner folder for a link
// between two starts.
func TestNestedVolumesRefusedWithoutPins(t *testing.T) {
	root := "/drive"
	vol := func(name, files string, ro bool) Volume {
		return Volume{Name: name, Mount: "/" + name, Files: files, ReadOnly: ro}
	}
	host := func(name, p string) Volume { return Volume{Name: name, Mount: "/" + name, Host: p} }
	for _, c := range []struct {
		vols []Volume
		bad  bool
	}{
		{[]Volume{vol("a", "Apps/Evil", false), vol("b", "Apps/Evil/x", false)}, true},
		{[]Volume{vol("b", "Apps/Evil/x", false), vol("a", "Apps/Evil", false)}, true},
		{[]Volume{vol("a", "Apps/Evil", true), vol("b", "Apps/Evil/x", false)}, false}, // outer read-only
		{[]Volume{vol("a", "Content", false), vol("b", "Content2", false)}, false},     // a sibling, not inside
		{[]Volume{vol("a", "Content", false), vol("b", "Content", false)}, false},      // the same folder twice
		{[]Volume{host("a", "/mnt/media"), host("b", "/mnt/media/tv")}, true},
		{[]Volume{host("a", "/mnt/media"), vol("b", "Content", false)}, false},
	} {
		err := checkNestedVolumes(&AppSpec{Volumes: c.vols}, root)
		if (err != nil) != c.bad {
			t.Errorf("%v: err = %v, want refused=%v", c.vols, err, c.bad)
		}
	}
	// And Render asks it when there is no pinning.
	m := &Materializer{DataDir: t.TempDir(), FilesRoot: func() string { return t.TempDir() }}
	spec := &AppSpec{Source: Source{Type: "image", Image: "x:1"}, Volumes: []Volume{vol("a", "Apps/Evil", false), vol("b", "Apps/Evil/x", false)}}
	if err := m.Render("evil", spec, nil, "x:1", "", io.Discard); err == nil {
		t.Fatal("rendered nested writable volumes without pins")
	}
}

// The helper runs as root: it pins and unpins only under binds.
func TestPinTargetMustBeUnderBinds(t *testing.T) {
	for _, target := range []string{"/etc", "/var/lib/fog/binds", "/var/lib/fog/binds/../x", "/var/lib/fog/bindsx/a"} {
		if err := checkPinTarget("/var/lib/fog/binds", target); err == nil {
			t.Errorf("%s accepted", target)
		}
	}
	if err := checkPinTarget("/var/lib/fog/binds", "/var/lib/fog/binds/app/vol"); err != nil {
		t.Error(err)
	}
}
