package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/store"
)

// The list is the system disk, then every share (mounted or not), then a
// filesystem fog has no record of but a volume points at. Each carries
// what depends on it.
func TestAssembleDisks(t *testing.T) {
	shares := []nas.ShareStatus{
		{NasShare: store.NasShare{Name: "media", MountPoint: "/mnt/nas/media"}, Mounted: true, TotalBytes: 1000, FreeBytes: 400},
		{NasShare: store.NasShare{Name: "vault", MountPoint: "/mnt/nas/vault"}, Mounted: false},
	}
	mounts := map[string]bool{"/": true, "/mnt/nas/media": true, "/mnt/usb": true, "/boot": true}
	p := diskPaths{
		dataDir:   "/var/lib/fog",
		filesRoot: "/mnt/nas/media/fog-files",
		backupDir: "/mnt/nas/vault/backups",
		apps: map[string][]string{
			"feed":    {"/var/lib/fog/data/feed", "/mnt/nas/media/library"},
			"manor":   {"/var/lib/fog/data/manor"},
			"photos":  {"/mnt/usb/photos"},
			"archive": {"/mnt/nas/vault/archive", "/mnt/nas/vault/more"},
		},
	}
	usage := func(path string) (uint64, uint64, error) {
		switch path {
		case "/":
			return 500, 100, nil
		case "/mnt/usb":
			return 2000, 1500, nil
		}
		t.Errorf("unexpected statfs of %q", path)
		return 0, 0, nil
	}
	got := assembleDisks(p, shares, mounts, usage, nil, time.Time{})
	if len(got) != 4 {
		t.Fatalf("got %d disks, want 4: %+v", len(got), got)
	}
	local := got[0]
	if local.ID != "local" || local.MountPoint != "/" || !local.Mounted || local.UsedBytes != 400 || local.TotalBytes != 500 {
		t.Errorf("local = %+v", local)
	}
	if !local.Uses.Fog || local.Uses.Drive || local.Uses.Backups || len(local.Uses.Apps) != 2 || local.Uses.Apps[0] != "feed" || local.Uses.Apps[1] != "manor" {
		t.Errorf("local uses = %+v", local.Uses)
	}
	media := got[1]
	if media.ID != "nas:media" || media.Share == nil || !media.Mounted || media.UsedBytes != 600 {
		t.Errorf("media = %+v", media)
	}
	if !media.Uses.Drive || media.Uses.Fog || media.Uses.Backups || len(media.Uses.Apps) != 1 || media.Uses.Apps[0] != "feed" {
		t.Errorf("media uses = %+v", media.Uses)
	}
	// Unmounted, so the mount table would put its paths on "/" — but the
	// dependency is on the share, and the backup dir stays with it.
	vault := got[2]
	if vault.ID != "nas:vault" || vault.Mounted || vault.TotalBytes != 0 {
		t.Errorf("vault = %+v", vault)
	}
	if !vault.Uses.Backups || len(vault.Uses.Apps) != 1 || vault.Uses.Apps[0] != "archive" {
		t.Errorf("vault uses = %+v", vault.Uses)
	}
	usb := got[3]
	if usb.ID != "fs:/mnt/usb" || usb.Kind != "other" || usb.Name != "usb" || usb.TotalBytes != 2000 || usb.FreeBytes != 1500 {
		t.Errorf("usb = %+v", usb)
	}
	if len(usb.Uses.Apps) != 1 || usb.Uses.Apps[0] != "photos" {
		t.Errorf("usb uses = %+v", usb.Uses)
	}
	// No sizing pass yet: no breakdown on any disk.
	for _, d := range got {
		if d.Usage != nil {
			t.Errorf("%s has usage before a sizing pass: %+v", d.ID, d.Usage)
		}
	}
}

