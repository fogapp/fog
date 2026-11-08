package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// install.sh resolves FOG_SOURCE the same way resolveManifest does — it has
// to, because one URL is meant to configure both the install and every
// update after it (install.sh writes it onto the unit as
// FOG_UPDATE_BASE_URL). The shell can't import the Go resolver, so these
// tests run both over every source shape and require the same answer: first
// the candidate tables, offline; then both against one live server.

func skipShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell installer")
	}
}

var sourceShapes = []struct {
	name, source, branch string
}{
	{"gitea repo", "http://gitea.lan/matthew/fog", ""},
	{"gitea repo, trailing slash", "http://gitea.lan/matthew/fog/", ""},
	{"gitea repo, doubled slash and port", "http://10.0.0.5:3000//matthew/fog", ""},
	{"gitea repo, another branch", "http://gitea.lan/matthew/fog", "release/1.x"},
	{"github repo", "https://github.com/you/fog", ""},
	{"github repo, mixed-case host", "https://GitHub.com/you/fog", ""},
	{"github repo, www", "https://www.github.com/you/fog", ""},
	{"github repo, another branch", "https://github.com/you/fog", "stable"},
	{"gitlab.com repo", "https://gitlab.com/you/fog", ""},
	{"self-hosted gitlab, subgroup", "https://gitlab.example.com/group/sub/fog", ""},
	{"plain directory", "https://files.example.com/fog", ""},
	{"directory at the root", "https://files.example.com", ""},
	{"directory with a repo-shaped path", "https://files.example.com/pub/fog", ""},
	{"manifest directly", "https://files.example.com/fog/latest.json", ""},
	{"percent-escaped path", "https://files.example.com/my%20fog", ""},
	{"IPv6 literal", "http://[fd00::1]:8080/fog", ""},
}

// Input neither resolver may accept: each would land in a systemd unit or a
// curl argument.
var badSources = []string{
	"gitea.lan/matthew/fog",
	"ftp://example.com/fog",
	"http://",
	"http://example.com/my fog",
	"http://example.com/fog;reboot",
	"http://example.com/$(id)",
	"http://example.com/`id`",
	"http://example.com/fog?x=1",
	"http://example.com/fog#frag",
	"http://user:pw@example.com/fog",
	"http://example.com/\"fog\"",
	"http://example.com/fog\\x",
}

func installCandidates(t *testing.T, source, branch string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "../../install.sh")
	cmd.Env = append(os.Environ(), "FOG_PRINT_CANDIDATES=1", "FOG_SOURCE="+source, "FOG_REPO_URL=")
	if branch != "" {
		cmd.Env = append(cmd.Env, "FOG_BRANCH="+branch)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// The two resolvers' tables, line for line, over every shape.
func TestInstallShAndResolverAgreeOnEveryShape(t *testing.T) {
	t.Parallel()

	skipShell(t)
	for _, tc := range sourceShapes {
		t.Run(tc.name, func(t *testing.T) {
			goCands, err := Candidates(tc.source, tc.branch)
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			var want []string
			for _, c := range goCands {
				want = append(want, c.Manifest+"|"+c.Raw)
			}
			got, err := installCandidates(t, tc.source, tc.branch)
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, got)
			}
			if got != strings.Join(want, "\n") {
				t.Errorf("install.sh:\n%s\nresolve.go:\n%s", got, strings.Join(want, "\n"))
			}
		})
	}
	for _, bad := range badSources {
		if _, err := Candidates(bad, ""); err == nil {
			t.Errorf("resolve.go accepted %q", bad)
		}
		if out, err := installCandidates(t, bad, ""); err == nil {
			t.Errorf("install.sh accepted %q:\n%s", bad, out)
		}
	}
	for _, bad := range []string{"../x", "-x", "a b", "a;b", "$(id)"} {
		if _, err := Candidates("http://gitea.lan/m/fog", bad); err == nil {
			t.Errorf("resolve.go accepted branch %q", bad)
		}
		if out, err := installCandidates(t, "http://gitea.lan/m/fog", bad); err == nil {
			t.Errorf("install.sh accepted branch %q:\n%s", bad, out)
		}
	}
}

