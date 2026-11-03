package cloudflare

import "testing"

// The wildcard is looked up by name in one request, and a non-address
// record that happens to sit on the wildcard name is not ours to delete.
func TestEnsureWildcardOneLookupLeavesNonAddressRecords(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "A", Name: "*.example.net", Content: "192.168.1.200", TTL: TTLLAN},
		{ID: "r2", Type: "TXT", Name: "*.example.net", Content: "v=spf1 -all"},
		{ID: "r3", Type: "A", Name: "www.example.net", Content: "203.0.113.9"},
	})
	if action := ensure(t, c); action != "ok" {
		t.Fatalf("action = %q, want ok", action)
	}
	if f.lists != 1 {
		t.Fatalf("record list fetched %d times, want 1", f.lists)
	}
	if got := f.wildcards("example.net"); len(got) != 2 {
		t.Fatalf("wildcard records: %+v", got)
	}
}
