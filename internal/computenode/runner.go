package computenode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

// Runner runs the ffmpeg fog hands this machine: each run confined to its
// own scratch directory (confine.go), within its limits, its outputs
// uploaded as the run's Outputs say, and what became of it reported on the
// next poll. Nothing fog sends is trusted: a run that names anything
// outside itself is refused.
type Runner struct {
	// Fog is fogd's address; a run's Server is a path on it.
	Fog string
	// FFmpeg is the node's own ffmpeg; an app's argv names "ffmpeg".
	FFmpeg string
	// Work is where runs' scratch directories go, inside fog-node's own.
	Work string
	// Unconfined is the machine's owner running ffmpeg without confinement
	// (fog-node unconfined on). Limits still hold.
	Unconfined bool
	// Limits are the most one run may use; zero takes the defaults. fog
	// may ask for less, per run.
	Limits compute.RunLimits
	// Auth is this machine, as it MACs a run's requests on fog's data
	// route: its id and its credential's digest (they change when it
	// rotates). nil sends them unsigned (tests of a fog that doesn't ask).
	Auth func() (node, digest string)
	// Self starts the confining helper: this binary, by default. Tests
	// name theirs.
	Self []string
	Log  *slog.Logger
	// HTTP uploads outputs and carries the data proxy's requests to fog.
	HTTP *http.Client

	mu      sync.Mutex
	runs    map[string]*nodeRun
	exited  []compute.Exited
	changed chan struct{}
	// The last confinement check: its kind, or why there's none.
	checked    bool
	confined   string
	confineErr error
}

type nodeRun struct {
	spec    compute.NodeRun
	dir     string
	root    *os.Root
	cmd     *exec.Cmd
	proxy   *http.Server
	port    int
	limits  compute.RunLimits
	started time.Time
	killed  bool
	// why is the node's own reason for ending it, when it did.
	why     string
	stopped chan struct{} // closed when Stop asked
	// uploaded names files already sent, so init goes once.
	uploaded map[string]bool
}

// Defaults for a run's limits, and the space fog-node leaves free on the
// disk its runs write to, whatever they may use.
const (
	DefaultRunSeconds   = 12 * 60 * 60
	DefaultScratchBytes = 32 << 30
	diskFloor           = 2 << 30
)

// uploadGiveUp is how long a failing upload is retried before the run's
// upload is given up on.
const uploadGiveUp = 30 * time.Second

// checkEvery is how often a run's limits are looked at.
const checkEvery = time.Second

var (
	// runID is what fog-node accepts as a run's id: it names a directory.
	runID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// dataServer is a run's Server: its token on fog's data route.
	dataServer = regexp.MustCompile(`^` + regexp.QuoteMeta(compute.DataPath) + `[A-Za-z0-9_-]{16,128}$`)
)

func (r *Runner) init() {
	if r.runs == nil {
		r.runs = map[string]*nodeRun{}
		r.changed = make(chan struct{}, 1)
	}
}

func (r *Runner) http() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// limits are the node's own, defaults filled in.
func (r *Runner) limits() compute.RunLimits {
	l := r.Limits
	if l.Seconds <= 0 {
		l.Seconds = DefaultRunSeconds
	}
	if l.ScratchBytes <= 0 {
		l.ScratchBytes = DefaultScratchBytes
	}
	return l
}

// Changed is signalled whenever a run ends.
func (r *Runner) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	return r.changed
}

func (r *Runner) signal() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// Report answers what's running, what ended since last asked, and how far
// along each run is.
func (r *Runner) Report() ([]string, []compute.Exited, map[string]float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	running := []string{}
	progress := map[string]float64{}
	for id, run := range r.runs {
		running = append(running, id)
		if s, ok := progressSeconds(run.root); ok {
			progress[id] = s
		}
	}
	slices.Sort(running)
	exited := r.exited
	r.exited = nil
	return running, exited, progress
}

// Requeue hands back exits a poll failed to deliver.
func (r *Runner) Requeue(exited []compute.Exited) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exited = append(exited, r.exited...)
}

// Video is what the inventory says about how this machine runs video:
// its confinement, checked now, and its limits.
func (r *Runner) Video(ctx context.Context, v *compute.VideoCaps) {
	if v == nil {
		return
	}
	kind, err := r.Confinement(ctx)
	v.Confinement, v.Unconfined = kind, r.Unconfined
	if err != nil && !r.Unconfined {
		v.ConfinementError = err.Error()
	}
	l := r.limits()
	v.Limits = &l
}

