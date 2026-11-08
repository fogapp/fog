package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A release directory is what release.sh assembles in bin/release/ and
// uploads: it must be enough, on its own, to install from and update from —
// served by any static host, no repo behind it. These tests build one with
// release.sh's own manifest() function (sourced out of the script), sign it,
// serve it as a plain directory, and point install.sh, flash.sh and the
// updater at it.

// releaseDir writes the files release.sh puts in bin/release/, with
// stand-in binaries, and the manifest release.sh's manifest() writes for
// them (relative names, and an installer image when withImage).
func releaseDir(t *testing.T, s *signer, version, fogd string, withImage bool) string {
	t.Helper()
	skipShell(t)
	dir := t.TempDir()
	files := map[string]string{
		"fogd-linux-amd64": fogd, "fogd-linux-arm64": fogd,
		"fog-node-darwin-arm64": "node", "fog-node-darwin-amd64": "node",
		"fog-node-linux-amd64": "node", "fog-node-linux-arm64": "node",
	}
	for _, f := range []string{"fogd.service", "fogd-rollback", "fogd.socket"} {
		b, err := os.ReadFile("../../packaging/" + f)
		if err != nil {
			t.Fatal(err)
		}
		files[f] = string(b)
	}
	for _, f := range []string{"install.sh", "imaging/flash.sh"} {
		b, err := os.ReadFile("../../" + f)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(f)] = string(b)
	}
	img := ""
	if withImage {
		files["fog-installer-x86_64.img"] = "a bootable image"
		img = "fog-installer-x86_64.img"
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := `set -eu
eval "$(sed -n '/^sha() {/,/^}/p' ../../scripts/release.sh)"
eval "$(sed -n '/^manifest() {/,/^}/p' ../../scripts/release.sh)"
manifest "" "$IMG_URL"`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "VERSION="+version, "REL="+dir, "IMG=fog-installer-x86_64.img", "IMG_URL="+img,
		fmt.Sprintf("ISSUED_AT=%d", time.Now().Unix()))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("release.sh manifest(): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "latest.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "latest.json.sig"), []byte(s.sign(string(out))), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func serveDir(t *testing.T, dir string) string {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/pub/fog/", http.FileServer(http.Dir(dir))))
	t.Cleanup(srv.Close)
	return srv.URL + "/pub/fog"
}

// install.sh, from the directory alone: the manifest, the binary, the
// unit and the guard all resolve beside it and verify.
func TestReleaseDirectoryInstallsOnItsOwn(t *testing.T) {
	t.Parallel()

	f := newInstallFixture(t, "v0.0.1", "#!/bin/sh\nexit 0\n")
	dir := releaseDir(t, f.s, "v1.4.0", "#!/bin/sh\n# fogd v1.4.0\nexit 0\n", false)
	src := serveDir(t, dir)

	out, err := f.run(t, "FOG_SOURCE="+src, "http_proxy=")
	if err != nil {
		t.Fatalf("install.sh from a release directory: %v\n%s", err, out)
	}
	if got := f.read(t, "usr/local/bin/fogd"); !strings.Contains(got, "fogd v1.4.0") {
		t.Errorf("fogd = %q", got)
	}
	want, _ := os.ReadFile("../../packaging/fogd-rollback")
	if got := f.read(t, "usr/local/bin/fogd-rollback"); got != string(want) {
		t.Error("fogd-rollback isn't the release's")
	}
	if got := f.read(t, "etc/systemd/system/fogd.service"); !strings.Contains(got, "ExecStartPre=-/usr/local/bin/fogd-rollback --boot") {
		t.Errorf("unit lacks the rollback guard:\n%s", got)
	}

	// The updater reads the same directory, and the manifest lists every
	// file install.sh and fogd look up.
	m, err := f.s.updater().resolveManifest(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"linux-amd64", "linux-arm64", "fogd.service", "fogd-rollback", "fogd.socket", "install.sh", "flash.sh",
		"fog-node-darwin-arm64", "fog-node-darwin-amd64", "fog-node-linux-amd64", "fog-node-linux-arm64"} {
		a, ok := m.Artifacts[k]
		if !ok || !strings.HasPrefix(a.URL, src+"/") || len(a.SHA256) != 64 {
			t.Errorf("manifest %s = %+v", k, a)
		}
	}
	// install.sh itself ships in the directory, where the Proxmox scripts
	// look for it beside a manifest.
	resp, err := http.Get(src + "/install.sh")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("install.sh not served from the release directory: %v", err)
	}
}

// flash.sh resolves a relative installer entry against the manifest's own
// URL — before, it handed curl the bare file name.
func TestFlashResolvesARelativeImage(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	needOpenSSL(t, s)
	dir := releaseDir(t, s, "v1.4.0", "#!/bin/sh\nexit 0\n", true)
	src := serveDir(t, dir)

	run := func(env ...string) (string, error) {
		cmd := exec.Command("sh", "../../imaging/flash.sh")
		cmd.Env = append(os.Environ(), "FOG_FLASH_RESOLVE_ONLY=1", "FOG_RELEASE_KEYS="+s.spki(t))
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run("FOG_MANIFEST_URL=" + src + "/latest.json")
	if err != nil {
		t.Fatalf("flash.sh: %v\n%s", err, out)
	}
	if want := "image=" + src + "/fog-installer-x86_64.img"; !strings.Contains(out, want) {
		t.Errorf("flash.sh resolved:\n%s\nwant %s", out, want)
	}
	if want := "sha256=" + sum("a bootable image"); !strings.Contains(out, want) {
		t.Errorf("flash.sh checksum:\n%s\nwant %s", out, want)
	}

	// A manifest it can't verify is refused, image and all.
	os.WriteFile(filepath.Join(dir, "latest.json.sig"), []byte(newSigner(t).sign("x")), 0o644)
	if out, err := run("FOG_MANIFEST_URL=" + src + "/latest.json"); err == nil || !strings.Contains(out, "doesn't match") {
		t.Errorf("flash.sh accepted a manifest signed by someone else (err %v):\n%s", err, out)
	}
}

// flash.sh hashes the bytes on their way to the stick: an image swapped in
// the cache after its checksum was checked is caught, not booted.
func TestFlashCatchesAnImageSwappedBeforeWriting(t *testing.T) {
	t.Parallel()

	skipShell(t)
	dir := t.TempDir()
	good, evil := "the verified image\n", "a swapped-in image\n"
	os.WriteFile(filepath.Join(dir, "img"), []byte(evil), 0o644) // swapped after the check
	ibs, obs := "1m", "4m"
	if runtime.GOOS == "linux" {
		ibs, obs = "1M", "4M"
	}
	script := `set -eu
say() { printf '%s\n' "$*"; }
die() { printf 'fog-flash: %s\n' "$*" >&2; exit 1; }
eval "$(sed -n '/^write_image() {/,/^}/p' ../../imaging/flash.sh)"
write_image "$DEV" "$IBS" "$OBS"`
	run := func(sha string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "WORK="+dir, "IMG="+filepath.Join(dir, "img"), "IMG_SHA="+sha,
			"DEV="+filepath.Join(dir, "stick"), "IBS="+ibs, "OBS="+obs)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(sum(good)); err == nil || !strings.Contains(out, "don't boot this stick") {
		t.Fatalf("a swapped image was written without complaint (err %v):\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "img"), []byte(good), 0o644)
	if out, err := run(sum(good)); err != nil || !strings.Contains(out, "written and verified") {
		t.Fatalf("the verified image: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stick")); string(b) != good {
		t.Errorf("stick holds %q", b)
	}
}
