//go:build linux && (amd64 || arm64)

package computenode

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// runFilter runs a seccomp filter over one syscall, as the kernel would.
func runFilter(t *testing.T, prog []unix.SockFilter, arch uint32, nr int, arg0 uint64) uint32 {
	t.Helper()
	return runFilterArgs(t, prog, arch, nr, arg0, 0)
}

func runFilterArgs(t *testing.T, prog []unix.SockFilter, arch uint32, nr int, arg0, arg1 uint64) uint32 {
	t.Helper()
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[0:], uint32(nr))
	binary.LittleEndian.PutUint32(data[4:], arch)
	binary.LittleEndian.PutUint64(data[16:], arg0)
	binary.LittleEndian.PutUint64(data[24:], arg1)
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			a = binary.LittleEndian.Uint32(data[in.K:])
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			if a >= in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if a&in.K != 0 {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return in.K
		default:
			t.Fatalf("instruction %d: code %#x", pc, in.Code)
		}
	}
	t.Fatal("the filter fell off its end")
	return 0
}

func TestSeccompFilter(t *testing.T) {
	prog := seccompFilter()
	if len(prog) > 4096 {
		t.Fatalf("%d instructions", len(prog))
	}
	allow := uint32(unix.SECCOMP_RET_ALLOW)
	eperm := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	for _, c := range []struct {
		name string
		nr   int
		arg0 uint64
		want uint32
	}{
		{"read", unix.SYS_READ, 0, allow},
		{"openat", unix.SYS_OPENAT, 0, allow},
		{"an IP socket", unix.SYS_SOCKET, unix.AF_INET, allow},
		{"an IPv6 socket", unix.SYS_SOCKET, unix.AF_INET6, allow},
		{"a unix socket", unix.SYS_SOCKET, unix.AF_UNIX, unix.SECCOMP_RET_ERRNO | uint32(unix.EAFNOSUPPORT)},
		{"a vsock", unix.SYS_SOCKET, unix.AF_VSOCK, unix.SECCOMP_RET_ERRNO | uint32(unix.EAFNOSUPPORT)},
		{"a packet socket", unix.SYS_SOCKET, unix.AF_PACKET, unix.SECCOMP_RET_ERRNO | uint32(unix.EAFNOSUPPORT)},
		{"a thread", unix.SYS_CLONE, unix.CLONE_VM | unix.CLONE_THREAD | unix.CLONE_SIGHAND, allow},
		{"a user namespace", unix.SYS_CLONE, unix.CLONE_NEWUSER, eperm},
		{"a network namespace", unix.SYS_CLONE, unix.CLONE_NEWNET | unix.CLONE_VM, eperm},
		{"clone3", unix.SYS_CLONE3, 0, unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		{"ptrace", unix.SYS_PTRACE, 0, eperm},
		{"unshare", unix.SYS_UNSHARE, 0, eperm},
		{"mount", unix.SYS_MOUNT, 0, eperm},
		{"bpf", unix.SYS_BPF, 0, eperm},
		{"io_uring", unix.SYS_IO_URING_SETUP, 0, eperm},
		{"keyctl", unix.SYS_KEYCTL, 0, eperm},
		{"execve (Landlock decides)", unix.SYS_EXECVE, 0, allow},
		{"fchmodat", unix.SYS_FCHMODAT, 0, eperm},
		{"fchmod", unix.SYS_FCHMOD, 0, eperm},
		{"fchownat", unix.SYS_FCHOWNAT, 0, eperm},
		{"setxattr", unix.SYS_SETXATTR, 0, eperm},
		{"fremovexattr", unix.SYS_FREMOVEXATTR, 0, eperm},
		{"utimensat", unix.SYS_UTIMENSAT, 0, eperm},
		{"truncate by path", unix.SYS_TRUNCATE, 0, eperm},
		{"ftruncate its own file", unix.SYS_FTRUNCATE, 0, allow},
		{"renameat2 (Landlock decides)", unix.SYS_RENAMEAT2, 0, allow},
	} {
		if got := runFilter(t, prog, seccompArch, c.nr, c.arg0); got != c.want {
			t.Errorf("%s: %#x, want %#x", c.name, got, c.want)
		}
	}
	if got := runFilterArgs(t, prog, seccompArch, unix.SYS_IOCTL, 3, unix.FS_IOC_SETFLAGS); got != eperm {
		t.Errorf("chattr: %#x", got)
	}
	if got := runFilterArgs(t, prog, seccompArch, unix.SYS_IOCTL, 3, fsIocFssetxattr); got != eperm {
		t.Errorf("FS_IOC_FSSETXATTR: %#x", got)
	}
	if got := runFilterArgs(t, prog, seccompArch, unix.SYS_IOCTL, 3, unix.TCGETS); got != allow {
		t.Errorf("another ioctl: %#x", got)
	}
	if got := runFilter(t, prog, 0x40000003, unix.SYS_READ, 0); got != unix.SECCOMP_RET_KILL_PROCESS {
		t.Errorf("another architecture: %#x", got)
	}
	if seccompX32 {
		if got := runFilter(t, prog, seccompArch, 0x40000000|unix.SYS_PTRACE, 0); got != eperm {
			t.Errorf("an x32 call: %#x", got)
		}
	}
}

// No rule reaches the owner's home or fog-node's own directory, even for
// an ffmpeg installed there: it gets only itself.
func TestLandlockRulesLeaveOutTheHome(t *testing.T) {
	home := "/home/me"
	node := "/home/me/.local/share/fog-node"
	j := &jail{Scratch: node + "/work/r1", FFmpeg: home + "/bin/ffmpeg", Hidden: []string{home, node}}
	for _, rule := range landlockRules(j) {
		switch {
		case rule.path == j.Scratch, rule.path == j.FFmpeg:
		case withinDir(rule.path, home) || withinDir(home, rule.path) && rule.path != "/":
			t.Errorf("a rule on %s", rule.path)
		}
		if rule.access&unix.LANDLOCK_ACCESS_FS_EXECUTE != 0 && rule.path != j.FFmpeg && !strings.Contains(rule.path, "ld-") {
			t.Errorf("%s may be executed", rule.path)
		}
		if rule.access&(unix.LANDLOCK_ACCESS_FS_WRITE_FILE|unix.LANDLOCK_ACCESS_FS_MAKE_REG) != 0 &&
			rule.path != j.Scratch && !strings.HasPrefix(rule.path, "/dev/") {
			t.Errorf("%s may be written", rule.path)
		}
	}
	if got := ffmpegInstall("/home/linuxbrew/.linuxbrew/bin/ffmpeg", []string{home, node}); len(got) != 1 || got[0] != "/home/linuxbrew/.linuxbrew" {
		t.Errorf("Linuxbrew: %v", got)
	}
	if got := ffmpegInstall("/usr/bin/ffmpeg", nil); len(got) != 0 {
		t.Errorf("the system's: %v", got)
	}
}

func TestInterpreterOfADynamicBinary(t *testing.T) {
	sh, _ := filepath.EvalSymlinks("/bin/sh")
	interp := interpreter(sh)
	if interp == "" {
		t.Skip("a static /bin/sh")
	}
	if _, err := os.Stat(interp); err != nil {
		t.Fatalf("interpreter %q: %v", interp, err)
	}
}

// fakeHandOver is the fake helper's handing over of the data socket.
func fakeHandOver(j *jail) {
	loopbackUp()
	handOverListener(j.Port)
}

// The ffmpeg a run starts has no privilege left: under a root fog-node it
// is nobody, and either way it has no capabilities, can't gain any, and is
// under seccomp.
func TestConfinedFFmpegHasNoPrivileges(t *testing.T) {
	r := confinedRunner(t)
	sh := shell(t, r)
	_, log := run(t, sh, "-c", `s=""; while read k v rest; do case $k in Uid:|Gid:|CapInh:|CapPrm:|CapEff:|CapAmb:|NoNewPrivs:|Seccomp:) s="$s $k$v";; esac; done < /proc/self/status; echo "status:$s"`)
	i := strings.Index(log, "status:")
	if i < 0 {
		t.Fatalf("no status: %q", log)
	}
	got := map[string]string{}
	for _, f := range strings.Fields(log[i+len("status:"):]) {
		k, v, _ := strings.Cut(f, ":")
		got[k] = v
	}
	uid := strconv.Itoa(os.Getuid())
	if os.Getuid() == 0 {
		uid = strconv.Itoa(sandboxID)
	}
	want := map[string]string{"Uid": uid, "CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000",
		"CapAmb": "0000000000000000", "NoNewPrivs": "1", "Seccomp": "2"}
	if os.Getuid() == 0 {
		want["Gid"] = strconv.Itoa(sandboxID)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q (%v)", k, got[k], v, got)
		}
	}
}

