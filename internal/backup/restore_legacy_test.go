package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// fakeDocker puts a stub docker on PATH so the compose stop/up bracketing a
// restore runs without the real thing. Invocations land in the returned log
// file, one line of args each; exitCode is what every invocation returns.
func fakeDocker(t *testing.T, exitCode string) string {
	t.Helper()
	bin := t.TempDir()
	logPath := filepath.Join(bin, "invocations.log")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logPath + "\"\nexit " + exitCode + "\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// legacyRestoreManager builds a Manager plus a legacy (volume-snapshot)
// backup for one app with a single managed volume named "data". The live
// volume holds old.txt; the snapshot archive holds new.txt.
func legacyRestoreManager(t *testing.T) (m *Manager, b *store.Backup, appRow *store.App, volDir string) {
	t.Helper()
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	m = &Manager{
		DataDir: t.TempDir(),
		Store:   st,
		Mat:     &apps.Materializer{DataDir: t.TempDir()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notify:  func(string, string, bool) {},
	}
	// The bring-up after a restore is the engine's (rendered afresh under
	// the rules); here it is said in the docker log, to be seen.
	m.startSeam = func(ctx context.Context, name string) error {
		_, err := execx.Run(execx.Command(ctx, "docker", "bring-up", name))
		return err
	}
	// The restore reads the live spec to learn where each volume lives, so
	// the app needs a revision as well as a row.
	created, err := st.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRevision(created.ID, `{"source":{"type":"image","image":"demo:latest"},"volumes":[{"name":"data","mount":"/data"}]}`, "initial", "test"); err != nil {
		t.Fatal(err)
	}
	appRow = created

	// ComposeCmd chdirs into the app's directory; it must exist.
	if err := os.MkdirAll(m.Mat.AppDir(appRow.Name), 0o755); err != nil {
		t.Fatal(err)
	}
	volDir = filepath.Join(m.Mat.DataPath(appRow.Name), "data")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "old.txt"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}

	backupDir := t.TempDir()
	meta := `{"app":"demo","rev":1,"volumes":["data"],"createdAt":"2026-08-24T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(backupDir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	writeVolumeArchive(t, filepath.Join(backupDir, "volume-data.tar.gz"))
	b = &store.Backup{ID: 1, AppID: 1, Path: backupDir}
	return m, b, appRow, volDir
}

// writeVolumeArchive builds what backupApp's `tar -C <parent> data` would:
// a gzipped tar whose tree is rooted at data/.
func writeVolumeArchive(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	payload := "restored"
	if err := tw.WriteHeader(&tar.Header{Name: "data/new.txt", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(payload))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertVolumeUntouched(t *testing.T, volDir string) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(volDir, "old.txt")); err != nil || string(got) != "live" {
		t.Fatalf("live volume was touched: %q, %v", got, err)
	}
	for _, suffix := range []string{".restoring", ".old"} {
		if _, err := os.Stat(volDir + suffix); !os.IsNotExist(err) {
			t.Fatalf("leftover %s directory after failed restore", suffix)
		}
	}
}

// A corrupt snapshot archive must fail the restore before anything live is
// touched: the volume keeps its contents and the app is never even stopped.
func TestRestoreCorruptArchiveLeavesVolume(t *testing.T) {
	dockerLog := fakeDocker(t, "0")
	m, b, appRow, volDir := legacyRestoreManager(t)
	if err := os.WriteFile(filepath.Join(b.Path, "volume-data.tar.gz"), []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := m.restoreApp(context.Background(), b, appRow, false)
	if err == nil {
		t.Fatal("restore of a corrupt archive reported success")
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("error does not name the unreadable archive: %v", err)
	}
	assertVolumeUntouched(t, volDir)
	if _, err := os.Stat(dockerLog); !os.IsNotExist(err) {
		t.Fatal("compose was invoked before the archives were validated")
	}
}

// A truncated archive passes a gzip-header sniff but not a full walk; it must
// fail validation the same way.
func TestRestoreTruncatedArchiveLeavesVolume(t *testing.T) {
	fakeDocker(t, "0")
	m, b, appRow, volDir := legacyRestoreManager(t)
	ar := filepath.Join(b.Path, "volume-data.tar.gz")
	whole, err := os.ReadFile(ar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ar, whole[:len(whole)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.restoreApp(context.Background(), b, appRow, false); err == nil {
		t.Fatal("restore of a truncated archive reported success")
	}
	assertVolumeUntouched(t, volDir)
}

// Deleting volumes under running containers corrupts both old and new state,
// so a compose stop that fails must abort the restore with the data intact.
func TestRestoreAbortsWhenStopFails(t *testing.T) {
	fakeDocker(t, "1")
	m, b, appRow, volDir := legacyRestoreManager(t)

	err := m.restoreApp(context.Background(), b, appRow, false)
	if err == nil {
		t.Fatal("restore proceeded past a failed compose stop")
	}
	if !strings.Contains(err.Error(), "stop before restore") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVolumeUntouched(t, volDir)
}

// The success path: valid archives replace the volume's contents wholesale
// and leave no staging or .old debris behind.
func TestRestoreSwapsValidArchive(t *testing.T) {
	dockerLog := fakeDocker(t, "0")
	m, b, appRow, volDir := legacyRestoreManager(t)

	if err := m.restoreApp(context.Background(), b, appRow, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(volDir, "new.txt")); err != nil || string(got) != "restored" {
		t.Fatalf("restored file: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(volDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("pre-restore contents survived the swap")
	}
	for _, suffix := range []string{".restoring", ".old"} {
		if _, err := os.Stat(volDir + suffix); !os.IsNotExist(err) {
			t.Fatalf("leftover %s directory after successful restore", suffix)
		}
	}
	log, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	if lines := composeVerbs(string(log)); lines != "compose stop\nbring-up demo" {
		t.Fatalf("compose invocations: %q", lines)
	}
}

// A restore into a volume that does not exist yet (never materialized) must
// still land the snapshot rather than fail the old-dir rename.
func TestRestoreIntoMissingVolume(t *testing.T) {
	fakeDocker(t, "0")
	m, b, appRow, volDir := legacyRestoreManager(t)
	if err := os.RemoveAll(volDir); err != nil {
		t.Fatal(err)
	}

	if err := m.restoreApp(context.Background(), b, appRow, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(volDir, "new.txt")); err != nil || string(got) != "restored" {
		t.Fatalf("restored file: %q, %v", got, err)
	}
}

// What an app owns travels in its snapshot and what it merely reads does
// not: its managed volumes and its own folder under Apps are addressable,
// a content library several apps share and an arbitrary host path are not.
func TestVolumePathDrawsTheOwnershipLine(t *testing.T) {
	m := &Manager{
		DataDir: t.TempDir(),
		Mat:     &apps.Materializer{DataDir: "/var/lib/fog", FilesRoot: func() string { return "/srv/drive" }},
	}
	cases := []struct {
		v    apps.Volume
		want string
	}{
		{apps.Volume{Name: "storage", Mount: "/rails/storage"}, "/var/lib/fog/data/paperless/storage"},
		{apps.Volume{Name: "config", Files: "Apps/Paperless/config"}, "/srv/drive/Apps/Paperless/config"},
		{apps.Volume{Name: "consume", Files: "Content/Scans"}, ""},
		{apps.Volume{Name: "media", Files: "Apps/Jellyfin/media"}, ""},
		{apps.Volume{Name: "archive", Host: "/mnt/big"}, ""},
		{apps.Volume{}, ""},
	}
	for _, c := range cases {
		if got := m.volumePath("paperless", c.v); got != c.want {
			t.Errorf("volumePath(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}
