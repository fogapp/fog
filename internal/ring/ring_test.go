package ring

import (
	"slices"
	"testing"
)

func TestRingKeepsNewestOldestFirst(t *testing.T) {
	r := New[int](3)
	if got := r.All(); len(got) != 0 {
		t.Fatalf("empty ring holds %v", got)
	}
	if _, ok := r.Latest(); ok {
		t.Fatal("empty ring has a latest value")
	}

	r.Add(1)
	r.Add(2)
	if got := r.All(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("partial ring = %v", got)
	}
	r.Add(3)
	r.Add(4) // wraps, evicting 1
	r.Add(5) // and 2
	if got := r.All(); !slices.Equal(got, []int{3, 4, 5}) {
		t.Fatalf("wrapped ring = %v", got)
	}
	if got := r.Recent(2); !slices.Equal(got, []int{4, 5}) {
		t.Fatalf("Recent(2) = %v", got)
	}
	if got := r.Recent(10); !slices.Equal(got, []int{3, 4, 5}) {
		t.Fatalf("Recent past the count = %v", got)
	}
	if v, ok := r.Latest(); !ok || v != 5 {
		t.Fatalf("Latest = %v, %v", v, ok)
	}
}

func TestRingCopiesOut(t *testing.T) {
	r := New[int](2)
	r.Add(1)
	got := r.All()
	got[0] = 99
	if v, _ := r.Latest(); v != 1 {
		t.Fatal("a returned slice aliases the ring")
	}
}

func TestRingSizeFloor(t *testing.T) {
	r := New[string](0)
	r.Add("a")
	r.Add("b")
	if got := r.All(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("zero-size ring = %v", got)
	}
}
