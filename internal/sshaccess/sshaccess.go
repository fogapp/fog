// Package sshaccess lets the operator authorize SSH public keys for root
// from Settings › People & access › SSH, so someone they trust (or a tool they run) can
// get a shell on the box to look at something fog can't show.
//
// fogd's systemd sandbox makes /root and /etc read-only, so fog never edits
// root's own ~/.ssh/authorized_keys. Its keys live in a file it owns,
// <data>/ssh/root/authorized_keys, and a one-line sshd drop-in adds that
// file to AuthorizedKeysFile beside the defaults. The drop-in is the only
// thing written outside the data dir, through a transient systemd unit
// (outside the sandbox), and sshd validates it before it is kept.
//
// The path is spelled with %u, so the file only ever authorizes the user
// it is named for: a key added here opens root and nothing else.
package sshaccess

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	gossh "golang.org/x/crypto/ssh"

	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/hostpkg"
)

// The account keys authorize. fogd is root and the things worth looking at
// (Docker's store, the disks) need root, so that's the account.
const User = "root"

// DropInPath sorts ahead of distro and cloud-init drop-ins: sshd keeps the
// first value it reads for AuthorizedKeysFile.
const DropInPath = "/etc/ssh/sshd_config.d/05-fog-ssh.conf"

var (
	ErrInvalid     = errors.New("invalid key")
	ErrExists      = errors.New("that key is already authorized")
	ErrNotFound    = errors.New("no such key")
	ErrUnsupported = errors.New("SSH access is managed on Linux servers only")
)

type Key struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment"`
}

type Status struct {
	Supported     bool `json:"supported"`
	SSHDInstalled bool `json:"sshdInstalled"`
	SSHDActive    bool `json:"sshdActive"`
	// Wired: sshd's effective configuration for root includes fog's keys
	// file. Read from sshd itself, not inferred from the drop-in existing.
	Wired bool `json:"wired"`
	// sshd's effective PermitRootLogin ("prohibit-password", "yes", "no"…).
	RootLogin  string `json:"rootLogin,omitempty"`
	CheckError string `json:"checkError,omitempty"`
	User       string `json:"user"`
	KeysPath   string `json:"keysPath"`
	Keys       []Key  `json:"keys"`
}

type Manager struct {
	dir string
	mu  sync.Mutex

	// Seams for tests. run is for read-only probes, which work inside
	// fogd's sandbox; host runs what must write the host's /etc.
	goos     string
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
	host     hostpkg.Runner
	sshdPath func() string
}

func New(dataDir string) *Manager {
	return &Manager{
		dir:  filepath.Join(dataDir, "ssh"),
		goos: runtime.GOOS,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return execx.Command(ctx, name, args...).CombinedOutput()
		},
		host: hostpkg.Run,
		sshdPath: func() string {
			for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
			p, _ := exec.LookPath("sshd")
			return p
		},
	}
}

// KeysPath is root's fog-managed authorized_keys file.
func (m *Manager) KeysPath() string { return filepath.Join(m.dir, User, "authorized_keys") }

func (m *Manager) keysPattern() string { return filepath.Join(m.dir, "%u", "authorized_keys") }

func (m *Manager) dropIn() string {
	return "# Written by fog (Settings › People & access › SSH). Adds fog's keys file\n" +
		"# beside the defaults; %u keeps each user to their own file.\n" +
		"AuthorizedKeysFile .ssh/authorized_keys .ssh/authorized_keys2 " + m.keysPattern() + "\n"
}

func (m *Manager) List() ([]Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lines, err := m.readLines()
	if err != nil {
		return nil, err
	}
	keys := []Key{}
	for _, l := range lines {
		pk, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(l))
		if err != nil {
			continue
		}
		keys = append(keys, Key{Type: pk.Type(), Fingerprint: gossh.FingerprintSHA256(pk), Comment: comment})
	}
	return keys, nil
}

// Add authorizes one public key. name labels it; blank keeps the key's own
// comment. Options (from=, command=…) are refused rather than silently
// dropped: a pasted line that meant to restrict itself must not become an
// unrestricted key.
func (m *Manager) Add(line, name string) (Key, error) {
	line = strings.TrimSpace(line)
	if strings.Contains(line, "PRIVATE KEY") {
		return Key{}, fmt.Errorf("%w: that's a private key — paste the public half (the .pub file)", ErrInvalid)
	}
	pk, comment, options, rest, err := gossh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return Key{}, fmt.Errorf("%w: not an SSH public key (expected something like \"ssh-ed25519 AAAA… you@laptop\")", ErrInvalid)
	}
	if len(options) > 0 {
		return Key{}, fmt.Errorf("%w: paste the key without options (%s)", ErrInvalid, strings.Join(options, ","))
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return Key{}, fmt.Errorf("%w: paste one key at a time", ErrInvalid)
	}
	if n := strings.TrimSpace(name); n != "" {
		comment = n
	}
	if len(comment) > 100 || strings.IndexFunc(comment, unicode.IsControl) >= 0 {
		return Key{}, fmt.Errorf("%w: the name must be one line of at most 100 characters", ErrInvalid)
	}
	key := Key{Type: pk.Type(), Fingerprint: gossh.FingerprintSHA256(pk), Comment: comment}

	m.mu.Lock()
	defer m.mu.Unlock()
	lines, err := m.readLines()
	if err != nil {
		return Key{}, err
	}
	for _, l := range lines {
		if have, _, _, _, err := gossh.ParseAuthorizedKey([]byte(l)); err == nil && gossh.FingerprintSHA256(have) == key.Fingerprint {
			return Key{}, ErrExists
		}
	}
	entry := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pk)))
	if comment != "" {
		entry += " " + comment
	}
	return key, m.writeLines(append(lines, entry))
}