// refuse reports a run that never started.
func (r *Runner) refuse(id, why string) {
	r.Log.Warn("run refused", "run", id, "why", why)
	r.mu.Lock()
	r.init()
	r.exited = append(r.exited, compute.Exited{ID: id, Log: "fog-node: " + why})
	r.mu.Unlock()
	r.signal()
}

// check refuses what fog sent that fog-node won't act on, before anything
// is made for it.
func check(spec compute.NodeRun) error {
	if !dataServer.MatchString(spec.Server) {
		return errors.New("the run's data route isn't fog's")
	}
	if !strings.HasPrefix(spec.Upload, "{server}"+compute.AppDataPrefix) {
		return fmt.Errorf("its uploads go somewhere other than {server}%s", compute.AppDataPrefix)
	}
	rest := strings.ReplaceAll(strings.TrimPrefix(spec.Upload, "{server}"), "{name}", "x")
	if strings.ContainsAny(rest, "{}?#\\") || cleanURLPath(rest) != rest {
		return errors.New("its upload URL is not a plain path on the data route")
	}
	out := spec.Outputs
	if out.Stream != "" {
		if strings.Contains(out.Stream, "/") || out.Stream == ".." || strings.HasPrefix(out.Stream, ".") {
			return fmt.Errorf("its stream outputs %q are not files in its own directory", out.Stream)
		}
		if _, err := filepath.Match(out.Stream, "x"); err != nil {
			return fmt.Errorf("its stream outputs %q are not a pattern", out.Stream)
		}
	}
	for _, name := range append(slices.Clone(out.Init), out.Final...) {
		if !filepath.IsLocal(name) || strings.ContainsAny(name, "\\*?[") {
			return fmt.Errorf("output %q is not a file in its own directory", name)
		}
	}
	if l := spec.Limits; l != nil && (l.Seconds < 0 || l.ScratchBytes < 0) {
		return errors.New("its limits are negative")
	}
	return nil
}

// cleanURLPath is p with dot segments resolved, for comparing: a path that
// changes was trying to climb.
func cleanURLPath(p string) string {
	u := url.URL{Path: p}
	return u.JoinPath().Path
}

// Start begins a run fog handed over.
func (r *Runner) Start(spec compute.NodeRun) {
	if !runID.MatchString(spec.ID) {
		// Not even reported under that id: it names no directory of ours.
		r.Log.Warn("run refused: an id fog-node won't use", "run", spec.ID)
		return
	}
	r.mu.Lock()
	r.init()
	_, dup := r.runs[spec.ID]
	checked := r.checked
	r.mu.Unlock()
	if dup {
		return
	}
	if r.FFmpeg == "" {
		r.refuse(spec.ID, "this machine has no ffmpeg")
		return
	}
	if !checked {
		r.Confinement(context.Background())
	}
	r.mu.Lock()
	confined, confineErr := r.confined, r.confineErr
	r.mu.Unlock()
	if confined == "" && !r.Unconfined {
		r.refuse(spec.ID, fmt.Sprintf("this machine can't confine ffmpeg, so it runs nothing (%v)", confineErr))
		return
	}
	if err := check(spec); err != nil {
		r.refuse(spec.ID, err.Error())
		return
	}
	limits := r.limits()
	if spec.Limits != nil {
		limits = limits.Lower(*spec.Limits)
	}
	if err := os.MkdirAll(r.Work, 0o700); err != nil {
		r.refuse(spec.ID, err.Error())
		return
	}
	dir := filepath.Join(realPath(r.Work), spec.ID)
	os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		r.refuse(spec.ID, err.Error())
		return
	}
	fail := func(err error) {
		os.RemoveAll(dir)
		r.refuse(spec.ID, err.Error())
	}
	ln, port, err := r.dataListener()
	if err != nil {
		fail(err)
		return
	}
	closeLn := func() {
		if ln != nil {
			ln.Close()
		}
	}
	ffmpeg := realPath(r.FFmpeg)
	// ffmpeg's {server} is the data proxy alone: the run's token stays in
	// this process, out of every argv.
	argv, err := Prepare(spec.Argv, ffmpeg, "http://127.0.0.1:"+strconv.Itoa(port), dir)
	if err != nil {
		closeLn()
		fail(err)
		return
	}
	logf, err := os.Create(filepath.Join(dir, "ffmpeg.log"))
	if err != nil {
		closeLn()
		fail(err)
		return
	}
	if err := prepareScratch(dir); err != nil {
		logf.Close()
		closeLn()
		fail(err)
		return
	}
	// The run's proxy secret: in ffmpeg's environment, never an argument.
	secret := compute.NewNonce()
	p, err := r.startConfined(dir, argv, logf, limits.ScratchBytes, ln, port, secret)
	logf.Close()
	if err != nil {
		closeLn()
		fail(fmt.Errorf("%v%s", err, withLog(logTailFile(filepath.Join(dir, "ffmpeg.log")))))
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		p.kill()
		p.cmd.Wait()
		p.listener.Close()
		fail(err)
		return
	}
	run := &nodeRun{spec: spec, dir: dir, root: root, cmd: p.cmd, limits: limits, started: time.Now(),
		stopped: make(chan struct{}), uploaded: map[string]bool{}}
	run.proxy = &http.Server{Handler: r.dataProxy(spec.Server, secret), ReadHeaderTimeout: dataProxyTimeout}
	run.port = port
	go run.proxy.Serve(p.listener)
	r.mu.Lock()
	r.runs[spec.ID] = run
	r.mu.Unlock()
	r.Log.Info("run started", "run", spec.ID, "class", spec.Class, "pid", p.cmd.Process.Pid,
		"confined", !r.Unconfined, "limit", time.Duration(limits.Seconds)*time.Second, "scratch", limits.ScratchBytes)
	go r.supervise(run, strings.TrimRight(r.Fog, "/")+spec.Server)
}

