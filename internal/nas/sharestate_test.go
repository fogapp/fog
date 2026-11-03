package nas

import (
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func TestShareState(t *testing.T) {
	shares := []store.NasShare{
		{Name: "media", MountPoint: "/mnt/media"},
		{Name: "dead", MountPoint: "/mnt/dead"},
		{Name: "gone", MountPoint: "/mnt/gone"},
	}
	mounted := map[string]bool{"/mnt/media": true, "/mnt/dead": true}
	hung := func(p string) bool { return p == "/mnt/dead" }
	for _, c := range []struct{ path, name, reason string }{
		{"/srv/data", "", ""},
		{"/mnt/media", "", ""},
		{"/mnt/media/films", "", ""},
		{"/mnt/mediabox", "", ""}, // a sibling sharing a prefix is not under the share
		{"/mnt/dead/x", "dead", "mounted but not responding"},
		{"/mnt/gone", "gone", "not mounted"},
	} {
		name, reason := ShareState(shares, c.path, mounted, hung)
		if name != c.name || reason != c.reason {
			t.Errorf("ShareState(%q) = %q, %q; want %q, %q", c.path, name, reason, c.name, c.reason)
		}
	}
}
