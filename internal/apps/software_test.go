package apps

import "testing"

func TestSameSoftware(t *testing.T) {
	for _, c := range []struct {
		declared, identity string
		want               bool
	}{
		{"hub", "hub", true},
		{"manor", "fin", true},
		{"fin", "manor", false},
		{"hub", "fin", false},
	} {
		if got := SameSoftware(c.declared, c.identity); got != c.want {
			t.Errorf("SameSoftware(%q, %q) = %v, want %v", c.declared, c.identity, got, c.want)
		}
	}
}
