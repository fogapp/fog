package randx

import (
	"regexp"
	"testing"
)

func TestHexShape(t *testing.T) {
	for _, n := range []int{3, 8, 16, 24, 32} {
		got := Hex(n)
		if !regexp.MustCompile(`^[0-9a-f]+$`).MatchString(got) || len(got) != 2*n {
			t.Errorf("Hex(%d) = %q", n, got)
		}
	}
	if Hex(16) == Hex(16) {
		t.Error("two draws matched")
	}
}

func TestTokenShape(t *testing.T) {
	got := Token()
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(got) {
		t.Errorf("Token() = %q", got)
	}
}

func TestULIDShape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	a, b := ULID(), ULID()
	if !re.MatchString(a) || a == b {
		t.Fatalf("ULID() = %q, %q", a, b)
	}
}
