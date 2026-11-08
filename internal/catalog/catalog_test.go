package catalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/hostcpu"
)

// fog's own catalogs load without a problem: every entry validates
// (format.go's validateEntry), every companion installs with a click, and
// what catalog.yaml features is there.
func TestOwnCatalogsLoadClean(t *testing.T) {
	ownEntries(t)
}

func TestPreflight(t *testing.T) {
	bookorbit := byID(t, "bookorbit")
	if bookorbit.Requires == nil || bookorbit.Requires.CPU != "x86-64-v2" {
		t.Fatalf("bookorbit should require x86-64-v2, has %+v", bookorbit.Requires)
	}
	kvm64 := hostcpu.Info{Arch: "amd64", Known: true, Level: 1, Virtual: true, Model: "Common KVM processor"}
	oldBox := hostcpu.Info{Arch: "amd64", Known: true, Level: 1, Model: "Intel(R) Core(TM)2 Duo"}
	cases := []struct {
		name  string
		host  hostcpu.Info
		block bool
		want  string
	}{
		{"kvm64 VM", kvm64, true, "Proxmox"},
		{"old physical box", oldBox, true, "physical processor"},
		{"v2 host", hostcpu.Info{Arch: "amd64", Known: true, Level: 2}, false, ""},
		{"v3 host", hostcpu.Info{Arch: "amd64", Known: true, Level: 3}, false, ""},
		{"arm64", hostcpu.Info{Arch: "arm64", Known: true}, false, ""},
		{"unreadable", hostcpu.Info{Arch: "amd64"}, false, ""},
	}
	for _, c := range cases {
		got := bookorbit.Preflight(c.host)
		if (got != "") != c.block {
			t.Errorf("%s: blocked=%v, want %v (%q)", c.name, got != "", c.block, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q should mention %q", c.name, got, c.want)
		}
		if got != "" && !strings.Contains(got, "Common KVM processor") && !strings.Contains(got, "Core(TM)2") {
			t.Errorf("%s: %q should name the processor", c.name, got)
		}
	}
	// An entry with no requirements never blocks, whatever the host.
	for _, e := range fixtureEntries(t) {
		if e.Requires == nil && e.Preflight(kvm64) != "" {
			t.Errorf("%s: blocked with no requirements", e.ID)
		}
	}
}

// A hand-built app on the same image inherits the entry's requirements —
// tag and digest don't change what the binaries need.
func TestPreflightSpec(t *testing.T) {
	kvm64 := hostcpu.Info{Arch: "amd64", Known: true, Level: 1, Virtual: true, Model: "Common KVM processor"}
	for _, img := range []string{"ghcr.io/bookorbit/bookorbit:latest", "ghcr.io/bookorbit/bookorbit:v1.2.3", "ghcr.io/bookorbit/bookorbit@sha256:abc"} {
		spec := apps.AppSpec{Source: apps.Source{Type: "image", Image: img}}
		if PreflightSpec(fixtureEntries(t), spec, kvm64) == "" {
			t.Errorf("%s: should be blocked on kvm64", img)
		}
	}
	for _, img := range []string{"ghcr.io/bookorbit/other:latest", "lscr.io/linuxserver/sonarr:latest", ""} {
		spec := apps.AppSpec{Source: apps.Source{Type: "image", Image: img}}
		if got := PreflightSpec(fixtureEntries(t), spec, kvm64); got != "" {
			t.Errorf("%q: unexpectedly blocked: %s", img, got)
		}
	}
	git := apps.AppSpec{Source: apps.Source{Type: "git", Repo: "https://example.com/x.git"}}
	if PreflightSpec(fixtureEntries(t), git, kvm64) != "" {
		t.Error("git source blocked")
	}
}

// SABnzbd refuses any request whose Host header it doesn't recognise, and
// behind Fog's proxy that header is the app's own hostname: without the init
// hook that whitelists it, a fresh install answers "Access denied - Hostname
// verification failed" at the address Fog just handed the operator.
func TestSabnzbdWhitelistsItsOwnHostname(t *testing.T) {
	sab := ownByID(t, "sabnzbd").Spec
	if len(sab.Files) == 0 {
		t.Fatal("no sabnzbd entry, or it renders no files")
	}
	if sab.Env["FOG_FQDN"] != "{{fqdn}}" {
		t.Errorf("FOG_FQDN = %q, want the hostname template", sab.Env["FOG_FQDN"])
	}
	found := false
	for _, f := range sab.Files {
		if !strings.HasPrefix(f.Mount, "/custom-cont-init.d/") {
			continue
		}
		if !f.Exec {
			t.Errorf("init hook %s is not executable — the image will skip it", f.Mount)
		}
		if strings.Contains(f.Content, "host_whitelist") && strings.Contains(f.Content, "FOG_FQDN") {
			found = true
		}
	}
	if !found {
		t.Error("no /custom-cont-init.d hook — the hostname never reaches SABnzbd")
	}
}

// Every image app links to its upstream project: nothing in an lscr.io or
// Docker Hub image name says where the code lives, so a new entry without a
// projectUrl would silently show no Source on its app page.
func TestImageEntriesHaveProjectURL(t *testing.T) {
	for _, e := range ownEntries(t) {
		if e.Spec.Source.Type != "image" {
			continue
		}
		if !strings.HasPrefix(e.ProjectURL, "https://") {
			t.Errorf("%s: image entry has no https projectUrl (got %q)", e.ID, e.ProjectURL)
		}
	}
}

// The API key hook holds SABnzbd's key to the vault's, keeping every other
// line, and leaves the file alone when there is no key to hold.
func TestSabnzbdAPIKeyHook(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	ini := filepath.Join(dir, "sabnzbd.ini")
	script := strings.NewReplacer("#!/usr/bin/with-contenv bash", "#!/bin/bash",
		"/config/sabnzbd.ini", ini, "lsiown", "true").Replace(sabHook(t, "api-key-init"))
	run := func(key string) string {
		t.Helper()
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "SABNZBD_API_KEY="+key)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(ini)
		return string(b)
	}
	os.WriteFile(ini, []byte("__version__ = 19\n[misc]\nhost_whitelist = sab.example\napi_key = minted\nnzb_key = n\n"), 0o644)
	if got := run(""); !strings.Contains(got, "api_key = minted") {
		t.Fatalf("no key must leave the file alone, got:\n%s", got)
	}
	got := run("fromvault")
	if !strings.Contains(got, "api_key = fromvault\n") || strings.Contains(got, "minted") ||
		!strings.Contains(got, "nzb_key = n") || !strings.Contains(got, "host_whitelist = sab.example") {
		t.Fatalf("replace:\n%s", got)
	}
	os.WriteFile(ini, []byte("[misc]\nhost_whitelist = sab.example\n"), 0o644)
	if got := run("k"); !strings.Contains(got, "[misc]\napi_key = k\nhost_whitelist") {
		t.Fatalf("insert:\n%s", got)
	}
	os.Remove(ini)
	if got := run("k"); got != "[misc]\napi_key = k\n" {
		t.Fatalf("first start:\n%q", got)
	}
}

