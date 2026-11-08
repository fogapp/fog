package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A name collision must surface as ErrAlreadyExists — matched by the
// driver's UNIQUE-violation code, not message text — because fed pairing
// branches on it to try the next alias.
func TestCreatePairedPeerNameCollision(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if _, err := s.CreatePairedPeer("inst-1", "attic", "", "pk1"); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreatePairedPeer("inst-2", "attic", "http://x", "pk2")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), `"attic"`) {
		t.Fatalf("message lost the peer name: %v", err)
	}
}

func TestRenameFedPeerCollision(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if _, err := s.CreatePairedPeer("inst-1", "attic", "", "pk1"); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePairedPeer("inst-2", "garage", "", "pk2")
	if err != nil {
		t.Fatal(err)
	}
	rename := func(name string) error { return s.EditFedPeer(p.ID, FedPeerEdit{Name: &name}) }
	if err := rename("attic"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	// A non-colliding rename still works.
	if err := rename("shed"); err != nil {
		t.Fatal(err)
	}
}

// A real, non-uniqueness failure must pass through unwrapped, not be
// mislabeled as a collision.
func TestCreatePairedPeerOtherErrorsPassThrough(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	s.Close() // closed DB: any error but not a UNIQUE violation
	_, err := s.CreatePairedPeer("inst-1", "attic", "", "pk1")
	if err == nil {
		t.Fatal("expected an error from a closed store")
	}
	if errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("non-uniqueness failure mislabeled as collision: %v", err)
	}
}

// "Keep on this box" exclusions follow a renamed folder, the folder itself
// and everything under it, and leave other paths alone.
func TestRenamePathShareExcludes(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	for _, p := range []string{"a", "a/b", "ab", "c"} {
		if err := s.AddFileShareExclude(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RenamePath("a", "x"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListFileShareExcludes()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ab", "c", "x", "x/b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excludes after rename = %v, want %v", got, want)
	}
}
