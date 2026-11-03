package fsx

import "testing"

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0:             "0 B",
		1023:          "1023 B",
		1024:          "1.0 KB",
		1536:          "1.5 KB",
		5 << 30:       "5.0 GB",
		3<<40 + 1<<39: "3.5 TB",
	} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
