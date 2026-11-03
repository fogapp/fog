package computenode

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

// These tests try to get out. A run's ffmpeg is confined (confine.go); here
// confined code tries to read the owner's files and fog-node's credential,
// to write outside its run, to run another program, and to reach the
// network, and each must fail, while the same thing unconfined works (so
// a failure is the confinement's doing).

// workDir is a work directory for a test's runs, inside fog-node's own as
// in production: one that ffmpeg run as nobody (under a root fog-node) can
// pass through to, as /var/lib/fog-node is.
func workDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if os.Geteuid() == 0 {
		for _, d := range []string{base, filepath.Dir(base)} {
			os.Chmod(d, 0o755)
		}
	}
	return filepath.Join(base, "fog-node", "work")
}

// confinesHere proves r's confinement. It answers false only where it is
// refused as it should be: a user's fog-node under Ubuntu's restriction on
// user namespaces, whose refusal must tell the owner what to do.
func confinesHere(t *testing.T, r *Runner) bool {
	t.Helper()
	if _, err := confinementKind(); err != nil {
		t.Logf("this machine can't confine: %v", err)
		return false
	}
	_, err := r.Confinement(context.Background())
	if err == nil {
		return true
	}
	if os.Geteuid() != 0 && restrictedUserns() {
		for _, advice := range []string{"install fog-node as root", "apparmor_restrict_unprivileged_userns=0"} {
			if !strings.Contains(err.Error(), advice) {
				t.Fatalf("the refusal doesn't tell the owner what to do (%q): %v", advice, err)
			}
		}
		t.Logf("refused, as it should be here: %v", err)
		return false
	}
	t.Fatalf("confinement isn't working here: %v", err)
	return false
}

// confinedRunner is a Runner on this machine whose confinement is proven.
func confinedRunner(t *testing.T) *Runner {
	t.Helper()
	ff := needFFmpeg(t)
	r := &Runner{Fog: "http://127.0.0.1:1", FFmpeg: ff, Work: workDir(t), Log: quiet()}
	if !confinesHere(t, r) {
		t.Skip("this machine refuses to confine (see the log), so there is nothing to escape")
	}
	kind, _ := r.Confinement(context.Background())
	want := map[string]string{"darwin": ConfinedMac, "linux": ConfinedLinux}[runtime.GOOS]
	if kind != want {
		t.Fatalf("confinement %q, want %q", kind, want)
	}
	return r
}

// Where a user's fog-node can't confine (Ubuntu's restriction), every run
// is refused, saying what to do about it.
func TestRefusedConfinementSaysWhatToDo(t *testing.T) {
	ff := needFFmpeg(t)
	if runtime.GOOS != "linux" || os.Geteuid() == 0 || !restrictedUserns() {
		t.Skip("only a user's fog-node under Ubuntu's user namespace restriction is refused")
	}
	r := &Runner{Fog: "http://127.0.0.1:1", FFmpeg: ff, Work: workDir(t), Log: quiet()}
	if confinesHere(t, r) {
		// An AppArmor profile lets it: nothing to refuse.
		t.Skip("user namespaces are allowed to this binary")
	}
	r.Start(compute.NodeRun{ID: "r1", Server: testServer, Argv: []string{"ffmpeg", "-version"}, Upload: "{server}/fog/compute/x/{name}"})
	if e := waitExit(t, r, "r1"); e.Status != nil || !strings.Contains(e.Log, "can't confine ffmpeg") || !strings.Contains(e.Log, "install fog-node as root") {
		t.Fatalf("exit: %+v", e)
	}
}

// shell is a shell, run as if it were the run's ffmpeg: arbitrary code
// inside the confinement, which is what a compromised ffmpeg would be.
func shell(t *testing.T, r *Runner) *Runner {
	t.Helper()
	sh := "/bin/bash"
	if _, err := os.Stat(sh); err != nil {
		sh = "/bin/sh"
	}
	return &Runner{Fog: r.Fog, FFmpeg: sh, Work: r.Work, Log: quiet(), Unconfined: r.Unconfined}
}