// dataListener is where a run's data proxy listens: made here, on a
// loopback port only the run's confinement lets it reach, or (on Linux)
// made by the helper inside the run's own network, at a known port.
func (r *Runner) dataListener() (net.Listener, int, error) {
	if !r.Unconfined && helperListens {
		return nil, helperPort, nil
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// proc is a run's process, started through the helper.
type proc struct {
	cmd      *exec.Cmd
	listener net.Listener
}

// kill ends the whole process group: the helper or ffmpeg it became.
func (p *proc) kill() {
	if p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// startConfined starts argv (its first element the node's ffmpeg) through
// the helper, confined to dir, writing to logf.
func (r *Runner) startConfined(dir string, argv []string, logf *os.File, fileBytes int64, ln net.Listener, port int, secret string) (*proc, error) {
	scratch := realPath(dir)
	j := &jail{Confine: !r.Unconfined, Scratch: scratch, FFmpeg: argv[0], Argv: argv, Port: port, FileBytes: fileBytes,
		Hidden: hidden(filepath.Dir(realPath(r.Work))), Env: runEnv(scratch, port, secret)}
	if j.Confine {
		j.RunAs = sandboxUser()
	}
	cmd, err := r.helperCmd(j)
	if err != nil {
		return nil, err
	}
	cmd.Dir = scratch
	cmd.Env = j.Env
	cmd.Stdout, cmd.Stderr = logf, logf
	p := &proc{cmd: cmd, listener: ln}
	if err := r.startHelper(cmd, j, p); err != nil {
		if cmd.Process != nil {
			p.kill()
			cmd.Wait()
		}
		return nil, err
	}
	return p, nil
}

// Stop kills a run at once: its uploads are let go, and it's reported
// stopped.
func (r *Runner) Stop(id string) { r.end(id, "") }

// end kills a run, with the node's reason when it's the node's doing.
func (r *Runner) end(id, why string) {
	r.mu.Lock()
	run := r.runs[id]
	if run != nil && !run.killed {
		run.killed, run.why = true, why
		close(run.stopped)
	}
	r.mu.Unlock()
	if run != nil {
		// The whole process group: the helper and the ffmpeg it became.
		syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// StopAll kills every run, and answers once they've all ended and their
// directories are gone, or ctx ends.
func (r *Runner) StopAll(ctx context.Context) {
	r.mu.Lock()
	r.init()
	ids := make([]string, 0, len(r.runs))
	for id := range r.runs {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.Stop(id)
	}
	for {
		r.mu.Lock()
		n := len(r.runs)
		r.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// supervise uploads a run's outputs as they appear, holds it to its
// limits, waits for it, sends what's left, and reports how it ended.
func (r *Runner) supervise(run *nodeRun, server string) {
	done := make(chan error, 1)
	go func() { done <- run.cmd.Wait() }()
	var uploadErr error
	ticker := time.NewTicker(50 * time.Millisecond)
	lastCheck := time.Now()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-ticker.C:
			if uploadErr == nil {
				uploadErr = r.uploadReady(run, server, false)
			}
			if time.Since(lastCheck) >= checkEvery {
				lastCheck = time.Now()
				if why := overLimit(run); why != "" {
					r.end(run.spec.ID, why)
				}
			}
		}
	}
	ticker.Stop()
	run.proxy.Close()
	r.mu.Lock()
	killed, why := run.killed, run.why
	r.mu.Unlock()
	var status *int
	if code := run.cmd.ProcessState.ExitCode(); code >= 0 && !killed {
		status = &code
	}
	// A file that reached the per-file limit ends ffmpeg one of two ways:
	// SIGXFSZ, or (where it is a namespace's first process, which ignores
	// that signal) a write failing "File too large" and ffmpeg giving up.
	// Either is the limit's doing, so it's reported as the node's, with no
	// status.
	ws, _ := run.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if why == "" && (ws.Signaled() && ws.Signal() == syscall.SIGXFSZ || status != nil && *status != 0 && fileAtLimit(run.dir, run.limits.ScratchBytes)) {
		why = fmt.Sprintf("it wrote past its scratch limit of %s", bytesString(run.limits.ScratchBytes))
		status = nil
	}
	if !killed && uploadErr == nil {
		uploadErr = r.uploadReady(run, server, true)
		if uploadErr == nil && status != nil && *status == 0 {
			uploadErr = r.uploadFinal(run, server)
		}
	}
	tail := logTail(run.root, "ffmpeg.log")
	if uploadErr != nil {
		tail = strings.TrimSpace("upload failed: " + uploadErr.Error() + "\n" + tail)
		if status != nil && *status == 0 {
			one := 1
			status = &one
		}
	}
	if why != "" {
		tail = strings.TrimSpace("fog-node: " + why + "\n" + tail)
	}
	run.root.Close()
	os.RemoveAll(run.dir)
	r.mu.Lock()
	delete(r.runs, run.spec.ID)
	r.exited = append(r.exited, compute.Exited{ID: run.spec.ID, Status: status, Log: tail})
	r.mu.Unlock()
	r.Log.Info("run ended", "run", run.spec.ID, "status", status, "killed", killed, "why", why)
	r.signal()
}

// overLimit says why a run must end now: past its time, its scratch
// space, or the free space fog-node keeps on the disk. "" while it's
// within them.
func overLimit(run *nodeRun) string {
	if l := run.limits.Seconds; l > 0 && time.Since(run.started) > time.Duration(l)*time.Second {
		return fmt.Sprintf("it ran past its time limit of %s", time.Duration(l)*time.Second)
	}
	if l := run.limits.ScratchBytes; l > 0 && dirBytes(run.dir) > l {
		return fmt.Sprintf("it wrote past its scratch limit of %s", bytesString(l))
	}
	if free, ok := freeBytes(run.dir); ok && free < diskFloor {
		return fmt.Sprintf("this machine's disk is down to %s free", bytesString(int64(free)))
	}
	return ""
}

// dirBytes is what a directory holds, symlinks not followed.
func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// fileAtLimit reports a file in dir that reached the per-file limit.
func fileAtLimit(dir string, limit int64) bool {
	found := false
	if limit <= 0 {
		return false
	}
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil && info.Size() >= limit {
				found = true
			}
		}
		return nil
	})
	return found
}

func freeBytes(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}

func bytesString(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', -1, 64) + " GiB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', -1, 64) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// uploadReady sends the stream files that exist, and the init files once
// a stream file exists or the run ended (atEnd). A failing upload is
// retried for uploadGiveUp before it's given up on.
func (r *Runner) uploadReady(run *nodeRun, server string, atEnd bool) error {
	out := run.spec.Outputs
	var ready []string
	if out.Stream != "" {
		entries, _ := os.ReadDir(run.dir)
		for _, e := range entries {
			if ok, _ := filepath.Match(out.Stream, e.Name()); ok && e.Type().IsRegular() {
				ready = append(ready, e.Name())
			}
		}
		sortNumeric(ready)
	}
	var inits []string
	for _, name := range out.Init {
		if !run.uploaded[name] && nonEmpty(run.root, name) && (len(ready) > 0 || atEnd) {
			inits = append(inits, name)
		}
	}
	if len(inits) < countPending(run, out.Init) && !atEnd {
		// Stream files wait for the init they need.
		ready = nil
	}
	for _, name := range append(inits, ready...) {
		if err := r.uploadRetrying(run, server, name); err != nil {
			return err
		}
		run.uploaded[name] = true
		if !slices.Contains(out.Init, name) {
			run.root.Remove(name)
		}
	}
	return nil
}

func countPending(run *nodeRun, inits []string) int {
	n := 0
	for _, name := range inits {
		if !run.uploaded[name] {
			n++
		}
	}
	return n
}

// uploadFinal sends a cleanly exited run's final files.
func (r *Runner) uploadFinal(run *nodeRun, server string) error {
	for _, name := range run.spec.Outputs.Final {
		if err := r.uploadRetrying(run, server, name); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) uploadRetrying(run *nodeRun, server, name string) error {
	deadline := time.Now().Add(uploadGiveUp)
	for {
		err := r.upload(run, server, name)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-run.stopped:
			return errors.New("stopped")
		case <-time.After(time.Second):
		}
	}
}

// openRegular opens a file a run wrote, inside its directory and nowhere
// else (a symlink it made can't lead out), and only if it's a plain file
// (a FIFO it made can't hold fog-node up).
func openRegular(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a file", name)
	}
	return f, nil
}

func (r *Runner) upload(run *nodeRun, server, name string) error {
	f, err := openRegular(run.root, name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// The digest the upload's MAC declares, and fog checks the bytes
	// against on the way to the app.
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, info.Size())); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	target := strings.ReplaceAll(strings.ReplaceAll(run.spec.Upload, "{server}", server), "{name}", url.PathEscape(name))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-run.stopped:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "PUT", target, io.LimitReader(f, info.Size()))
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	r.signData(req, sum)
	resp, err := r.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", name, resp.Status)
	}
	return nil
}

