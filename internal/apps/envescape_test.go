package apps

import (
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// parseEnvFile reads a .env the way docker compose does: compose-go's own
// dotenv parser, with nothing from the process environment to expand into.
func parseEnvFile(t *testing.T, src string) map[string]string {
	t.Helper()
	got, err := dotenv.ParseWithLookup(strings.NewReader(src), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("compose cannot parse the .env fog wrote: %v\n%s", err, src)
	}
	return got
}

// envRoundTrip renders vals as fog's .env does and asserts compose reads
// back exactly vals: every value byte for byte, and no variable added.
func envRoundTrip(t *testing.T, vals map[string]string) {
	t.Helper()
	var b strings.Builder
	for _, k := range sortedEnvKeys(vals) {
		line, err := envFileLine(k, vals[k])
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
	}
	got := parseEnvFile(t, b.String())
	if len(got) != len(vals) {
		t.Fatalf("compose read %d variables from %d written:\n%s\ngot %q", len(got), len(vals), b.String(), got)
	}
	for k, want := range vals {
		if got[k] != want {
			t.Fatalf("%s: wrote %q, compose read %q\n.env:\n%s", k, want, got[k], b.String())
		}
	}
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The attack: a provider app's integration mint answers with a value ending
// in a backslash. Single-quoted, compose read \' as an escaped quote, ran on
// into the next line, and the provider's second value — sorting after
// FOG_API_URL, so its lines win — came out as FOG_API_URL pointing at the
// provider, which then collected the consumer's FOG_API_TOKEN. This exact
// .env parsed to FOG_API_URL=http://evil.example under the old quoting.
func TestEnvFileProviderValueCannotOverrideFogAPIURL(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{Source: Source{Type: "image", Image: "x:latest"}}
	env := map[string]string{
		"FOG_API_TOKEN": "fog_secret",
		"FOG_API_URL":   "http://172.17.0.1:8080",
		"Z_ONE":         "x\\",
		"Z_TWO":         "\nFOG_API_URL=http://evil.example\nZZ=\\",
	}
	if err := m.Render("app", spec, env, "x:latest", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "apps", "app", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseEnvFile(t, string(b))
	if got["FOG_API_URL"] != "http://172.17.0.1:8080" {
		t.Fatalf("FOG_API_URL overridden to %q:\n%s", got["FOG_API_URL"], b)
	}
	for k, want := range env {
		if got[k] != want {
			t.Errorf("%s: wrote %q, compose read %q", k, want, got[k])
		}
	}
	if len(got) != len(env) {
		t.Errorf("compose read variables fog never wrote: %q", got)
	}
}

func TestEnvFileRoundTripsHostileValues(t *testing.T) {
	cases := []string{
		"", "simple", "hello world", "value #notacomment", "#leading", " # x",
		`$2b$12$KIXQ8ZgqYQm3vBnO`, `p@ss$word1`, `${HOME}`, `$$`, `$`, `\$`, `$\`,
		`C:\path\to`, `\`, `\\`, `\\\`, `x\`, `x\\\`, `\'`, `'\`, `\"`, `"\`,
		`it's fine`, `say "hi"`, `'`, `"`, `''`, `""`, `'"'"`,
		"line1\nline2", "\n", "\r\n", "trailing\\\n", "\\\nB=1",
		"x'\nFOG_API_URL=http://evil\n'", "x\"\nFOG_API_URL=http://evil\n\"",
		`\0123`, `\0`, `\a\b\f\v\t`, `\u0041`, `\x41`, "tab\there", " lead", "trail ",
		"a=b=c", "export X=1", "日本語", "\xff\xfe invalid utf8",
	}
	vals := map[string]string{}
	for i, v := range cases {
		vals["V"+string(rune('A'+i/26))+string(rune('A'+i%26))] = v
	}
	envRoundTrip(t, vals)
	for _, v := range cases {
		envRoundTrip(t, map[string]string{"A": v, "FOG_API_URL": "http://fog"})
	}
}

// Random values over the characters compose's parser cares about: whatever
// comes in reads back exactly, and never adds a variable.
func TestEnvFileRoundTripFuzz(t *testing.T) {
	alphabet := []string{`\`, `'`, `"`, `$`, `#`, "\n", "\r", "{", "}", "=", " ", "\t", "0", "1", "a", "n", "r", "x", "FOG_API_URL=", "é"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		var b strings.Builder
		for range r.IntN(16) {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		envRoundTrip(t, map[string]string{"A": b.String(), "FOG_API_URL": "http://fog", "Z": b.String()})
	}
}

// Go's native fuzzer, for `go test -fuzz`; the seed corpus runs as a test.
func FuzzEnvFileRoundTrip(f *testing.F) {
	for _, s := range []string{"x\\", "a'b", `"\$`, "\n", "\\\nX=1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if strings.IndexByte(v, 0) >= 0 {
			if _, err := envFileLine("A", v); err == nil {
				t.Fatal("a NUL byte was written into .env")
			}
			return
		}
		envRoundTrip(t, map[string]string{"A": v, "FOG_API_URL": "http://fog"})
	})
}

// Keys reach Render from several places (spec, secrets, fog's own, Postgres
// inject); one that compose would read as something else must fail the
// render, not be written.
func TestEnvFileRefusesBadKeysAndNUL(t *testing.T) {
	for _, k := range []string{"A\nB", "A=B", "A B", "", "1A", "A'"} {
		if _, err := envFileLine(k, "v"); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	if _, err := envFileLine("A", "a\x00b"); err == nil {
		t.Error("NUL accepted")
	}
}

// The whole .env file must survive a round trip, including the secret
// values Render is trusted to write correctly.
func TestRenderEnvFileQuotesSecrets(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{Source: Source{Type: "image", Image: "x:latest"}}
	env := map[string]string{
		"SECRET_KEY_BASE": "$2b$12$KIXQ8ZgqYQm3vBnO",
		"PLAIN":           "1000",
	}
	if err := m.Render("app", spec, env, "x:latest", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "apps", "app", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseEnvFile(t, string(b))
	for k, v := range env {
		if got[k] != v {
			t.Errorf("%s: compose reads %q, want %q\n%s", k, got[k], v, b)
		}
	}
}