// The pinned release keys are written down three times: resolve.go's
// ReleaseKeys (raw), and install.sh and flash.sh (as SPKI, which openssl
// reads). Every copy must name the same keys.
func TestShellScriptsPinTheReleaseKeys(t *testing.T) {
	t.Parallel()

	var want []string
	keys, err := pinnedKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		der, err := x509.MarshalPKIXPublicKey(k)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, base64.StdEncoding.EncodeToString(der))
	}
	re := regexp.MustCompile(`RELEASE_KEYS="\$\{FOG_RELEASE_KEYS:-([^}]*)\}"`)
	for _, f := range []string{"../../install.sh", "../../imaging/flash.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindSubmatch(b)
		if m == nil {
			t.Errorf("%s pins no RELEASE_KEYS", f)
			continue
		}
		if got := strings.Fields(string(m[1])); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s pins %v, resolve.go pins %v", f, got, want)
		}
	}
}

// --- live: one server answering for several hosts ------------------------

const shellManifest = `{"version":"%s","issued_at":%d,"artifacts":{
  "linux-amd64":{"url":"%s","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "linux-arm64":{"url":"%s","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}`

// hostServer is an HTTP proxy that serves files by host and path, so curl
// (through http_proxy) and the Go updater (through its transport's Proxy)
// can both reach "gitea.test", "gitlab.test" and "files.test" without DNS.
type hostServer struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string // "host/path" → body
}

func (h *hostServer) set(hostPath, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[hostPath] = body
}

func (h *hostServer) get(hostPath string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.files[hostPath]
	return b, ok
}

