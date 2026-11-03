package config

import "testing"

// The marker install.sh finds in the binary never reaches anything that
// reads Version.
func TestVersionStampIsRemoved(t *testing.T) {
	for in, want := range map[string]string{"fogd-version=v1.2.3": "v1.2.3", "v1.2.3": "v1.2.3", "dev": "dev"} {
		if got := unstamp(in); got != want {
			t.Errorf("unstamp(%q) = %q, want %q", in, got, want)
		}
	}
}
