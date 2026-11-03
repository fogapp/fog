package compute

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The pairing code is never on a command line: not the installer's, not
// that of anything it runs (curl, sed, shasum, …, fog-node itself). Any
// user of the machine can read a command line (/proc/<pid>/cmdline, ps)
// and pair a machine of theirs first.
func TestInstallKeepsTheCodeOffEveryCommandLine(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the installer writes /var/lib/fog-node")
	}
	const code = "QWER-TYUP"
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "argv.log")
	// fog-node, as fog serves it: a stub that notes how it was run, and
	// whether join found the code where it should.
	stub := "#!/bin/sh\nprintf '%s\\n' \"fog-node $*\" >> \"$ARGVLOG\"\n" +
		"if [ \"$1\" = join ] && [ \"$FOG_PAIR_CODE\" = " + code + " ]; then echo join-got-the-code >> \"$ARGVLOG\"; fi\n" +
		"if [ \"$1\" = install ] && [ -n \"${FOG_PAIR_CODE:-}\" ]; then echo install-saw-the-code >> \"$ARGVLOG\"; fi\n"
	sum := sha256.Sum256([]byte(stub))
	platform := runtime.GOOS + "-" + map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	fog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/compute/v1/fog-node/" + platform:
			w.Write([]byte(stub))
		case "/compute/v1/fog-node/" + platform + ".sha256":
			w.Write([]byte(hex.EncodeToString(sum[:]) + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fog.Close()

	// Every program the script runs goes through a shim that notes its
	// arguments, then runs the real one.
	shims := filepath.Join(tmp, "shims")
	os.Mkdir(shims, 0o755)
	for _, prog := range []string{"curl", "sed", "uname", "id", "mkdir", "shasum", "sha256sum", "awk", "tr", "chmod", "mv", "rm", "brew", "ffmpeg"} {
		real, err := exec.LookPath(prog)
		if err != nil {
			continue
		}
		shim := "#!/bin/sh\nprintf '%s\\n' \"" + prog + " $*\" >> \"$ARGVLOG\"\nexec " + real + " \"$@\"\n"
		os.WriteFile(filepath.Join(shims, prog), []byte(shim), 0o755)
	}
	for _, need := range []string{"curl", "sed", "awk"} {
		if _, err := os.Stat(filepath.Join(shims, need)); err != nil {
			t.Skipf("no %s here", need)
		}
	}
	sh := exec.Command("/bin/sh", "-s", "--", fog.URL)
	sh.Stdin = strings.NewReader(InstallScript)
	sh.Env = append(os.Environ(), "PATH="+shims+":"+os.Getenv("PATH"), "HOME="+tmp, "ARGVLOG="+argvLog, "FOG_PAIR_CODE="+code)
	out, err := sh.CombinedOutput()
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if strings.Contains(strings.Join(sh.Args, " "), code) {
		t.Fatal("the installer's own arguments carry the code")
	}
	raw, _ := os.ReadFile(argvLog)
	log := string(raw)
	if strings.Contains(log, code) {
		t.Fatalf("a command line carried the code:\n%s", log)
	}
	if !strings.Contains(log, "join-got-the-code") || !strings.Contains(log, "fog-node join --dir") || !strings.Contains(log, "curl ") {
		t.Fatalf("the install didn't run as expected:\n%s\n%s", log, out)
	}
	if strings.Contains(log, "install-saw-the-code") {
		t.Fatal("the code outlived the pairing, into the service install")
	}

	// A code given as an argument anyway is refused before anything runs.
	os.Remove(argvLog)
	bad := exec.Command("/bin/sh", "-s", "--", fog.URL, code)
	bad.Stdin = strings.NewReader(InstallScript)
	bad.Env = append(os.Environ(), "PATH="+shims+":"+os.Getenv("PATH"), "HOME="+tmp, "ARGVLOG="+argvLog)
	if out, err := bad.CombinedOutput(); err == nil || !strings.Contains(string(out), "FOG_PAIR_CODE") {
		t.Fatalf("a code as an argument: %v %s", err, out)
	}
	if raw, _ := os.ReadFile(argvLog); strings.Contains(string(raw), "fog-node") {
		t.Fatal("fog-node ran with the code refused")
	}
}
