// Regression coverage for managed-volume ownership: the recursive chown must
// run only when it can change anything. It runs against a fake volumeChown
// that records each call as chown(1)'s argv would read.
package apps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeChown stands in for volumeChown: it appends each call to a log file,
// as "[-R ]uid:gid path", changes nothing, and fails with failure when that
// is set.
func fakeChown(t *testing.T, failure string) (logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "chown.log")
	real := volumeChown
	t.Cleanup(func() { volumeChown = real })
	volumeChown = func(_ *os.File, path string, uid, gid int, recurse bool) error {
		line := fmt.Sprintf("%d:%d %s", uid, gid, path)
		if recurse {
			line = "-R " + line
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintln(f, line)
		f.Close()
		if failure != "" {
			return errors.New(failure)
		}
		return nil
	}
	return logPath
}

// A volume tree that already carries its spec'd Owner must not be walked
// again on every deploy — chown -R over a media archive takes minutes. The
// full walk happens exactly twice: when the dir is first created, and when
// the Owner is later changed out from under an existing tree.
func TestVolumeOwnerChownOnlyWhenNeeded(t *testing.T) {
	logPath := fakeChown(t, "")
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	// The test process owns what it creates, so this Owner matches the root
	// that the first render materializes.
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	spec := &AppSpec{
		Source:  Source{Type: "image", Image: "x:latest"},
		Volumes: []Volume{{Name: "media", Mount: "/data", Owner: owner}},
	}
	render := func() {
		t.Helper()
		if err := m.Render("app", spec, nil, "x:latest", "", io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	chowns := func() []string {
		b, _ := os.ReadFile(logPath)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}

	render()
	dataDir := filepath.Join(dir, "data", "app", "media")
	if got := chowns(); len(got) != 1 || got[0] != "-R "+owner+" "+dataDir {
		t.Fatalf("first render: chown calls = %q, want one recursive chown of the new dir", got)
	}

	// Redeploy: the root already matches, so the whole walk is skipped.
	render()
	if got := chowns(); len(got) != 1 {
		t.Errorf("second render re-chowned an already-owned tree: %q", got)
	}

	// A changed Owner is the rare case that earns the full walk.
	spec.Volumes[0].Owner = "12345:54321"
	render()
	if got := chowns(); len(got) != 2 || got[1] != "-R 12345:54321 "+dataDir {
		t.Errorf("changed owner: chown calls = %q, want a second recursive chown", got)
	}
}

// Ownership problems are non-fatal (chown fails on dev machines where uid
// mapping doesn't matter) but must land in the deploy log, not the void —
// and both halves of "uid:gid" are validated before anything is executed.
func TestVolumeOwnerProblemsSurfaceAsWarnings(t *testing.T) {
	logPath := fakeChown(t, "cannot-touch-this")
	m := &Materializer{DataDir: t.TempDir()}
	n := 0
	render := func(owner string) string {
		t.Helper()
		n++ // a fresh app per case, so every volume dir is newly created
		spec := &AppSpec{
			Source:  Source{Type: "image", Image: "x:latest"},
			Volumes: []Volume{{Name: "v", Mount: "/v", Owner: owner}},
		}
		var warn bytes.Buffer
		if err := m.Render(fmt.Sprintf("app%d", n), spec, nil, "x:latest", "", &warn); err != nil {
			t.Fatalf("owner %q: render must stay non-fatal: %v", owner, err)
		}
		return warn.String()
	}

	// A failed chown: the warning carries what it said.
	if got := render("12345:54321"); !strings.Contains(got, "cannot-touch-this") {
		t.Errorf("chown failure not surfaced with its output: %q", got)
	}
	// Malformed owners never reach chown at all.
	before, _ := os.ReadFile(logPath)
	if got := render("12345"); !strings.Contains(got, "not uid:gid") {
		t.Errorf("colonless owner not reported: %q", got)
	}
	if got := render("abc:12345"); !strings.Contains(got, "uid") || !strings.Contains(got, "not numeric") {
		t.Errorf("non-numeric uid not reported: %q", got)
	}
	if got := render("12345:abc"); !strings.Contains(got, "gid") || !strings.Contains(got, "not numeric") {
		t.Errorf("non-numeric gid not reported: %q", got)
	}
	if after, _ := os.ReadFile(logPath); !bytes.Equal(before, after) {
		t.Error("a malformed owner was handed to chown")
	}
}

// A host path is someone else's directory: fog creates the bind source itself
// (Docker would make it root:root, which is how an app pointed at a NAS share
// ends up unable to write to it) and chowns the mount root only — never a
// recursive walk over a share fog doesn't own.
func TestHostVolumeOwnerChownsRootOnly(t *testing.T) {
	logPath := fakeChown(t, "")
	share := t.TempDir()
	hostPath := filepath.Join(share, "media")
	m := &Materializer{DataDir: t.TempDir()}
	spec := &AppSpec{
		Source:                Source{Type: "image", Image: "x:latest"},
		Volumes:               []Volume{{Name: "media", Mount: "/data/media", Host: hostPath, Owner: "1000:1000"}},
		AcknowledgeHostAccess: []string{hostPath}, // a host folder outside fog's storage
	}
	render := func() {
		t.Helper()
		if err := m.Render("app", spec, nil, "x:latest", "", io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	chowns := func() []string {
		b, _ := os.ReadFile(logPath)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}

	render()
	if st, err := os.Stat(hostPath); err != nil || !st.IsDir() {
		t.Fatalf("missing bind source not created: %v", err)
	}
	if got := chowns(); len(got) != 1 || got[0] != "1000:1000 "+hostPath {
		t.Fatalf("chown calls = %q, want one non-recursive chown of the mount root", got)
	}

	// The dir now exists but is owned by whoever ran the test, not 1000:1000
	// (the fake chown changes nothing), so a redeploy chowns the root again —
	// cheap, and the only way an unapplied Owner ever gets applied.
	render()
	if got := chowns(); len(got) != 2 {
		t.Fatalf("chown calls = %q, want the root re-chowned while it disagrees", got)
	}

	// Once it matches, the deploy stops touching it.
	spec.Volumes[0].Owner = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	render()
	before := len(chowns())
	render()
	if got := chowns(); len(got) != before {
		t.Errorf("chown calls = %q, want none once the root already matches", got)
	}
}

// A host path whose parent is missing is the "share isn't mounted" case.
// Creating a tree under a dead mountpoint would hide that behind a directory
// quietly filling the root disk, so fog warns and creates nothing.
func TestHostVolumeMissingParentIsNotMaterialized(t *testing.T) {
	logPath := fakeChown(t, "")
	hostPath := filepath.Join(t.TempDir(), "unmounted-share", "media")
	m := &Materializer{DataDir: t.TempDir()}
	spec := &AppSpec{
		Source:                Source{Type: "image", Image: "x:latest"},
		Volumes:               []Volume{{Name: "media", Mount: "/data/media", Host: hostPath, Owner: "1000:1000"}},
		AcknowledgeHostAccess: []string{hostPath}, // a host folder outside fog's storage
	}
	var warn bytes.Buffer
	if err := m.Render("app", spec, nil, "x:latest", "", &warn); err != nil {
		t.Fatalf("an unmounted share must not fail the render: %v", err)
	}
	if got := warn.String(); !strings.Contains(got, "is the share mounted?") {
		t.Errorf("no warning about the missing parent: %q", got)
	}
	if _, err := os.Stat(hostPath); !os.IsNotExist(err) {
		t.Error("fog created a tree under a missing mountpoint")
	}
	if b, _ := os.ReadFile(logPath); len(b) != 0 {
		t.Errorf("chown ran on a path fog refused to create: %q", b)
	}
}

// A host path with no Owner keeps its old behavior exactly: fog leaves both
// the directory and its ownership alone.
func TestHostVolumeWithoutOwnerIsUntouched(t *testing.T) {
	logPath := fakeChown(t, "")
	hostPath := filepath.Join(t.TempDir(), "media")
	m := &Materializer{DataDir: t.TempDir()}
	spec := &AppSpec{
		Source:                Source{Type: "image", Image: "x:latest"},
		Volumes:               []Volume{{Name: "media", Mount: "/data/media", Host: hostPath}},
		AcknowledgeHostAccess: []string{hostPath},
	}
	if err := m.Render("app", spec, nil, "x:latest", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hostPath); !os.IsNotExist(err) {
		t.Error("an ownerless host path was created by fog")
	}
	if b, _ := os.ReadFile(logPath); len(b) != 0 {
		t.Errorf("chown ran for a volume with no Owner: %q", b)
	}
}

// An init hook is only run by the image if it is executable — a rendered
// 0644 script is skipped with a line in the log and nothing else, so the
// mode is the whole point of the file.
func TestExecFileIsRenderedExecutable(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{
		Source: Source{Type: "image", Image: "x:latest"},
		Files: []File{
			{Name: "init", Mount: "/custom-cont-init.d/10-init", Content: "#!/bin/sh\n", Exec: true},
			{Name: "conf", Mount: "/etc/app.conf", Content: "k = v\n"},
		},
	}
	if err := m.Render("app", spec, nil, "x:latest", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"init": 0o755, "conf": 0o644} {
		st, err := os.Stat(filepath.Join(dir, "apps", "app", "files", name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("files/%s mode = %v, want %v", name, st.Mode().Perm(), want)
		}
	}
}
