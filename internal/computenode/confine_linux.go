//go:build linux && (amd64 || arm64)

package computenode

import (
	"debug/elf"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// On Linux a run's ffmpeg is confined with what the kernel offers an
// ordinary user, needing neither root nor any package:
//
//   - namespaces: its own network (holding only a loopback, where the run's
//     data proxy listens), PIDs, IPC and hostname; as a user that is not
//     root, inside a user namespace mapping only that user;
//   - Landlock: it reads the system's libraries and configuration and its
//     own install, executes only its own binary, and writes only its run's
//     directory; the owner's home, fog-node's directory and its credential
//     are outside every rule, so out of reach; it may connect only to the
//     data proxy's port;
//   - seccomp: no ptrace, mounts, namespaces, keyrings, BPF, io_uring or
//     kernel modules, and no sockets but IP and netlink (a filesystem
//     socket like Docker's is not a file Landlock rules on);
//   - no capabilities, no new privileges, no core dumps, a per-file size
//     limit.
//
// fog-node running as root confines the same way, and runs ffmpeg as
// nobody, with the groups that own the GPU's render nodes.

// helperListens is true: the data proxy's socket is made inside the run's
// own network namespace, by the helper, which hands it to fog-node.
const helperListens = true

// helperPort is where the data proxy listens inside the run's namespace;
// nothing else listens there.
const helperPort = 7719

// sandboxID is who ffmpeg runs as when fog-node runs as root.
const sandboxID = 65534

func confinementKind() (string, error) {
	if seccompArch == 0 {
		return "", fmt.Errorf("fog-node can't confine ffmpeg on %s", runtime.GOARCH)
	}
	abi, err := landlockABI()
	if err != nil {
		return "", fmt.Errorf("this kernel has no Landlock (it needs Linux 5.13 or later with landlock in its lsm= list): %v", err)
	}
	if abi < 1 {
		return "", errors.New("this kernel has no Landlock")
	}
	return ConfinedLinux, nil
}

// prepareScratch gives a run's directory to the user ffmpeg runs as, when
// that isn't fog-node's own, and lets it reach the directory: fog-node's
// work directory and its own are opened to passing through (never to
// listing), and any directory above them must already be, or the run is
// refused.
func prepareScratch(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	work := filepath.Dir(dir)
	for _, d := range []string{filepath.Dir(work), work} {
		info, err := os.Stat(d)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o001 == 0 {
			if err := os.Chmod(d, info.Mode().Perm()|0o111); err != nil {
				return err
			}
		}
	}
	for d := filepath.Dir(filepath.Dir(work)); ; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("ffmpeg runs as nobody and can't pass through %s to reach its run: keep fog-node's directory "+
				"somewhere every user can pass through (the default, /var/lib/fog-node, is)", d)
		}
		if d == "/" {
			break
		}
	}
	if err := os.Chown(dir, sandboxID, sandboxID); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, sandboxID, sandboxID)
	})
}

// startHelper starts the helper in its namespaces and, when it confines,
// takes the data proxy's socket it made there.
func (r *Runner) startHelper(cmd *exec.Cmd, j *jail, p *proc) error {
	sys := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.SysProcAttr = sys
	if !j.Confine {
		return cmd.Start()
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	ours := os.NewFile(uintptr(fds[0]), "confine")
	theirs := os.NewFile(uintptr(fds[1]), "confine-helper")
	defer ours.Close()
	cmd.ExtraFiles = []*os.File{theirs} // fd 3
	sys.Cloneflags = syscall.CLONE_NEWNET | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS
	uid, gid := os.Getuid(), os.Getgid()
	if uid != 0 {
		// As a user: a user namespace mapping only that user, with the
		// capabilities that bring up the loopback and mount the run's own
		// /proc, dropped before ffmpeg.
		sys.Cloneflags |= syscall.CLONE_NEWUSER
		sys.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
		sys.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
		sys.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true}
		sys.AmbientCaps = []uintptr{unix.CAP_NET_ADMIN, unix.CAP_SYS_ADMIN}
	}
	// As root the helper starts as root (fog-node's own binary need be
	// executable by no one else), and becomes nobody itself, last thing
	// before it becomes ffmpeg (j.RunAs).
	err = cmd.Start()
	theirs.Close()
	if err != nil {
		if uid != 0 {
			return fmt.Errorf("%w (%s)", err, usernsAdvice())
		}
		return err
	}
	ln, err := receiveListener(ours, 15*time.Second)
	if err != nil {
		return err
	}
	p.listener = ln
	return nil
}