// With a sizing pass, each disk's used space splits into the categories
// fog sized on it, in bar order, with the unsized rest as "other" — and a
// disk with no reading gets no breakdown at all.
func TestAssembleDisksUsage(t *testing.T) {
	shares := []nas.ShareStatus{
		{NasShare: store.NasShare{Name: "media", MountPoint: "/mnt/nas/media"}, Mounted: true, TotalBytes: 1000, FreeBytes: 400},
		{NasShare: store.NasShare{Name: "vault", MountPoint: "/mnt/nas/vault"}, Mounted: false},
	}
	mounts := map[string]bool{"/": true, "/mnt/nas/media": true}
	p := diskPaths{dataDir: "/var/lib/fog", filesRoot: "/var/lib/fog/files", backupDir: "/mnt/nas/vault/backups",
		apps: map[string][]string{"feed": {"/var/lib/fog/data/feed", "/mnt/nas/media/library"}}}
	usage := func(string) (uint64, uint64, error) { return 500, 100, nil }
	sized := []sizedPath{
		{path: "/var/lib/fog/data", category: "apps", bytes: 100},
		{path: "/var/lib/fog/files", category: "drive", bytes: 50},
		{path: "/var/lib/fog/postgres", category: "postgres", bytes: 30},
		{path: "/var/lib/fog/fog.db", category: "fog", bytes: 20},
		{path: "/var/lib/fog/chat", category: "fog", bytes: 0},
		{path: "/mnt/nas/vault/backups", category: "backups", bytes: 70},
		{path: "/mnt/nas/media/library", category: "apps", bytes: 450},
		{path: "/var/lib/docker", category: "docker", bytes: 120, reclaimable: 40},
	}
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	got := assembleDisks(p, shares, mounts, usage, sized, at)
	local := got[0].Usage
	if local == nil || !local.At.Equal(at) {
		t.Fatalf("local usage = %+v", local)
	}
	want := []diskSegment{
		{Key: "apps", Label: "Apps", Bytes: 100}, {Key: "drive", Label: "Drive", Bytes: 50}, {Key: "postgres", Label: "Postgres", Bytes: 30},
		{Key: "fog", Label: "Fog", Bytes: 20}, {Key: "docker", Label: "Docker", Bytes: 120, Reclaimable: 40}, {Key: "other", Label: "Other", Bytes: 80},
	}
	if len(local.Segments) != len(want) {
		t.Fatalf("local segments = %+v, want %+v", local.Segments, want)
	}
	for i, seg := range want {
		if local.Segments[i] != seg {
			t.Errorf("local segment %d = %+v, want %+v", i, local.Segments[i], seg)
		}
	}
	media := got[1].Usage
	if media == nil || len(media.Segments) != 2 || media.Segments[0] != (diskSegment{Key: "apps", Label: "Apps", Bytes: 450}) || media.Segments[1] != (diskSegment{Key: "other", Label: "Other", Bytes: 150}) {
		t.Errorf("media usage = %+v", media)
	}
	if got[2].Usage != nil {
		t.Errorf("unmounted vault has usage: %+v", got[2].Usage)
	}
}

// The data directory's top-level entries are categorised by name, the
// backup directory and Drive root are added wherever they live, a
// volume inside something already listed is not counted twice, and
// Docker's data-root is left to Docker.
func TestOwnedPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"data", "postgres", "files", "chat", "docker-root"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Docker's data-root inside the data directory (the installer's
	// layout) is Docker's to size, not a "fog" directory to walk.
	p := diskPaths{dataDir: dir, filesRoot: filepath.Join(dir, "files"), backupDir: "/mnt/nas/vault/backups", dockerRoot: filepath.Join(dir, "docker-root"),
		apps: map[string][]string{
			"feed":   {filepath.Join(dir, "data", "feed"), "/mnt/nas/media/library"},
			"photos": {filepath.Join(dir, "files", "Apps", "Photos"), "/mnt/nas/media/library/sub"},
		}}
	got := ownedPaths(p)
	want := map[string]string{
		filepath.Join(dir, "chat"): "fog", filepath.Join(dir, "data"): "apps", filepath.Join(dir, "files"): "drive",
		filepath.Join(dir, "postgres"): "postgres", "/mnt/nas/vault/backups": "backups", "/mnt/nas/media/library": "apps",
	}
	if len(got) != len(want) {
		t.Fatalf("owned = %+v, want %d entries", got, len(want))
	}
	for _, sp := range got {
		if want[sp.path] != sp.category {
			t.Errorf("%s = %q, want %q", sp.path, sp.category, want[sp.path])
		}
	}
}

// The sizer answers from the last pass and refreshes in the background:
// nothing on the first call, then the figures, then no re-walk until the
// pass is stale or a new path appears.
func TestDiskSizer(t *testing.T) {
	walks := 0
	d := diskSizer{walk: func(_ context.Context, path string) int64 { walks++; return int64(len(path)) }}
	var spawned []func()
	spawn := func(fn func()) { spawned = append(spawned, fn) }
	if pass := d.sizes(context.Background(), []string{"/a", "/bb"}, spawn); pass != nil {
		t.Errorf("sizes before a pass = %v", pass)
	}
	if len(spawned) != 1 {
		t.Fatalf("spawned %d passes, want 1", len(spawned))
	}
	// A second poll while the pass runs does not start another.
	d.sizes(context.Background(), []string{"/a", "/bb"}, spawn)
	if len(spawned) != 1 {
		t.Fatalf("spawned %d passes while running, want 1", len(spawned))
	}
	spawned[0]()
	pass := d.sizes(context.Background(), []string{"/a", "/bb"}, spawn)
	if pass == nil || pass.sizes["/a"] != 2 || pass.sizes["/bb"] != 3 || pass.at.IsZero() || pass.docker != nil || walks != 2 || len(spawned) != 1 {
		t.Errorf("after pass: pass=%+v walks=%d spawned=%d", pass, walks, len(spawned))
	}
	// A path the pass didn't cover triggers a new one.
	d.sizes(context.Background(), []string{"/a", "/bb", "/ccc"}, spawn)
	if len(spawned) != 2 {
		t.Errorf("new path spawned %d passes, want 2", len(spawned))
	}
}

