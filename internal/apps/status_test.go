package apps

import "testing"

func TestShortRef(t *testing.T) {
	cases := map[string]string{
		"sha256:0123456789abcdef0123456789abcdef": "0123456789ab",
		"0123456789abcdef":                        "0123456789ab",
		"abc123":                                  "abc123",
		"":                                        "",
	}
	for in, want := range cases {
		if got := ShortRef(in); got != want {
			t.Errorf("ShortRef(%q) = %q, want %q", in, got, want)
		}
	}
}
