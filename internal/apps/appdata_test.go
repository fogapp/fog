package apps

import "testing"

func TestAppDataFolder(t *testing.T) {
	for in, want := range map[string]string{
		"paperless": "Apps/Paperless",
		"Immich":    "Apps/Immich",
		"my-app":    "Apps/My-app",
		" hook ":    "Apps/Hook",
		"":          "Apps",
	} {
		if got := AppDataFolder(in); got != want {
			t.Errorf("AppDataFolder(%q) = %q, want %q", in, got, want)
		}
	}
}

// The line the app's backups draw: its own folder under Apps travels in
// its snapshot; a shared library, another app's folder and a host path do
// not.
func TestVolumeIsAppData(t *testing.T) {
	cases := []struct {
		v    Volume
		want bool
	}{
		{Volume{Files: "Apps/Paperless"}, true},
		{Volume{Files: "Apps/Paperless/config"}, true},
		{Volume{Files: "apps/paperless/config"}, true}, // however it is spelled
		{Volume{Files: "Apps/PaperlessNG/config"}, false},
		{Volume{Files: "Apps/Immich/upload"}, false},
		{Volume{Files: "Content/Scans"}, false},
		{Volume{Files: "Photos"}, false},
		{Volume{Host: "/mnt/media"}, false},
		{Volume{}, false},
	}
	for _, c := range cases {
		if got := c.v.IsAppData("paperless"); got != c.want {
			t.Errorf("Volume{Files:%q, Host:%q}.IsAppData(paperless) = %v, want %v", c.v.Files, c.v.Host, got, c.want)
		}
	}
}
