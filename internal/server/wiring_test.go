package server

import (
	"context"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

func latestSpec(t *testing.T, s *Server, appID int64) *apps.AppSpec {
	t.Helper()
	_, spec, err := apps.SpecAt(s.store, appID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// Installing a consumer mounts the shared folders of the apps it uses that
// are already installed; installing the provider afterwards wires the
// consumer that was waiting for it.
func TestWiringOnInstall(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t)
	s.engine = &apps.Engine{Store: s.store, Vault: s.vault}
	streamSpec := apps.AppSpec{
		Source:     apps.Source{Type: "image", Image: "stream"},
		Volumes:    []apps.Volume{{Name: "movies", Mount: "/media/movies", Host: "/srv/content/Movies"}},
		Connection: &apps.Connection{Port: 80, Shares: []string{"movies"}},
	}
	streamID, _ := appWithToken(t, s, "stream")
	withSpec(t, s, streamID, streamSpec)

	hook := apps.AppSpec{
		Source:  apps.Source{Type: "image", Image: "hook"},
		Uses:    []string{"stream", "sabnzbd"},
		Volumes: []apps.Volume{{Name: "storage", Mount: "/rails/storage"}},
	}
	added, changed := s.wireSpec("hook", &hook)
	if !changed || len(added) != 1 || added[0] != "stream-movies" {
		t.Fatalf("added %v changed %v", added, changed)
	}
	if err := s.engine.Create(context.Background(), "hook", &hook, nil, "test"); err != nil {
		t.Fatal(err)
	}

	// SABnzbd arrives later; Hook is rewired to see its downloads.
	sabSpec := apps.AppSpec{
		Source:     apps.Source{Type: "image", Image: "sab"},
		Volumes:    []apps.Volume{{Name: "media", Mount: "/data", Host: "/srv/content/Downloads"}},
		Connection: &apps.Connection{Port: 8080, Shares: []string{"media"}},
	}
	if err := s.engine.Create(context.Background(), "sabnzbd", &sabSpec, nil, "test"); err != nil {
		t.Fatal(err)
	}
	s.wireConsumersOf(context.Background(), "sabnzbd", "test")
	hookApp, _ := s.store.AppByName("hook")
	got := latestSpec(t, s, hookApp.ID)
	var follows []string
	for _, v := range got.Volumes {
		if v.Follows != "" {
			follows = append(follows, v.Follows+"@"+v.Mount+"="+v.Host)
		}
	}
	want := []string{"stream/movies@/shared/stream/movies=/srv/content/Movies", "sabnzbd/media@/shared/sabnzbd/media=/srv/content/Downloads"}
	if len(follows) != 2 || follows[0] != want[0] || follows[1] != want[1] {
		t.Fatalf("follows = %v", follows)
	}

	// SABnzbd's folder moves; Hook's follower catches up.
	sabSpec.Volumes[0].Host = "/srv/elsewhere/Downloads"
	if err := s.engine.UpdateSpec(context.Background(), "sabnzbd", &sabSpec, nil, "move", "test"); err != nil {
		t.Fatal(err)
	}
	s.onSpecChanged(context.Background(), "sabnzbd")
	got = latestSpec(t, s, hookApp.ID)
	for _, v := range got.Volumes {
		if v.Follows == "sabnzbd/media" && v.Host != "/srv/elsewhere/Downloads" {
			t.Fatalf("follower not moved: %+v", v)
		}
	}
}