// lookAt reports whether pid shows in /proc, and whether its command line
// or status can be read.
func lookAt(pid string) (listed, args bool) {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		listed = listed || e.Name() == pid
	}
	cmdline, err1 := os.ReadFile("/proc/" + pid + "/cmdline")
	_, err2 := os.ReadFile("/proc/" + pid + "/status")
	return listed, (err1 == nil && strings.Contains(string(cmdline), "fog-node-canary")) || err2 == nil
}

// meddle tries to change a file every way but writing it, and says which
// worked.
func meddle(path string) string {
	var out []string
	try := func(what string, err error) {
		if err == nil {
			out = append(out, what+":changed")
		} else {
			out = append(out, what+":refused")
		}
	}
	try("chmod", os.Chmod(path, 0o666))
	try("chown", os.Chown(path, os.Getuid(), os.Getgid()))
	try("setxattr", unix.Setxattr(path, "user.fog", []byte("x"), 0))
	try("removexattr", unix.Removexattr(path, "user.nothing"))
	try("utimes", os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)))
	try("truncate", os.Truncate(path, 0))
	if f, err := os.Open(path); err == nil {
		try("fchmod", f.Chmod(0o666))
		try("chattr", unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, 0))
		f.Close()
	} else {
		out = append(out, "open:refused")
	}
	return strings.Join(out, " ")
}