func newHostServer(t *testing.T) *hostServer {
	t.Helper()
	h := &hostServer{files: map[string]string{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Host
		if host == "" {
			host = r.Host
		}
		body, ok := h.get(host + r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(h.Close)
	return h
}

// manifest puts a signed manifest at host/path.
func (h *hostServer) manifest(s *signer, hostPath, version, art string) {
	body := fmt.Sprintf(shellManifest, version, time.Now().Unix(), art+"-amd64", art+"-arm64")
	h.set(hostPath, body)
	h.set(hostPath+".sig", s.sign(body))
}

func (h *hostServer) updater(s *signer) *Updater {
	proxy, _ := url.Parse(h.URL)
	u := s.updater()
	u.transport = &http.Transport{Proxy: http.ProxyURL(proxy)}
	return u
}

func runInstallSh(t *testing.T, env ...string) (map[string]string, string, error) {
	t.Helper()
	skipShell(t)
	cmd := exec.Command("sh", "../../install.sh")
	cmd.Env = append(os.Environ(), "FOG_PRINT_SOURCE=1", "FOG_REPO_URL=", "no_proxy=", "NO_PROXY=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return got, string(out), err
}

// needOpenSSL skips when this machine has no openssl that can verify
// ed25519 (install.sh needs one; macOS's LibreSSL can't).
func needOpenSSL(t *testing.T, s *signer) {
	t.Helper()
	skipShell(t)
	_, out, err := runInstallSh(t, "FOG_SOURCE=http://127.0.0.1:1/none", "FOG_RELEASE_KEYS="+s.spki(t))
	if err != nil && strings.Contains(out, "needs openssl") {
		t.Skip("no ed25519-capable openssl here: " + out)
	}
}

func shellArch() string { return "linux-" + runtime.GOARCH }

func TestInstallShAndUpdaterResolveTheSameRelease(t *testing.T) {
	s := newSigner(t)
	needOpenSSL(t, s)
	h := newHostServer(t)
	// A Gitea repo with its committed manifest.
	h.manifest(s, "gitea.test/matthew/fog/raw/branch/main/release/latest.json", "v1.2.3", "http://cdn.test/fogd")
	// The same repo on another branch.
	h.manifest(s, "gitea.test/matthew/fog/raw/branch/beta/release/latest.json", "v1.3.0-rc1", "http://cdn.test/beta")
	// A GitLab repo.
	h.manifest(s, "gitlab.test/you/fog/-/raw/main/release/latest.json", "v1.2.4", "http://cdn.test/gl")
	// A plain directory with a repo-shaped path, naming artifacts relatively.
	h.manifest(s, "files.test/pub/fog/latest.json", "v1.2.5", "fogd-linux")
	// A repo whose committed manifest is signed by someone else: both must
	// pass over it to the directory candidate, not stop at it.
	evil := newSigner(t)
	h.manifest(evil, "gitea.test/evil/fog/raw/branch/main/release/latest.json", "v9.9.9", "http://evil.test/fogd")
	h.manifest(s, "gitea.test/evil/fog/latest.json", "v1.2.6", "http://cdn.test/good")

	for _, tc := range []struct {
		name, source, branch, wantManifest, wantVersion string
	}{
		{"gitea", "http://gitea.test/matthew/fog", "", "http://gitea.test/matthew/fog/raw/branch/main/release/latest.json", "v1.2.3"},
		{"gitea, branch", "http://gitea.test/matthew/fog", "beta", "http://gitea.test/matthew/fog/raw/branch/beta/release/latest.json", "v1.3.0-rc1"},
		{"gitlab", "http://gitlab.test/you/fog", "", "http://gitlab.test/you/fog/-/raw/main/release/latest.json", "v1.2.4"},
		{"directory", "http://files.test/pub/fog", "", "http://files.test/pub/fog/latest.json", "v1.2.5"},
		{"manifest directly", "http://files.test/pub/fog/latest.json", "", "http://files.test/pub/fog/latest.json", "v1.2.5"},
		{"forged first candidate", "http://gitea.test/evil/fog", "", "http://gitea.test/evil/fog/latest.json", "v1.2.6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"FOG_SOURCE=" + tc.source, "FOG_RELEASE_KEYS=" + s.spki(t), "http_proxy=" + h.URL}
			if tc.branch != "" {
				env = append(env, "FOG_BRANCH="+tc.branch)
				t.Setenv(EnvBranch, tc.branch)
			}
			got, out, err := runInstallSh(t, env...)
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			m, err := h.updater(s).resolveManifest(context.Background(), tc.source)
			if err != nil {
				t.Fatalf("resolveManifest: %v", err)
			}
			if got["manifest"] != tc.wantManifest || m.FetchedFrom != tc.wantManifest {
				t.Errorf("manifest: install.sh %q, updater %q, want %q", got["manifest"], m.FetchedFrom, tc.wantManifest)
			}
			if got["version"] != tc.wantVersion || m.Version != tc.wantVersion {
				t.Errorf("version: install.sh %q, updater %q, want %q", got["version"], m.Version, tc.wantVersion)
			}
			if got["artifact"] != m.Artifacts[shellArch()].URL {
				t.Errorf("artifact: install.sh %q, updater %q", got["artifact"], m.Artifacts[shellArch()].URL)
			}
		})
	}
}

// A manifest from a mirror that isn't signed by the release key is refused
// by install.sh just as by the updater, and FOG_MANIFEST_URL doesn't get
// around it.
func TestInstallShRefusesAForgedManifest(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	needOpenSSL(t, s)
	h := newHostServer(t)
	h.manifest(newSigner(t), "files.test/fog/latest.json", "v1.0.0", "fogd")
	_, out, err := runInstallSh(t, "FOG_SOURCE=http://files.test/fog", "FOG_RELEASE_KEYS="+s.spki(t), "http_proxy="+h.URL)
	if err == nil || !strings.Contains(out, "signature doesn't match") {
		t.Fatalf("install.sh accepted a forged manifest (err %v):\n%s", err, out)
	}
	_, out, err = runInstallSh(t, "FOG_MANIFEST_URL=http://files.test/fog/latest.json", "FOG_RELEASE_KEYS="+s.spki(t), "http_proxy="+h.URL)
	if err == nil || !strings.Contains(out, "signature doesn't match") {
		t.Fatalf("install.sh accepted a forged manifest by FOG_MANIFEST_URL (err %v):\n%s", err, out)
	}
}

func TestInstallShExplicitOverrides(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	needOpenSSL(t, s)
	h := newHostServer(t)
	h.manifest(s, "files.test/pub/fog/latest.json", "v1.2.3", "fogd-linux")
	// A host neither resolver could guess: the operator names both values.
	got, out, err := runInstallSh(t,
		"FOG_SOURCE=http://unguessable.invalid/fog",
		"FOG_MANIFEST_URL=http://files.test/pub/fog/latest.json",
		"FOG_RAW_BASE=http://files.test/pub/fog",
		"FOG_RELEASE_KEYS="+s.spki(t), "http_proxy="+h.URL)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if want := "http://files.test/pub/fog/latest.json"; got["manifest"] != want {
		t.Errorf("manifest = %q, want %q", got["manifest"], want)
	}
	if got["version"] != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", got["version"])
	}
}

// install.sh without FOG_SOURCE uses fog's public releases; when that can't
// be reached (no network here: the proxy refuses everything), the failure
// says which default it tried and what to set to install from elsewhere.
func TestInstallShDefaultSourceFailureSaysWhatToSet(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	needOpenSSL(t, s)
	h := newHostServer(t) // refuses everything, like a network without GitHub
	cmd := exec.Command("sh", "../../install.sh")
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "FOG_") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "FOG_PRINT_SOURCE=1", "FOG_RELEASE_KEYS="+s.spki(t), "http_proxy="+h.URL,
		"https_proxy="+h.URL, "HTTPS_PROXY="+h.URL, "no_proxy=", "NO_PROXY=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install.sh resolved the default source through a server that has nothing:\n%s", out)
	}
	for _, want := range []string{"No source was given", "https://github.com/fogapp/fog", "FOG_SOURCE=<that place>"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("failure doesn't say %q:\n%s", want, out)
		}
	}
}

