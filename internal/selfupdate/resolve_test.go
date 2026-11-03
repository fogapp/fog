package selfupdate

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// resolveManifest accepts whatever the operator pasted into the settings box,
// which is the whole point of it — and also why its branching deserves tests:
// each accepted shape is a different guess at what a URL means, and a wrong
// guess surfaces only as "no release manifest found".

func manifestJSON(version string) string {
	m := Manifest{
		Version:   version,
		IssuedAt:  time.Now().Unix(),
		Artifacts: map[string]ManifestArtifact{"linux-amd64": {URL: "http://example/bin", SHA256: "abc"}},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestResolveManifestDirectURL(t *testing.T) {
	s := newSigner(t)
	mux := http.NewServeMux()
	s.serve(mux, "/release/latest.json", manifestJSON("v1.2.3"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m, err := s.updater().resolveManifest(context.Background(), srv.URL+"/release/latest.json")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", m.Version)
	}
}

// A plain directory URL gets latest.json appended.
func TestResolveManifestDirectoryURL(t *testing.T) {
	s := newSigner(t)
	mux := http.NewServeMux()
	s.serve(mux, "/dl/latest.json", manifestJSON("v2.0.0"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m, err := s.updater().resolveManifest(context.Background(), srv.URL+"/dl")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v2.0.0" {
		t.Errorf("version = %q, want v2.0.0", m.Version)
	}
}

// A Gitea repo reads its committed manifest at the raw-file path, and the
// release API supplies the notes shown in the UI — only for the release the
// manifest names.
func TestResolveManifestGiteaRepoWithNotes(t *testing.T) {
	s := newSigner(t)
	mux := http.NewServeMux()
	s.serve(mux, "/matthew/fog/raw/branch/main/release/latest.json", manifestJSON("v3.1.0"))
	mux.HandleFunc("/api/v1/repos/matthew/fog/releases/tags/v3.1.0", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v3.1.0", "body": "the release notes"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m, err := s.updater().resolveManifest(context.Background(), srv.URL+"//matthew/fog/")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v3.1.0" {
		t.Errorf("version = %q, want v3.1.0", m.Version)
	}
	if m.Notes != "the release notes" {
		t.Errorf("notes = %q, want the release body", m.Notes)
	}
	if m.SourceOrigin != srv.URL {
		t.Errorf("source origin = %q, want %q", m.SourceOrigin, srv.URL)
	}
}

// FOG_UPDATE_BRANCH (install.sh's FOG_BRANCH, carried onto the unit) picks
// the branch a repo's committed manifest is read from.
func TestResolveManifestFollowsBranch(t *testing.T) {
	s := newSigner(t)
	mux := http.NewServeMux()
	s.serve(mux, "/matthew/fog/raw/branch/stable/release/latest.json", manifestJSON("v0.9.0"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv(EnvBranch, "stable")
	m, err := s.updater().resolveManifest(context.Background(), srv.URL+"/matthew/fog")
	if err != nil {
		t.Fatal(err)
	}
	if want := srv.URL + "/matthew/fog/raw/branch/stable/release/latest.json"; m.FetchedFrom != want {
		t.Errorf("fetched from %q, want %q", m.FetchedFrom, want)
	}
}

func TestResolveManifestErrors(t *testing.T) {
	u := &Updater{}
	for _, tc := range []struct{ name, base, wantSubstr string }{
		{"empty", "", "no update URL configured"},
		{"not a URL", "gibberish", "not a valid URL"},
		{"another scheme", "ftp://example.com/fog", "not a valid URL"},
		{"shell syntax", "http://example.com/fog;reboot", "not a valid URL"},
		{"a space", "http://example.com/my fog", "not a valid URL"},
		{"a query", "http://example.com/fog?x=1", "not a valid URL"},
		{"credentials", "http://user:pw@example.com/fog", "not a valid URL"},
	} {
		_, err := u.resolveManifest(context.Background(), tc.base)
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubstr) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.wantSubstr)
		}
	}
}

// Nothing anywhere: the error has to gather what was tried, or the operator
// has no idea which URL shape was wrong.
func TestResolveManifestReportsAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()

	_, err := newSigner(t).updater().resolveManifest(context.Background(), srv.URL+"/matthew/fog")
	if err == nil {
		t.Fatal("expected an error when nothing resolves")
	}
	if !strings.Contains(err.Error(), "no release manifest found") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "raw/branch/main/release/latest.json") || !strings.Contains(err.Error(), "/matthew/fog/latest.json") {
		t.Errorf("error does not say what was tried: %v", err)
	}
}

// A 200 that isn't a manifest must not be mistaken for one — a proxy or a
// login page returning HTML is the usual cause.
func TestFetchManifestRejectsNonManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"something":"else"}`))
	}))
	defer srv.Close()

	_, err := newSigner(t).updater().fetchManifestURL(context.Background(), srv.URL+"/latest.json")
	if err == nil {
		t.Fatal("a response with no version was accepted as a manifest")
	}
	if !strings.Contains(err.Error(), "not a release manifest") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The update-hijack cases: a mirror (or anyone on a plain-http path) that
// serves its own manifest — unsigned, signed with another key, or a signed
// one with its bytes changed — is refused, and the resolver doesn't fall
// through to treating it as valid.
func TestFetchManifestRequiresTheReleaseSignature(t *testing.T) {
	release := newSigner(t)
	attacker := newSigner(t)
	body := manifestJSON("v9.0.0")
	for _, tc := range []struct {
		name, sig, want string
	}{
		{"unsigned", "", "unsigned"},
		{"another key", attacker.sign(body), "doesn't match"},
		{"tampered bytes", release.sign(manifestJSON("v8.0.0")), "doesn't match"},
		{"garbage", "not base64!", "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
			if tc.sig != "" {
				mux.HandleFunc("/latest.json.sig", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.sig)) })
			}
			srv := httptest.NewServer(mux)
			defer srv.Close()
			_, err := release.updater().resolveManifest(context.Background(), srv.URL+"/latest.json")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal mentioning %q", err, tc.want)
			}
		})
	}
}

// FOG_UPDATE_ALLOW_UNSIGNED is the hand-set escape hatch for a host that
// releases its own unsigned builds.
func TestFetchManifestAllowUnsigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.json" {
			w.Write([]byte(manifestJSON("v1.0.0")))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv(EnvAllowUnsigned, "1")
	if _, err := newSigner(t).updater().fetchManifestURL(context.Background(), srv.URL+"/latest.json"); err != nil {
		t.Fatalf("unsigned manifest refused with the escape hatch set: %v", err)
	}
}

// The pinned keys must parse: a typo here would refuse every release.
func TestPinnedReleaseKeysParse(t *testing.T) {
	keys, err := pinnedKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("no release keys pinned")
	}
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			t.Errorf("key %s has %d bytes", base64.StdEncoding.EncodeToString(k), len(k))
		}
	}
}

// The access token goes to the source's own host, never to wherever a
// manifest points its downloads.
func TestTokenStaysOnTheSourceHost(t *testing.T) {
	for _, tc := range []struct {
		source, host string
		want         bool
	}{
		{"http://gitea.lan/matthew/fog", "gitea.lan", true},
		{"http://gitea.lan/matthew/fog", "GITEA.lan", true},
		{"http://gitea.lan/matthew/fog", "cdn.example.com", false},
		{"https://github.com/you/fog", "raw.githubusercontent.com", true},
		{"https://github.com/you/fog", "api.github.com", true},
		{"https://gitlab.com/you/fog", "raw.githubusercontent.com", false},
	} {
		if got := (&Updater{}).tokenHostFor(tc.source, tc.host); got != tc.want {
			t.Errorf("source %s, host %s: token sent = %v, want %v", tc.source, tc.host, got, tc.want)
		}
	}
}

// Relative artifact names resolve against the manifest's own URL.
func TestFetchManifestResolvesRelativeArtifacts(t *testing.T) {
	s := newSigner(t)
	mux := http.NewServeMux()
	s.serve(mux, "/pub/fog/latest.json", fmt.Sprintf(`{"version":"v1.0.0","issued_at":%d,"artifacts":{"linux-amd64":{"url":"fogd-linux-amd64","sha256":"x"}}}`, time.Now().Unix()))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m, err := s.updater().fetchManifestURL(context.Background(), srv.URL+"/pub/fog/latest.json")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := url.Parse(m.Artifacts["linux-amd64"].URL)
	if got.Path != "/pub/fog/fogd-linux-amd64" {
		t.Errorf("artifact = %s, want it beside the manifest", m.Artifacts["linux-amd64"].URL)
	}
}

func TestVersionOrder(t *testing.T) {
	for _, tc := range []struct {
		cand, cur string
		want      bool
	}{
		{"v1.2.4", "v1.2.3", true},
		{"v1.10.0", "v1.9.9", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.2", "v1.2.3", false},
		{"v0.14.23", "v0.15.0", false},
		{"v1.2.3", "v1.2.3-rc1", true},
		{"v1.2.3-rc2", "v1.2.3-rc1", true},
		{"v1.2.3-rc1", "v1.2.3", false},
		{"v2.0.0", "dev", false}, // no place in the order: see TestUnversionedFogdNeedsAnExplicitOptIn
		{"garbage", "v1.0.0", false},
		{"v1.2", "v1.0.0", false},
	} {
		if got := isNewer(tc.cand, tc.cur); got != tc.want {
			t.Errorf("isNewer(%s, %s) = %v, want %v", tc.cand, tc.cur, got, tc.want)
		}
	}
}

// Semver 2.0.0 precedence, section 11's own example chain, plus the cases
// plain text order gets wrong (rc.9 < rc.10).
func TestPreReleasePrecedence(t *testing.T) {
	chain := []string{
		"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta",
		"v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0-rc.9", "v1.0.0-rc.10", "v1.0.0",
		"v1.0.1-rc.1", "v1.0.1",
	}
	for i := range chain {
		for j := range chain {
			a, ok1 := parseVersion(chain[i])
			b, ok2 := parseVersion(chain[j])
			if !ok1 || !ok2 {
				t.Fatalf("parse %s / %s", chain[i], chain[j])
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := compareVersions(a, b); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", chain[i], chain[j], got, want)
			}
		}
	}
	if !isNewer("v2.0.0-rc.10", "v2.0.0-rc.9") || isNewer("v2.0.0-rc.9", "v2.0.0-rc.10") {
		t.Error("rc.10 must be newer than rc.9")
	}
	a, _ := parseVersion("v1.0.0+build.7")
	b, _ := parseVersion("v1.0.0")
	if compareVersions(a, b) != 0 {
		t.Error("build metadata changed precedence")
	}
	for _, bad := range []string{"v1.0.0-", "v1.0.0-rc..1", "v1.0.0-rc.$", "v1.0.0-rc_1"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("%q parsed as a version", bad)
		}
	}
}

// A fogd whose own version can't be parsed can't tell an update from a
// downgrade: it takes nothing, unless the operator sets the opt-in by hand,
// and then counts as the lowest version.
func TestUnversionedFogdNeedsAnExplicitOptIn(t *testing.T) {
	if isNewer("v1.0.0", "dev") || isNewer("v0.0.1", "garbage") {
		t.Error("an unversioned fogd took a release without the opt-in")
	}
	u, exe := applyFixture(t, "old binary", "new binary", nil)
	u.version = "dev"
	err := u.Apply(context.Background(), func(int) { t.Error("exit requested") })
	if err == nil || !strings.Contains(err.Error(), EnvFromUnversioned) {
		t.Fatalf("err = %v, want a refusal naming %s", err, EnvFromUnversioned)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Errorf("fogd = %q", b)
	}
	t.Setenv(EnvFromUnversioned, "1")
	if !isNewer("v0.0.1", "dev") {
		t.Error("with the opt-in, an unversioned fogd isn't the lowest version")
	}
	if err := u.Apply(context.Background(), func(int) {}); err != nil {
		t.Fatalf("Apply with the opt-in: %v", err)
	}
}

func TestVersionsWithLeadingZerosAreRefused(t *testing.T) {
	for _, bad := range []string{"v01.0.0", "v1.00.0", "v1.0.00", "v1.0.0-rc.01", "v1.0.0-.", "v1.0.0-rc..1"} {
		if ValidVersion(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"v0.0.0", "v10.20.30", "v1.0.0-rc.0", "v1.0.0-0a", "v1.0.0-x-y.z"} {
		if !ValidVersion(good) {
			t.Errorf("%q refused", good)
		}
	}
}
