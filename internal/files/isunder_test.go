package files

import "testing"

func TestIsUnder(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		rel, dir string
		want     bool
	}{
		{"photos", "", true},
		{"", "", true},
		{"photos", "photos", true},
		{"photos/2024/a.jpg", "photos", true},
		{"photos2", "photos", false},
		{"photo", "photos", false},
		{"Photos/x", "photos", false},
		{"/mnt/media/films", "/mnt/media", true},
		{"/mnt/mediabox", "/mnt/media", false},
	} {
		if got := IsUnder(c.rel, c.dir); got != c.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", c.rel, c.dir, got, c.want)
		}
	}
}
