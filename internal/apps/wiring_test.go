package apps

import (
	"reflect"
	"testing"
)

const wiringRoot = "/srv/files"

func sab() *AppSpec {
	return &AppSpec{
		Volumes: []Volume{
			{Name: "config", Mount: "/config"},
			{Name: "media", Mount: "/data", Files: "Content/Downloads"},
		},
		Connection: &Connection{Port: 8080, Shares: []string{"media"}},
	}
}

func stream() *AppSpec {
	return &AppSpec{
		Volumes: []Volume{
			{Name: "storage", Mount: "/rails/storage"},
			{Name: "movies", Mount: "/media/movies", Files: "Content/Movies"},
			{Name: "tv", Mount: "/media/tv", Files: "Content/TV"},
			{Name: "stock", Mount: "/media/stock", Host: "/mnt/stock"},
		},
		Connection: &Connection{Port: 80, Shares: []string{"movies", "tv", "stock"}},
	}
}

// The layout that broke on fog.lan: SABnzbd sees Content/Downloads at
// /data, Hook at /data/downloads.
func TestTranslatePathsEqualFolders(t *testing.T) {
	hook := &AppSpec{Volumes: []Volume{
		{Name: "storage", Mount: "/rails/storage"},
		{Name: "downloads", Mount: "/data/downloads/", Files: "Content/Downloads"},
		{Name: "movies", Mount: "/data/movies", Files: "Content/Movies"},
	}}
	got := TranslatePaths(sab(), hook, wiringRoot)
	want := []PathRow{{Remote: "/data", Local: "/data/downloads"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if u := UnseenShares(sab(), hook, wiringRoot); len(u) != 0 {
		t.Fatalf("unseen %v", u)
	}
}

// Hook mounting the whole Content tree sees every library inside it.
func TestTranslatePathsNested(t *testing.T) {
	hook := &AppSpec{Volumes: []Volume{{Name: "content", Mount: "/data", Files: "Content"}}}
	got := TranslatePaths(stream(), hook, wiringRoot)
	want := []PathRow{
		{Remote: "/media/movies", Local: "/data/Movies"},
		{Remote: "/media/tv", Local: "/data/TV"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if u := UnseenShares(stream(), hook, wiringRoot); !reflect.DeepEqual(u, []string{"stock"}) {
		t.Fatalf("unseen %v, want [stock]", u)
	}

	// And the other way round: the provider mounts the wider folder.
	wide := &AppSpec{Volumes: []Volume{{Name: "all", Mount: "/data", Files: "Content"}}}
	narrow := &AppSpec{Volumes: []Volume{{Name: "dl", Mount: "/downloads", Files: "Content/Downloads"}}}
	if got := TranslatePaths(wide, narrow, wiringRoot); !reflect.DeepEqual(got, []PathRow{{Remote: "/data/Downloads", Local: "/downloads"}}) {
		t.Fatalf("got %v", got)
	}
}

func TestTranslatePathsIgnoresManagedAndUnrelated(t *testing.T) {
	a := &AppSpec{Volumes: []Volume{{Name: "config", Mount: "/config"}, {Name: "x", Mount: "/x", Files: "Content/Photos"}}}
	b := &AppSpec{Volumes: []Volume{{Name: "config", Mount: "/config"}, {Name: "y", Mount: "/y", Files: "Content/Photosynthesis"}}}
	if got := TranslatePaths(a, b, wiringRoot); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestWireSharesAddsFollowingVolumes(t *testing.T) {
	hook := &AppSpec{Volumes: []Volume{{Name: "content", Mount: "/data", Files: "Content"}}}
	added := WireShares(hook, "stream", stream(), wiringRoot)
	if !reflect.DeepEqual(added, []string{"stream-stock"}) {
		t.Fatalf("added %v", added)
	}
	v := hook.volume("stream-stock")
	if v.Mount != "/shared/stream/stock" || v.Host != "/mnt/stock" || v.Follows != "stream/stock" {
		t.Fatalf("volume %+v", v)
	}
	if err := (&AppSpec{Source: Source{Type: "image", Image: "x"}, Volumes: hook.Volumes}).Validate(); err != nil {
		t.Fatalf("wired spec invalid: %v", err)
	}
	if u := UnseenShares(stream(), hook, wiringRoot); len(u) != 0 {
		t.Fatalf("still unseen %v", u)
	}
	// Wiring again adds nothing.
	if again := WireShares(hook, "stream", stream(), wiringRoot); len(again) != 0 {
		t.Fatalf("second wire added %v", again)
	}
}

func TestResolveFollowsTracksAMove(t *testing.T) {
	hook := &AppSpec{Uses: []string{"sabnzbd"}, Volumes: []Volume{{Name: "downloads", Mount: "/data/downloads", Files: "Content/Downloads", Follows: "sabnzbd/media"}}}
	moved := sab()
	moved.Volumes[1].Files = "Apps/Sabnzbd/media"
	lookup := func(app string) *AppSpec {
		if app == "sabnzbd" {
			return moved
		}
		return nil
	}
	if !ResolveFollows(hook, lookup) {
		t.Fatal("expected a change")
	}
	if hook.Volumes[0].Files != "Apps/Sabnzbd/media" {
		t.Fatalf("files %q", hook.Volumes[0].Files)
	}
	if ResolveFollows(hook, lookup) {
		t.Fatal("second resolve should be a no-op")
	}
	// A target that is not installed leaves the follower alone.
	if ResolveFollows(hook, func(string) *AppSpec { return nil }) {
		t.Fatal("missing target changed the spec")
	}
}

func TestValidateWiringFields(t *testing.T) {
	base := func() *AppSpec {
		return &AppSpec{Source: Source{Type: "image", Image: "x"}, Volumes: []Volume{{Name: "media", Mount: "/data", Files: "Content/Downloads"}}}
	}
	s := base()
	s.Connection = &Connection{Port: 8080, Shares: []string{"nope"}}
	if err := s.Validate(); err == nil {
		t.Error("a share naming no volume should be refused")
	}
	s = base()
	s.Volumes[0].Follows = "sabnzbd"
	if err := s.Validate(); err == nil {
		t.Error("follows without a volume should be refused")
	}
	s = base()
	s.Volumes[0].Follows = "sabnzbd/media"
	s.Connection = &Connection{Port: 8080, Shares: []string{"media"}}
	if err := s.Validate(); err != nil {
		t.Errorf("valid wiring refused: %v", err)
	}
}

// The attack: a spec follows another app's own data — a volume it doesn't
// share — or follows a share of an app it doesn't use. Neither copies the
// target's folder in, so neither mounts it.
func TestResolveFollowsOnlyCopiesSharesOfUsedApps(t *testing.T) {
	vault := &AppSpec{
		Volumes:    []Volume{{Name: "data", Mount: "/data", Files: "Apps/Vault/data"}, {Name: "public", Mount: "/pub", Files: "Content/Pub"}},
		Connection: &Connection{Port: 80, Shares: []string{"public"}},
	}
	lookup := func(app string) *AppSpec {
		if app == "vault" {
			return vault
		}
		return nil
	}
	thief := &AppSpec{Uses: []string{"vault"}, Volumes: []Volume{{Name: "loot", Mount: "/loot", Files: "Content/Empty", Follows: "vault/data"}}}
	if ResolveFollows(thief, lookup) || thief.Volumes[0].Files != "Content/Empty" {
		t.Fatalf("followed an unshared volume: %+v", thief.Volumes[0])
	}
	stranger := &AppSpec{Volumes: []Volume{{Name: "pub", Mount: "/pub", Files: "Content/Empty", Follows: "vault/public"}}}
	if ResolveFollows(stranger, lookup) || stranger.Volumes[0].Files != "Content/Empty" {
		t.Fatalf("followed a share of an app it doesn't use: %+v", stranger.Volumes[0])
	}
	user := &AppSpec{Uses: []string{"vault"}, Volumes: []Volume{{Name: "pub", Mount: "/pub", Files: "Content/Empty", Follows: "vault/public"}}}
	if !ResolveFollows(user, lookup) || user.Volumes[0].Files != "Content/Pub" {
		t.Fatalf("a used app's share wasn't followed: %+v", user.Volumes[0])
	}
}
