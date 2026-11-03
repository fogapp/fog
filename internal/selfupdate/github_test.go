package selfupdate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The public default, https://github.com/fogapp/fog, end to end against a
// local fake of GitHub's release layout: github.com's releases/latest/
// download/<file> redirecting to releases/download/<tag>/<file>, which
// redirects to objects.githubusercontent.com; raw.githubusercontent.com for
// the committed tree; api.github.com for release notes. Everything is https
// with a certificate for those names, reached through a CONNECT proxy, so
// the Go updater and curl (install.sh) take the same path they take on the
// internet — redirects, TLS and all.

type fakeGitHub struct {
	dir       string // the release directory published as the release
	version   string
	latest    bool              // the release is marked latest
	committed map[string]string // raw.githubusercontent.com path → body
	proxy     *httptest.Server
	caFile    string
	pool      *x509.CertPool
}

func newFakeGitHub(t *testing.T, dir, version string) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{dir: dir, version: version, latest: true, committed: map[string]string{}}

	// One self-signed certificate for every GitHub name, trusted by both
	// clients for this test only.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake github"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames:              []string{"github.com", "raw.githubusercontent.com", "objects.githubusercontent.com", "api.github.com"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	g.pool = x509.NewCertPool()
	g.pool.AddCert(cert)
	g.caFile = filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(g.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)

	site := httptest.NewUnstartedServer(http.HandlerFunc(g.serve))
	site.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	site.StartTLS()
	t.Cleanup(site.Close)

	// The proxy tunnels every CONNECT to the fake; plain http answers 404,
	// like a host that isn't there.
	g.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.NotFound(w, r)
			return
		}
		up, err := net.Dial("tcp", site.Listener.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, _ := w.(http.Hijacker)
		conn, buf, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { io.Copy(up, buf); up.Close() }()
		io.Copy(conn, up)
		conn.Close()
	}))
	t.Cleanup(g.proxy.Close)
	return g
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	host, _, _ := strings.Cut(r.Host, ":")
	p := r.URL.Path
	switch host {
	case "github.com":
		if f, ok := strings.CutPrefix(p, "/fogapp/fog/releases/latest/download/"); ok && g.latest {
			http.Redirect(w, r, "/fogapp/fog/releases/download/"+g.version+"/"+f, http.StatusFound)
			return
		}
		if f, ok := strings.CutPrefix(p, "/fogapp/fog/releases/download/"+g.version+"/"); ok {
			http.Redirect(w, r, "https://objects.githubusercontent.com/github-production-release-asset/"+f+"?X-Amz-Signature=x", http.StatusFound)
			return
		}
	case "objects.githubusercontent.com":
		if f, ok := strings.CutPrefix(p, "/github-production-release-asset/"); ok && !strings.Contains(f, "/") {
			if b, err := os.ReadFile(filepath.Join(g.dir, f)); err == nil {
				w.Write(b)
				return
			}
		}
	case "raw.githubusercontent.com":
		if b, ok := g.committed[p]; ok {
			w.Write([]byte(b))
			return
		}
	case "api.github.com":
		if p == "/repos/fogapp/fog/releases/tags/"+g.version {
			json.NewEncoder(w).Encode(map[string]string{"tag_name": g.version, "body": "notes from github"})
			return
		}
	}
	http.NotFound(w, r)
}

// updater is s's updater, reaching the internet only through the fake.
func (g *fakeGitHub) updater(s *signer) *Updater {
	proxy, _ := url.Parse(g.proxy.URL)
	u := s.updater()
	u.transport = &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{RootCAs: g.pool}}
	return u
}

// env is what puts curl on the same path.
func (g *fakeGitHub) env() []string {
	return []string{"https_proxy=" + g.proxy.URL, "HTTPS_PROXY=" + g.proxy.URL, "http_proxy=" + g.proxy.URL,
		"CURL_CA_BUNDLE=" + g.caFile, "SSL_CERT_FILE=" + g.caFile, "no_proxy=", "NO_PROXY="}
}

func readFileT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// needCurlCA skips where curl ignores CURL_CA_BUNDLE (macOS's system curl,
// built on SecureTransport, reads only the keychain).
func needCurlCA(t *testing.T, g *fakeGitHub) {
	t.Helper()
	cmd := exec.Command("curl", "-fsS", "-o", "/dev/null", "https://api.github.com/repos/fogapp/fog/releases/tags/"+g.version)
	cmd.Env = append(os.Environ(), g.env()...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this curl won't trust a test CA through CURL_CA_BUNDLE (%v: %s); runs on Linux (Colima)", err, out)
	}
}