// Confined code can't change a file it can't write: not its mode, owner,
// extended attributes, times, flags or length. As a user, fog-node's own
// config is the file an attacker would want (it holds the credential).
func TestConfinedCodeCantChangeFilesOutsideItsRun(t *testing.T) {
	r := confinedRunner(t)
	target := filepath.Join(filepath.Dir(r.Work), "config.json")
	if err := os.WriteFile(target, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		// Under a root fog-node ffmpeg is nobody: make the file nobody's,
		// so that only the confinement, not its owner, stands in the way.
		os.Chown(target, sandboxID, sandboxID)
	}
	self := filepath.Join(filepath.Dir(filepath.Dir(r.Work)), "meddler")
	raw, _ := os.ReadFile(os.Args[0])
	os.WriteFile(self, raw, 0o755)
	free := &Runner{Fog: r.Fog, FFmpeg: self, Work: r.Work, Log: quiet(), Unconfined: true}
	confined := &Runner{Fog: r.Fog, FFmpeg: self, Work: r.Work, Log: quiet()}
	_, log := run(t, confined, meddleArg, target)
	if strings.Contains(log, ":changed") || !strings.Contains(log, "truncate:") {
		t.Fatalf("confined code changed the file: %q", log)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o600 || info.Size() == 0 || info.ModTime().Before(time.Unix(2, 0)) {
		t.Fatalf("the file changed: %v %d %v", info.Mode(), info.Size(), info.ModTime())
	}
	// Unconfined, the same code can (so the refusals are the
	// confinement's).
	if _, log := run(t, free, meddleArg, target); !strings.Contains(log, "chmod:changed") {
		t.Fatalf("unconfined: %q", log)
	}
}