// --- a whole install, against a scratch root ------------------------------

// installFixture serves a signed release of three files (the binary for this
// machine's arch, the unit, the guard) and builds a scratch root plus a PATH
// of stubs: id says root, systemctl records what it is asked and fails a
// restart when STUB_FAIL_RESTART names the binary's content.
type installFixture struct {
	issued int64 // the manifest's issued_at; now unless a test backdates it
	h      *hostServer
	s      *signer
	root   string
	stubs  string
	log    string
}

func newInstallFixture(t *testing.T, version, binary string) *installFixture {
	t.Helper()
	f := &installFixture{issued: time.Now().Unix(), h: newHostServer(t), s: newSigner(t), root: t.TempDir(), stubs: t.TempDir()}
	needOpenSSL(t, f.s)
	f.log = filepath.Join(f.stubs, "systemctl.log")
	f.release(version, binary, nil)
	writeStub(t, f.stubs, "id", "#!/bin/sh\necho 0\n")
	writeStub(t, f.stubs, "journalctl", "#!/bin/sh\necho 'journal: stub'\n")
	writeStub(t, f.stubs, "hostname", "#!/bin/sh\necho 192.0.2.10\n")
	// systemctl: restart "runs" the installed fogd — a test binary that
	// exits non-zero stands for one that can't start.
	writeStub(t, f.stubs, "systemctl", fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1" in
  restart|start) case "$*" in *fogd.socket*) ;; *) sh %q/usr/local/bin/fogd ;; esac ;;
  is-active) [ -n "${STUB_RUNNING:-}" ] || sh %q/usr/local/bin/fogd ;;
  show) case "$*" in
      *EnvironmentFiles*) [ -z "${STUB_ENVFILE:-}" ] || echo "EnvironmentFiles=$STUB_ENVFILE (ignore_errors=no)" ;;
      *) echo "Environment=FOG_DATA_DIR=/var/lib/fog FOG_LISTEN=${STUB_LISTEN:-:8080}" ;;
    esac ;;
