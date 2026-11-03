package computenode

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

// Confinement: the command lines a machine runs come from fog on behalf of
// apps, so fog-node assumes either could be hostile, and checking an argv
// (argv.go) is only the first line. Each run's ffmpeg runs confined:
//
//   - it reads the system's libraries and its own install, and writes only
//     its run's scratch directory; the owner's home, fog-node's own
//     directory and its credential are out of reach;
//   - it reaches the network only through the run's data proxy on loopback,
//     which forwards to fog's data route for that run and nowhere else
//     (dataproxy.go);
//   - it runs within a time and a scratch-space limit (limits.go), with a
//     per-file size limit and no core dumps.
//
// On a Mac that is sandbox-exec with a deny-by-default profile; on Linux,
// Landlock, unprivileged namespaces and seccomp (confine_linux.go). Either
// is proven when fog-node starts by a run that tries to read past it. When
// confinement isn't available the machine lends no video, unless its owner
// has said, loudly, to do without it (fog-node unconfined on), which fog's
// Settings › Compute then shows.

// ConfineArg is the hidden fog-node command a run's ffmpeg starts through:
// fog-node re-runs itself as the confining helper, which sets the run's
// limits and confinement and then becomes ffmpeg.
const ConfineArg = "__confine"

// Confinement kinds, as the inventory reports them.
const (
	ConfinedMac   = "sandbox-exec"
	ConfinedLinux = "landlock"
)

// jail is one run's confinement, handed to the helper as JSON.
type jail struct {
	// Confine false runs ffmpeg with limits but unconfined: the owner's
	// choice.
	Confine bool `json:"confine"`
	// Scratch is the run's directory, the only place it may write.
	Scratch string `json:"scratch"`
	// FFmpeg is the node's ffmpeg, symlinks resolved; Argv[0] is it.
	FFmpeg string   `json:"ffmpeg"`
	Argv   []string `json:"argv"`
	// Port is the run's data proxy on loopback, the only address it may
	// reach.
	Port int `json:"port"`
	// FileBytes is the largest file it may write (RLIMIT_FSIZE).
	FileBytes int64 `json:"fileBytes"`
	// Hidden are directories it must never read even if a rule above would
	// let it: the owner's home and fog-node's own directory.
	Hidden []string `json:"hidden"`
	// Env is its whole environment. It isn't in the helper's argument (it
	// holds the run's proxy secret): the helper is started with it, and
	// hands it on.
	Env []string `json:"-"`
	// RunAs is who ffmpeg becomes when fog-node runs as root (Linux).
	RunAs *runAs `json:"runAs,omitempty"`
}

// ConfineMain is the helper: it confines itself as j says and becomes
// ffmpeg. It returns only on failure, and then the run must not go ahead.
func ConfineMain(args []string) error {
	if len(args) != 1 {
		return errors.New("fog-node " + ConfineArg + " is fog-node's own; it takes one argument")
	}
	var j jail
	if err := json.Unmarshal([]byte(args[0]), &j); err != nil {
		return err
	}
	if len(j.Argv) == 0 || j.Argv[0] != j.FFmpeg || !filepath.IsAbs(j.FFmpeg) || !filepath.IsAbs(j.Scratch) {
		return errors.New("a confinement that names no ffmpeg or scratch directory")
	}
	return confine(&j)
}

// helperCmd is how a run's helper is started: this binary again, with the
// hidden command.
func (r *Runner) helperCmd(j *jail) (*exec.Cmd, error) {
	raw, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	self := r.Self
	if len(self) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		self = []string{exe}
	}
	argv := append(append([]string{}, self[1:]...), ConfineArg, string(raw))
	return exec.Command(self[0], argv...), nil
}

// runEnv is everything a run's ffmpeg is told: nothing of the node's own
// environment but what a VA-API driver needs found, and its data proxy as
// its HTTP proxy, with the run's secret (proxySecret). A process's
// environment is readable only by its own user (and root), unlike its
// arguments, so no other account on the machine can use the proxy.
func runEnv(scratch string, port int, secret string) []string {
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + scratch, "TMPDIR=" + scratch, "LANG=C",
		"http_proxy=http://" + proxyUser + ":" + secret + "@127.0.0.1:" + strconv.Itoa(port)}
	for _, k := range []string{"LIBVA_DRIVER_NAME", "LIBVA_DRIVERS_PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// hidden is what a run must never read: the owner's home and fog-node's
// own directory (where its credential is).
func hidden(nodeDir string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, realPath(home))
	}
	if nodeDir != "" {
		out = append(out, realPath(nodeDir))
	}
	return out
}

