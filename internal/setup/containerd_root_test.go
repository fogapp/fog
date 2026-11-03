package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config.toml that Docker's containerd.io package ships, trimmed.
const stockContainerdConfig = `#   Copyright 2018-2022 Docker Inc.

disabled_plugins = ["cri"]

#root = "/var/lib/containerd"
#state = "/run/containerd"

#[grpc]
#  address = "/run/containerd/containerd.sock"
`

func TestPatchContainerdRoot(t *testing.T) {
	const want = `root = "/var/lib/fog/containerd"`
	for name, tc := range map[string]struct{ in, mustNot string }{
		"stock file":    {in: stockContainerdConfig, mustNot: `#root = "/var/lib/containerd"`},
		"existing root": {in: "version = 3\nroot = \"/srv/c\"\n[plugins]\n  x = 1\n", mustNot: `/srv/c`},
		"tables only":   {in: "[plugins.\"io.containerd.grpc.v1.cri\"]\n  root = \"/table/scoped\"\n"},
		"missing file":  {},
		"root in table": {in: "version = 3\n[debug]\n  level = \"info\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "etc", "containerd", "config.toml")
			if tc.in != "" {
				os.MkdirAll(filepath.Dir(path), 0o755)
				os.WriteFile(path, []byte(tc.in), 0o644)
			}
			if err := patchContainerdRoot(path, "/var/lib/fog/containerd"); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			out := string(raw)
			if strings.Count(out, want) != 1 {
				t.Fatalf("want exactly one %s:\n%s", want, out)
			}
			if tc.mustNot != "" && strings.Contains(out, tc.mustNot) {
				t.Fatalf("still contains %q:\n%s", tc.mustNot, out)
			}
			// The key must sit before the first table to be top-level.
			if i := strings.Index(out, "\n["); i >= 0 && strings.Index(out, want) > i {
				t.Fatalf("root placed inside a table:\n%s", out)
			}
			// Table-scoped root keys are left alone.
			if strings.Contains(tc.in, "/table/scoped") && !strings.Contains(out, "/table/scoped") {
				t.Fatalf("table-scoped root was rewritten:\n%s", out)
			}
			// Idempotent.
			patchContainerdRoot(path, "/var/lib/fog/containerd")
			if again, _ := os.ReadFile(path); string(again) != out {
				t.Fatalf("second patch changed the file:\n%s", again)
			}
		})
	}
}
