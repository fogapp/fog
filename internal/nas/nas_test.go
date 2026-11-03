package nas

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

func share(mutate func(*store.NasShare)) store.NasShare {
	sh := store.NasShare{
		Name: "media", Proto: "nfs", Server: "nas.lan",
		Remote: "/volume1/media", MountPoint: "/mnt/nas/media",
	}
	if mutate != nil {
		mutate(&sh)
	}
	return sh
}

func TestValidate(t *testing.T) {
	cases := []struct {
		desc   string
		mutate func(*store.NasShare)
		wantIn string // "" = valid
	}{
		{"valid nfs", nil, ""},
		{"valid cifs", func(s *store.NasShare) { s.Proto = "cifs"; s.Remote = "media" }, ""},
		{"bad proto", func(s *store.NasShare) { s.Proto = "sshfs" }, "protocol"},
		{"no server", func(s *store.NasShare) { s.Server = "" }, "server"},
		{"no remote", func(s *store.NasShare) { s.Remote = "" }, "share path"},
		{"relative nfs export", func(s *store.NasShare) { s.Remote = "volume1/media" }, "absolute"},
		{"relative mount point", func(s *store.NasShare) { s.MountPoint = "mnt/media" }, "absolute"},
		{"root mount point", func(s *store.NasShare) { s.MountPoint = "/" }, "clean absolute"},
		{"dotdot mount point", func(s *store.NasShare) { s.MountPoint = "/mnt/../etc/x" }, "clean absolute"},
		{"system mount point", func(s *store.NasShare) { s.MountPoint = "/etc/nas" }, "not allowed"},
		{"var mount point", func(s *store.NasShare) { s.MountPoint = "/var/lib/fog/data" }, "not allowed"},
		{"spaces in options", func(s *store.NasShare) { s.Options = "rw, soft" }, "comma-separated"},
	}
	for _, c := range cases {
		err := Validate(share(c.mutate))
		if c.wantIn == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", c.desc, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.wantIn) {
			t.Errorf("%s: want error containing %q, got %v", c.desc, c.wantIn, err)
		}
	}
}

// The timeout-safe statfs must return real numbers for a healthy filesystem
// and must not leave a stray probe registered afterwards.
func TestStatfsHealthyPath(t *testing.T) {
	fs, stale, err := Statfs(t.TempDir())
	if err != nil || stale {
		t.Fatalf("statfs on a healthy dir: stale=%v err=%v", stale, err)
	}
	if fs.Blocks == 0 {
		t.Error("statfs returned no size for a healthy filesystem")
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	if len(probes) != 0 {
		t.Errorf("probe left registered after completion: %v", probes)
	}
}

// swapVar replaces a package-level seam for one test and restores it after.
func swapVar[T any](t *testing.T, v *T, val T) {
	t.Helper()
	orig := *v
	*v = val
	t.Cleanup(func() { *v = orig })
}

// MountsBlocked must tell an unprivileged container's user namespace apart
// from the initial namespace, and say nothing where it can't read the map.
func TestMountsBlocked(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, uidMap string
		blocked      bool
	}{
		{"initial namespace", "         0          0 4294967295\n", false},
		{"unprivileged LXC", "         0     100000      65536\n", true},
		{"privileged LXC", "         0          0 4294967295\n", false},
		{"split map, root still root", "0 0 1000\n1000 101000 64536\n", false},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
		if err := os.WriteFile(p, []byte(tc.uidMap), 0o644); err != nil {
			t.Fatal(err)
		}
		swapVar(t, &uidMapPath, p)
		if got := MountsBlocked() != ""; got != tc.blocked {
			t.Errorf("%s: blocked = %v, want %v", tc.name, got, tc.blocked)
		}
	}
	swapVar(t, &uidMapPath, filepath.Join(dir, "missing"))
	if msg := MountsBlocked(); msg != "" {
		t.Errorf("unreadable uid_map: got %q, want empty", msg)
	}
}