// realPath resolves symlinks as far as the path exists (a sandbox rules on
// real paths: /var/folders is /private/var/folders on a Mac).
func realPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// within reports whether p is dir or under it.
func withinDir(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// Confinement answers how this machine confines a run, proven by trying:
// one confined ffmpeg must start, and one must fail to read a file it can
// read unconfined. "" with the reason when it can't. The owner's opt-out
// skips the proof and answers "".
func (r *Runner) Confinement(ctx context.Context) (string, error) {
	kind, err := r.proveConfinement(ctx)
	r.mu.Lock()
	r.checked, r.confined, r.confineErr = true, kind, err
	r.mu.Unlock()
	return kind, err
}

func (r *Runner) proveConfinement(ctx context.Context) (string, error) {
	if r.Unconfined {
		return "", errors.New("its owner turned confinement off")
	}
	kind, err := confinementKind()
	if err != nil {
		return "", err
	}
	if r.FFmpeg == "" {
		return "", errors.New("no ffmpeg")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	base := filepath.Dir(filepath.Clean(r.Work))
	if err := os.MkdirAll(r.Work, 0o700); err != nil {
		return "", err
	}
	// The canary is a real, readable sound file outside the run: ffmpeg
	// unconfined reads it cleanly, so a confined one that does is not
	// confined.
	canary := filepath.Join(base, "confinement-canary.wav")
	if err := os.WriteFile(canary, silence(), 0o644); err != nil {
		return "", err
	}
	defer os.Remove(canary)
	if out, err := exec.CommandContext(ctx, r.FFmpeg, "-v", "error", "-i", canary, "-f", "null", "-").CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg can't read a sound file even unconfined: %v: %s", err, lastLine(out))
	}
	if status, log, err := r.runConfined(ctx, "confinement-check-1", []string{"-hide_banner", "-version"}); err != nil || status != 0 {
		return "", fmt.Errorf("a confined ffmpeg doesn't start: %v%s", errOr(err, status), withLog(log))
	}
	if status, log, err := r.runConfined(ctx, "confinement-check-2", []string{"-v", "error", "-i", canary, "-f", "null", "-"}); err != nil || status == 0 {
		if err != nil {
			return "", fmt.Errorf("the confinement check failed: %v%s", err, withLog(log))
		}
		return "", errors.New("a confined ffmpeg could read a file outside its run, so it isn't confined")
	}
	return kind, nil
}

// runConfined runs ffmpeg with args through the helper in a scratch
// directory of its own, as a run would be, and answers its exit status.
func (r *Runner) runConfined(ctx context.Context, id string, args []string) (int, string, error) {
	dir := filepath.Join(realPath(r.Work), id)
	os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return 0, "", err
	}
	defer os.RemoveAll(dir)
	logName := filepath.Join(dir, "ffmpeg.log")
	logf, err := os.Create(logName)
	if err != nil {
		return 0, "", err
	}
	defer logf.Close()
	if err := prepareScratch(dir); err != nil {
		return 0, "", err
	}
	ln, port, err := r.dataListener()
	if err != nil {
		return 0, "", err
	}
	p, err := r.startConfined(dir, append([]string{realPath(r.FFmpeg)}, args...), logf, r.limits().ScratchBytes, ln, port, compute.NewNonce())
	if err != nil {
		if ln != nil {
			ln.Close()
		}
		return 0, logTailFile(logName), err
	}
	if p.listener != nil {
		p.listener.Close()
	}
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-ctx.Done():
		p.kill()
		<-done
		return 0, logTailFile(logName), ctx.Err()
	case <-done:
	}
	return p.cmd.ProcessState.ExitCode(), logTailFile(logName), nil
}

func errOr(err error, status int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("exit status %d", status)
}

func withLog(log string) string {
	if log = strings.TrimSpace(log); log != "" {
		return ": " + lastLine([]byte(log))
	}
	return ""
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// silence is a tenth of a second of 8 kHz mono PCM, as a WAV file.
func silence() []byte {
	const rate, samples = 8000, 800
	data := make([]byte, samples*2)
	b := make([]byte, 0, 44+len(data))
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, 1) // mono
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint32(b, rate*2)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	return append(b, data...)
}
