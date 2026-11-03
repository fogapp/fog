package store

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestAllocateHostPortReusesFreedPorts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(name string) int64 {
		a, err := s.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}

	first, second, third := mk("one"), mk("two"), mk("three")
	for _, id := range []int64{first, second, third} {
		if _, err := s.AllocateHostPort(id); err != nil {
			t.Fatal(err)
		}
	}
	p1, _ := s.AllocateHostPort(first)
	p2, _ := s.AllocateHostPort(second)
	p3, _ := s.AllocateHostPort(third)
	if p1 != HostPortMin || p2 != HostPortMin+1 || p3 != HostPortMin+2 {
		t.Fatalf("got %d, %d, %d; want %d, %d, %d", p1, p2, p3, HostPortMin, HostPortMin+1, HostPortMin+2)
	}

	// Allocation is idempotent — an app keeps the port it already has.
	if again, _ := s.AllocateHostPort(second); again != p2 {
		t.Errorf("reallocation moved the port from %d to %d", p2, again)
	}

	// Deleting the middle app frees its port, and the next app takes it back
	// rather than climbing past the end of the window forever.
	if err := s.DeleteApp(second); err != nil {
		t.Fatal(err)
	}
	fourth := mk("four")
	p4, err := s.AllocateHostPort(fourth)
	if err != nil {
		t.Fatal(err)
	}
	if p4 != p2 {
		t.Errorf("got port %d, want the freed %d reused", p4, p2)
	}
}

// Two apps allocating at the same time must never share a port: the
// transaction serializes the read-pick-write, and the unique index on
// host_port turns any interleaving that slips through into a retry.
func TestAllocateHostPortConcurrent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const n = 8
	ids := make([]int64, n)
	for i := range ids {
		a, err := s.CreateApp(string(rune('a' + i)))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = a.ID
	}
	ports := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			ports[i], errs[i] = s.AllocateHostPort(id)
		}(i, id)
	}
	wg.Wait()
	seen := map[int]int{}
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("allocate app %d: %v", ids[i], errs[i])
		}
		if prev, dup := seen[ports[i]]; dup {
			t.Fatalf("apps %d and %d both got port %d", prev, ids[i], ports[i])
		}
		seen[ports[i]] = int(ids[i])
	}
}

// The schema itself refuses a duplicate host port — the invariant holds even
// against writes that bypass the allocator.
func TestHostPortUniqueIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.DB().Exec(`INSERT INTO apps (name, desired_state, host_port, created_at, updated_at) VALUES ('one', 'running', ?, '', '')`, HostPortMin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO apps (name, desired_state, host_port, created_at, updated_at) VALUES ('two', 'running', ?, '', '')`, HostPortMin); err == nil {
		t.Error("duplicate host_port inserted; want a unique-index refusal")
	}
	// The 0 sentinel is exempt: every unrouted app carries it.
	for _, name := range []string{"three", "four"} {
		if _, err := s.DB().Exec(`INSERT INTO apps (name, desired_state, host_port, created_at, updated_at) VALUES (?, 'running', 0, '', '')`, name); err != nil {
			t.Fatalf("port-0 app %s refused: %v", name, err)
		}
	}
}

func TestAllocateHostPortExhaustion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Occupy the window directly rather than creating a thousand apps.
	if _, err := s.DB().Exec(
		`INSERT INTO apps (name, desired_state, host_port, created_at, updated_at)
		 WITH RECURSIVE seq(n) AS (SELECT ? UNION ALL SELECT n+1 FROM seq WHERE n < ?)
		 SELECT 'app' || n, 'running', n, '', '' FROM seq`,
		HostPortMin, HostPortMax); err != nil {
		t.Fatal(err)
	}
	spare, err := s.CreateApp("spare")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllocateHostPort(spare.ID); err == nil {
		t.Error("allocation succeeded with the whole window taken; want a refusal")
	}
}
