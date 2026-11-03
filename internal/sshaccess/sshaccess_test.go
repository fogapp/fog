package sshaccess

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func pubLine(t *testing.T, comment string) (string, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pk))) + " " + comment, gossh.FingerprintSHA256(pk)
}

func TestAddListRemove(t *testing.T) {
	m := New(t.TempDir())
	line, fp := pubLine(t, "me@laptop")

	k, err := m.Add(line, "")
	if err != nil {
		t.Fatal(err)
	}
	if k.Fingerprint != fp || k.Comment != "me@laptop" || k.Type != "ssh-ed25519" {
		t.Fatalf("added %+v", k)
	}
	if _, err := m.Add(line, "again"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	line2, fp2 := pubLine(t, "")
	if k, err := m.Add(line2, "claude"); err != nil || k.Comment != "claude" {
		t.Fatalf("named add: %+v %v", k, err)
	}

	keys, err := m.List()
	if err != nil || len(keys) != 2 {
		t.Fatalf("list: %+v %v", keys, err)
	}

	for path, want := range map[string]os.FileMode{
		m.KeysPath():                             0o600,
		filepath.Dir(m.KeysPath()):               0o700,
		filepath.Dir(filepath.Dir(m.KeysPath())): 0o700,
	} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s mode %v, want %v (%v)", path, fi.Mode().Perm(), want, err)
		}
	}

	if _, err := m.Remove(fp); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(fp); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	keys, _ = m.List()
	if len(keys) != 1 || keys[0].Fingerprint != fp2 {
		t.Fatalf("after remove: %+v", keys)
	}
}

func TestAddRejects(t *testing.T) {
	m := New(t.TempDir())
	line, _ := pubLine(t, "x")
	line2, _ := pubLine(t, "y")
	for name, in := range map[string]string{
		"garbage":     "hello",
		"private key": "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
		"options":     `command="/bin/true" ` + line,
		"two keys":    line + "\n" + line2,
	} {
		if _, err := m.Add(in, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := m.Add(line, "bad\nname"); !errors.Is(err, ErrInvalid) {
		t.Errorf("multi-line name: %v", err)
	}
	if keys, _ := m.List(); len(keys) != 0 {
		t.Fatalf("rejected keys were written: %+v", keys)
	}
}

func TestStatusReadsSSHD(t *testing.T) {
	m := New(t.TempDir())
	m.goos = "linux"
	m.sshdPath = func() string { return "/usr/sbin/sshd" }
	pattern := m.keysPattern()
	m.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "systemctl":
			return []byte("active\ninactive\ninactive\n"), errors.New("exit 3")
		case "/usr/sbin/sshd":
			return []byte("port 22\npermitrootlogin prohibit-password\nauthorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2 " + pattern + "\n"), nil
		}
		return nil, errors.New("unexpected " + name)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.SSHDInstalled || !st.SSHDActive || !st.Wired || st.RootLogin != "prohibit-password" {
		t.Fatalf("status %+v", st)
	}
}

func TestParseEffectiveUnwired(t *testing.T) {
	wired, root := parseEffective([]byte("authorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2\npermitrootlogin no\n"), "/x/%u/authorized_keys", "/x/root/authorized_keys")
	if wired || root != "no" {
		t.Fatalf("wired=%v root=%q", wired, root)
	}
}

func TestDropInScopesToUser(t *testing.T) {
	m := New("/var/lib/fog")
	d := m.dropIn()
	if !strings.Contains(d, "AuthorizedKeysFile .ssh/authorized_keys .ssh/authorized_keys2 /var/lib/fog/ssh/%u/authorized_keys\n") {
		t.Fatalf("drop-in:\n%s", d)
	}
}

func TestWireRunsTheScriptOnTheHost(t *testing.T) {
	m := New(t.TempDir())
	m.goos = "linux"
	m.sshdPath = func() string { return "/usr/sbin/sshd" }
	pattern := m.keysPattern()
	wired := false
	m.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "systemctl":
			return []byte("active\n"), nil
		case "/usr/sbin/sshd":
			keys := "authorizedkeysfile .ssh/authorized_keys"
			if wired {
				keys += " " + pattern
			}
			return []byte(keys + "\n"), nil
		}
		return nil, errors.New("unexpected " + name)
	}
	var script string
	m.host = func(_ context.Context, _ map[string]string, name string, args ...string) ([]byte, error) {
		if name != "/bin/sh" || len(args) != 2 || args[0] != "-c" {
			t.Fatalf("host ran %s %v", name, args)
		}
		script, wired = args[1], true
		return nil, nil
	}
	if err := m.Wire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "install -D -m 0644") || !strings.Contains(script, DropInPath) {
		t.Fatalf("script:\n%s", script)
	}
}