var digits = regexp.MustCompile(`\d+`)

// sortNumeric orders gop-9 before gop-10.
func sortNumeric(names []string) {
	slices.SortFunc(names, func(a, b string) int {
		na, _ := strconv.Atoi(digits.FindString(a))
		nb, _ := strconv.Atoi(digits.FindString(b))
		if na != nb {
			return na - nb
		}
		return strings.Compare(a, b)
	})
}

func nonEmpty(root *os.Root, name string) bool {
	f, err := openRegular(root, name)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	return err == nil && info.Size() > 0
}

// tailBytes is how much of a file fog-node reads from its end: a run's log
// or progress may grow without bound.
const tailBytes = 64 << 10

func readTail(root *os.Root, name string) []byte {
	if root == nil {
		return nil
	}
	f, err := openRegular(root, name)
	if err != nil {
		return nil
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > tailBytes {
		f.Seek(info.Size()-tailBytes, io.SeekStart)
	}
	raw, _ := io.ReadAll(io.LimitReader(f, tailBytes))
	return raw
}

var outTime = regexp.MustCompile(`(?m)^out_time_us=(\d+)`)

// progressSeconds reads ffmpeg's -progress file, when the run writes one.
func progressSeconds(root *os.Root) (float64, bool) {
	all := outTime.FindAllSubmatch(readTail(root, "progress"), -1)
	if len(all) == 0 {
		return 0, false
	}
	us, _ := strconv.ParseFloat(string(all[len(all)-1][1]), 64)
	return us / 1e6, true
}

// logTail is the end of what ffmpeg said.
func logTail(root *os.Root, name string) string {
	return lastLines(readTail(root, name))
}

// logTailFile is logTail for a run that never got its root.
func logTailFile(p string) string {
	root, err := os.OpenRoot(filepath.Dir(p))
	if err != nil {
		return ""
	}
	defer root.Close()
	return logTail(root, filepath.Base(p))
}

func lastLines(raw []byte) string {
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