// With nothing configured the answer is one disk carrying everything —
// and its app list is an empty array, never null.
func TestAssembleDisksBare(t *testing.T) {
	got := assembleDisks(diskPaths{dataDir: "/var/lib/fog", filesRoot: "/var/lib/fog/files", backupDir: "/var/lib/fog/backups"}, nil, nil, nil, nil, time.Time{})
	if len(got) != 1 {
		t.Fatalf("got %d disks, want 1", len(got))
	}
	u := got[0].Uses
	if !u.Fog || !u.Drive || !u.Backups || u.Apps == nil || len(u.Apps) != 0 {
		t.Errorf("uses = %+v", u)
	}
}

func TestSystemDisks(t *testing.T) {
	s, h := newTestServer(t)
	c := setup(t, h)
	if w := do(t, h, "GET", "/api/system/disks", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: got %d, want 401", w.Code)
	}
	app, err := s.store.CreateApp("feed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateRevision(app.ID, `{"source":{"type":"image","image":"x"},"volumes":[{"name":"data","mount":"/data"}]}`, "", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateNasShare(store.NasShare{Name: "media", Proto: "nfs", Server: "nas.lan", Remote: "/volume1/media", MountPoint: "/mnt/nas/media", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "GET", "/api/system/disks", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("disks: %d %s", w.Code, w.Body)
	}
	var got struct {
		Disks []diskEntry `json:"disks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Disks) != 2 || got.Disks[0].ID != "local" || got.Disks[1].ID != "nas:media" {
		t.Fatalf("disks = %+v", got.Disks)
	}
	local := got.Disks[0]
	if local.TotalBytes == 0 || !local.Uses.Fog || !local.Uses.Drive || !local.Uses.Backups {
		t.Errorf("local = %+v", local)
	}
	if len(local.Uses.Apps) != 1 || local.Uses.Apps[0] != "feed" {
		t.Errorf("local apps = %v", local.Uses.Apps)
	}
	if got.Disks[1].Share == nil || got.Disks[1].Share.Server != "nas.lan" || got.Disks[1].Mounted {
		t.Errorf("share = %+v", got.Disks[1])
	}
}

// A pass runs under the context sizes was handed — the server's background
// context — so Shutdown reaches the walk, and a pass cut short leaves the
// last whole one standing rather than figures that undercount.
func TestDiskSizerStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var walkCtx context.Context
	d := diskSizer{walk: func(c context.Context, path string) int64 { walkCtx = c; cancel(); return 1 }}
	var spawned []func()
	d.sizes(ctx, []string{"/a"}, func(fn func()) { spawned = append(spawned, fn) })
	spawned[0]()
	if walkCtx != ctx {
		t.Error("the walk did not run under the context sizes was given")
	}
	if pass := d.sizes(context.Background(), []string{"/a"}, func(fn func()) { spawned = append(spawned, fn) }); pass != nil {
		t.Errorf("a cancelled pass was kept: %+v", pass)
	}
	if len(spawned) != 2 {
		t.Errorf("spawned %d passes, want a fresh one after the cancelled pass", len(spawned))
	}
}

// A pass asks Docker once, and Docker's data-root is never walked — its
// bytes are Docker's own count — even when the poll listed it.
func TestDiskSizerDocker(t *testing.T) {
	walked := []string{}
	d := diskSizer{
		walk: func(_ context.Context, path string) int64 { walked = append(walked, path); return 1 },
		docker: func(context.Context) (*dockerUsage, error) {
			return &dockerUsage{root: "/bb", bytes: 77, reclaimable: 7}, nil
		},
	}
	var spawned []func()
	d.sizes(context.Background(), []string{"/a", "/bb"}, func(fn func()) { spawned = append(spawned, fn) })
	if len(spawned) != 1 {
		t.Fatalf("spawned %d passes, want 1", len(spawned))
	}
	spawned[0]()
	if got := d.dockerRoot(); got != "/bb" {
		t.Errorf("dockerRoot = %q, want /bb", got)
	}
	pass := d.sizes(context.Background(), []string{"/a"}, func(func()) { t.Error("re-ran with the root left out") })
	if pass == nil || pass.docker == nil || pass.docker.bytes != 77 || len(walked) != 1 || walked[0] != "/a" {
		t.Errorf("pass=%+v walked=%v", pass, walked)
	}
	if _, ok := pass.sizes["/bb"]; ok {
		t.Error("docker root was walked")
	}
}
