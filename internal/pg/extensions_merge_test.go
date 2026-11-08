package pg

import (
	"reflect"
	"testing"
)

// A declared extension the dump already names keeps the dump's schema; one
// it left out goes into public, as provisioning puts it.
func TestWithDeclared(t *testing.T) {
	got := withDeclared([]extTarget{{Name: "citext", Schema: "ext"}}, []string{"citext", "pg_trgm"})
	want := []extTarget{{Name: "citext", Schema: "ext"}, {Name: "pg_trgm", Schema: "public"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := withDeclared(nil, nil); len(got) != 0 {
		t.Fatalf("nothing declared, nothing dumped: %v", got)
	}
}