func (m *Manager) Remove(fingerprint string) (Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lines, err := m.readLines()
	if err != nil {
		return Key{}, err
	}
	kept := lines[:0:0]
	var removed *Key
	for _, l := range lines {
		pk, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(l))
		if err == nil && gossh.FingerprintSHA256(pk) == fingerprint {
			removed = &Key{Type: pk.Type(), Fingerprint: fingerprint, Comment: comment}
			continue
		}
		kept = append(kept, l)
	}
	if removed == nil {
		return Key{}, ErrNotFound
	}
	return *removed, m.writeLines(kept)
}

func (m *Manager) readLines() ([]string, error) {
	b, err := os.ReadFile(m.KeysPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// writeLines replaces the file atomically. sshd's StrictModes refuses a
// keys file whose file or directories are group- or world-writable, so the
// modes are set explicitly rather than trusting the umask.
func (m *Manager) writeLines(lines []string) error {
	userDir := filepath.Dir(m.KeysPath())
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		return err
	}
	for _, d := range []string{m.dir, userDir} {
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	buf.WriteString("# Managed by fog: Settings › People & access › SSH. Edits here are kept.\n")
	for _, l := range lines {
		buf.WriteString(l + "\n")
	}
	// Durable before the rename, or a crash can leave an empty
	// authorized_keys in place of the old one and lock everyone out.
	return fsx.WriteFileAtomic(m.KeysPath(), buf.Bytes(), 0o600)
}

// Status reports the keys and what sshd will actually do with them.
func (m *Manager) Status(ctx context.Context) (*Status, error) {
	keys, err := m.List()
	if err != nil {
		return nil, err
	}
	st := &Status{Supported: m.goos == "linux", User: User, KeysPath: m.KeysPath(), Keys: keys}
	if !st.Supported {
		return st, nil
	}
	sshd := m.sshdPath()
	st.SSHDInstalled = sshd != ""
	if !st.SSHDInstalled {
		return st, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// is-active exits non-zero when any unit is inactive; the per-unit
	// lines are the answer.
	out, _ := m.run(cctx, "systemctl", "is-active", "ssh.service", "sshd.service", "ssh.socket")
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) == "active" {
			st.SSHDActive = true
		}
	}
	eff, err := m.run(cctx, sshd, "-T", "-C", "user="+User+",host=localhost,addr=127.0.0.1")
	if err != nil {
		st.CheckError = "couldn't read sshd's configuration: " + strings.TrimSpace(firstLine(eff, err))
		return st, nil
	}
	st.Wired, st.RootLogin = parseEffective(eff, m.keysPattern(), filepath.Join(m.dir, User, "authorized_keys"))
	return st, nil
}

// parseEffective reads `sshd -T` output: whether AuthorizedKeysFile names
// fog's file (as the pattern or expanded) and the PermitRootLogin value.
func parseEffective(out []byte, pattern, expanded string) (wired bool, rootLogin string) {
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "authorizedkeysfile":
			for _, p := range f[1:] {
				if p == pattern || p == expanded {
					wired = true
				}
			}
		case "permitrootlogin":
			rootLogin = f[1]
		}
	}
	return wired, rootLogin
}

// Wire installs the sshd drop-in if sshd doesn't already read fog's file.
// It runs in a transient systemd unit, outside fogd's sandbox; sshd -t
// validates the result and a rejected config is removed again, so a bad
// write can never stop sshd from starting.
func (m *Manager) Wire(ctx context.Context) error {
	if m.goos != "linux" {
		return ErrUnsupported
	}
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if !st.SSHDInstalled {
		return errors.New("sshd isn't installed on this server (apt install openssh-server)")
	}
	if st.Wired {
		return nil
	}
	m.mu.Lock()
	staged := filepath.Join(m.dir, "sshd-dropin.conf")
	err = os.MkdirAll(m.dir, 0o700)
	if err == nil {
		err = os.WriteFile(staged, []byte(m.dropIn()), 0o644)
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	sshd := shellQuote(m.sshdPath())
	script := strings.Join([]string{
		"set -e",
		"install -D -m 0644 " + shellQuote(staged) + " " + DropInPath,
		"if ! " + sshd + " -t; then rm -f " + DropInPath + "; echo 'sshd rejected the drop-in; removed it' >&2; exit 1; fi",
		"systemctl try-reload-or-restart ssh.service sshd.service 2>/dev/null || true",
	}, "\n")
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Through hostpkg: outside fogd's read-only /etc, under a fixed unit
	// name (a retry can't stack a second writer), stopped on cancel and
	// reaped by PID 1 at the deadline.
	out, err := m.host(cctx, nil, "/bin/sh", "-c", script)
	if err != nil {
		return fmt.Errorf("configuring sshd: %s", strings.TrimSpace(firstLine(out, err)))
	}
	st, err = m.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Wired {
		return errors.New("the drop-in was written, but sshd still doesn't read fog's keys file — " +
			"this server's sshd_config may not include /etc/ssh/sshd_config.d")
	}
	return nil
}

func firstLine(out []byte, err error) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return err.Error()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