// receiveListener takes the socket the helper made in the run's namespace.
func receiveListener(f *os.File, timeout time.Duration) (net.Listener, error) {
	c, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, errors.New("not a unix socket")
	}
	uc.SetReadDeadline(time.Now().Add(timeout))
	buf, oob := make([]byte, 1), make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("the confining helper didn't hand over its socket: %w", err)
	}
	if oobn == 0 {
		return nil, errors.New("the confining helper stopped before confining ffmpeg")
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return nil, errors.New("the confining helper sent something other than its socket")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return nil, errors.New("the confining helper sent something other than its socket")
	}
	lf := os.NewFile(uintptr(fds[0]), "run-data")
	defer lf.Close()
	return net.FileListener(lf)
}

// deviceGroups are the groups that own the GPU's device nodes, for ffmpeg
// run as nobody to open them.
func deviceGroups() []uint32 {
	var out []uint32
	entries, _ := filepath.Glob("/dev/dri/*")
	for _, e := range entries {
		var st unix.Stat_t
		if unix.Stat(e, &st) == nil && st.Gid != 0 {
			seen := false
			for _, g := range out {
				seen = seen || g == st.Gid
			}
			if !seen {
				out = append(out, st.Gid)
			}
		}
	}
	return out
}

// confine is the helper's half on Linux. It runs in the namespaces
// startHelper made, and returns only on failure: then ffmpeg never starts.
func confine(j *jail) error {
	// Landlock, capabilities and seccomp are per thread: everything happens
	// on this one, which then becomes ffmpeg.
	runtime.LockOSThread()
	if err := setLimits(j.FileBytes); err != nil {
		return err
	}
	if err := os.Chdir(j.Scratch); err != nil {
		return err
	}
	if !j.Confine {
		return syscall.Exec(j.FFmpeg, j.Argv, os.Environ())
	}
	if err := loopbackUp(); err != nil {
		if os.Getuid() != 0 && errors.Is(err, unix.EPERM) {
			return fmt.Errorf("bringing up the run's own loopback: %w: %s", err, usernsAdvice())
		}
		return fmt.Errorf("bringing up the run's own loopback: %w", err)
	}
	if err := ownProc(); err != nil {
		return fmt.Errorf("giving the run its own /proc: %w", err)
	}
	if err := handOverListener(j.Port); err != nil {
		return fmt.Errorf("handing over the run's data socket: %w", err)
	}
	if a := j.RunAs; a != nil {
		if err := becomeNobody(a); err != nil {
			return fmt.Errorf("dropping to uid %d: %w", a.UID, err)
		}
	}
	if err := dropCapabilities(); err != nil {
		return fmt.Errorf("dropping capabilities: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no new privileges: %w", err)
	}
	if err := landlockRestrict(j); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	if err := seccompRestrict(); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	return syscall.Exec(j.FFmpeg, j.Argv, os.Environ())
}

// usernsAdvice tells the owner how to let a user's fog-node confine.
func usernsAdvice() string {
	if userns := restrictedUserns(); userns {
		return "this machine restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns=1, " +
			"Ubuntu's default), which fog-node needs to confine ffmpeg when it runs as a user. Either install fog-node " +
			"as root (sudo: it then runs ffmpeg as nobody), or allow them with " +
			"sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 (and the same in /etc/sysctl.d to keep it)"
	}
	return "this machine doesn't let a user make the namespaces fog-node confines ffmpeg in " +
		"(see kernel.unprivileged_userns_clone and user.max_user_namespaces); install fog-node as root instead"
}