esac
`, f.log, f.root, f.root))
	return f
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func sum(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

// release publishes version as a relative-URL release directory at
// files.test/fog; tamper, when set, changes a served file after its digest
// went into the manifest.
func (f *installFixture) release(version, binary string, tamper map[string]string) {
	// Stamped like a real fogd (internal/config's VersionStamp), so a later
	// install reads its version from the file.
	binary += "\n# fogd-version=" + version + "\n"
	unit, guard, socket := "[Service]\nExecStart=/usr/local/bin/fogd\n", "#!/bin/sh\nexit 0\n", "[Socket]\nListenStream=8080\n"
	body := fmt.Sprintf(`{"version":%q,"issued_at":%d,"artifacts":{%q:{"url":"fogd-bin","sha256":%q},"fogd.service":{"url":"fogd.service","sha256":%q},"fogd-rollback":{"url":"fogd-rollback","sha256":%q},"fogd.socket":{"url":"fogd.socket","sha256":%q}}}`,
		version, f.issued, shellArch(), sum(binary), sum(unit), sum(guard), sum(socket))
	f.h.set("files.test/fog/latest.json", body)
	f.h.set("files.test/fog/latest.json.sig", f.s.sign(body))
	f.h.set("files.test/fog/fogd-bin", binary)
	f.h.set("files.test/fog/fogd.service", unit)
	f.h.set("files.test/fog/fogd-rollback", guard)
	f.h.set("files.test/fog/fogd.socket", socket)
	for k, v := range tamper {
		f.h.set("files.test/fog/"+k, v)
	}
}

func (f *installFixture) run(t *testing.T, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "../../install.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+f.stubs+":"+os.Getenv("PATH"),
		"FOG_INSTALL_ROOT="+f.root, "FOG_SOURCE=http://files.test/fog", "FOG_REPO_URL=",
		"FOG_RELEASE_KEYS="+f.s.spki(t), "http_proxy="+f.h.URL, "no_proxy=", "NO_PROXY=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *installFixture) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, rel))
	if err != nil {
		return "<missing: " + err.Error() + ">"
	}
	return string(b)
}

func TestInstallShInstallsAVerifiedRelease(t *testing.T) {
	t.Parallel()

	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n# fogd v1.0.0\n")
	out, err := f.run(t, "FOG_SOURCE=http://files.test/fog/", "FOG_BRANCH=main")
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if got := f.read(t, "usr/local/bin/fogd"); !strings.Contains(got, "fogd v1.0.0") {
		t.Errorf("fogd = %q", got)
	}
	if got := f.read(t, "usr/local/bin/fogd-rollback"); got != "#!/bin/sh\nexit 0\n" {
		t.Errorf("fogd-rollback = %q", got)
	}
	dropin := f.read(t, "etc/systemd/system/fogd.service.d/10-update-source.conf")
	for _, want := range []string{`Environment="FOG_UPDATE_BASE_URL=http://files.test/fog/"`, `Environment="FOG_UPDATE_BRANCH=main"`} {
		if !strings.Contains(dropin, want) {
			t.Errorf("drop-in lacks %s:\n%s", want, dropin)
		}
	}
	if !strings.Contains(out, "installed and running (v1.0.0") {
		t.Errorf("no success line:\n%s", out)
	}
}

// A % in the source is a systemd specifier; it reaches the unit doubled, so
// systemd hands fogd the URL as given.
func TestInstallShEscapesTheSourceForSystemd(t *testing.T) {
	t.Parallel()

	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n")
	// The proxy sees the decoded path.
	for _, k := range []string{"latest.json", "latest.json.sig", "fogd-bin", "fogd.service", "fogd-rollback", "fogd.socket"} {
		b, _ := f.h.get("files.test/fog/" + k)
		f.h.set("files.test/my fog/"+k, b)
	}
	out, err := f.run(t, "FOG_SOURCE=http://files.test/my%20fog")
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if got := f.read(t, "etc/systemd/system/fogd.service.d/10-update-source.conf"); !strings.Contains(got, `"FOG_UPDATE_BASE_URL=http://files.test/my%%20fog"`) {
		t.Errorf("drop-in:\n%s", got)
	}
}

// Any file not matching the signed manifest stops the install before a
// single file on the system changes.
func TestInstallShRefusesATamperedFileAndChangesNothing(t *testing.T) {
	t.Parallel()

	skipShell(t)
	for _, file := range []string{"fogd-bin", "fogd.service", "fogd-rollback"} {
		t.Run(file, func(t *testing.T) {
			f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n")
			f.release("v1.0.0", "#!/bin/sh\nexit 0\n", map[string]string{file: "#!/bin/sh\necho owned\n"})
			out, err := f.run(t)
			if err == nil || !strings.Contains(out, "matches the manifest") {
				t.Fatalf("install.sh accepted a tampered %s (err %v):\n%s", file, err, out)
			}
			for _, p := range []string{"usr/local/bin/fogd", "usr/local/bin/fogd-rollback", "etc/systemd/system/fogd.service"} {
				if _, err := os.Stat(filepath.Join(f.root, p)); err == nil {
					t.Errorf("%s was installed from a refused release", p)
				}
			}
			if leftovers, _ := filepath.Glob(filepath.Join(f.root, "usr/local/bin/.fogd*")); len(leftovers) > 0 {
				t.Errorf("temp files left: %v", leftovers)
			}
		})
	}
}

// A new fogd that can't start is replaced by the previous one, loudly.
func TestInstallShPutsThePreviousFogdBackWhenTheNewOneWontStart(t *testing.T) {
	t.Parallel()

	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n# good v1.0.0\n")
	if out, err := f.run(t); err != nil {
		t.Fatalf("first install: %v\n%s", err, out)
	}
	f.release("v1.1.0", "#!/bin/sh\nexit 1\n# broken v1.1.0\n", nil)
	out, err := f.run(t)
	if err == nil || !strings.Contains(out, "previous fogd binary was put back") {
		t.Fatalf("install of a broken fogd didn't fail loudly (err %v):\n%s", err, out)
	}
	if got := f.read(t, "usr/local/bin/fogd"); !strings.Contains(got, "good v1.0.0") {
		t.Errorf("fogd after a failed install = %q, want the previous one", got)
	}
}

// Re-running install.sh against an older release than the installed one is
// refused unless asked for. The installed version is read from the binary
// (its stamp), never asked over HTTP: anyone on the machine can answer on a
// port, here claiming the installed fogd is ancient.
func TestInstallShRefusesADowngrade(t *testing.T) {
	t.Parallel()

	skipShell(t)
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"v0.0.1"}`)
	}))
	defer liar.Close()
	port := liar.URL[strings.LastIndex(liar.URL, ":")+1:]
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n# old\n")
	os.MkdirAll(filepath.Join(f.root, "usr/local/bin"), 0o755)
	os.WriteFile(filepath.Join(f.root, "usr/local/bin/fogd"), []byte("\x7fELF\x00binary\x00fogd-version=v2.0.0\x00more\n# running v2\n"), 0o755)

	out, err := f.run(t, "STUB_RUNNING=1", "STUB_LISTEN=127.0.0.1:"+port)
	if err == nil || !strings.Contains(out, "runs fogd v2.0.0") || !strings.Contains(out, "refusing to downgrade") {
		t.Fatalf("downgrade not refused (err %v):\n%s", err, out)
	}
	if got := f.read(t, "usr/local/bin/fogd"); !strings.Contains(got, "running v2") {
		t.Errorf("fogd = %q after a refused downgrade", got)
	}
	if out, err := f.run(t, "FOG_ALLOW_DOWNGRADE=1"); err != nil {
		t.Fatalf("FOG_ALLOW_DOWNGRADE=1: %v\n%s", err, out)
	}
	if got := f.read(t, "usr/local/bin/fogd"); !strings.Contains(got, "# old") {
		t.Errorf("fogd = %q, want the older release installed on request", got)
	}
}