// sabHook is one of the init hooks fog's SABnzbd entry mounts.
func sabHook(t *testing.T, name string) string {
	t.Helper()
	for _, f := range ownByID(t, "sabnzbd").Spec.Files {
		if f.Name == name {
			return f.Content
		}
	}
	t.Fatalf("sabnzbd has no file %s", name)
	return ""
}

// runSabHook runs one of SABnzbd's init hooks against a scratch ini (and a
// scratch /data), answering the ini afterwards.
func runSabHook(t *testing.T, hook, ini, data string, env ...string) string {
	t.Helper()
	script := strings.NewReplacer("#!/usr/bin/with-contenv bash", "#!/bin/bash",
		"/config/sabnzbd.ini", ini, "lsiown", "true", "shared=/data", "shared="+data).Replace(hook)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(ini)
	return string(b)
}

// The whitelist holds both names SABnzbd is reached by: its hostname behind
// the proxy and "sabnzbd" on the fog network, which the apps that use it
// call. Entries the operator added stay.
func TestSabnzbdHostWhitelistHook(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	ini := filepath.Join(dir, "sabnzbd.ini")
	if got := runSabHook(t, sabHook(t, "host-whitelist-init"), ini, dir, "FOG_FQDN=Sab.Example.com"); got != "[misc]\nhost_whitelist = sab.example.com, sabnzbd\n" {
		t.Fatalf("first start:\n%q", got)
	}
	os.WriteFile(ini, []byte("[misc]\nhost_whitelist = mine, sab.example.com\napi_key = k\n"), 0o644)
	got := runSabHook(t, sabHook(t, "host-whitelist-init"), ini, dir, "FOG_FQDN=sab.example.com")
	if !strings.Contains(got, "host_whitelist = mine, sab.example.com, sabnzbd\n") || !strings.Contains(got, "api_key = k") {
		t.Fatalf("merge:\n%s", got)
	}
	os.Remove(ini)
	if got := runSabHook(t, sabHook(t, "host-whitelist-init"), ini, dir, "FOG_FQDN="); got != "[misc]\nhost_whitelist = sabnzbd\n" {
		t.Fatalf("no domain yet:\n%q", got)
	}
}

