package notify

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func activeOf(t *testing.T, st *store.Store, kind, key string) int {
	t.Helper()
	ns, err := st.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range ns {
		if x.Kind == kind && x.App == key {
			n++
		}
	}
	return n
}

// The default cadence raises at the threshold and again at every multiple,
// so a notice dismissed mid-outage comes back.
func TestFailStreakReraisesAtMultiples(t *testing.T) {
	f := &FailStreak{Store: testStore(t), Kind: "k", Threshold: 3}
	var raised []int
	for range 7 {
		if n, raise := f.Fail(""); raise {
			raised = append(raised, n)
		}
	}
	if len(raised) != 2 || raised[0] != 3 || raised[1] != 6 {
		t.Fatalf("raised at %v, want [3 6]", raised)
	}
}

// Once raises a single time per run, however long the run lasts, and the
// success that ends it dismisses the notice and reports the recovery.
func TestFailStreakOnce(t *testing.T) {
	st := testStore(t)
	f := &FailStreak{Store: st, Kind: "k", Threshold: 2, Once: true}
	raises := 0
	for range 9 {
		if _, raise := f.FailErr("", errors.New("down")); raise {
			raises++
			st.CreateNotice("k", "", "down", "")
		}
	}
	if raises != 1 {
		t.Fatalf("raised %d times, want once", raises)
	}
	if s := f.State(""); !s.Raised || s.Count != 9 || s.LastErr != "down" {
		t.Fatalf("state = %+v", s)
	}
	if !f.Success("") {
		t.Fatal("ending a raised run did not report a recovery")
	}
	if activeOf(t, st, "k", "") != 0 {
		t.Fatal("recovery left the notice up")
	}
	if f.Success("") {
		t.Fatal("a success with no run reported a recovery")
	}
}

// Window holds the raise until the run has lasted long enough as well as
// reaching the count.
func TestFailStreakWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	f := &FailStreak{Store: testStore(t), Kind: "k", Threshold: 2, Once: true, Window: time.Minute,
		Now: func() time.Time { return now }}
	for range 5 {
		if _, raise := f.Fail("a"); raise {
			t.Fatal("raised inside the window")
		}
	}
	now = now.Add(time.Minute)
	if _, raise := f.Fail("a"); !raise {
		t.Fatal("a run past threshold and window did not raise")
	}
}

// ClearLeftover dismisses a notice the previous process raised, on the
// first success only; without it, a success with no run here leaves it.
func TestFailStreakClearLeftover(t *testing.T) {
	st := testStore(t)
	st.CreateNotice("k", "", "left over", "")
	plain := &FailStreak{Store: st, Kind: "k", Threshold: 3}
	plain.Success("")
	if activeOf(t, st, "k", "") != 1 {
		t.Fatal("a plain streak dismissed a notice it never raised")
	}
	f := &FailStreak{Store: st, Kind: "k", Threshold: 3, ClearLeftover: true}
	f.Success("")
	if activeOf(t, st, "k", "") != 0 {
		t.Fatal("first success did not clear the leftover")
	}
	st.CreateNotice("k", "", "someone else's", "")
	f.Success("")
	if activeOf(t, st, "k", "") != 1 {
		t.Fatal("a later success with no run dismissed the notice")
	}
}

// A latch raises on the crossing only, and its first Clear takes down a
// notice the previous process left.
func TestLatch(t *testing.T) {
	st := testStore(t)
	st.CreateNotice("port", "", "left over", "")
	l := &Latch{Store: st, Kind: "port"}
	l.Clear()
	if activeOf(t, st, "port", "") != 0 {
		t.Fatal("first Clear left the leftover up")
	}
	if !l.Raise("needed", "body") || l.Raise("needed", "body") {
		t.Fatal("Raise must report the crossing only")
	}
	if activeOf(t, st, "port", "") != 1 {
		t.Fatal("Raise did not put the notice up")
	}
	l.Clear()
	if activeOf(t, st, "port", "") != 0 {
		t.Fatal("Clear did not take the notice down")
	}
	if !l.Raise("needed", "body") {
		t.Fatal("a new crossing did not raise")
	}
}
