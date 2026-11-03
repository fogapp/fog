package backup

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A restore never runs the compose file on disk — it may be a render fog's
// rules have since refused; every bring-up is the engine's (startApp).
func TestNoRestoreRunsComposeUp(t *testing.T) {
	up := regexp.MustCompile(`ComposeCmd\([^)]*"(up|start|restart)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := up.Find(b); m != nil {
			t.Errorf("%s runs %s", f, m)
		}
	}
}
