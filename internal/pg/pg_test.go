package pg

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeVersion(t *testing.T, path, major string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(major+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The data directory is the source of truth for the major: a pre-18
// install (PG_VERSION at the top) is legacy 16, an upgraded one is nested,
// a missing or empty directory is a fresh install on the target major, and
// anything fog has no image for is an error rather than a guess.
func TestInspectData(t *testing.T) {
	t.Run("missing dir is fresh on the target major", func(t *testing.T) {
		st, err := inspectData(filepath.Join(t.TempDir(), "data"))
		if err != nil || !st.Fresh || st.Major != TargetMajor || st.Layout != LayoutNested {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
	t.Run("empty dir is fresh", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		os.MkdirAll(dir, 0o700)
		st, err := inspectData(dir)
		if err != nil || !st.Fresh {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
	t.Run("legacy 16", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		writeVersion(t, filepath.Join(dir, "PG_VERSION"), "16")
		st, err := inspectData(dir)
		if err != nil || st.Fresh || st.Major != 16 || st.Layout != LayoutLegacy {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
	t.Run("nested 18", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		writeVersion(t, filepath.Join(dir, "18", "docker", "PG_VERSION"), "18")
		st, err := inspectData(dir)
		if err != nil || st.Fresh || st.Major != 18 || st.Layout != LayoutNested {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
	t.Run("unknown major refuses", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		writeVersion(t, filepath.Join(dir, "PG_VERSION"), "15")
		if _, err := inspectData(dir); err == nil || !strings.Contains(err.Error(), "no image") {
			t.Fatalf("expected a no-image error, got %v", err)
		}
	})
	t.Run("two nested clusters refuse", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		writeVersion(t, filepath.Join(dir, "16", "docker", "PG_VERSION"), "16")
		writeVersion(t, filepath.Join(dir, "18", "docker", "PG_VERSION"), "18")
		if _, err := inspectData(dir); err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("expected an interrupted-upgrade error, got %v", err)
		}
	})
	t.Run("non-empty dir with no cluster refuses", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		os.MkdirAll(filepath.Join(dir, "18", "docker"), 0o700) // initdb died before PG_VERSION
		if _, err := inspectData(dir); err == nil || !strings.Contains(err.Error(), "no recognizable cluster") {
			t.Fatalf("expected a refusal, got %v", err)
		}
	})
}

// The bind mount follows the layout, and in the nested layout PGDATA is
// explicit, so a major's image default can never decide where the data
// lives. This is the rule that keeps a new image from initdb-ing an empty
// cluster beside the real one.
// Sealed apps reach Postgres by the same name as every other app.
func TestComposeDocJoinsTheSealedNetwork(t *testing.T) {
	raw, err := composeDoc("fog", "fog-sealed", 18, LayoutNested)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Networks map[string]struct {
				Aliases []string `yaml:"aliases"`
			} `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]struct {
			External bool `yaml:"external"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"fog", "fog-sealed"} {
		if !doc.Networks[n].External {
			t.Errorf("%s isn't external:\n%s", n, raw)
		}
		if got := doc.Services["postgres"].Networks[n].Aliases; !slices.Equal(got, []string{ContainerName}) {
			t.Errorf("%s aliases = %v", n, got)
		}
	}
}

func TestComposeDocLayouts(t *testing.T) {
	legacy, err := composeDoc("fog", "", 16, LayoutLegacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"image: pgvector/pgvector:pg16-trixie", "./data:/var/lib/postgresql/data"} {
		if !strings.Contains(string(legacy), want) {
			t.Errorf("legacy compose lacks %q:\n%s", want, legacy)
		}
	}
	// Byte-for-byte what existing installs already have: the fog release
	// that adds layouts must not recreate a running cluster's container.
	if strings.Contains(string(legacy), "PGDATA") {
		t.Errorf("legacy compose must leave PGDATA to the image default:\n%s", legacy)
	}
	nested, err := composeDoc("fog", "", 18, LayoutNested)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"image: pgvector/pgvector:pg18-trixie", "PGDATA: /var/lib/postgresql/18/docker", "- ./data:/var/lib/postgresql\n"} {
		if !strings.Contains(string(nested), want) {
			t.Errorf("nested compose lacks %q:\n%s", want, nested)
		}
	}
	if _, err := composeDoc("fog", "", 15, LayoutLegacy); err == nil {
		t.Fatal("a major with no image must not render")
	}
	// Both layouts run the server with the shared-cluster settings.
	for _, doc := range [][]byte{legacy, nested} {
		for _, want := range []string{"max_connections=400", "tcp_keepalives_idle=60"} {
			if !strings.Contains(string(doc), want) {
				t.Errorf("compose lacks %q:\n%s", want, doc)
			}
		}
	}
}

func TestParseMajor(t *testing.T) {
	cases := map[string]int{
		"pg_dump (PostgreSQL) 15.19 (Debian 15.19-0+deb12u1)": 15,
		"pg_dump (PostgreSQL) 16.4":                           16,
		"psql (PostgreSQL) 17.0 (Debian 17.0-1.pgdg120+1)":    17,
		"pg_dump (PostgreSQL) 18.6 (Debian 18.6-1.pgdg13+2)":  18,
	}
	for in, want := range cases {
		if got, ok := ParseMajor(in); !ok || got != want {
			t.Errorf("%q: got %d/%v, want %d", in, got, ok, want)
		}
	}
	if _, ok := ParseMajor("pg_dump 16"); ok {
		t.Error("a line without the (PostgreSQL) marker must not parse")
	}
}

// Every image is a trixie build: mixing Debian bases across majors is the
// collation trap the Images comment describes.
func TestImagesShareABase(t *testing.T) {
	for major, img := range Images {
		if !strings.HasSuffix(img, "-trixie") {
			t.Errorf("Images[%d] = %s is not a trixie build", major, img)
		}
	}
	if _, ok := Images[TargetMajor]; !ok {
		t.Fatalf("TargetMajor %d has no image", TargetMajor)
	}
}

// The nested layout's data directory is the parent of a PGDATA owned by
// another uid; it must be traversable or Postgres 18 dies at startup on a
// real Linux filesystem. Legacy stays private: it is PGDATA itself.
func TestPrepareDataDirModes(t *testing.T) {
	for layout, want := range map[Layout]os.FileMode{LayoutLegacy: 0o700, LayoutNested: 0o755} {
		dir := filepath.Join(t.TempDir(), "data")
		if err := prepareDataDir(dir, layout); err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(dir); info.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", layout, info.Mode().Perm(), want)
		}
	}
	// An existing directory with the wrong mode is corrected, not kept.
	dir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dir, 0o700)
	if err := prepareDataDir(dir, LayoutNested); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Errorf("existing nested dir left at %o", info.Mode().Perm())
	}
}

func TestPgExtIdent(t *testing.T) {
	for _, ok := range []string{"vector", "pg_trgm", "uuid-ossp", "a1-b"} {
		if !pgExtIdent(ok) {
			t.Errorf("%q should be a valid extension name", ok)
		}
	}
	for _, bad := range []string{"", "-ossp", "1abc", "Vector", `uuid"ossp`, "a b"} {
		if pgExtIdent(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	// Database names stay strict.
	if pgIdent("uuid-ossp") {
		t.Error("pgIdent should still reject hyphens")
	}
}

// With -A -t there is no framing to guess past: an empty result is no
// rows, and a value is the whole line.
func TestRows(t *testing.T) {
	if rs := rows("\n"); len(rs) != 0 {
		t.Errorf("empty result = %q", rs)
	}
	if rs := rows("3\n"); len(rs) != 1 || rs[0] != "3" {
		t.Errorf("single value = %q", rs)
	}
	if rs := rows("plpgsql|1.0\nvector|0.7.0\n"); len(rs) != 2 || rs[1] != "vector|0.7.0" {
		t.Errorf("two rows = %q", rs)
	}
}
