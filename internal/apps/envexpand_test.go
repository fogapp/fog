package apps

import "testing"

func TestExpandEnvRefs(t *testing.T) {
	env := map[string]string{"A": "1", "EMPTY": "", "DB_PASSWORD": "s3"}
	for in, want := range map[string]string{
		"${DB_PASSWORD}": "s3", "$A-x": "1-x", "${A}${A}": "11", "${MISSING}": "", "$$A": "$A",
		"${EMPTY:-d}": "d", "${EMPTY-d}": "", "${MISSING-d}": "d", "${MISSING:-d}": "d",
		"cost $5": "cost $5", "trailing $": "trailing $", "${UNCLOSED": "${UNCLOSED",
	} {
		if got := expandEnvRefs(in, env); got != want {
			t.Errorf("expandEnvRefs(%q) = %q, want %q", in, got, want)
		}
	}
}