// The keys the tests sign with are never the pinned ones — a test passing
// because it accidentally used the real key would prove nothing.
func TestTestSignerIsNotTheReleaseKey(t *testing.T) {
	t.Parallel()

	keys, _ := pinnedKeys()
	s := newSigner(t)
	for _, k := range keys {
		if ed25519.PublicKey(k).Equal(s.pub) {
			t.Fatal("test signer collided with a pinned key")
		}
	}
}

// A mirror serving a manifest whose absolute URLs still name the original
// host: install.sh, like the updater's downloadArtifact, takes each file from
// beside the manifest instead — and the sha256 still decides.
func TestInstallShFallsBackToFilesBesideAMirroredManifest(t *testing.T) {
	t.Parallel()

	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n# mirrored\n")
	body, _ := f.h.get("files.test/fog/latest.json")
	body = strings.ReplaceAll(body, `"url":"`, `"url":"http://gone.test/releases/v1.0.0/`)
	f.h.set("files.test/fog/latest.json", body)
	f.h.set("files.test/fog/latest.json.sig", f.s.sign(body))
	got, out, err := runInstallSh(t, "FOG_SOURCE=http://files.test/fog", "FOG_RELEASE_KEYS="+f.s.spki(t), "http_proxy="+f.h.URL)
	if err != nil {
		t.Fatalf("print: %v\n%s", err, out)
	}
	if want := "http://files.test/fog/fogd-bin"; got["artifact"] != want {
		t.Errorf("print mode names %q, want the reachable %q", got["artifact"], want)
	}
	if out, err := f.run(t); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if got := f.read(t, "usr/local/bin/fogd"); !strings.Contains(got, "mirrored") {
		t.Errorf("fogd = %q", got)
	}
}

