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

// Every catalog entry must produce a spec that validates and a name that's
// legal — a broken store entry is a broken one-click install.
func TestEntriesValid(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Resolve(Entries(), exampleSources) {
		if seen[e.ID] {
			t.Errorf("duplicate catalog id %q", e.ID)
		}
		seen[e.ID] = true
		if err := apps.ValidateName(e.ID); err != nil {
			t.Errorf("%s: bad id: %v", e.ID, err)
		}
		if e.Title == "" || e.Description == "" || e.Category == "" || e.Icon == "" {
			t.Errorf("%s: missing display fields", e.ID)
		}
		if err := e.Spec.Validate(); err != nil {
			t.Errorf("%s: invalid spec: %v", e.ID, err)
		}
		if len(e.Spec.Routes) == 0 {
			t.Errorf("%s: no route", e.ID)
		}
		// A folder a template names is a content library: Content itself
		// (one shared root for a whole stack) or one capitalised level
		// inside it. An app's own data names no folder at all — it is
		// given Apps/<App>/<volume> when the app is created — so a
		// template that hardcodes one under Apps is a mistake.
		for _, v := range e.Spec.AllVolumes() {
			if v.Files == "" || v.Files == apps.FilesContentDir {
				continue
			}
			parts := strings.Split(v.Files, "/")
			if len(parts) != 2 || parts[0] != apps.FilesContentDir || parts[1] == "" || strings.ToUpper(parts[1][:1]) != parts[1][:1] {
				t.Errorf("%s: volume %s lives at %q, want %s or %s/<Library>", e.ID, v.Name, v.Files, apps.FilesContentDir, apps.FilesContentDir)
			}
		}
		for _, o := range e.Options {
			switch {
			case len(o.Target) > 4 && o.Target[:4] == "env:":
			case len(o.Target) > 7 && o.Target[:7] == "volume:":
				// A volume option names a volume the spec actually has.
				found := false
				for _, v := range e.Spec.AllVolumes() {
					if v.Name == o.Target[7:] {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: volume option %s names no volume %q", e.ID, o.Key, o.Target[7:])
				}
			case len(o.Target) > 7 && o.Target[:7] == "secret:":
				// Secret options must correspond to a declared spec secret.
				found := false
				for _, k := range e.Spec.Secrets {
					if k == o.Target[7:] {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: secret option %s not in spec.secrets", e.ID, o.Key)
				}
			case o.Target == "repo":
				if e.Spec.Source.Type != "git" {
					t.Errorf("%s: repo option on non-git entry", e.ID)
				}
			default:
				t.Errorf("%s: option %s has bad target %q", e.ID, o.Key, o.Target)
			}
		}
	}
	for _, id := range DefaultEnabled {
		if !seen[id] {
			t.Errorf("default-enabled id %q not in catalog", id)
		}
	}
}

// Companions install with one click alongside their parent, so they must
// resolve to real entries whose every option is fillable without input
// (a default, a generator, or the browser-prefilled timezone).
func TestCompanionsInstallable(t *testing.T) {
	// As a fog with a source for its own apps sees them: a first-party
	// companion (fit-collector) shares its parent's repository, so it has
	// a source whenever the parent does.
	entries := Resolve(Entries(), exampleSources)
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, e := range entries {
		for _, c := range e.Companions {
			target, ok := byID[c.ID]
			if !ok {
				t.Errorf("%s: companion %q not in catalog", e.ID, c.ID)
				continue
			}
			if c.ID == e.ID {
				t.Errorf("%s: entry lists itself as a companion", e.ID)
			}
			if c.Blurb == "" {
				t.Errorf("%s: companion %q has no blurb", e.ID, c.ID)
			}
			for _, o := range target.Options {
				if strings.HasPrefix(o.Target, "volume:") {
					// The spec's placement is the default; a companion's
					// folder must have one so nothing is asked.
					placed := false
					for _, v := range target.Spec.AllVolumes() {
						if v.Name == o.Target[7:] && !v.Managed() {
							placed = true
						}
					}
					if !placed {
						t.Errorf("%s: companion %q volume option %q has no placement in its spec", e.ID, c.ID, o.Key)
					}
					continue
				}
				if o.Default == "" && o.Generate == "" && o.Key != "tz" {
					t.Errorf("%s: companion %q option %q needs a default or generator for one-click install", e.ID, c.ID, o.Key)
				}
			}
		}
	}
}

// A requirement tag nobody can parse would silently never block — every
// declared one must name a level.
func TestRequiresKnown(t *testing.T) {
	for _, e := range Entries() {
		if e.Requires != nil && e.Requires.CPU != "" && cpuLevel(e.Requires.CPU) == 0 {
			t.Errorf("%s: unknown CPU requirement %q", e.ID, e.Requires.CPU)
		}
		if e.Blocker != "" {
			t.Errorf("%s: catalog carries a blocker; that's per host, filled by the API", e.ID)
		}
	}
}

func TestPreflight(t *testing.T) {
	var bookorbit Entry
	for _, e := range Entries() {
		if e.ID == "bookorbit" {
			bookorbit = e
		}
	}
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
	for _, e := range Entries() {
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
		if PreflightSpec(spec, kvm64) == "" {
			t.Errorf("%s: should be blocked on kvm64", img)
		}
	}
	for _, img := range []string{"ghcr.io/bookorbit/other:latest", "lscr.io/linuxserver/sonarr:latest", ""} {
		spec := apps.AppSpec{Source: apps.Source{Type: "image", Image: img}}
		if got := PreflightSpec(spec, kvm64); got != "" {
			t.Errorf("%q: unexpectedly blocked: %s", img, got)
		}
	}
	git := apps.AppSpec{Source: apps.Source{Type: "git", Repo: "https://example.com/x.git"}}
	if PreflightSpec(git, kvm64) != "" {
		t.Error("git source blocked")
	}
}

// SABnzbd refuses any request whose Host header it doesn't recognise, and
// behind Fog's proxy that header is the app's own hostname: without the init
// hook that whitelists it, a fresh install answers "Access denied - Hostname
// verification failed" at the address Fog just handed the operator.
func TestSabnzbdWhitelistsItsOwnHostname(t *testing.T) {
	var sab apps.AppSpec
	for _, e := range Entries() {
		if e.ID == "sabnzbd" {
			sab = e.Spec
		}
	}
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
	ids := map[string]bool{}
	for _, e := range Entries() {
		ids[e.ID] = true
		if e.Spec.Source.Type != "image" {
			continue
		}
		if !strings.HasPrefix(e.ProjectURL, "https://") {
			t.Errorf("%s: image entry has no https projectUrl (got %q)", e.ID, e.ProjectURL)
		}
	}
	for id := range projectURLs {
		if !ids[id] {
			t.Errorf("projectURLs names %q, which is not a catalog entry", id)
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
		"/config/sabnzbd.ini", ini, "lsiown", "true").Replace(sabAPIKeyInit)
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
	if got := runSabHook(t, sabHostWhitelistInit, ini, dir, "FOG_FQDN=Sab.Example.com"); got != "[misc]\nhost_whitelist = sab.example.com, sabnzbd\n" {
		t.Fatalf("first start:\n%q", got)
	}
	os.WriteFile(ini, []byte("[misc]\nhost_whitelist = mine, sab.example.com\napi_key = k\n"), 0o644)
	got := runSabHook(t, sabHostWhitelistInit, ini, dir, "FOG_FQDN=sab.example.com")
	if !strings.Contains(got, "host_whitelist = mine, sab.example.com, sabnzbd\n") || !strings.Contains(got, "api_key = k") {
		t.Fatalf("merge:\n%s", got)
	}
	os.Remove(ini)
	if got := runSabHook(t, sabHostWhitelistInit, ini, dir, "FOG_FQDN="); got != "[misc]\nhost_whitelist = sabnzbd\n" {
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

	got := runSabHook(t, sabFoldersInit, ini, data)
	if got != "[misc]\ndownload_dir = "+data+"/incomplete\ncomplete_dir = "+data+"/complete\n" {
		t.Fatalf("first start:\n%q", got)
	}
	if fi, err := os.Stat(filepath.Join(data, "complete")); err != nil || !fi.IsDir() {
		t.Fatalf("complete dir not made: %v", err)
	}

	os.WriteFile(ini, []byte("[misc]\napi_key = k\ndownload_dir = Downloads/incomplete\ncomplete_dir = /downloads\n[categories]\n"), 0o644)
	got = runSabHook(t, sabFoldersInit, ini, data)
	want := "[misc]\napi_key = k\ndownload_dir = " + data + "/incomplete\ncomplete_dir = " + data + "/complete\n[categories]\n"
	if got != want {
		t.Fatalf("outside /data:\n%q\nwant\n%q", got, want)
	}

	// The operator's own layout inside /data is kept (fog.lan's).
	keep := "[misc]\ndownload_dir = " + data + "/Incomplete\ncomplete_dir = " + data + "/Downloads/complete\n"
	os.WriteFile(ini, []byte(keep), 0o644)
	if got := runSabHook(t, sabFoldersInit, ini, data); got != keep {
		t.Fatalf("inside /data:\n%q", got)
	}
}

// Apps that declare uses: [immich] (Once) read Immich's address and key
// from /app/v1/connections; the key is one Immich mints, so it is pasted
// into the vault after setup and must not be a required secret.
func TestImmichOffersAConnection(t *testing.T) {
	immich := byID("immich")
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
	fit := byID("fit")
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
	collector := byID("fit-collector")
	if !collector.Spec.SealedPeer {
		t.Error("fit-collector is not a sealed peer: fit could not reach it")
	}
	if collector.Spec.Needs != nil {
		t.Error("fit-collector has a database")
	}
}

// think-fleet holds a household's model records and files and answers its
// local AI: it stays on the LAN, its console stays behind fog's gate, and
// its model files stay out of backups, where tens of GiB each would crowd
// out everything else.
func TestThinkFleetStaysInTheHouse(t *testing.T) {
	fleet := byID("think-fleet")
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
