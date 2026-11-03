package dockerx

import "testing"

// Pinning names one exact manifest. The name is everything before the tag —
// and a colon inside a registry host's port is not a tag, so cutting at the
// wrong colon would leave a reference to a machine rather than an image.
func TestPinDigest(t *testing.T) {
	const d = "sha256:abc"
	cases := []struct{ ref, want string }{
		{"lscr.io/linuxserver/jellyfin:latest", "lscr.io/linuxserver/jellyfin@" + d},
		{"lscr.io/linuxserver/jellyfin", "lscr.io/linuxserver/jellyfin@" + d},
		{"nas.lan:5000/media/app", "nas.lan:5000/media/app@" + d},
		{"nas.lan:5000/media/app:v2", "nas.lan:5000/media/app@" + d},
		{"postgres:16", "postgres@" + d},
		{"x/y@sha256:old", "x/y@" + d},
	}
	for _, c := range cases {
		if got := PinDigest(c.ref, d); got != c.want {
			t.Errorf("PinDigest(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
	// No digest to pin: the reference is left exactly as it was, so a
	// deploy that could not resolve a version runs the tag rather than a
	// mangled name.
	if got := PinDigest("x/y:latest", ""); got != "x/y:latest" {
		t.Errorf("PinDigest with no digest = %q", got)
	}
}