func TestGitHubDefaultSourceEndToEnd(t *testing.T) {
	skipShell(t)
	f := newInstallFixture(t, "v0.0.1", "#!/bin/sh\nexit 0\n")
	fogd := "#!/bin/sh\n# fogd v1.4.0 from github\nexit 0\n"
	dir := releaseDir(t, f.s, "v1.4.0", fogd, false)
	g := newFakeGitHub(t, dir, "v1.4.0")

	// The updater, given the public repo.
	u := g.updater(f.s)
	m, err := u.resolveManifest(context.Background(), "https://github.com/fogapp/fog")
	if err != nil {
		t.Fatalf("resolveManifest: %v", err)
	}
	if m.Version != "v1.4.0" || m.FetchedFrom != "https://github.com/fogapp/fog/releases/latest/download/latest.json" || m.Notes != "notes from github" {
		t.Errorf("manifest = %s from %s, notes %q", m.Version, m.FetchedFrom, m.Notes)
	}
	dst := filepath.Join(t.TempDir(), "fogd")
	if _, err := u.downloadArtifact(context.Background(), m, m.Artifacts["linux-amd64"], dst); err != nil {
		t.Fatalf("downloadArtifact: %v", err)
	}
	if b := readFileT(t, dst); b != fogd {
		t.Errorf("downloaded %q", b)
	}

	// install.sh with no FOG_SOURCE at all: the public default.
	needCurlCA(t, g)
	got, out, err := runInstallSh(t, append(g.env(), "FOG_SOURCE=", "FOG_RELEASE_KEYS="+f.s.spki(t))...)
	if err != nil {
		t.Fatalf("install.sh print: %v\n%s", err, out)
	}
	if got["manifest"] != m.FetchedFrom || got["version"] != "v1.4.0" {
		t.Errorf("install.sh resolved %q %q, updater %q", got["manifest"], got["version"], m.FetchedFrom)
	}
	out, err = f.run(t, append(g.env(), "FOG_SOURCE=")...)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if b := readFileT(t, filepath.Join(f.root, "usr/local/bin/fogd")); b != fogd {
		t.Errorf("installed fogd = %q", b)
	}
	if d := readFileT(t, filepath.Join(f.root, "etc/systemd/system/fogd.service.d/10-update-source.conf")); !strings.Contains(d, `FOG_UPDATE_BASE_URL=https://github.com/fogapp/fog"`) {
		t.Errorf("update source on the unit:\n%s", d)
	}
}

// A GitHub source never falls back to the repo's committed manifest, whose
// URLs name the private host the release was cut on: with no release marked
// latest, both resolvers stop and say so — even where that host would
// answer (here, with something else entirely).
func TestGitHubSourceNeverUsesTheCommittedManifest(t *testing.T) {
	skipShell(t)
	f := newInstallFixture(t, "v0.0.1", "#!/bin/sh\nexit 0\n")
	dir := releaseDir(t, f.s, "v1.4.0", "#!/bin/sh\n# fogd v1.4.0\nexit 0\n", false)
	g := newFakeGitHub(t, dir, "v1.4.0")
	g.latest = false
	rel := readFileT(t, filepath.Join(dir, "latest.json"))
	abs := strings.ReplaceAll(rel, `"url": "`, `"url": "http://gitea.invalid/matthew/fog/releases/download/v1.4.0/`)
	g.committed["/fogapp/fog/main/release/latest.json"] = abs
	g.committed["/fogapp/fog/main/release/latest.json.sig"] = f.s.sign(abs)

	_, err := g.updater(f.s).resolveManifest(context.Background(), "https://github.com/fogapp/fog")
	if err == nil || strings.Contains(err.Error(), "raw.githubusercontent.com") {
		t.Fatalf("resolveManifest = %v, want a failure that never tried the committed manifest", err)
	}
	if !strings.Contains(err.Error(), "releases/latest/download/latest.json") {
		t.Errorf("error doesn't say what was tried: %v", err)
	}

	needCurlCA(t, g)
	out, err := f.run(t, append(g.env(), "FOG_SOURCE=https://github.com/fogapp/fog")...)
	if err == nil || strings.Contains(out, "raw.githubusercontent.com") {
		t.Fatalf("install.sh (err %v):\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(f.root, "usr/local/bin/fogd")); err == nil {
		t.Error("installed something")
	}
}

// An upgrade never moves a server to the public default: re-run without
// FOG_SOURCE, install.sh keeps the source the server was installed from.
func TestInstallShUpgradeKeepsTheServersSource(t *testing.T) {
	skipShell(t)
	f := newInstallFixture(t, "v1.0.0", "#!/bin/sh\nexit 0\n# from the lan\n")
	if out, err := f.run(t); err != nil { // installed from files.test/fog
		t.Fatalf("first install: %v\n%s", err, out)
	}
	// The old drop-in format, from before the values were quoted.
	os.WriteFile(filepath.Join(f.root, "etc/systemd/system/fogd.service.d/10-update-source.conf"),
		[]byte("[Service]\nEnvironment=FOG_UPDATE_BASE_URL=http://files.test/fog\n"), 0o644)
	f.release("v1.1.0", "#!/bin/sh\nexit 0\n# v1.1.0 from the lan\n", nil)
	out, err := f.run(t, "FOG_SOURCE=")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(out, "keeping this server's source, http://files.test/fog") {
		t.Errorf("didn't say it kept the source:\n%s", out)
	}
	if b := readFileT(t, filepath.Join(f.root, "usr/local/bin/fogd")); !strings.Contains(b, "v1.1.0 from the lan") {
		t.Errorf("fogd = %q", b)
	}
	d := readFileT(t, filepath.Join(f.root, "etc/systemd/system/fogd.service.d/10-update-source.conf"))
	if !strings.Contains(d, `FOG_UPDATE_BASE_URL=http://files.test/fog"`) || strings.Contains(d, "github") {
		t.Errorf("the server's update source changed:\n%s", d)
	}
}