// In an unprivileged container Mount must not run mount(8) — it can only
// fail with a bare EPERM — but must still accept a share bound in from the
// host at its mount point.
func TestMountBlockedInUserNamespace(t *testing.T) {
	uidMap := filepath.Join(t.TempDir(), "uid_map")
	if err := os.WriteFile(uidMap, []byte("0 100000 65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(t.TempDir(), "share")
	sh := share(func(s *store.NasShare) { s.MountPoint = mp })
	bound := false
	swapVar(t, &uidMapPath, uidMap)
	swapVar(t, &linuxHost, true)
	swapVar(t, &helperInstalled, func(string) bool { return true })
	swapVar(t, &mountedPoints, func() map[string]bool { return map[string]bool{mp: bound} })
	swapVar(t, &hostRun, func(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error) {
		t.Errorf("unexpected host command %s %v", name, args)
		return nil, nil
	})
	m := &Manager{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := m.Mount(context.Background(), sh)
	if err == nil || !strings.Contains(err.Error(), "unprivileged container") {
		t.Fatalf("Mount = %v, want the unprivileged-container explanation", err)
	}
	if !strings.Contains(m.getErr(sh.Name), "unprivileged container") {
		t.Errorf("share's last error = %q, want the explanation", m.getErr(sh.Name))
	}

	bound = true
	if err := m.Mount(context.Background(), sh); err != nil {
		t.Fatalf("Mount with the share bound in from the host = %v, want nil", err)
	}
}

// Unresponsive must report a mount whose statfs never comes back.
func TestUnresponsive(t *testing.T) {
	swapVar(t, &statfsTimeout, 30*time.Millisecond)
	swapVar(t, &statfsFn, func(path string, fs *syscall.Statfs_t) error {
		if path == "/mnt/alive" {
			return nil
		}
		time.Sleep(300 * time.Millisecond) // a hard mount that never answers
		return nil
	})

	if !Unresponsive("/mnt/dead") {
		t.Error("a point whose statfs hangs must read as unresponsive")
	}
	if Unresponsive("/mnt/alive") {
		t.Error("a point that answers must not read as unresponsive")
	}
	// Wait for the stuck probe to drain before cleanup restores the seams:
	// observing the empty registry under probeMu is the happens-before edge
	// to the probe goroutine's last read of statfsFn.
	deadline := time.Now().Add(2 * time.Second)
	for {
		probeMu.Lock()
		n := len(probes)
		probeMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stuck probe never drained")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// /proc/mounts escapes every character that would break its space-separated
// format, not just space. A decoder handling only \040 mis-keys any mount
// point containing the others, silently defeating the share guards.
func TestDecodeMountEscapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/mnt/media", "/mnt/media"},
		{`/mnt/my\040share`, "/mnt/my share"},
		{`/mnt/tab\011here`, "/mnt/tab\there"},
		{`/mnt/line\012break`, "/mnt/line\nbreak"},
		{`/mnt/back\134slash`, `/mnt/back\slash`},
		{`/mnt/a\040b\011c\012d\134e`, "/mnt/a b\tc\nd\\e"},
		// A backslash not opening three octal digits passes through verbatim.
		{`/mnt/trailing\04`, `/mnt/trailing\04`},
		{`/mnt/not\089octal`, `/mnt/not\089octal`},
	}
	for _, c := range cases {
		if got := decodeMountEscapes(c.in); got != c.want {
			t.Errorf("decodeMountEscapes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Two concurrent EnsureForPaths for the same share must run mount(8) once:
// the per-mountpoint lock serializes check-then-mount, and the second caller
// re-reads the mount table under the lock instead of stacking a second mount
// over the first.
func TestEnsureForPathsMountsOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mp := filepath.Join(t.TempDir(), "share")
	if _, err := st.CreateNasShare(store.NasShare{
		Name: "backups", Proto: "nfs", Server: "nas.lan",
		Remote: "/volume1/backups", MountPoint: mp, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	table := map[string]bool{}
	mountCalls := 0
	swapVar(t, &linuxHost, true)
	swapVar(t, &helperInstalled, func(string) bool { return true })
	swapVar(t, &mountedPoints, func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]bool{}
		for k, v := range table {
			out[k] = v
		}
		return out
	})
	swapVar(t, &hostRun, func(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error) {
		if name != "mount" {
			t.Errorf("unexpected host command %s %v", name, args)
		}
		time.Sleep(50 * time.Millisecond) // widen the race window
		mu.Lock()
		mountCalls++
		table[mp] = true
		mu.Unlock()
		return nil, nil
	})

	m := &Manager{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.EnsureForPaths(context.Background(), []string{filepath.Join(mp, "sub")})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureForPaths[%d]: %v", i, err)
		}
	}
	if mountCalls != 1 {
		t.Fatalf("mount(8) ran %d times; the per-mountpoint lock must collapse concurrent mounts into one", mountCalls)
	}
}

// An app waiting for a share is woken however the share mounts: a deploy's
// EnsureForPaths, the Mount button, not only the reconcile loop.
func TestEveryMountFiresTheHook(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mp := filepath.Join(t.TempDir(), "share")
	sh := store.NasShare{Name: "media", Proto: "nfs", Server: "nas.lan", Remote: "/v/media", MountPoint: mp, Enabled: true}
	if _, err := st.CreateNasShare(sh); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	mounted := false
	swapVar(t, &linuxHost, true)
	swapVar(t, &helperInstalled, func(string) bool { return true })
	swapVar(t, &mountedPoints, func() map[string]bool { mu.Lock(); defer mu.Unlock(); return map[string]bool{mp: mounted} })
	swapVar(t, &hostRun, func(context.Context, map[string]string, string, ...string) ([]byte, error) {
		mu.Lock()
		mounted = true
		mu.Unlock()
		return nil, nil
	})
	fired := make(chan string, 4)
	m := &Manager{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnLateMount: func(sh store.NasShare) { fired <- sh.Name }}
	for name, mount := range map[string]func() error{
		"EnsureForPaths": func() error { return m.EnsureForPaths(context.Background(), []string{filepath.Join(mp, "x")}) },
		"Mount":          func() error { return m.Mount(context.Background(), sh) },
	} {
		mu.Lock()
		mounted = false
		mu.Unlock()
		if err := mount(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case got := <-fired:
			if got != "media" {
				t.Fatalf("%s fired for %q", name, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s mounted the share and fired nothing", name)
		}
	}
	// Already mounted: nothing new to wake anyone for.
	if err := m.Mount(context.Background(), sh); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
		t.Fatal("fired for a share already mounted")
	case <-time.After(200 * time.Millisecond):
	}
}
