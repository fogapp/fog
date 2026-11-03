package issues

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
)

func TestAppUnwired(t *testing.T) {
	sab := &apps.AppSpec{
		Volumes:    []apps.Volume{{Name: "media", Mount: "/data", Files: "Content/Downloads"}},
		Connection: &apps.Connection{Port: 8080, Shares: []string{"media"}},
	}
	blind := &apps.AppSpec{Uses: []string{"sabnzbd", "events", "missing"}, Volumes: []apps.Volume{{Name: "storage", Mount: "/rails/storage"}}}
	sighted := &apps.AppSpec{Uses: []string{"sabnzbd"}, Volumes: []apps.Volume{{Name: "content", Mount: "/data", Files: "Content"}}}
	sn := &snapshot{filesRoot: "/srv/files", apps: []appInfo{appWith(1, "sabnzbd", sab), appWith(2, "hook", blind), appWith(3, "hud", sighted)}}
	rep := checkAppUnwired(context.Background(), sn)
	if len(rep.evaluated) != 3 {
		t.Fatalf("evaluated %v", rep.evaluated)
	}
	if len(rep.findings) != 1 || rep.findings[0].AppName != "hook" || rep.findings[0].Subject != "sabnzbd" || rep.findings[0].Kind != "app-unwired" {
		t.Fatalf("findings %+v", rep.findings)
	}
}

func TestMountStale(t *testing.T) {
	spec := &apps.AppSpec{Volumes: []apps.Volume{
		{Name: "storage", Mount: "/rails/storage"},
		{Name: "downloads", Mount: "/data/downloads", Files: "Content/Downloads"},
		{Name: "movies", Mount: "/data/movies", Files: "Content/Movies"},
	}}
	running := func() appInfo {
		a := appWith(1, "hook", spec)
		a.main = &dockerx.ContainerSummary{State: "running"}
		return a
	}
	scan := func(out string, err error) report {
		sc := &Scanner{Log: slog.Default(), ExecInApp: func(_ context.Context, _ string, _ time.Duration, argv ...string) (string, error) {
			if len(argv) != 4 || argv[2] != "/data/downloads" {
				t.Fatalf("argv %v", argv)
			}
			return out, err
		}}
		return sc.checkMountStale(context.Background(), &snapshot{dockerOK: true, apps: []appInfo{running()}})
	}
	rep := scan("", errors.New("exec in hook: ls: cannot access '/data/downloads': Stale file handle\n/data/movies"))
	if len(rep.findings) != 1 || rep.findings[0].Subject != "/data/downloads" || len(rep.evaluated) != 1 {
		t.Fatalf("stale: %+v", rep)
	}
	if rep := scan("/data/downloads\n/data/movies\n", nil); len(rep.findings) != 0 || len(rep.evaluated) != 1 {
		t.Fatalf("healthy: %+v", rep)
	}
	if rep := scan("", errors.New(`exec: "ls": executable file not found`)); len(rep.evaluated) != 0 {
		t.Fatalf("no ls is no verdict: %+v", rep)
	}
}
