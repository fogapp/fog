package vpn

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gitea.lan/matthew/fog/internal/supervise"
)

func TestAutoRouteIsAHostRoute(t *testing.T) {
	got, err := AutoRoute("192.168.1.200")
	if err != nil || got != "192.168.1.200/32" {
		t.Fatalf("AutoRoute = %q, %v; want 192.168.1.200/32", got, err)
	}
	if _, err := AutoRoute(""); err == nil {
		t.Fatal("empty LAN address accepted")
	}
	if _, err := AutoRoute("fog.lan"); err == nil {
		t.Fatal("hostname accepted as an address")
	}
}

func TestParseRoutes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"192.168.1.200/32", "192.168.1.200/32"},
		{"192.168.1.200", "192.168.1.200/32"},
		{"192.168.1.0/24, 10.0.0.0/8", "192.168.1.0/24,10.0.0.0/8"},
		// Host bits set is what someone typing their own address with a /24
		// produces; tailscaled would refuse it, so normalize instead of
		// reporting a success that isn't one.
		{"192.168.1.200/24", "192.168.1.0/24"},
	}
	for _, c := range cases {
		got, err := ParseRoutes(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseRoutes(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "  ", "not-a-route", "192.168.1.0/33"} {
		if got, err := ParseRoutes(bad); err == nil {
			t.Errorf("ParseRoutes(%q) = %q, want an error", bad, got)
		}
	}
}

func TestHostname(t *testing.T) {
	cases := map[string]string{
		"":                 "fog",
		"Fog":              "fog",
		"Matthew's Server": "matthews-server",
		"  fog.lan  ":      "fog-lan",
		"!!!":              "fog",
		"a--b":             "a-b",
	}
	for in, want := range cases {
		if got := Hostname(in); got != want {
			t.Errorf("Hostname(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Hostname(string(bytes.Repeat([]byte("y"), 100))); len(got) != 63 {
		t.Errorf("long name not truncated to a legal DNS label: %d chars", len(got))
	}
}

// The route the operator has to approve in the admin console is the one
// fact the page exists to report, so approval must be judged against what
// the tailnet actually says — and only when every advertised route is there.
func TestParseStatusRouteApproval(t *testing.T) {
	js := []byte(`{
	  "BackendState": "Running",
	  "HaveNodeKey": true,
	  "MagicDNSSuffix": "example-tail.ts.net",
	  "CurrentTailnet": {"Name": "example.com"},
	  "Self": {
	    "DNSName": "fog.example-tail.ts.net.",
	    "HostName": "fog",
	    "Online": true,
	    "TailscaleIPs": ["100.101.102.103", "fd7a:115c::1"],
	    "PrimaryRoutes": ["192.168.1.200/32"]
	  }
	}`)
	s, err := parseStatus(js, "192.168.1.200/32")
	if err != nil {
		t.Fatal(err)
	}
	if !s.RouteApproved {
		t.Error("approved route reported as unapproved")
	}
	if s.TailnetIP != "100.101.102.103" {
		t.Errorf("TailnetIP = %q, want the v4 address", s.TailnetIP)
	}
	if s.DNSName != "fog.example-tail.ts.net" {
		t.Errorf("DNSName = %q, trailing dot not trimmed", s.DNSName)
	}
	if s.Tailnet != "example.com" || !s.Online || !s.HaveNodeKey {
		t.Errorf("snapshot lost fields: %+v", s)
	}

	// A different route approved is not this route approved.
	s2, _ := parseStatus(js, "10.0.0.0/8")
	if s2.RouteApproved {
		t.Error("unrelated approved route counted as approval")
	}
}

func TestRoutesApproved(t *testing.T) {
	cases := []struct {
		advertised string
		primary    []string
		want       bool
	}{
		{"192.168.1.200/32", []string{"192.168.1.200/32"}, true},
		{"192.168.1.200/32", nil, false},
		{"192.168.1.200/32", []string{"192.168.1.0/24"}, false},
		// Partial approval is not approval: some name still won't connect.
		{"192.168.1.0/24,10.0.0.0/8", []string{"192.168.1.0/24"}, false},
		{"192.168.1.0/24,10.0.0.0/8", []string{"10.0.0.0/8", "192.168.1.0/24"}, true},
		// Nothing advertised can't be "approved".
		{"", []string{"192.168.1.0/24"}, false},
	}
	for _, c := range cases {
		if got := routesApproved(c.advertised, c.primary); got != c.want {
			t.Errorf("routesApproved(%q, %v) = %v, want %v", c.advertised, c.primary, got, c.want)
		}
	}
}

// A pre-login daemon reports no Self at all; reading it must not panic and
// must not invent an approval.
func TestParseStatusBeforeLogin(t *testing.T) {
	s, err := parseStatus([]byte(`{"BackendState":"NeedsLogin","AuthURL":"https://login.tailscale.com/a/abc","HaveNodeKey":false,"Self":null}`), "192.168.1.200/32")
	if err != nil {
		t.Fatal(err)
	}
	if s.RouteApproved || s.HaveNodeKey || s.Online {
		t.Errorf("logged-out daemon reported as working: %+v", s)
	}
	if s.AuthURL == "" {
		t.Error("AuthURL dropped — it's the link the operator needs")
	}
}

func TestNeedsForwarding(t *testing.T) {
	// A host route to fog's own address is delivered locally.
	if needsForwarding("192.168.1.200/32") || needsForwarding("fd7a::1/128") {
		t.Error("host route wrongly said to need IP forwarding")
	}
	if !needsForwarding("192.168.1.0/24") || !needsForwarding("192.168.1.200/32,10.0.0.0/8") {
		t.Error("routed subnet not flagged as needing IP forwarding")
	}
}

func TestParseSHA256(t *testing.T) {
	const sum = "36ddd9b51be57ffc2990cf76323cfa13643bfbb1b8a969f6183fa164741cdef5"
	for _, in := range []string{sum, sum + "\n", sum + "  tailscale_1.2.3_amd64.tgz\n", "36DDD9B51BE57FFC2990CF76323CFA13643BFBB1B8A969F6183FA164741CDEF5"} {
		got, err := parseSHA256(in)
		if err != nil || got != sum {
			t.Errorf("parseSHA256(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "\n", "not-hex-and-short", "<html>404</html>"} {
		if _, err := parseSHA256(bad); err == nil {
			t.Errorf("parseSHA256(%q) accepted", bad)
		}
	}
}

func TestLatestTarballRejectsAPathFromTheIndex(t *testing.T) {
	// The index is fetched over TLS, but its contents still choose the file
	// to download; a name with a path in it must not get to pick a URL.
	if _, _, err := pickTarball(map[string]string{"amd64": "../../etc/passwd"}, "1.0", "amd64"); err == nil {
		t.Fatal("path traversal in the release index accepted")
	}
	if _, _, err := pickTarball(map[string]string{"amd64": "x.tgz"}, "1.0", "arm64"); err == nil {
		t.Fatal("missing arch accepted")
	}
	name, ver, err := pickTarball(map[string]string{"amd64": "tailscale_1.0_amd64.tgz"}, "1.0", "amd64")
	if err != nil || name != "tailscale_1.0_amd64.tgz" || ver != "1.0" {
		t.Fatalf("pickTarball = %q, %q, %v", name, ver, err)
	}
}

// minimalELF builds the smallest header debug/elf will parse.
func minimalELF(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], machine)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}

func hostELF(t *testing.T) []byte {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64":
		return minimalELF(62)
	case "arm64":
		return minimalELF(183)
	}
	t.Skipf("no ELF machine mapping for %s", runtime.GOARCH)
	return nil
}

func tarball(t *testing.T, files map[string][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "tailscale.tgz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), BinDir: t.TempDir(), DataDir: t.TempDir()}
}

func TestExtractInstallsBothBinaries(t *testing.T) {
	elf := hostELF(t)
	m := testManager(t)
	tgz := tarball(t, map[string][]byte{
		"tailscale_1.102.3_amd64/tailscale":         elf,
		"tailscale_1.102.3_amd64/tailscaled":        elf,
		"tailscale_1.102.3_amd64/systemd/README.md": []byte("not a binary"),
	})
	if err := m.extract(tgz); err != nil {
		t.Fatal(err)
	}
	if !m.installed() {
		t.Fatal("extract reported success but the binaries aren't installed")
	}
	if _, err := os.Stat(filepath.Join(m.binDir(), "README.md")); err == nil {
		t.Error("extract wrote a file it had no business writing")
	}
}

// Half an install — a new CLI against a missing daemon — is worse than none,
// so a tarball missing one binary must leave nothing behind.
func TestExtractRefusesAPartialTarball(t *testing.T) {
	m := testManager(t)
	tgz := tarball(t, map[string][]byte{"tailscale_1.0_amd64/tailscale": hostELF(t)})
	if err := m.extract(tgz); err == nil {
		t.Fatal("partial tarball accepted")
	}
	if supervise.Executable(m.cliBin()) {
		t.Error("the one binary present was installed anyway")
	}
	ents, _ := os.ReadDir(m.binDir())
	if len(ents) != 0 {
		t.Errorf("staging files left behind: %v", ents)
	}
}

// An HTML error page renamed .tgz, or a wrong-arch build, must not land.
func TestExtractRejectsNonELFContent(t *testing.T) {
	m := testManager(t)
	tgz := tarball(t, map[string][]byte{
		"tailscale_1.0_amd64/tailscale":  []byte("<html>404</html>"),
		"tailscale_1.0_amd64/tailscaled": hostELF(t),
	})
	if err := m.extract(tgz); err == nil {
		t.Fatal("non-ELF content accepted")
	}
	if m.installed() {
		t.Error("binaries installed despite verification failure")
	}
}

// The archive names files; it must never get to name a destination.
func TestExtractIgnoresPathsInTheArchive(t *testing.T) {
	m := testManager(t)
	tgz := tarball(t, map[string][]byte{
		"../../../../tmp/tailscale":      hostELF(t),
		"tailscale_1.0_amd64/tailscaled": hostELF(t),
	})
	if err := m.extract(tgz); err != nil {
		t.Fatal(err)
	}
	// The traversing entry was matched on its base name and written into
	// binDir like any other, not at the path it asked for.
	if !m.installed() {
		t.Fatal("expected both binaries in binDir")
	}
}