// restrictedUserns reports Ubuntu's AppArmor restriction on unprivileged
// user namespaces being on.
func restrictedUserns() bool {
	raw, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	return err == nil && len(raw) > 0 && raw[0] == '1'
}

// runAs is who ffmpeg runs as when fog-node is root.
type runAs struct {
	UID    int   `json:"uid"`
	GID    int   `json:"gid"`
	Groups []int `json:"groups,omitempty"`
}

// sandboxUser is ffmpeg's user under a root fog-node: nobody, with the
// groups that own the GPU's render nodes. nil under any other user.
func sandboxUser() *runAs {
	if os.Getuid() != 0 {
		return nil
	}
	a := &runAs{UID: sandboxID, GID: sandboxID}
	for _, g := range deviceGroups() {
		a.Groups = append(a.Groups, int(g))
	}
	return a
}

// becomeNobody drops root for good: groups, then group, then user, for
// every thread. Changing who it is clears the parent-death signal, so it
// is set again.
func becomeNobody(a *runAs) error {
	if err := syscall.Setgroups(a.Groups); err != nil {
		return err
	}
	if err := syscall.Setgid(a.GID); err != nil {
		return err
	}
	if err := syscall.Setuid(a.UID); err != nil {
		return err
	}
	if os.Getuid() != a.UID || os.Geteuid() != a.UID || os.Getgid() != a.GID {
		return errors.New("still root")
	}
	return unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0)
}

func setLimits(fileBytes int64) error {
	if fileBytes > 0 {
		if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: uint64(fileBytes), Max: uint64(fileBytes)}); err != nil {
			return fmt.Errorf("limiting file size: %w", err)
		}
	}
	return unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{})
}

// ownProc mounts, in the run's own mount namespace, a /proc for its own
// PID namespace: the only processes there are the run's. Mounts made here
// stay here.
func ownProc() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	return unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// handOverListener listens for the data proxy on the run's loopback and
// sends the socket to fog-node on fd 3, then closes both here.
func handOverListener(port int) error {
	defer unix.Close(3)
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	f, err := ln.(*net.TCPListener).File()
	ln.Close()
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Sendmsg(3, []byte{1}, unix.UnixRights(int(f.Fd())), nil, 0)
}

func dropCapabilities() error {
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return err
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	return unix.Capset(&hdr, &data[0])
}

// --- Landlock ----------------------------------------------------------------

func landlockABI() (int, error) {
	v, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0, e
	}
	return int(v), nil
}

const (
	llRead = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	// llFile is what a rule on a file (not a directory) may grant.
	llFile = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	llScratch = llRead | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REFER
	llDevice = llRead | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// landlockHandled is every filesystem right the kernel's Landlock knows:
// each is denied unless a rule grants it.
func landlockHandled(abi int) uint64 {
	all := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		all |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		all |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		all |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return all
}

type landlockRule struct {
	path     string
	access   uint64
	optional bool
}

// landlockRules is everything a run may touch on the filesystem.
func landlockRules(j *jail) []landlockRule {
	rules := []landlockRule{{path: j.Scratch, access: llScratch}, {path: j.FFmpeg, access: llRead | unix.LANDLOCK_ACCESS_FS_EXECUTE}}
	for _, p := range []string{"/usr", "/lib", "/lib64", "/lib32", "/libx32", "/bin", "/sbin", "/etc",
		"/nix/store", "/run/opengl-driver",
		// Of /proc (the run's own: only its processes are in it), itself
		// and what describes the machine; of /sys, what describes the CPUs
		// and finds the GPU (VA-API walks /sys/dev/char and /sys/class/drm
		// into /sys/devices).
		"/proc/self", "/proc/cpuinfo", "/proc/meminfo", "/proc/stat",
		"/sys/devices", "/sys/class/drm", "/sys/dev/char", "/sys/bus/pci"} {
		rules = append(rules, landlockRule{path: p, access: llRead, optional: true})
	}
	for _, p := range []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom"} {
		rules = append(rules, landlockRule{path: p, access: unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE, optional: true})
	}
	rules = append(rules, landlockRule{path: "/dev/dri", access: llDevice, optional: true})
	if interp := interpreter(j.FFmpeg); interp != "" {
		rules = append(rules, landlockRule{path: interp, access: llRead | unix.LANDLOCK_ACCESS_FS_EXECUTE})
	}
	for _, d := range ffmpegInstall(j.FFmpeg, j.Hidden) {
		rules = append(rules, landlockRule{path: d, access: llRead, optional: true})
	}
	return rules
}

