package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each checked-in file must be what the generator writes today: a Go
// struct changed without `make gen-types` fails here, in `make check`.
func TestGeneratedFilesAreCurrent(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outputs {
		want, err := generate(root, o)
		if err != nil {
			t.Fatalf("%s: %v", o.File, err)
		}
		got, err := os.ReadFile(filepath.Join(root, o.File))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: run `make gen-types` and commit the result", o.File)
		}
	}
}

func TestEncodingJSONRules(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	g := newGen(root)
	sample := modulePath + "/internal/tools/tsgen/testdata/sample"
	if _, err := g.named(typeKey{sample, "Thing"}, ""); err != nil {
		t.Fatal(err)
	}
	for len(g.queue) > 0 {
		k := g.queue[0]
		g.queue = g.queue[1:]
		if err := g.emit(k); err != nil {
			t.Fatal(err)
		}
	}
	out := strings.Join(g.decls, "\n")
	for _, want := range []string{
		"  id: number;\n",               // embedded struct flattened
		"  shade: number;\n",            // the shallower field wins
		"  note?: string;\n",            // omitempty is optional
		"  kind: Kind;\n",               //
		`export type Kind = "a" | "b";`, // typed constants are a union
		`  mode: "x" | "y";`,            // a ts tag overrides
		"  when: string;\n",             // time.Time
		"  child: Child | null;\n",      // a nil pointer encodes null
		"  maybe?: Child;\n",            // unless omitempty drops it
		"  always: Child;\n",            // omitempty never drops a struct
		"  tags: string[];\n",           //
		"  attrs: Record<string, number>;\n",
		"  count: string;\n", // ,string
		"  raw: string;\n",   // []byte is base64
		"  Untagged: boolean;\n",
		"  nested: {\n    X: number;\n  };\n",
		"  byName?: Record<string, Child>;\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"shade: string", "Hidden", "inner"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%q should not appear in:\n%s", unwanted, out)
		}
	}
}

func TestRefusesNameCollisions(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	g := newGen(root)
	sample := modulePath + "/internal/tools/tsgen/testdata/sample"
	if _, err := g.named(typeKey{sample, "Child"}, "Shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.named(typeKey{sample, "Base"}, "Shared"); err == nil {
		t.Fatal("two Go types mapped to one TypeScript name without complaint")
	}
}

func TestKeySet(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	g := newGen(root)
	if err := g.keySet(keySet{Path: "internal/tools/tsgen/testdata/sample", Var: "settings", TS: "Settings", Secret: "secrets"}); err != nil {
		t.Fatal(err)
	}
	want := `// settings are the editable keys.
export interface Settings {
  plain: string;
  // Doc above an entry carries over.
  documented: string;
  local: string; // a local constant, with a trailing comment
  kind: string; // a typed constant
  "dotted.key": string;
  pass: string; // stored sealed (write-only: GET always answers "")
  pass_set: string; // read-only: "on" when pass is stored
  multi: string; // a func value spanning lines
}
`
	if got := strings.Join(g.decls, ""); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	g = newGen(root)
	if err := g.keySet(keySet{Path: "internal/tools/tsgen/testdata/sample", Var: "settings", TS: "Settings", Secret: "strays"}); err == nil {
		t.Error("a secret key the key set lacks went unnoticed")
	}
}