// The manifest's host answers, but with other bytes (a different server now
// lives at that name): install.sh passes over it to the next location, as
// the updater does, instead of stopping at the first 200.
func TestInstallShSkipsALocationWithTheWrongBytes(t *testing.T) {
	t.Parallel()

	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n# the real one\n")
	body, _ := f.h.get("files.test/fog/latest.json")
	body = strings.ReplaceAll(body, `"url":"`, `"url":"http://old.test/fog/`)
	f.h.set("files.test/fog/latest.json", body)
	f.h.set("files.test/fog/latest.json.sig", f.s.sign(body))
	for _, name := range []string{"fogd-bin", "fogd.service", "fogd-rollback", "fogd.socket"} {
		f.h.set("old.test/fog/"+name, "<html>this name belongs to someone else now</html>")
	}
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if got := f.read(t, "usr/local/bin/fogd"); !strings.Contains(got, "the real one") {
		t.Errorf("fogd = %q", got)
	}
	// And when no location has the release's bytes, nothing is installed.
	g := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n")
	g.h.set("files.test/fog/fogd-bin", "<html>junk</html>")
	if out, err := g.run(t); err == nil || !strings.Contains(out, "no download of fogd-bin matches the manifest") {
		t.Fatalf("junk accepted (err %v):\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(g.root, "usr/local/bin/fogd")); err == nil {
		t.Error("installed from junk")
	}
}

// The shell resolver block orders and validates versions exactly as
// version.go does: every pair from semver's own precedence chain (plus
// rc.9 < rc.10), and the same refusals.
func TestShellSemverMatchesGo(t *testing.T) {
	t.Parallel()

	skipShell(t)
	chain := []string{
		"v0.9.9", "v1.0.0-0", "v1.0.0-1", "v1.0.0-2", "v1.0.0-10", "v1.0.0-alpha", "v1.0.0-alpha.1",
		"v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1",
		"v1.0.0-rc.9", "v1.0.0-rc.10", "v1.0.0+build.5", "v1.0.1-rc.1", "v1.0.1", "v1.10.0", "v2.0.0",
	}
	invalid := []string{"1.0.0", "v1.0", "v1.0.0.0", "v01.0.0", "v1.0.0-", "v1.0.0-.", "v1.0.0-rc..1",
		"v1.0.0-rc.01", "v1.0.0-rc_1", "v1.0.0-$(id)", "v1.0.0 ", "vx.y.z", "v1234567890.0.0", ""}
	var script strings.Builder
	script.WriteString(`set -eu
eval "$(sed -n '/^semver_valid() {/,/^}/p' ../../install.sh)"
eval "$(sed -n '/^semver_ident_cmp() {/,/^}/p' ../../install.sh)"
eval "$(sed -n '/^semver_cmp() {/,/^}/p' ../../install.sh)"
`)
	for _, a := range chain {
		for _, b := range chain {
			fmt.Fprintf(&script, "echo \"cmp %s %s $(semver_cmp %s %s)\"\n", a, b, a, b)
		}
	}
	for _, v := range append(append([]string{}, chain...), invalid...) {
		fmt.Fprintf(&script, "if semver_valid '%s'; then echo 'valid %s yes'; else echo 'valid %s no'; fi\n", v, v, v)
	}
	out, err := exec.Command("sh", "-c", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, " ", 4)
		switch f[0] {
		case "cmp":
			n++
			want, err := CompareVersions(f[1], f[2])
			if err != nil {
				t.Fatal(err)
			}
			if f[3] != fmt.Sprint(want) {
				t.Errorf("shell semver_cmp %s %s = %s, Go says %d", f[1], f[2], f[3], want)
			}
		case "valid":
			v := strings.TrimSuffix(strings.TrimSuffix(line[len("valid "):], " yes"), " no")
			if got, want := strings.HasSuffix(line, " yes"), ValidVersion(v); got != want {
				t.Errorf("shell semver_valid %q = %v, Go says %v", v, got, want)
			}
		}
	}
	if n != len(chain)*len(chain) {
		t.Errorf("compared %d pairs, want %d", n, len(chain)*len(chain))
	}
}