// interpreter is the dynamic loader an ELF binary names, which the kernel
// executes along with it; "" for a static one.
func interpreter(bin string) string {
	f, err := elf.Open(bin)
	if err != nil {
		return ""
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			raw := make([]byte, p.Filesz)
			if _, err := p.ReadAt(raw, 0); err == nil {
				for i, c := range raw {
					if c == 0 {
						raw = raw[:i]
						break
					}
				}
				return string(raw)
			}
		}
	}
	return ""
}

// ffmpegInstall is the rest of ffmpeg's own install when it isn't the
// system's (Linuxbrew, /opt/ffmpeg, a build in /usr/local): the prefix its
// bin directory sits in. Not when that is the root or /usr (already read),
// and never a hidden directory or one that holds one: a static ffmpeg in
// the owner's home gets only itself.
func ffmpegInstall(ff string, hide []string) []string {
	prefix := filepath.Dir(filepath.Dir(ff))
	switch prefix {
	case "/", "/usr", ".":
		return nil
	}
	for _, h := range hide {
		if withinDir(prefix, h) || withinDir(h, prefix) {
			return nil
		}
	}
	return []string{prefix}
}

func landlockRestrict(j *jail) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	handled := landlockHandled(abi)
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	size := uintptr(8)
	if abi >= 4 {
		attr.Access_net = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
		size = 16
	}
	if abi >= 6 {
		attr.Scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
		size = 24
	}
	rfd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
	if e != 0 {
		return e
	}
	defer unix.Close(int(rfd))
	for _, rule := range landlockRules(j) {
		if err := landlockPath(rfd, rule.path, rule.access&handled); err != nil {
			if rule.optional && errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("%s: %w", rule.path, err)
		}
	}
	if abi >= 4 {
		port := landlockNetPort{allowed: unix.LANDLOCK_ACCESS_NET_CONNECT_TCP, port: uint64(j.Port)}
		if _, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, rfd, landlockRuleNetPort, uintptr(unsafe.Pointer(&port)), 0, 0, 0); e != 0 {
			return fmt.Errorf("port rule: %w", e)
		}
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, rfd, 0, 0); e != 0 {
		return e
	}
	return nil
}

// landlockRuleNetPort is LANDLOCK_RULE_NET_PORT (Landlock ABI 4).
const landlockRuleNetPort = 2

// landlockNetPort is struct landlock_net_port_attr.
type landlockNetPort struct {
	allowed uint64
	port    uint64
}

func landlockPath(rfd uintptr, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= llFile
	}
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, rfd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

// --- seccomp -----------------------------------------------------------------

// nsFlags are the clone flags that make a namespace.
const nsFlags = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER |
	unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWTIME

// deniedSyscalls are refused with EPERM: nothing a transcode needs, and
// each a way to reach past a sandbox or into the kernel.
func deniedSyscalls() []uintptr {
	return append([]uintptr{
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD,
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE,
		unix.SYS_FSOPEN, unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_FSCONFIG, unix.SYS_MOUNT_SETATTR,
		unix.SYS_UNSHARE, unix.SYS_SETNS,
		unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
		unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD, unix.SYS_FANOTIFY_INIT,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
		unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
		unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_REBOOT, unix.SYS_ACCT, unix.SYS_QUOTACTL,
		unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT, unix.SYS_SYSLOG, unix.SYS_VHANGUP,
		unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME, unix.SYS_ADJTIMEX, unix.SYS_CLOCK_ADJTIME,
		// What changes a file without the rights Landlock rules on: its
		// mode, owner, extended attributes, times, or (by path) length.
		// Truncating a file it has open for writing (only its own) stays
		// allowed. Links, renames and device nodes are Landlock's: they
		// need rights it grants only in the run's directory.
		unix.SYS_FCHMOD, unix.SYS_FCHMODAT, unix.SYS_FCHMODAT2,
		unix.SYS_FCHOWN, unix.SYS_FCHOWNAT,
		unix.SYS_SETXATTR, unix.SYS_LSETXATTR, unix.SYS_FSETXATTR, unix.SYS_SETXATTRAT,
		unix.SYS_REMOVEXATTR, unix.SYS_LREMOVEXATTR, unix.SYS_FREMOVEXATTR, unix.SYS_REMOVEXATTRAT,
		unix.SYS_UTIMENSAT, unix.SYS_TRUNCATE,
	}, archDeniedSyscalls...)
}

