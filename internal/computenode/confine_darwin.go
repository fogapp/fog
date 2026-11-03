package computenode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// On a Mac a run's ffmpeg runs under sandbox-exec, with a profile that
// denies everything and then allows what a transcode needs: the system's
// own baseline (Apple's system.sb: its libraries and a few system
// services, no Apple Events, pasteboard or launchd jobs), reading its own
// install, reading and writing its run's directory, the media engine
// (through IOSurface), and one network address: the run's data proxy on
// loopback. Other processes it can neither list nor inspect.

const sandboxExec = "/usr/bin/sandbox-exec"

// helperListens is false: on a Mac the data proxy listens in fog-node, on
// the host's loopback. The run's profile lets ffmpeg reach that port and no
// other; other processes can reach it too, so the proxy serves only the
// run's secret, which ffmpeg holds in its environment (dataproxy.go).
const helperListens = false

const helperPort = 0

func confinementKind() (string, error) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return "", errors.New("macOS has no " + sandboxExec + " here")
	}
	if _, err := os.Stat("/System/Library/Sandbox/Profiles/system.sb"); err != nil {
		return "", errors.New("macOS has no system.sb sandbox profile here")
	}
	return ConfinedMac, nil
}

func prepareScratch(string) error { return nil }

func (r *Runner) startHelper(cmd *exec.Cmd, _ *jail, _ *proc) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Start()
}

// confine is the helper's half on a Mac: limits, then sandbox-exec, which
// becomes ffmpeg.
func confine(j *jail) error {
	if err := setLimits(j.FileBytes); err != nil {
		return err
	}
	if err := os.Chdir(j.Scratch); err != nil {
		return err
	}
	if !j.Confine {
		return syscall.Exec(j.FFmpeg, j.Argv, os.Environ())
	}
	profile, err := macProfile(j)
	if err != nil {
		return err
	}
	return syscall.Exec(sandboxExec, append([]string{"sandbox-exec", "-p", profile}, j.Argv...), os.Environ())
}

func setLimits(fileBytes int64) error {
	if fileBytes > 0 {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: uint64(fileBytes), Max: uint64(fileBytes)}); err != nil {
			return fmt.Errorf("limiting file size: %w", err)
		}
	}
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
}

// macProfile is the run's sandbox profile. Later rules win: what is hidden
// is denied after everything allowed, and the run's own directory (inside
// fog-node's) is allowed after that.
func macProfile(j *jail) (string, error) {
	q := func(s string) (string, error) {
		if strings.ContainsAny(s, "\x00\n\r") {
			return "", fmt.Errorf("a path the sandbox can't name: %q", s)
		}
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
	}
	ff, err := q(j.FFmpeg)
	if err != nil {
		return "", err
	}
	scratch, err := q(j.Scratch)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n(import \"system.sb\")\n")
	for _, dir := range ffmpegInstall(j.FFmpeg, j.Hidden) {
		d, err := q(dir)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", d)
	}
	for _, h := range j.Hidden {
		d, err := q(h)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %s))\n", d)
	}
	fmt.Fprintf(&b, "(allow process-exec (literal %s))\n(allow file-read* (literal %s))\n", ff, ff)
	fmt.Fprintf(&b, "(allow file-read-metadata (path-ancestors %s))\n", scratch)
	fmt.Fprintf(&b, "(allow file-read* file-write* (subpath %s))\n", scratch)
	b.WriteString("(allow iokit-open (iokit-user-client-class \"IOSurfaceRootUserClient\"))\n")
	// Other processes: not listed, not inspected, their arguments and
	// environments not read (system.sb lets every sysctl be read).
	b.WriteString("(deny sysctl-read (sysctl-name-prefix \"kern.proc\") (sysctl-name-prefix \"kern.procargs\"))\n")
	b.WriteString("(deny process-info*)\n(allow process-info* (target self))\n")
	if j.Port > 0 {
		fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%s\"))\n", strconv.Itoa(j.Port))
	}
	return b.String(), nil
}

// ffmpegInstall is what ffmpeg reads of its own install: under Homebrew,
// the kegs its libraries live in; elsewhere the lib directory beside its
// bin. Nothing inside a hidden directory (a static ffmpeg in the owner's
// home gets only itself).
func ffmpegInstall(ff string, hide []string) []string {
	var dirs []string
	if prefix, _, ok := strings.Cut(ff, "/Cellar/"); ok && prefix != "" {
		dirs = []string{prefix + "/Cellar", prefix + "/opt", prefix + "/lib"}
	} else {
		dirs = []string{filepath.Join(filepath.Dir(filepath.Dir(ff)), "lib")}
	}
	var out []string
	for _, d := range dirs {
		if d == "/" || d == "/lib" || isHidden(d, hide) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// isHidden reports whether d is a hidden directory, inside one, or holds
// one.
func isHidden(d string, hide []string) bool {
	for _, h := range hide {
		if withinDir(d, h) || withinDir(h, d) {
			return true
		}
	}
	return false
}

// runAs is Linux's: elsewhere ffmpeg runs as fog-node's own user.
type runAs struct{}

func sandboxUser() *runAs { return nil }

func restrictedUserns() bool { return false }