// Re-running install.sh on a server that listens on its own port (a
// FOG_LISTEN drop-in) must keep that port: the socket, which now holds
// fog's port, gets the same one in a drop-in — unless it already has one.
func TestInstallShKeepsACustomPort(t *testing.T) {
	t.Parallel()

	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n")
	dropin := filepath.Join(f.root, "etc/systemd/system/fogd.socket.d/10-listen.conf")

	if out, err := f.run(t); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(dropin); err == nil {
		t.Error("a socket drop-in was written for the default port")
	}

	out, err := f.run(t, "STUB_LISTEN=:9000")
	if err != nil {
		t.Fatalf("re-run on :9000: %v\n%s", err, out)
	}
	if got := f.read(t, "etc/systemd/system/fogd.socket.d/10-listen.conf"); got != "[Socket]\nListenStream=\nListenStream=9000\n" {
		t.Errorf("socket drop-in = %q", got)
	}

	// A socket drop-in of the server's own is left exactly as it is.
	own := "[Socket]\nListenStream=\nListenStream=192.0.2.5:9000\n"
	os.Remove(dropin)
	os.WriteFile(filepath.Join(f.root, "etc/systemd/system/fogd.socket.d/50-mine.conf"), []byte(own), 0o644)
	if out, err := f.run(t, "STUB_LISTEN=:9000"); err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
	if _, err := os.Stat(dropin); err == nil {
		t.Error("wrote a socket drop-in beside the server's own")
	}
	if got := f.read(t, "etc/systemd/system/fogd.socket.d/50-mine.conf"); got != own {
		t.Errorf("the server's own socket drop-in changed: %q", got)
	}
}

// The downgrade check orders by semver, pre-releases included, and a fogd
// whose version can't be read (no stamp, or not a release) can't be told
// from a downgrade.
func TestInstallShDowngradeUsesSemver(t *testing.T) {
	t.Parallel()

	skipShell(t)
	for _, tc := range []struct{ installed, offered string }{
		{"fogd-version=v1.0.0", "v1.0.0-rc.9"}, // a pre-release of the installed version is older
		{"fogd-version=v1.0.0-rc.10", "v1.0.0-rc.9"},
		{"fogd-version=dev", "v1.0.0"},                         // unversioned
		{"no stamp at all", "v1.0.0"},                          // from before versions were stamped
		{"fogd-version=vintage fogd-version=v2.0.0", "v1.0.0"}, // only a real stamp counts
	} {
		f := newInstallFixture(t, tc.offered, "#!/bin/sh\nexit 0\n")
		os.MkdirAll(filepath.Join(f.root, "usr/local/bin"), 0o755)
		os.WriteFile(filepath.Join(f.root, "usr/local/bin/fogd"), []byte("#!/bin/sh\nexit 0\n# "+tc.installed+"\n"), 0o755)
		out, err := f.run(t)
		if err == nil || !strings.Contains(out, "refusing") {
			t.Errorf("installed %q, offered %s: not refused (err %v)\n%s", tc.installed, tc.offered, err, out)
		}
		if out, err := f.run(t, "FOG_ALLOW_DOWNGRADE=1"); err != nil {
			t.Errorf("installed %q, offered %s, FOG_ALLOW_DOWNGRADE=1: %v\n%s", tc.installed, tc.offered, err, out)
		}
	}
}

// A port set through EnvironmentFile= (which overrides Environment=, as
// systemd reads them) is carried to the socket too.
func TestInstallShKeepsAPortFromAnEnvironmentFile(t *testing.T) {
	t.Parallel()

	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n")
	env := filepath.Join(t.TempDir(), "fog.env")
	os.WriteFile(env, []byte("FOG_DATA_DIR=/var/lib/fog\nFOG_LISTEN=\":9100\"\n"), 0o644)
	if out, err := f.run(t, "STUB_ENVFILE="+env); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := f.read(t, "etc/systemd/system/fogd.socket.d/10-listen.conf"); got != "[Socket]\nListenStream=\nListenStream=9100\n" {
		t.Errorf("socket drop-in = %q", got)
	}
}
