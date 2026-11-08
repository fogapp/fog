//go:build linux

package apps

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/livetest"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// The upgrade, live: an app's container was created under an older render
// — NET_RAW kept, its Drive folder bound by its raw path — and an upgraded
// fog starts. The boot pass (ReapplyRules) renders the app's current
// revision under today's rules and recreates the container: NET_RAW
// dropped, the folder bound through its pin. Same image, nothing pulled.
//
// Needs Linux, root (it pins) and docker compose. On a Mac with Colima:
//
//	colima ssh -- sudo bash -lc 'cd ~/src/fog && go test -run LiveUpgrade ./internal/apps'
func TestLiveUpgradeReappliesTheRulesToRunningApps(t *testing.T) {
	if testing.Short() || os.Geteuid() != 0 || exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("live: needs root and docker compose")
	}
	livetest.Lock(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client")
	}
	if _, err := docker("image", "inspect", liveNetImage); err != nil {
		if out, err := docker("pull", liveNetImage); err != nil {
			t.Skipf("cannot pull %s: %s", liveNetImage, out)
		}
	}
	base, err := os.MkdirTemp("/var/tmp", "fogre")
	if err != nil {
		t.Fatal(err)
	}
	data, drive := filepath.Join(base, "data"), filepath.Join(base, "drive")
	os.MkdirAll(filepath.Join(drive, "Content/Movies"), 0o777)
	os.WriteFile(filepath.Join(drive, "Content/Movies/movie.mkv"), []byte("m"), 0o644)

	st, err := storetest.Open(filepath.Join(base, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, err := vault.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	m := &Materializer{DataDir: data, FilesRoot: func() string { return drive }}
	m.Pin = func(_ context.Context, root, rel, target, owner string) error {
		return pinBind(m.BindsDir(), root, rel, target, owner)
	}
	m.Unpin = func(_ context.Context, target string, keep ...string) error {
		return unpinBind(m.BindsDir(), target, keep...)
	}
	e := &Engine{Store: st, Vault: vlt, Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler), Mat: m}

	name := "old" + strings.ToLower(randx.Hex(3))
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		execx.Run(m.ComposeCmd(c, name, "down"))
		e.RemoveAppNetworks(c, name)
		unpinBind(m.BindsDir(), filepath.Join(m.BindsDir(), name))
		unix.Unmount(m.BindsDir(), unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	spec := &AppSpec{Source: Source{Type: "image", Image: liveNetImage}, Command: []string{"sleep", "3600"},
		Volumes: []Volume{{Name: "movies", Mount: "/m", Files: "Content/Movies"}}}
	a, _ := st.CreateApp(name)
	b, _ := json.Marshal(spec)
	rev, _ := st.CreateRevision(a.ID, string(b), "", "t")
	st.SetAppCurrentRev(a.ID, rev.Rev)
	st.SetAppDesiredState(a.ID, "running")
	st.RecordAppVersion(a.ID, "image", "sha256:x", liveNetImage, rev.Rev, "t")

	// The render an older fog wrote: capabilities kept, the raw path bound.
	dir := m.AppDir(name)
	os.MkdirAll(dir, 0o755)
	old := "services:\n  " + name + ":\n    image: " + liveNetImage + "\n    command: [sleep, \"3600\"]\n" +
		"    labels: {fog.app: " + name + "}\n    volumes: [\"" + filepath.Join(drive, "Content/Movies") + ":/m\"]\n"
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(old), 0o644)
	if out, err := execx.Run(m.ComposeCmd(ctx, name, "up", "-d")); err != nil {
		t.Fatalf("the old render: %s %v", out, err)
	}
	inspect := func() (caps, source string) {
		id, err := docker("ps", "-q", "--filter", "label=fog.app="+name)
		if err != nil || id == "" {
			t.Fatalf("no container: %s", id)
		}
		caps, _ = docker("inspect", "-f", "{{.HostConfig.CapDrop}}", id)
		source, _ = docker("inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/m"}}{{.Source}}{{end}}{{end}}`, id)
		return caps, source
	}
	if caps, source := inspect(); strings.Contains(caps, "NET_RAW") || source != filepath.Join(drive, "Content/Movies") {
		t.Fatalf("control: the old render already has caps %s, source %s", caps, source)
	}

	e.ReapplyRules(ctx)

	caps, source := inspect()
	if !strings.Contains(caps, "NET_RAW") {
		t.Errorf("after the upgrade the container still has NET_RAW (cap_drop %s)", caps)
	}
	if !strings.HasPrefix(source, m.BindsDir()+"/") {
		t.Errorf("after the upgrade /m is bound from %s, not its pin", source)
	}
	if why := e.RulesRefused()[name]; why != "" {
		t.Errorf("refused: %s", why)
	}
	if out, _ := docker("exec", strings.TrimSpace(mustPS(t, docker, name)), "ls", "/m"); !strings.Contains(out, "movie.mkv") {
		t.Errorf("the pinned folder lists %q", out)
	}
}

func mustPS(t *testing.T, docker func(...string) (string, error), name string) string {
	id, err := docker("ps", "-q", "--filter", "label=fog.app="+name)
	if err != nil || id == "" {
		t.Fatalf("no container for %s", name)
	}
	return id
}

// liveRig is a store, vault and pinning materializer under base, on the
// real Docker host.
func liveRig(t *testing.T, base string) (*Engine, *Materializer, string) {
	t.Helper()
	data, drive := filepath.Join(base, "data"), filepath.Join(base, "drive")
	os.MkdirAll(drive, 0o755)
	st, err := storetest.Open(filepath.Join(base, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	m := &Materializer{DataDir: data, FilesRoot: func() string { return drive }}
	m.Pin = func(_ context.Context, root, rel, target, owner string) error {
		return pinBind(m.BindsDir(), root, rel, target, owner)
	}
	m.Unpin = func(_ context.Context, target string, keep ...string) error {
		return unpinBind(m.BindsDir(), target, keep...)
	}
	e := &Engine{Store: st, Vault: vlt, Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler), Mat: m}
	t.Cleanup(func() {
		// fog's names for the images each app ran (pinRepo).
		list, _ := st.ListApps()
		for _, a := range list {
			out, _ := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}", pinRepo(a.Name)).Output()
			for _, ref := range strings.Fields(string(out)) {
				exec.Command("docker", "rmi", "-f", ref).Run()
			}
		}
	})
	return e, m, drive
}

func liveSkip(t *testing.T) (context.Context, func(...string) (string, error)) {
	t.Helper()
	if testing.Short() || os.Geteuid() != 0 || exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("live: needs root and docker compose")
	}
	livetest.Lock(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client")
	}
	if _, err := docker("image", "inspect", liveNetImage); err != nil {
		if out, err := docker("pull", liveNetImage); err != nil {
			t.Skipf("cannot pull %s: %s", liveNetImage, out)
		}
	}
	return ctx, docker
}

// The reviewer's attack on the upgrade: an app's Drive folder was swapped
// for a link to /etc before the upgrade, under a container an older fog
// rendered with the raw path. Its restart policy (or a reboot) remounted
// the path — the host's /etc. Now the boot pass first takes the restart
// policy from every container of older renders, then the pin refuses the
// link, and the app's containers are removed rather than left to come back.
func TestLivePlantedLinkIsNotRevived(t *testing.T) {
	ctx, docker := liveSkip(t)
	base, err := os.MkdirTemp("/var/tmp", "fogln")
	if err != nil {
		t.Fatal(err)
	}
	e, m, drive := liveRig(t, base)
	name := "linked" + strings.ToLower(randx.Hex(3))
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		execx.Run(m.ComposeCmd(c, name, "down"))
		exec.Command("docker", "rm", "-f", name+"-raw").Run()
		e.RemoveAppNetworks(c, name)
		unpinBind(m.BindsDir(), filepath.Join(m.BindsDir(), name))
		unix.Unmount(m.BindsDir(), unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	movies := filepath.Join(drive, "Content/Movies")
	os.MkdirAll(movies, 0o777)
	spec := &AppSpec{Source: Source{Type: "image", Image: liveNetImage}, Command: []string{"sleep", "3600"},
		Volumes: []Volume{{Name: "movies", Mount: "/m", Files: "Content/Movies"}}}
	a, _ := e.Store.CreateApp(name)
	b, _ := json.Marshal(spec)
	rev, _ := e.Store.CreateRevision(a.ID, string(b), "", "t")
	e.Store.SetAppCurrentRev(a.ID, rev.Rev)
	e.Store.SetAppDesiredState(a.ID, "running")
	e.Store.RecordAppVersion(a.ID, "image", "sha256:x", liveNetImage, rev.Rev, "t")

	// The old render, and the swap under it.
	dir := m.AppDir(name)
	os.MkdirAll(dir, 0o755)
	old := "services:\n  " + name + ":\n    image: " + liveNetImage + "\n    command: [sleep, \"3600\"]\n    restart: unless-stopped\n" +
		"    labels: {fog.app: " + name + "}\n    volumes: [\"" + movies + ":/m\"]\n"
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(old), 0o644)
	if out, err := execx.Run(m.ComposeCmd(ctx, name, "up", "-d")); err != nil {
		t.Fatalf("the old render: %s %v", out, err)
	}
	os.Rename(movies, movies+".old")
	os.Symlink("/etc", movies)
	id := mustPS(t, docker, name)
	if out, err := docker("restart", id); err != nil {
		t.Fatalf("restart: %s", out)
	}
	if out, _ := docker("exec", id, "ls", "/m"); !strings.Contains(out, "passwd") {
		t.Fatalf("control: the restart didn't remount the link (%q); the test proves nothing", out)
	}

	// First thing at boot: no restart policy left on it.
	e.disarmOldContainers(ctx)
	if pol, _ := docker("inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", id); pol != "no" {
		t.Fatalf("an old render's container keeps restart policy %q", pol)
	}
	e.ReapplyRules(ctx)
	if why := e.RulesRefused()[name]; !strings.Contains(why, "Content/Movies") {
		t.Errorf("refusal = %q", why)
	}
	if out, _ := docker("ps", "-aq", "--filter", "label=fog.app="+name); out != "" {
		t.Fatalf("containers left for a restart policy or reboot to revive: %s", out)
	}
}

// Every compose app binds /etc/localtime read-only, and many bind a config
// file: both deploy, start and restart — /etc/localtime through root's own
// link into the zone database, the file pinned as a file. And a secret the
// running revision lists, taken away, doesn't stop it restarting.
func TestLiveFileBindsDeployStartAndRestart(t *testing.T) {
	ctx, docker := liveSkip(t)
	if fi, err := os.Lstat("/etc/localtime"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Skip("/etc/localtime isn't a link here")
	}
	base, err := os.MkdirTemp("/var/tmp", "fogfb")
	if err != nil {
		t.Fatal(err)
	}
	e, m, _ := liveRig(t, base)
	name := "files" + strings.ToLower(randx.Hex(3))
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		execx.Run(m.ComposeCmd(c, name, "down"))
		e.RemoveAppNetworks(c, name)
		unpinBind(m.BindsDir(), filepath.Join(m.BindsDir(), name))
		unix.Unmount(m.BindsDir(), unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	cfg := filepath.Join(base, "conf", "app.ini")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.WriteFile(cfg, []byte("greeting=hello\n"), 0o644)
	spec := &AppSpec{Source: Source{Type: "image", Image: liveNetImage}, Command: []string{"sleep", "3600"}, DeployStrategy: "restart",
		Secrets: []string{"API_KEY"},
		Volumes: []Volume{
			{Name: "tz", Mount: "/etc/localtime", Host: "/etc/localtime", ReadOnly: true},
			{Name: "ini", Mount: "/config/app.ini", Host: cfg},
		},
		AcknowledgeHostAccess: []string{cfg}}
	if err := e.Create(ctx, name, spec, map[string]string{"API_KEY": "k"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.ApproveSpec(name, spec, "admin"); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		id := mustPS(t, docker, name)
		if out, err := docker("exec", id, "cat", "/config/app.ini"); err != nil || !strings.Contains(out, "greeting=hello") {
			t.Fatalf("%s: the config file reads %q (%v)", when, out, err)
		}
		if out, err := docker("exec", id, "sh", "-c", "test -s /etc/localtime && echo ok"); err != nil || out != "ok" {
			t.Fatalf("%s: /etc/localtime: %q (%v)", when, out, err)
		}
		src, _ := docker("inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/config/app.ini"}}{{.Source}}{{end}}{{end}}`, id)
		if !strings.HasPrefix(src, m.BindsDir()+"/") {
			t.Fatalf("%s: the config file is bound from %s, not its pin", when, src)
		}
	}
	if err := e.Deploy(ctx, name, 0, "admin"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	check("after the deploy")
	if err := e.Start(ctx, name, "admin"); err != nil {
		t.Fatalf("start: %v", err)
	}
	check("after a start")
	if err := e.Restart(ctx, name, "admin"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	check("after a restart")
	a, _ := e.Store.AppByName(name)
	e.Store.DeleteSecret(a.ID, "API_KEY")
	if err := e.Restart(ctx, name, "admin"); err != nil {
		t.Fatalf("restart with its secret taken away: %v", err)
	}
	check("after a restart without its secret")
}

// After a power cut the NAS comes up last. An app whose bind is on a share
// not mounted yet at boot waits for it — stopped, not removed, its old
// render unable to start on its own — and comes up when the share mounts.
func TestLiveSlowShareAtBootWaitsThenStarts(t *testing.T) {
	ctx, docker := liveSkip(t)
	base, err := os.MkdirTemp("/var/tmp", "fognas")
	if err != nil {
		t.Fatal(err)
	}
	e, m, _ := liveRig(t, base)
	share := filepath.Join(base, "nas")
	os.MkdirAll(share, 0o755)
	m.ManagedRoots = func() []string { return []string{share} }
	mountShare := func() {
		t.Helper()
		if err := unix.Mount("fog-test-nas", share, "tmpfs", 0, "size=4m"); err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(share, "media"), 0o777)
		os.WriteFile(filepath.Join(share, "media", "song.flac"), []byte("x"), 0o644)
	}
	mounted := false
	e.EnsureMounts = func(context.Context, []string) error {
		if !mounted {
			return errors.New("the share nas isn't mounted: no route to host")
		}
		return nil
	}
	name := "nasapp" + strings.ToLower(randx.Hex(3))
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		execx.Run(m.ComposeCmd(c, name, "down"))
		e.RemoveAppNetworks(c, name)
		unpinBind(m.BindsDir(), filepath.Join(m.BindsDir(), name))
		unix.Unmount(m.BindsDir(), unix.MNT_DETACH)
		unix.Unmount(share, unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	mountShare()
	media := filepath.Join(share, "media")
	spec := &AppSpec{Source: Source{Type: "image", Image: liveNetImage}, Command: []string{"sleep", "3600"},
		Volumes: []Volume{{Name: "media", Mount: "/media", Host: media}}}
	a, _ := e.Store.CreateApp(name)
	b, _ := json.Marshal(spec)
	rev, _ := e.Store.CreateRevision(a.ID, string(b), "", "t")
	e.Store.SetAppCurrentRev(a.ID, rev.Rev)
	e.Store.SetAppDesiredState(a.ID, "running")
	e.Store.RecordAppVersion(a.ID, "image", "sha256:x", liveNetImage, rev.Rev, "t")

	// Running under an older render; then the power cut: the host goes
	// down, and comes back before the NAS.
	dir := m.AppDir(name)
	os.MkdirAll(dir, 0o755)
	old := "services:\n  " + name + ":\n    image: " + liveNetImage + "\n    command: [sleep, \"3600\"]\n    restart: unless-stopped\n" +
		"    labels: {fog.app: " + name + "}\n    volumes: [\"" + media + ":/media\"]\n"
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(old), 0o644)
	if out, err := execx.Run(m.ComposeCmd(ctx, name, "up", "-d")); err != nil {
		t.Fatalf("the old render: %s %v", out, err)
	}
	id := mustPS(t, docker, name)
	docker("stop", id)
	if err := unix.Unmount(share, unix.MNT_DETACH); err != nil {
		t.Fatal(err)
	}

	e.ReapplyRules(ctx)
	waiting := func(when string) {
		t.Helper()
		if why := e.Waiting()[name]; why == "" {
			t.Fatalf("%s: not waiting for its share (refused: %q)", when, e.RulesRefused()[name])
		}
		if why := e.RulesRefused()[name]; why != "" {
			t.Fatalf("%s: refused, not waiting: %s", when, why)
		}
		if out, _ := docker("ps", "-aq", "--filter", "label=fog.app="+name); out == "" {
			t.Fatalf("%s: its containers were removed", when)
		}
		if out, _ := docker("ps", "-q", "--filter", "label=fog.app="+name); out != "" {
			t.Fatalf("%s: it runs without its share", when)
		}
		if pol, _ := docker("inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", id); pol != "no" {
			t.Fatalf("%s: the old render can still start itself (restart %q)", when, pol)
		}
	}
	waiting("after the boot pass")
	// Where nothing reports the share down, the missing folder says it.
	e.EnsureMounts = nil
	if err := e.Start(ctx, name, "system"); err == nil {
		t.Fatal("started with its share missing")
	}
	waiting("after a start with the folder missing")

	// The NAS comes up; the NAS manager's hook starts what waited.
	mountShare()
	mounted = true
	e.EnsureMounts = func(context.Context, []string) error { return nil }
	e.RestartAppsUnder(ctx, share, "nas")
	id = mustPS(t, docker, name)
	if out, err := docker("exec", id, "ls", "/media"); err != nil || !strings.Contains(out, "song.flac") {
		t.Fatalf("after the share mounted it lists %q (%v)", out, err)
	}
	if src, _ := docker("inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/media"}}{{.Source}}{{end}}{{end}}`, id); !strings.HasPrefix(src, m.BindsDir()+"/") {
		t.Errorf("bound from %s, not its pin", src)
	}
	if why := e.Waiting()[name]; why != "" {
		t.Errorf("still waiting: %s", why)
	}
}

// The eighth review's attack: a root container plants a root-owned link
// where another acknowledged host volume's folder was, inside a folder it
// can write — owner and mode can't tell who made it — and the next start
// pinned /etc read-write. No link in a bind source is followed now (but
// the zone links, read-only), and one app holding another volume of its
// own inside a writable one is refused before anything is pinned.
func TestLiveRootPlantedLinkIsRefused(t *testing.T) {
	ctx, docker := liveSkip(t)
	// Under folders only root can write — where owner and mode said the
	// old rule could trust a root-owned link.
	os.MkdirAll("/srv", 0o755)
	base, err := os.MkdirTemp("/srv", "fogrl")
	if err != nil {
		t.Fatal(err)
	}
	e, m, _ := liveRig(t, base)
	area := filepath.Join(base, "srv", "app")
	os.MkdirAll(filepath.Join(area, "conf"), 0o755)
	sfx := strings.ToLower(randx.Hex(3))
	writer, victim := "writer"+sfx, "victim"+sfx
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		for _, n := range []string{writer, victim, "nested" + sfx} {
			execx.Run(m.ComposeCmd(c, n, "down"))
			e.RemoveAppNetworks(c, n)
			unpinBind(m.BindsDir(), filepath.Join(m.BindsDir(), n))
		}
		unix.Unmount(m.BindsDir(), unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	mk := func(name string, vols ...Volume) {
		t.Helper()
		spec := &AppSpec{Source: Source{Type: "image", Image: liveNetImage}, Command: []string{"sleep", "3600"}, Volumes: vols}
		for _, v := range vols {
			spec.AcknowledgeHostAccess = append(spec.AcknowledgeHostAccess, v.Host)
		}
		a, _ := e.Store.CreateApp(name)
		b, _ := json.Marshal(spec)
		rev, _ := e.Store.CreateRevision(a.ID, string(b), "", "t")
		e.Store.SetAppCurrentRev(a.ID, rev.Rev)
		e.Store.SetAppDesiredState(a.ID, "running")
		e.Store.RecordAppVersion(a.ID, "image", "sha256:x", liveNetImage, rev.Rev, "t")
		if err := e.ApproveSpec(name, spec, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	// One app, both volumes: refused outright.
	mk("nested"+sfx, Volume{Name: "app", Mount: "/a", Host: area}, Volume{Name: "conf", Mount: "/c", Host: filepath.Join(area, "conf")})
	if err := e.Start(ctx, "nested"+sfx, "admin"); err == nil || !strings.Contains(err.Error(), "inside volume") {
		t.Fatalf("an app holding its own volume inside a writable one = %v", err)
	}

	// Two apps: the writer's root container plants the link.
	mk(writer, Volume{Name: "app", Mount: "/a", Host: area})
	mk(victim, Volume{Name: "conf", Mount: "/c", Host: filepath.Join(area, "conf")})
	if err := e.Start(ctx, victim, "admin"); err != nil {
		t.Fatalf("victim's first start: %v", err)
	}
	if err := e.Start(ctx, writer, "admin"); err != nil {
		t.Fatalf("writer's start: %v", err)
	}
	if out, err := docker("exec", mustPS(t, docker, writer), "sh", "-c", "mv /a/conf /a/conf.old && ln -s /etc /a/conf"); err != nil {
		t.Fatalf("planting the link: %s", out)
	}
	if fi, err := os.Lstat(filepath.Join(area, "conf")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("control: no link was planted")
	}
	err = e.Restart(ctx, victim, "admin")
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("restart through the planted link = %v", err)
	}
	if out, _ := docker("ps", "-aq", "--filter", "label=fog.app="+victim); out != "" {
		t.Fatalf("the victim's containers were left: %s", out)
	}
	if mounted, _ := isMountPoint(m.PinPath(victim, "conf")); mounted {
		var st unix.Stat_t
		unix.Stat(m.PinPath(victim, "conf")+"/passwd", &st)
		if st.Ino != 0 {
			t.Fatal("the pin is /etc")
		}
	}
}

// The eighth review: an image's own LABEL fog.rules=2 showed through on a
// container an older fog rendered (which set no such label), and the boot
// pass left it its restart policy. Whether a container is today's render
// is read from compose's config hash against fog's own compose file — no
// image sets that — and one fog rendered today keeps its policy.
func TestLiveImageLabelCannotDodgeTheDisarm(t *testing.T) {
	ctx, docker := liveSkip(t)
	base, err := os.MkdirTemp("/var/tmp", "fogds")
	if err != nil {
		t.Fatal(err)
	}
	e, m, _ := liveRig(t, base)
	sfx := strings.ToLower(randx.Hex(3))
	img := "fog-rules-label:" + sfx
	bdir := t.TempDir()
	os.WriteFile(filepath.Join(bdir, "Dockerfile"), []byte("FROM "+liveNetImage+"\nLABEL fog.rules=2\n"), 0o644)
	if out, err := docker("build", "-q", "-t", img, bdir); err != nil {
		t.Fatalf("build: %s", out)
	}
	old, cur := "oldr"+sfx, "curr"+sfx
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		for _, n := range []string{old, cur} {
			execx.Run(m.ComposeCmd(c, n, "down"))
			e.RemoveAppNetworks(c, n)
			unpinBind(m.BindsDir(), filepath.Join(m.BindsDir(), n))
		}
		exec.Command("docker", "rmi", "-f", img).Run()
		unix.Unmount(m.BindsDir(), unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	for _, n := range []string{old, cur} {
		spec := &AppSpec{Source: Source{Type: "image", Image: img}, Command: []string{"sleep", "3600"}}
		a, _ := e.Store.CreateApp(n)
		b, _ := json.Marshal(spec)
		rev, _ := e.Store.CreateRevision(a.ID, string(b), "", "t")
		e.Store.SetAppCurrentRev(a.ID, rev.Rev)
		e.Store.SetAppDesiredState(a.ID, "running")
		e.Store.RecordAppVersion(a.ID, "image", "sha256:x", img, rev.Rev, "t")
	}
	// An older render of one, by hand: no fog.rules of its own.
	os.MkdirAll(m.AppDir(old), 0o755)
	os.WriteFile(filepath.Join(m.AppDir(old), "compose.yaml"), []byte("services:\n  "+old+":\n    image: "+img+
		"\n    command: [sleep, \"3600\"]\n    restart: unless-stopped\n    labels: {fog.app: "+old+"}\n"), 0o644)
	if out, err := execx.Run(m.ComposeCmd(ctx, old, "up", "-d")); err != nil {
		t.Fatalf("old render: %s %v", out, err)
	}
	// Today's render of the other.
	if err := e.Start(ctx, cur, "admin"); err != nil {
		t.Fatal(err)
	}
	oldID, curID := mustPS(t, docker, old), mustPS(t, docker, cur)
	if l, _ := docker("inspect", "-f", `{{index .Config.Labels "fog.rules"}}`, oldID); l != "2" {
		t.Fatalf("control: the image's label isn't on the old container (%q)", l)
	}
	e.disarmOldContainers(ctx)
	if pol, _ := docker("inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", oldID); pol != "no" {
		t.Errorf("the old render, wearing the image's fog.rules=2, kept restart %q", pol)
	}
	if pol, _ := docker("inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", curID); pol != "unless-stopped" {
		t.Errorf("today's render lost its restart policy: %q", pol)
	}
}

// The final review: a failed update leaves the app's tag on the new image,
// and another app's update moves a sidecar tag both share; the next start
// recreated each on whatever its tag pointed to. Starts run the exact
// images the app last ran — only a deploy moves them — and a container
// fog has recorded gets its restart policy back.
func TestLiveStartsRunTheImagesTheyRan(t *testing.T) {
	ctx, docker := liveSkip(t)
	base, err := os.MkdirTemp("/var/tmp", "fogim")
	if err != nil {
		t.Fatal(err)
	}
	e, m, _ := liveRig(t, base)
	sfx := strings.ToLower(randx.Hex(3))
	appImg, dbImg := "fogpin-app:"+sfx, "fogpin-db:"+sfx
	build := func(tag, v string) string {
		t.Helper()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+liveNetImage+"\nLABEL fog.test.version="+v+"\n"), 0o644)
		if out, err := docker("build", "-q", "-t", tag, dir); err != nil {
			t.Fatalf("build %s: %s", tag, out)
		}
		id, _ := docker("image", "inspect", "-f", "{{.Id}}", tag)
		return id
	}
	app1, db1 := build(appImg, "1"), build(dbImg, "1")
	a, b := "pina"+sfx, "pinb"+sfx
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		for _, n := range []string{a, b} {
			execx.Run(m.ComposeCmd(c, n, "down"))
			e.RemoveAppNetworks(c, n)
		}
		exec.Command("docker", "rmi", "-f", appImg, dbImg).Run()
		for _, n := range []string{a, b} {
			exec.Command("docker", "rmi", "-f", pinRepo(n)+":main", pinRepo(n)+":aux-db").Run()
		}
		unix.Unmount(m.BindsDir(), unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	spec := func() *AppSpec {
		return &AppSpec{Source: Source{Type: "image", Image: appImg}, Command: []string{"sleep", "3600"}, DeployStrategy: "restart",
			Aux: []AuxService{{Name: "db", Image: dbImg, Command: []string{"sleep", "3600"}}}}
	}
	for _, n := range []string{a, b} {
		if err := e.Create(ctx, n, spec(), nil, "admin"); err != nil {
			t.Fatal(err)
		}
		if err := e.Deploy(ctx, n, 0, "admin"); err != nil {
			t.Fatalf("deploy %s: %v", n, err)
		}
	}
	images := func(n string) (main, db string) {
		t.Helper()
		cs, _ := e.Docker.ListByLabel(ctx, "fog.app="+n)
		for _, c := range cs {
			if c.State != "running" {
				continue
			}
			if c.Labels["fog.aux"] == "db" {
				db = c.ImageID
			} else if c.Labels["fog.process"] == "" {
				main = c.ImageID
			}
		}
		return
	}

	// A's update: both tags move (its own, and the shared sidecar's), and
	// its release fails — the deploy fails, A keeps its old revision.
	app2, db2 := build(appImg, "2"), build(dbImg, "2")
	if app2 == app1 || db2 == db1 {
		t.Fatal("control: the rebuilds didn't move the tags")
	}
	bad := spec()
	bad.Release = []string{"sh", "-c", "exit 1"}
	if err := e.UpdateSpec(ctx, a, bad, nil, "update", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.Deploy(ctx, a, 0, "admin"); err == nil {
		t.Fatal("control: the update didn't fail")
	}

	for _, n := range []string{a, b} {
		if err := e.Restart(ctx, n, "admin"); err != nil {
			t.Fatalf("restart %s: %v", n, err)
		}
		main, db := images(n)
		if main != app1 || db != db1 {
			t.Errorf("%s after a restart runs %s / sidecar %s; want %s / %s", n, main, db, app1, db1)
		}
	}

	// Disarmed (as at boot), then started with nothing to recreate: the
	// recorded containers get their restart policy back.
	cs, _ := e.Docker.ListByLabel(ctx, "fog.app="+b)
	for _, c := range cs {
		e.Docker.SetRestartPolicy(ctx, c.ID, "no")
	}
	if err := e.Start(ctx, b, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if pol, _ := docker("inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", c.ID); pol != "unless-stopped" {
			t.Errorf("%s kept restart policy %q", c.ID, pol)
		}
	}
}