// Download folders are moved into the shared /data unless the operator
// already put them there.
func TestSabnzbdFoldersHook(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	ini := filepath.Join(dir, "sabnzbd.ini")

	got := runSabHook(t, sabHook(t, "folders-init"), ini, data)
	if got != "[misc]\ndownload_dir = "+data+"/incomplete\ncomplete_dir = "+data+"/complete\n" {
		t.Fatalf("first start:\n%q", got)
	}
	if fi, err := os.Stat(filepath.Join(data, "complete")); err != nil || !fi.IsDir() {
		t.Fatalf("complete dir not made: %v", err)
	}

	os.WriteFile(ini, []byte("[misc]\napi_key = k\ndownload_dir = Downloads/incomplete\ncomplete_dir = /downloads\n[categories]\n"), 0o644)
	got = runSabHook(t, sabHook(t, "folders-init"), ini, data)
	want := "[misc]\napi_key = k\ndownload_dir = " + data + "/incomplete\ncomplete_dir = " + data + "/complete\n[categories]\n"
	if got != want {
		t.Fatalf("outside /data:\n%q\nwant\n%q", got, want)
	}

	// The operator's own layout inside /data is kept (fog.lan's).
	keep := "[misc]\ndownload_dir = " + data + "/Incomplete\ncomplete_dir = " + data + "/Downloads/complete\n"
	os.WriteFile(ini, []byte(keep), 0o644)
	if got := runSabHook(t, sabHook(t, "folders-init"), ini, data); got != keep {
		t.Fatalf("inside /data:\n%q", got)
	}
}

// Apps that declare uses: [immich] (Once) read Immich's address and key
// from /app/v1/connections; the key is one Immich mints, so it is pasted
// into the vault after setup and must not be a required secret.
func TestImmichOffersAConnection(t *testing.T) {
	immich := ownByID(t, "immich")
	c := immich.Spec.Connection
	if c == nil || c.Port != 2283 || c.Secret != "IMMICH_API_KEY" {
		t.Fatalf("immich connection = %+v, want port 2283 with secret IMMICH_API_KEY", c)
	}
	for _, s := range immich.Spec.Secrets {
		if s == "IMMICH_API_KEY" {
			t.Fatal("IMMICH_API_KEY must not be a required secret: Immich mints it after setup")
		}
	}
}

// Fit's boundary is the sealed network and the LAN-only route, and its
// collector is how vendor data reaches it. An entry that lost any of these
// would install and run while quietly breaking what Fit promises.
func TestFitKeepsItsBoundary(t *testing.T) {
	fit := ownByID(t, "fit")
	if !fit.Spec.Sealed() {
		t.Error("fit is not sealed")
	}
	for _, r := range fit.Spec.Routes {
		if !r.LanOnly {
			t.Errorf("fit route on port %d is not lanOnly", r.Port)
		}
	}
	if fit.Spec.Connection != nil {
		t.Error("fit offers a connection: no other app may hold a Fit credential")
	}
	collector := ownByID(t, "fit-collector")
	if !collector.Spec.SealedPeer {
		t.Error("fit-collector is not a sealed peer: fit could not reach it")
	}
	if collector.Spec.Needs != nil {
		t.Error("fit-collector has a database")
	}
	if !ownByID(t, "glass").Spec.SealedPeer {
		t.Error("glass is not a sealed peer: fit could not report to it")
	}
}

// think-fleet holds a household's model records and files and answers its
// local AI: it stays on the LAN, its console stays behind fog's gate, and
// its model files stay out of backups, where tens of GiB each would crowd
// out everything else.
func TestThinkFleetStaysInTheHouse(t *testing.T) {
	fleet := ownByID(t, "think-fleet")
	gated := false
	for _, r := range fleet.Spec.Routes {
		if !r.LanOnly {
			t.Errorf("think-fleet route on port %d is not lanOnly", r.Port)
		}
		gated = gated || (r.Auth && r.Host == "")
	}
	if !gated {
		t.Error("think-fleet's console is not behind fog's gate")
	}
	for _, v := range fleet.Spec.Volumes {
		if v.Managed() {
			t.Errorf("think-fleet volume %s is managed, so its model files would be backed up", v.Name)
		}
	}
	if fleet.Spec.Needs == nil || fleet.Spec.Needs.Postgres == nil {
		t.Error("think-fleet's ledger needs Postgres")
	}
}
