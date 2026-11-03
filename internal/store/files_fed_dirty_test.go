package store

import (
	"slices"
	"testing"
)

// MarkFileFedSharesDirty names every share a change touches, dirty already
// or not (each needs its cached manifest dropped), and
// TakeDirtyFileFedShares hands each dirty share over once.
func TestFileFedSharesDirtyFlag(t *testing.T) {
	s := testStore(t)
	peer, err := s.CreatePairedPeer("inst-1", "peer", "", "pk")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(path string) int64 {
		sh, err := s.CreateFileFedShare(&FileFedShare{PeerID: peer.ID, Path: path, Title: path, Mode: "read", Scope: "all", Detail: "full", Apps: "denied"})
		if err != nil {
			t.Fatal(err)
		}
		return sh.ID
	}
	a, b, other := mk("Photos"), mk("Photos/2026"), mk("Docs")

	for range 2 {
		ids, err := s.MarkFileFedSharesDirty("Photos/2026/x.jpg")
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []int64{a, b}) {
			t.Fatalf("touched %v, want [%d %d]", ids, a, b)
		}
	}
	taken, err := s.TakeDirtyFileFedShares()
	if err != nil {
		t.Fatal(err)
	}
	got := taken[peer.ID]
	slices.Sort(got)
	if len(taken) != 1 || !slices.Equal(got, []int64{a, b}) {
		t.Fatalf("took %v, want [%d %d] for peer %d (and not %d)", taken, a, b, peer.ID, other)
	}
	if again, _ := s.TakeDirtyFileFedShares(); len(again) != 0 {
		t.Fatalf("dirty shares taken twice: %v", again)
	}
}
