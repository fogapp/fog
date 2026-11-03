package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// Apply swaps the running executable, so these tests point the updater's
// exePath seam at a scratch file — the swap machinery is exercised for real,
// against a file that isn't the test binary.

// applyFixture serves a manifest plus one artifact for this platform and
// returns an Updater whose exePath is a scratch "fogd" containing oldBinary.
// The manifest's checksum is always newBinary's; a non-nil artifact handler
// lets a test serve something else, or stall the download.
func applyFixture(t *testing.T, oldBinary, newBinary string, artifact http.HandlerFunc) (*Updater, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "fogd")
	if err := os.WriteFile(exe, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte(newBinary))
	mux := http.NewServeMux()
	signer := newSigner(t)
	signer.serve(mux, "/latest.json", fmt.Sprintf(`{"version":"v9.9.9","issued_at":%d,"artifacts":{%q:{"url":"fogd-new","sha256":%q}}}`,
		time.Now().Unix(), (&Updater{}).platformKey(), hex.EncodeToString(sum[:])))
	if artifact == nil {
		artifact = func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(newBinary))
		}
	}
	mux.HandleFunc("/fogd-new", artifact)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetSetting(SettingUpdateBaseURL, srv.URL+"/latest.json"); err != nil {
		t.Fatal(err)
	}
	// The stand-in binaries are text, which the real shape check refuses on
	// Linux; TestApplyRefusesABinaryThatCannotRunHere covers that check.
	// The guard is "armed" by recording it: rollback_test.go runs it.
	u := signer.updater()
	u.Store, u.exePath, u.version = st, exe, "v9.0.0"
	u.verify = func(string) error { return nil }
	u.arm = func(string, time.Duration) error { return nil }
	return u, exe
}

// leftoverTemps reports any .fogd-new-* files next to the executable —
// failed Applies must clean up after themselves.
func leftoverTemps(t *testing.T, exe string) []string {
	t.Helper()
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(exe), ".fogd-new-*"))
	if err != nil {
		t.Fatal(err)
	}
	return temps
}

func TestApplySwapsBinary(t *testing.T) {
	u, exe := applyFixture(t, "old binary", "new binary", nil)

	exited := make(chan int, 1)
	if err := u.Apply(context.Background(), func(code int) { exited <- code }); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Errorf("executable = %q, want the downloaded binary", got)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("executable mode = %v (err %v), want 0755", fi.Mode(), err)
	}
	prev, err := os.ReadFile(exe + ".prev")
	if err != nil {
		t.Fatalf(".prev missing: %v", err)
	}
	if string(prev) != "old binary" {
		t.Errorf(".prev = %q, want the old binary", prev)
	}
	if temps := leftoverTemps(t, exe); len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}
	select {
	case code := <-exited:
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Error("Apply never requested the restart exit")
	}
}

// The previous binary is linked aside, never renamed away: exe stays a
// complete binary throughout, and an older .prev is replaced.
func TestKeepPreviousLeavesExeInPlace(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "fogd")
	if err := os.WriteFile(exe, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".prev", []byte("ancient"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := keepPrevious(exe); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{exe: "running", exe + ".prev": "running"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", path, got, err, want)
		}
	}
}

func TestApplyChecksumMismatchLeavesBinaryAlone(t *testing.T) {
	// The manifest's checksum is for "new binary"; the server serves
	// something else, as a corrupted mirror would.
	u, exe := applyFixture(t, "old binary", "new binary", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tampered bytes"))
	})

	err := u.Apply(context.Background(), func(int) { t.Error("exit requested on a failed apply") })
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old binary" {
		t.Errorf("executable = %q, want it untouched", got)
	}
	if temps := leftoverTemps(t, exe); len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}
}

// A download whose checksum matches but which can't run on this machine —
// a manifest pointing linux-amd64 at the arm64 build, say — is refused
// before it replaces the running fogd. Before, only the checksum was
// checked, and systemd was left restarting a binary that could never start.
func TestApplyRefusesABinaryThatCannotRunHere(t *testing.T) {
	u, exe := applyFixture(t, "old binary", "new binary", nil)
	u.verify = func(path string) error { return checkBinary(path, "linux", "amd64") }

	err := u.Apply(context.Background(), func(int) { t.Error("exit requested on a failed apply") })
	if err == nil || !strings.Contains(err.Error(), "not an ELF binary") {
		t.Fatalf("err = %v, want the ELF check's refusal", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Errorf("executable = %q, want it untouched", got)
	}
	if temps := leftoverTemps(t, exe); len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}
}

