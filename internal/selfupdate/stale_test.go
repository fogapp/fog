package selfupdate

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/store"
)

// Signatures don't expire, so an old signed manifest stays valid forever:
// a mirror could hand a fresh install an old release, or hold a running
// server on one. The signed issued_at bounds that to MaxManifestAge.

func datedManifest(version string, issued time.Time) string {
	if issued.IsZero() {
		return fmt.Sprintf(`{"version":%q,"artifacts":{}}`, version)
	}
	return fmt.Sprintf(`{"version":%q,"issued_at":%d,"artifacts":{}}`, version, issued.Unix())
}

func TestUpdaterRefusesAnOldSignedRelease(t *testing.T) {
	s := newSigner(t)
	old := time.Now().Add(-MaxManifestAge - 24*time.Hour)
	for _, tc := range []struct {
		name   string
		issued time.Time
	}{{"older than the limit", old}, {"undated", time.Time{}}} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			s.serve(mux, "/latest.json", datedManifest("v1.0.0", tc.issued))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			_, err := s.updater().resolveManifest(context.Background(), srv.URL+"/latest.json")
			var stale *StaleError
			if !errors.As(err, &stale) {
				t.Fatalf("err = %v, want a StaleError", err)
			}
		})
	}

	// A stale manifest at the first candidate doesn't stop a current one
	// at the next.
	mux := http.NewServeMux()
	s.serve(mux, "/me/fog/raw/branch/main/release/latest.json", datedManifest("v1.0.0", old))
	s.serve(mux, "/me/fog/latest.json", datedManifest("v1.1.0", time.Now()))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m, err := s.updater().resolveManifest(context.Background(), srv.URL+"/me/fog")
	if err != nil || m.Version != "v1.1.0" {
		t.Fatalf("m = %+v, err = %v; want the current v1.1.0", m, err)
	}
}

// A source that serves only an old release is reachable but withholding:
// the Inbox says so at once (and the phone hears it once), and the notice
// comes down when a current release is back.
func TestUpdaterRaisesAFreezeNotice(t *testing.T) {
	s := newSigner(t)
	var body string
	mux := http.NewServeMux()
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	mux.HandleFunc("/latest.json.sig", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(s.sign(body))) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingUpdateBaseURL, srv.URL+"/latest.json")
	u := s.updater()
	u.Store, u.version = st, "v1.0.0"
	streak := &notify.FailStreak{Store: st, Kind: "selfupdate-unreachable", Threshold: 3}
	var sent []string
	send := func(title, msg string, ok bool) { sent = append(sent, title) }
	active := func(kind string) bool {
		ns, _ := st.ActiveNotices()
		for _, n := range ns {
			if n.Kind == kind {
				return true
			}
		}
		return false
	}

	body = datedManifest("v1.0.0", time.Now().Add(-200*24*time.Hour))
	u.checkOnce(context.Background(), streak, send)
	u.checkOnce(context.Background(), streak, send)
	if !active(staleNoticeKind) {
		t.Fatal("no freeze notice for a source serving a 200-day-old release")
	}
	if active("selfupdate-unreachable") {
		t.Error("a reachable, withholding source was reported unreachable")
	}
	if len(sent) != 1 {
		t.Errorf("phone told %d times, want once: %v", len(sent), sent)
	}
	if res := u.Cached(); res.UpdateAvailable || !strings.Contains(res.Error, "days ago") {
		t.Errorf("cached check = %+v", res)
	}

	body = datedManifest("v1.1.0", time.Now())
	u.checkOnce(context.Background(), streak, send)
	if active(staleNoticeKind) {
		t.Error("freeze notice stayed up after a current release came back")
	}
}

// install.sh on a fresh machine — nothing running to compare versions
// with — refuses an old release, however validly signed.
func TestInstallShRefusesAnOldReleaseOnAFreshMachine(t *testing.T) {
	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n")
	f.issued = time.Now().Add(-200 * 24 * time.Hour).Unix()
	f.release("v1.0.0", "#!/bin/sh\nexit 0\n", nil)
	out, err := f.run(t)
	if err == nil || !strings.Contains(out, "past the 180-day limit") {
		t.Fatalf("an old release was installed (err %v):\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(f.root, "usr/local/bin/fogd")); err == nil {
		t.Error("fogd installed from a refused release")
	}
	if out, err := f.run(t, "FOG_ALLOW_OLD_RELEASE=1"); err != nil {
		t.Fatalf("FOG_ALLOW_OLD_RELEASE=1: %v\n%s", err, out)
	}
}

func TestFlashRefusesAnOldRelease(t *testing.T) {
	s := newSigner(t)
	needOpenSSL(t, s)
	h := newHostServer(t)
	body := fmt.Sprintf(`{"version":"v1.0.0","issued_at":%d,"artifacts":{"installer-x86_64":{"url":"img","sha256":%q}}}`,
		time.Now().Add(-200*24*time.Hour).Unix(), sum("img"))
	h.set("files.test/fog/latest.json", body)
	h.set("files.test/fog/latest.json.sig", s.sign(body))
	cmd := exec.Command("sh", "../../imaging/flash.sh")
	cmd.Env = append(os.Environ(), "FOG_FLASH_RESOLVE_ONLY=1", "FOG_RELEASE_KEYS="+s.spki(t),
		"FOG_MANIFEST_URL=http://files.test/fog/latest.json", "http_proxy="+h.URL, "no_proxy=", "NO_PROXY=")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "past the 180-day limit") {
		t.Fatalf("flash.sh took an old release (err %v):\n%s", err, out)
	}
}

// SignedManifest (what fogd hands paired machines) holds the same age rule:
// an old signed manifest is passed over for a current one, and refused when
// it is all the source has.
func TestSignedManifestRefusesAnOldRelease(t *testing.T) {
	s := newSigner(t)
	old := datedManifest("v1.0.0", time.Now().Add(-MaxManifestAge-24*time.Hour))
	fresh := datedManifest("v1.1.0", time.Now())

	mux := http.NewServeMux()
	s.serve(mux, "/me/fog/raw/branch/main/release/latest.json", old)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingUpdateBaseURL, srv.URL+"/me/fog")
	u := s.updater()
	u.Store = st

	_, _, err = u.SignedManifest(context.Background())
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want a StaleError for a source with only an old release", err)
	}

	s.serve(mux, "/me/fog/latest.json", fresh)
	body, sig, err := u.SignedManifest(context.Background())
	if err != nil || string(body) != fresh {
		t.Fatalf("body %q, err %v; want the current manifest", body, err)
	}
	keys := ReleaseKeys
	ReleaseKeys = []string{base64.StdEncoding.EncodeToString(s.pub)}
	defer func() { ReleaseKeys = keys }()
	if err := VerifyReleaseManifest(body, sig); err != nil {
		t.Errorf("VerifyReleaseManifest(current): %v", err)
	}
	if err := VerifyReleaseManifest([]byte(old), []byte(s.sign(old))); !errors.As(err, &stale) {
		t.Errorf("VerifyReleaseManifest(old) = %v, want a StaleError", err)
	}
}