// deniedIoctls are ioctl commands refused with EPERM: a file's flags
// (immutable, append-only) and its extended attributes.
var deniedIoctls = []uint32{unix.FS_IOC_SETFLAGS, fsIocFssetxattr}

// fsIocFssetxattr is FS_IOC_FSSETXATTR, _IOW('X', 32, struct fsxattr).
const fsIocFssetxattr = 0x401c5820

// seccompFilter is the run's filter, as classic BPF.
func seccompFilter() []unix.SockFilter {
	const (
		ld   = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		jset = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
		// Offsets in struct seccomp_data: nr, arch, then the arguments; the
		// low half of the first on a little-endian machine.
		offNR, offArch, offArg0, offArg1 = 0, 4, 16, 24
	)
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jump := func(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: code, K: k, Jt: jt, Jf: jf}
	}
	errno := func(e syscall.Errno) uint32 { return unix.SECCOMP_RET_ERRNO | uint32(e) }
	allow := uint32(unix.SECCOMP_RET_ALLOW)

	p := []unix.SockFilter{
		stmt(ld, offArch),
		jump(jeq, seccompArch, 1, 0),
		stmt(ret, unix.SECCOMP_RET_KILL_PROCESS),
		stmt(ld, offNR),
	}
	if seccompX32 {
		// x32 numbers alias the 64-bit ones with a high bit set.
		p = append(p, jump(jge, 0x40000000, 0, 1), stmt(ret, errno(unix.EPERM)))
	}
	for _, nr := range deniedSyscalls() {
		p = append(p, jump(jeq, uint32(nr), 0, 1), stmt(ret, errno(unix.EPERM)))
	}
	// clone3's flags are behind a pointer seccomp can't follow: refused as
	// unknown, so the C library falls back to clone, whose flags it can.
	p = append(p, jump(jeq, unix.SYS_CLONE3, 0, 1), stmt(ret, errno(unix.ENOSYS)))
	p = append(p,
		jump(jeq, unix.SYS_SOCKET, 0, 6),
		stmt(ld, offArg0),
		jump(jeq, unix.AF_INET, 3, 0),
		jump(jeq, unix.AF_INET6, 2, 0),
		jump(jeq, unix.AF_NETLINK, 1, 0),
		stmt(ret, errno(unix.EAFNOSUPPORT)),
		stmt(ret, allow),
	)
	// ioctl: its command is the second argument.
	p = append(p, jump(jeq, unix.SYS_IOCTL, 0, uint8(len(deniedIoctls)*2+2)), stmt(ld, offArg1))
	for _, cmd := range deniedIoctls {
		p = append(p, jump(jeq, cmd, 0, 1), stmt(ret, errno(unix.EPERM)))
	}
	p = append(p, stmt(ret, allow))
	p = append(p,
		jump(jeq, unix.SYS_CLONE, 0, 4),
		stmt(ld, offArg0),
		jump(jset, nsFlags, 0, 1),
		stmt(ret, errno(unix.EPERM)),
		stmt(ret, allow),
	)
	return append(p, stmt(ret, allow))
}

func seccompRestrict() error {
	prog := seccompFilter()
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, e := unix.Syscall(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		return e
	}
	return nil
}
