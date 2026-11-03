package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Proxmox scripts run as root on the hypervisor. They used to fetch
// install.sh from the source and run it there (to resolve the release)
// before anything was confirmed, --dry-run included, then push the same
// unverified file into the guest. Now the node resolves and verifies the
// release with the shared resolver block and takes install.sh from the
// signed manifest by sha256; nothing fetched is executed on the node.
//
// These tests play a hostile mirror against both scripts, with every
// Proxmox command stubbed. The mirror's install.sh, if anything ever ran
// it, would create a marker file.

type proxmoxFixture struct {
	h      *hostServer
	s      *signer
	stubs  string
	marker string
}

func newProxmoxFixture(t *testing.T) *proxmoxFixture {
	t.Helper()
	skipShell(t)
	f := &proxmoxFixture{h: newHostServer(t), s: newSigner(t), stubs: t.TempDir()}
	needOpenSSL(t, f.s)
	f.marker = filepath.Join(t.TempDir(), "PWNED")
	for name, body := range map[string]string{
		"id":          "echo 0",
		"pveversion":  "echo pve-manager/8.2.4/faaa0a4e",
		"pct":         "exit 1",
		"qm":          "exit 1",
		"pvesh":       "echo 150",
		"pveam":       `case "$1" in available) echo 'system          debian-13-standard_13.1-2_amd64.tar.zst' ;; esac`,
		"pvesm":       `printf 'Name Type Status Total Used Available %%\nlocal-lvm lvmthin active 100 50 104857600 50%%\n'`,
		"dpkg":        "echo amd64",
		"ip":          "exit 0",
		"timedatectl": "echo UTC",
		"getent":      `echo "127.0.0.1 STREAM $2"`,
		"genisoimage": "exit 0",
		"perl":        "exit 0",
		"sha512sum":   "exit 0",
		"whiptail":    "exit 1",
	} {
		writeStub(t, f.stubs, name, "#!/bin/sh\n"+body+"\n")
	}
	return f
}

// release publishes a release directory at files.test/fog whose install.sh
// would create the marker; signer signs the manifest, and tamper replaces
// install.sh after its digest went into it.
func (f *proxmoxFixture) release(signer *signer, tamper bool) {
	install := fmt.Sprintf("#!/bin/sh\necho 'PWNED: running as root' > %q\n", f.marker)
	body := fmt.Sprintf(`{"version":"v1.0.0","issued_at":%d,"artifacts":{"linux-amd64":{"url":"fogd-linux-amd64","sha256":%q},"install.sh":{"url":"install.sh","sha256":%q}}}`,
		time.Now().Unix(), sum("fogd"), sum(install))
	f.h.set("files.test/fog/latest.json", body)
	f.h.set("files.test/fog/latest.json.sig", signer.sign(body))
	f.h.set("files.test/fog/fogd-linux-amd64", "fogd")
	if tamper {
		install += "# changed on the mirror\n"
	}
	f.h.set("files.test/fog/install.sh", install)
}

func (f *proxmoxFixture) run(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"../../" + script}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+f.stubs+":"+os.Getenv("PATH"),
		"FOG_SOURCE=http://files.test/fog", "FOG_RELEASE_KEYS="+f.s.spki(t), "FOG_YES=1", "NO_COLOR=1",
		"http_proxy="+f.h.URL, "no_proxy=", "NO_PROXY=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *proxmoxFixture) pwned(t *testing.T) {
	t.Helper()
	if b, err := os.ReadFile(f.marker); err == nil {
		t.Fatalf("the mirror's install.sh ran on the node: %s", b)
	}
}

func TestProxmoxScriptsNeverRunUnverifiedCode(t *testing.T) {
	for _, script := range []string{"proxmox-lxc.sh", "proxmox-vm.sh"} {
		t.Run(script, func(t *testing.T) {
			t.Run("manifest signed by someone else", func(t *testing.T) {
				f := newProxmoxFixture(t)
				f.release(newSigner(t), false)
				out, err := f.run(t, script, "--dry-run")
				if err == nil || !strings.Contains(out, "signature doesn't match") {
					t.Fatalf("a forged release was accepted (err %v):\n%s", err, out)
				}
				f.pwned(t)
			})
			t.Run("install.sh changed on the mirror", func(t *testing.T) {
				f := newProxmoxFixture(t)
				f.release(f.s, true)
				out, err := f.run(t, script, "--dry-run")
				if err == nil || !strings.Contains(out, "matches the manifest") {
					t.Fatalf("a tampered install.sh was accepted (err %v):\n%s", err, out)
				}
				f.pwned(t)
			})
			// Even a genuine install.sh is only fetched and checked on the
			// node — --dry-run prints the plan and runs nothing it fetched.
			t.Run("dry run of a genuine release", func(t *testing.T) {
				f := newProxmoxFixture(t)
				f.release(f.s, false)
				out, err := f.run(t, script, "--dry-run")
				if err != nil {
					t.Fatalf("dry run: %v\n%s", err, out)
				}
				if !strings.Contains(out, " create") || !strings.Contains(out, "http://files.test/fog/install.sh") {
					t.Errorf("dry run didn't print the plan:\n%s", out)
				}
				f.pwned(t)
			})
		})
	}
}