func run(t *testing.T, r *Runner, args ...string) (int, string) {
	t.Helper()
	status, log, err := r.runConfined(context.Background(), "escape", args)
	if err != nil {
		t.Fatalf("%v: %v (%s)", args, err, log)
	}
	return status, log
}

func TestConfinedCodeCantReachPastItsRun(t *testing.T) {
	r := confinedRunner(t)
	sh := shell(t, r)
	free := shell(t, r)
	free.Unconfined = true

	// Secrets: one in the owner's real home, fog-node's own credential
	// beside its work directory, and a file elsewhere on disk.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	inHome, err := os.MkdirTemp(home, ".fog-node-confinement-test-")
	if err != nil {
		t.Skipf("can't write in the home directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(inHome) })
	secrets := map[string]string{
		"the owner's home":       filepath.Join(inHome, "secret.txt"),
		"fog-node's credential":  filepath.Join(filepath.Dir(r.Work), "config.json"),
		"a file outside the run": filepath.Join(t.TempDir(), "elsewhere.txt"),
	}
	for _, p := range secrets {
		if err := os.WriteFile(p, []byte("s3cret-"+filepath.Base(p)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for what, p := range secrets {
		script := `read l < "` + p + `"; echo "got:$l"`
		if _, log := run(t, free, "-c", script); !strings.Contains(log, "got:s3cret") {
			t.Fatalf("%s: unconfined, the test itself can't read it: %q", what, log)
		}
		if _, log := run(t, sh, "-c", script); strings.Contains(log, "s3cret") {
			t.Errorf("%s: confined code read it: %q", what, log)
		}
	}

	// Writing: only in its own directory.
	outside := filepath.Join(t.TempDir(), "written")
	run(t, sh, "-c", `echo x > "`+outside+`"`)
	if _, err := os.Stat(outside); err == nil {
		t.Error("confined code wrote outside its run")
	}
	if _, log := run(t, sh, "-c", `echo ok > mine.txt && read l < mine.txt && echo "inside:$l"`); !strings.Contains(log, "inside:ok") {
		t.Errorf("confined code can't use its own directory: %q", log)
	}

	// Another program: only ffmpeg runs.
	if status, log := run(t, sh, "-c", `/bin/cat /etc/hosts >/dev/null && echo ran-cat`); status == 0 || strings.Contains(log, "ran-cat") {
		t.Errorf("confined code ran another program: %d %q", status, log)
	}
}

// Other processes on the machine are invisible to a run: not listed, and
// their command lines and status unreadable, whoever runs them.
func TestConfinedCodeCantSeeOtherProcesses(t *testing.T) {
	r := confinedRunner(t)
	// A process with a recognisable command line, of fog-node's own user.
	canary := exec.Command("/bin/sh", "-c", "sleep 30; : fog-node-canary")
	if err := canary.Start(); err != nil {
		t.Fatal(err)
	}
	defer canary.Process.Kill()
	pid := strconv.Itoa(canary.Process.Pid)
	// This test binary is the "ffmpeg", copied where a run's user can
	// execute it.
	self := filepath.Join(filepath.Dir(filepath.Dir(r.Work)), "looker")
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	look := &Runner{Fog: r.Fog, FFmpeg: self, Work: r.Work, Log: quiet()}
	free := &Runner{Fog: r.Fog, FFmpeg: self, Work: r.Work, Log: quiet(), Unconfined: true}
	time.Sleep(100 * time.Millisecond)
	if _, log := run(t, free, listProcessesArg, pid); !strings.Contains(log, "listed:true args:true") {
		t.Fatalf("unconfined, the test itself can't see it: %q", log)
	}
	if _, log := run(t, look, listProcessesArg, pid); !strings.Contains(log, "listed:false args:false") {
		t.Fatalf("a confined run saw another process: %q", log)
	}
}

// The network: nothing but the run's data proxy. A server on this machine's
// own loopback, one on its LAN address, and a filesystem socket (Docker's
// would be one) are all out of reach.
func TestConfinedFFmpegCantReachTheNetwork(t *testing.T) {
	r := confinedRunner(t)
	free := &Runner{Fog: r.Fog, FFmpeg: r.FFmpeg, Work: r.Work, Log: quiet(), Unconfined: true}
	wav := silence()
	var hits atomic.Int32
	serve := func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write(wav)
	}
	targets := map[string]string{}
	loop := httptest.NewServer(http.HandlerFunc(serve))
	defer loop.Close()
	targets["this machine's loopback"] = loop.URL + "/x.wav"
	if lan := lanIP(); lan != "" {
		if ln, err := net.Listen("tcp", net.JoinHostPort(lan, "0")); err == nil {
			go http.Serve(ln, http.HandlerFunc(serve))
			defer ln.Close()
			targets["its LAN address"] = "http://" + ln.Addr().String() + "/x.wav"
		}
	}
	for what, u := range targets {
		args := []string{"-v", "error", "-i", u, "-f", "null", "-"}
		// ffmpeg as anyone runs it, outside fog-node: it reaches them.
		if out, err := exec.Command(r.FFmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: the test itself can't reach it: %v %s", what, err, out)
		}
		before := hits.Load()
		if status, log := run(t, r, args...); status == 0 || hits.Load() != before {
			t.Errorf("%s: confined ffmpeg reached it: %d %q", what, status, log)
		}
	}

	sockDir, err := os.MkdirTemp("/tmp", "fn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "s")
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	var unixHits atomic.Int32
	go func() {
		for {
			c, err := ul.Accept()
			if err != nil {
				return
			}
			unixHits.Add(1)
			c.Close()
		}
	}()
	run(t, free, "-v", "error", "-i", "unix:"+sock, "-f", "null", "-")
	time.Sleep(100 * time.Millisecond)
	if unixHits.Load() == 0 {
		t.Fatal("unconfined, the test itself can't reach the socket")
	}
	unixHits.Store(0)
	run(t, r, "-v", "error", "-i", "unix:"+sock, "-f", "null", "-")
	time.Sleep(100 * time.Millisecond)
	if unixHits.Load() != 0 {
		t.Error("confined ffmpeg connected to a filesystem socket")
	}
}

func lanIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

// The proof is a real one: a helper that starts ffmpeg without confining
// it (as a sandbox that silently did nothing would) is caught.
func TestAConfinementThatDoesntConfineIsCaught(t *testing.T) {
	ff := needFFmpeg(t)
	if !confinesHere(t, &Runner{Fog: "http://127.0.0.1:1", FFmpeg: ff, Work: workDir(t), Log: quiet()}) {
		t.Skip("this machine refuses to confine, so there is no proof to fool")
	}
	r := &Runner{Fog: "http://127.0.0.1:1", FFmpeg: ff, Work: workDir(t), Log: quiet(),
		Self: []string{os.Args[0], fakeConfineArg}}
	if kind, err := r.Confinement(context.Background()); kind != "" || err == nil || !strings.Contains(err.Error(), "isn't confined") {
		t.Fatalf("a helper that doesn't confine passed: %q %v", kind, err)
	}
}

// A machine that can't confine refuses every run, unless its owner turned
// confinement off; and says why in its inventory.
func TestNoConfinementNoRuns(t *testing.T) {
	ff := needFFmpeg(t)
	r := &Runner{Fog: "http://127.0.0.1:1", FFmpeg: ff, Work: workDir(t), Log: quiet(),
		Self: []string{"/usr/bin/false"}}
	v := &compute.VideoCaps{Family: "videotoolbox"}
	r.Video(context.Background(), v)
	if v.Confinement != "" || v.ConfinementError == "" || v.Unconfined {
		t.Fatalf("inventory: %+v", v)
	}
	r.Start(compute.NodeRun{ID: "r1", Server: testServer, Argv: []string{"ffmpeg", "-version"},
		Upload: "{server}/fog/compute/x/{name}"})
	if e := waitExit(t, r, "r1"); e.Status != nil || !strings.Contains(e.Log, "can't confine ffmpeg") {
		t.Fatalf("exit: %+v", e)
	}

	r.Unconfined = true
	r.Self = nil
	v = &compute.VideoCaps{Family: "videotoolbox"}
	r.Video(context.Background(), v)
	if v.Confinement != "" || !v.Unconfined || v.ConfinementError != "" || v.Limits == nil {
		t.Fatalf("unconfined inventory: %+v", v)
	}
}

// A run is held to its limits on the machine: its time, which fog may
// lower, and its scratch space, in one file or many.
func TestRunLimitsEndARun(t *testing.T) {
	ff := needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	if out, err := exec.Command(ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2", "-c:v", "libx264",
		"-g", "25", src).CombinedOutput(); err != nil {
		t.Skipf("can't make a source: %v %s", err, out)
	}
	app := &appStub{source: src}
	srv := app.server(t)
	defer srv.Close()
	r := &Runner{Fog: srv.URL, FFmpeg: ff, Work: workDir(t), Log: quiet(), Limits: compute.RunLimits{ScratchBytes: 1 << 20}}
	// Limits hold confined or not.
	r.Unconfined = !confinesHere(t, r)

	// fog lowers the time to a second.
	start := time.Now()
	r.Start(compute.NodeRun{ID: "slow", Server: testServer, Limits: &compute.RunLimits{Seconds: 1},
		Argv:    []string{"ffmpeg", "-v", "error", "-re", "-stream_loop", "-1", "-i", "{server}/fog/compute/s/source", "-f", "null", "-"},
		Outputs: compute.Outputs{Final: []string{"x"}}, Upload: "{server}/fog/compute/files/{name}"})
	if e := waitExit(t, r, "slow"); e.Status != nil || !strings.Contains(e.Log, "time limit of 1s") {
		t.Fatalf("time limit: %+v", e)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("ended only after %v", took)
	}

	// One file past the scratch limit.
	r.Start(compute.NodeRun{ID: "big", Server: testServer,
		Argv:    []string{"ffmpeg", "-v", "error", "-stream_loop", "-1", "-i", "{server}/fog/compute/s/source", "-c", "copy", "-f", "mpegts", "{out}/big.ts"},
		Outputs: compute.Outputs{Final: []string{"big.ts"}}, Upload: "{server}/fog/compute/files/{name}"})
	if e := waitExit(t, r, "big"); e.Status != nil || !strings.Contains(e.Log, "scratch limit of 1 MiB") {
		t.Fatalf("one big file: %+v", e)
	}
	// Many files, each small.
	r.Start(compute.NodeRun{ID: "many", Server: testServer,
		Argv: []string{"ffmpeg", "-v", "error", "-stream_loop", "-1", "-i", "{server}/fog/compute/s/source", "-c", "copy", "-f", "segment",
			"-segment_time", "0.04", "{out}/seg-%05d.ts"},
		Outputs: compute.Outputs{Final: []string{"x"}}, Upload: "{server}/fog/compute/files/{name}"})
	if e := waitExit(t, r, "many"); e.Status != nil || !strings.Contains(e.Log, "scratch limit of 1 MiB") {
		t.Fatalf("many files: %+v", e)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.uploaded) != 0 {
		t.Fatalf("a run over its limits uploaded %v", app.uploaded)
	}
}

// What a run wrote is read only inside its directory, and only as plain
// files: a symlink it made can't lead fog-node to upload the owner's
// files, and a FIFO can't hold it up.
func TestFogNodeReadsOnlyARunsOwnFiles(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("s3cret"), 0o600)
	os.Symlink(secret, filepath.Join(dir, "init.mp4"))
	os.Symlink("../../../../../../../../"+secret, filepath.Join(dir, "rel.mp4"))
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.mp4"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ok.mp4"), []byte("data"), 0o600)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, name := range []string{"init.mp4", "rel.mp4", "fifo.mp4", "../secret"} {
			if f, err := openRegular(root, name); err == nil {
				f.Close()
				t.Errorf("%s opened", name)
			}
		}
		if f, err := openRegular(root, "ok.mp4"); err != nil {
			t.Errorf("its own file: %v", err)
		} else {
			f.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO held fog-node up")
	}
}
