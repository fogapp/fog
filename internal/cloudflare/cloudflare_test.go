package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeAPI is an httptest-backed Cloudflare DNS API for one zone: it serves
// the record list and applies POST/PUT/DELETE so tests assert on the state
// EnsureWildcard actually leaves behind, not on which calls it made.
type fakeAPI struct {
	mu      sync.Mutex
	records []Record
	nextID  int
	lists   int // GETs of the record list
}

func envelope(w http.ResponseWriter, result any) {
	raw, _ := json.Marshal(result)
	json.NewEncoder(w).Encode(map[string]any{"success": true, "result": json.RawMessage(raw)})
}

func newFake(t *testing.T, initial []Record) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{records: initial, nextID: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		typ, name := r.URL.Query().Get("type"), r.URL.Query().Get("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lists++
		out := []Record{}
		for _, rec := range f.records {
			if (typ == "" || rec.Type == typ) && (name == "" || rec.Name == name) {
				out = append(out, rec)
			}
		}
		envelope(w, out)
	})
	mux.HandleFunc("POST /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		var rec Record
		json.NewDecoder(r.Body).Decode(&rec)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.nextID++
		rec.ID = fmt.Sprintf("rec-%d", f.nextID)
		f.records = append(f.records, rec)
		envelope(w, rec)
	})
	mux.HandleFunc("PUT /zones/z1/dns_records/{id}", func(w http.ResponseWriter, r *http.Request) {
		var rec Record
		json.NewDecoder(r.Body).Decode(&rec)
		rec.ID = r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.records {
			if f.records[i].ID == rec.ID {
				f.records[i] = rec
				envelope(w, rec)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "record not found"}}})
	})
	mux.HandleFunc("DELETE /zones/z1/dns_records/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.records {
			if f.records[i].ID == id {
				f.records = append(f.records[:i], f.records[i+1:]...)
				envelope(w, map[string]string{"id": id})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "record not found"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, &Client{Token: "tok", HTTP: srv.Client(), Base: srv.URL}
}

// wildcards returns the records still on the wildcard name, for asserting
// on converged state.
func (f *fakeAPI) wildcards(domain string) []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Record
	for _, r := range f.records {
		if r.Name == "*."+domain {
			out = append(out, r)
		}
	}
	return out
}

func ensure(t *testing.T, c *Client) string {
	t.Helper()
	action, err := c.EnsureWildcard(context.Background(), "z1", "example.net", "192.168.1.200", TTLLAN)
	if err != nil {
		t.Fatal(err)
	}
	return action
}

func TestEnsureWildcardCreates(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "A", Name: "other.example.net", Content: "10.0.0.1"},
	})
	if action := ensure(t, c); action != "created" {
		t.Fatalf("action = %q, want created", action)
	}
	got := f.wildcards("example.net")
	if len(got) != 1 || got[0].Type != "A" || got[0].Content != "192.168.1.200" || got[0].Proxied {
		t.Fatalf("converged records: %+v", got)
	}
}

func TestEnsureWildcardOkWhenCorrect(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "A", Name: "*.example.net", Content: "192.168.1.200", TTL: TTLLAN},
	})
	if action := ensure(t, c); action != "ok" {
		t.Fatalf("action = %q, want ok", action)
	}
	got := f.wildcards("example.net")
	if len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("correct record must be left untouched: %+v", got)
	}
}

func TestEnsureWildcardUpdatesWrongIP(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "A", Name: "*.example.net", Content: "10.9.9.9"},
	})
	if action := ensure(t, c); action != "updated" {
		t.Fatalf("action = %q, want updated", action)
	}
	got := f.wildcards("example.net")
	if len(got) != 1 || got[0].Content != "192.168.1.200" {
		t.Fatalf("converged records: %+v", got)
	}
}

func TestEnsureWildcardUpdatesProxied(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "A", Name: "*.example.net", Content: "192.168.1.200", Proxied: true},
	})
	if action := ensure(t, c); action != "updated" {
		t.Fatalf("action = %q, want updated", action)
	}
	if got := f.wildcards("example.net"); got[0].Proxied {
		t.Fatalf("record still proxied: %+v", got)
	}
}

// A correct A record with a stray AAAA beside it: the stray shadows
// resolution nondeterministically and must be deleted, not left as the
// "first match wins" of old.
func TestEnsureWildcardDeletesStrayOfOtherType(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "AAAA", Name: "*.example.net", Content: "::1"},
		{ID: "r2", Type: "A", Name: "*.example.net", Content: "192.168.1.200"},
	})
	if action := ensure(t, c); action != "updated" {
		t.Fatalf("action = %q, want updated", action)
	}
	got := f.wildcards("example.net")
	if len(got) != 1 || got[0].ID != "r2" || got[0].Type != "A" {
		t.Fatalf("converged records: %+v", got)
	}
}

// No A at all, just strays: they all go, and the A record is created.
func TestEnsureWildcardReplacesStraysWithA(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "CNAME", Name: "*.example.net", Content: "old.example.org"},
		{ID: "r2", Type: "AAAA", Name: "*.example.net", Content: "::1"},
	})
	if action := ensure(t, c); action != "created" {
		t.Fatalf("action = %q, want created", action)
	}
	got := f.wildcards("example.net")
	if len(got) != 1 || got[0].Type != "A" || got[0].Content != "192.168.1.200" {
		t.Fatalf("converged records: %+v", got)
	}
}

// Surplus A records are strays too: one is kept and corrected, the rest are
// deleted.
func TestEnsureWildcardDeletesSurplusA(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "A", Name: "*.example.net", Content: "10.0.0.1"},
		{ID: "r2", Type: "A", Name: "*.example.net", Content: "10.0.0.2"},
	})
	if action := ensure(t, c); action != "updated" {
		t.Fatalf("action = %q, want updated", action)
	}
	got := f.wildcards("example.net")
	if len(got) != 1 || got[0].Content != "192.168.1.200" {
		t.Fatalf("converged records: %+v", got)
	}
}

// A Client built without an injected transport must still carry a timeout —
// no code path may issue an unbounded API call.
func TestDefaultClientHasTimeout(t *testing.T) {
	c := New("tok")
	if got := c.httpClient().Timeout; got == 0 {
		t.Fatal("default client has no timeout")
	}
}

// Switching the record to the public address shortens its TTL; a record
// that is right in every other way but carries the LAN TTL is corrected.
func TestEnsureWildcardUpdatesTTL(t *testing.T) {
	f, c := newFake(t, []Record{
		{ID: "r1", Type: "A", Name: "*.example.net", Content: "203.0.113.9", TTL: TTLLAN},
	})
	action, err := c.EnsureWildcard(context.Background(), "z1", "example.net", "203.0.113.9", TTLWAN)
	if err != nil || action != "updated" {
		t.Fatalf("action = %q, err %v; want updated", action, err)
	}
	got := f.wildcards("example.net")
	if len(got) != 1 || got[0].TTL != TTLWAN {
		t.Fatalf("TTL not corrected: %+v", got)
	}
}