// The ELF check is Linux's: a dev machine's own binaries aren't ELF.
func TestCheckBinaryOnlyChecksLinux(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fogd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkBinary(path, "darwin", "arm64"); err != nil {
		t.Errorf("darwin: %v", err)
	}
	if err := checkBinary(path, "linux", "arm64"); err == nil {
		t.Error("linux accepted a shell script as fogd")
	}
}

// Two operators (or one double-click) must not race the swap: the second
// Apply is refused while the first still holds the download.
func TestApplyRefusesConcurrentApply(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	u, exe := applyFixture(t, "old binary", "new binary", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Write([]byte("new binary"))
	})

	first := make(chan error, 1)
	go func() {
		first <- u.Apply(context.Background(), func(int) {})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first Apply never reached the download")
	}
	err := u.Apply(context.Background(), func(int) { t.Error("exit requested by the refused apply") })
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second Apply err = %v, want already-in-progress", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "new binary" {
		t.Errorf("executable = %q, want the downloaded binary", got)
	}
}

// A manifest committed with gitea.lan artifact URLs, read by a server that
// reaches Gitea by IP: the binary lives at the same path on the configured
// origin, not next to the manifest, so that path is tried.
func TestDownloadArtifactRebasesOntoSourceOrigin(t *testing.T) {
	const assetPath = "/matthew/fog/releases/download/v9.9.9/fogd-linux-amd64"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == assetPath {
			w.Write([]byte("new binary"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u := &Updater{}
	m := &Manifest{
		FetchedFrom:  srv.URL + "/matthew/fog/raw/branch/main/release/latest.json",
		SourceOrigin: srv.URL,
	}
	sum := sha256.Sum256([]byte("new binary"))
	dst := filepath.Join(t.TempDir(), "fogd")
	if _, err := u.downloadArtifact(context.Background(), m, ManifestArtifact{URL: "http://gitea.invalid" + assetPath, SHA256: hex.EncodeToString(sum[:])}, dst); err != nil {
		t.Fatalf("downloadArtifact: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new binary" {
		t.Errorf("downloaded %q", b)
	}
}

// A location that answers 200 with the wrong bytes — another server where
// the manifest's host used to be, a captive portal — is passed over for the
// next, not taken as the end of the download; and if none has the release's
// bytes, nothing unverified is left behind.
func TestDownloadArtifactSkipsALocationWithTheWrongBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old-host/fogd": // where the manifest points: something else now
			w.Write([]byte("<html>welcome to the hotel wifi</html>"))
		case "/pub/fogd": // beside the manifest
			w.Write([]byte("the real fogd"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u := &Updater{}
	m := &Manifest{FetchedFrom: srv.URL + "/pub/latest.json"}
	sum := sha256.Sum256([]byte("the real fogd"))
	art := ManifestArtifact{URL: srv.URL + "/old-host/fogd", SHA256: hex.EncodeToString(sum[:])}
	dst := filepath.Join(t.TempDir(), "fogd")
	if _, err := u.downloadArtifact(context.Background(), m, art, dst); err != nil {
		t.Fatalf("downloadArtifact: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "the real fogd" {
		t.Errorf("downloaded %q", b)
	}

	art.SHA256 = strings.Repeat("0", 64)
	_, err := u.downloadArtifact(context.Background(), m, art, dst)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("unverified bytes left at the destination")
	}
}

// An app mid-deploy blocks the update: fogd's restart would kill the run
// halfway. Checked up front, and again after the download in case a deploy
// started while it ran.
func TestApplyRefusesWhileAppsDeploy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		busyFrom int // Busy calls before this one report idle
	}{
		{"before download", 0},
		{"after download", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, exe := applyFixture(t, "old binary", "new binary", nil)
			calls := 0
			u.Busy = func() []string {
				calls++
				if calls > tc.busyFrom {
					return []string{"sonarr"}
				}
				return nil
			}
			err := u.Apply(context.Background(), func(int) { t.Error("exit requested while an app deploys") })
			if err == nil || !strings.Contains(err.Error(), "sonarr") {
				t.Fatalf("err = %v, want a refusal naming sonarr", err)
			}
			if got, _ := os.ReadFile(exe); string(got) != "old binary" {
				t.Errorf("executable = %q, want it untouched", got)
			}
			if temps := leftoverTemps(t, exe); len(temps) != 0 {
				t.Errorf("temp files left behind: %v", temps)
			}
		})
	}
}
