//go:build linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// An app whose bind source isn't there yet — a NAS share still mounting —
// waits for it, through the real pin helper (`fogd pin-bind` under
// systemd-run): the helper's not-found reaches the engine as not-found, not
// as a refusal that removes the app. It comes up once the folder is there;
// a link in its place is still refused. Run as TestPinThroughSystemd is:
//
//	GOOS=linux go test -c -o /usr/local/bin/fogd.test ./cmd/fogd
//	systemd-run --wait --pipe -p ProtectSystem=full -p ProtectHome=read-only \
//	  -p PrivateTmp=yes -p ReadWritePaths=/var/lib \
//	  /usr/local/bin/fogd.test -test.run WaitsThroughThePinHelper -test.v
func TestAppWaitsThroughThePinHelper(t *testing.T) {
	if os.Getenv("INVOCATION_ID") == "" || os.Geteuid() != 0 {
		t.Skip("run as root inside a systemd unit (see the comment)")
	}
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("docker compose not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		out, _ := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	const image = "caddy:2.10-alpine"
	base, err := os.MkdirTemp("/var/lib", "fogwait")
	if err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(base, "nas")
	os.MkdirAll(share, 0o755)
	st, err := storetest.Open(filepath.Join(base, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, err := vault.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	m := newMaterializer(filepath.Join(base, "data"), false)
	if m.Pin == nil {
		t.Fatal("no pinning on Linux as root")
	}
	m.ManagedRoots = func() []string { return []string{share} }
	e := &apps.Engine{Store: st, Vault: vlt, Docker: dockerx.New(), Mat: m, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	name := "waits" + strings.ToLower(strings.ReplaceAll(filepath.Base(base), "fogwait", ""))
	t.Cleanup(func() {
		c, cl := context.WithTimeout(context.Background(), time.Minute)
		defer cl()
		m.ComposeCmd(c, name, "down").Run()
		e.RemoveAppNetworks(c, name)
		m.Unpin(c, filepath.Join(m.BindsDir(), name))
		exec.Command("systemd-run", "--quiet", "--wait", "--collect", "--", "umount", "-l", m.BindsDir()).Run()
		os.RemoveAll(base)
	})
	spec := &apps.AppSpec{Source: apps.Source{Type: "image", Image: image}, Command: []string{"sleep", "3600"},
		Volumes: []apps.Volume{{Name: "media", Mount: "/media", Host: filepath.Join(share, "media")}}}
	a, _ := st.CreateApp(name)
	b, _ := json.Marshal(spec)
	rev, _ := st.CreateRevision(a.ID, string(b), "", "t")
	st.SetAppCurrentRev(a.ID, rev.Rev)
	st.SetAppDesiredState(a.ID, "running")
	st.RecordAppVersion(a.ID, "image", "sha256:x", image, rev.Rev, "t")

	// The share's folder isn't there yet.
	if err := e.Start(ctx, name, "system"); err == nil {
		t.Fatal("started with its folder missing")
	}
	if why := e.Waiting()[name]; why == "" {
		t.Fatalf("not waiting; refused: %q", e.RulesRefused()[name])
	}
	if why := e.RulesRefused()[name]; why != "" {
		t.Fatalf("the helper's not-found read as a refusal: %s", why)
	}

	// The share comes up; whatever wakes the app, it starts.
	os.MkdirAll(filepath.Join(share, "media"), 0o755)
	os.WriteFile(filepath.Join(share, "media", "song.flac"), []byte("x"), 0o644)
	e.RestartAppsUnder(ctx, share, "nas")
	id := docker("ps", "-q", "--filter", "label=fog.app="+name)
	if id == "" || !strings.Contains(docker("exec", id, "ls", "/media"), "song.flac") {
		t.Fatalf("not up on its share after it mounted (container %q, waiting %q)", id, e.Waiting()[name])
	}

	// A link where the folder was is still a refusal, through the helper.
	os.Rename(filepath.Join(share, "media"), filepath.Join(share, "media.old"))
	os.Symlink("/etc", filepath.Join(share, "media"))
	if err := e.Restart(ctx, name, "system"); err == nil {
		t.Fatal("restarted through a link to /etc")
	}
	if e.Waiting()[name] != "" || e.RulesRefused()[name] == "" {
		t.Fatalf("a link read as storage not ready (waiting %q)", e.Waiting()[name])
	}
	if id := docker("ps", "-aq", "--filter", "label=fog.app="+name); id != "" {
		t.Fatalf("the refused app's containers were left: %s", id)
	}
}
