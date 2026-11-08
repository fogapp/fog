package store

import (
	"errors"
	"fmt"
	"testing"
)

func pinPaths(t *testing.T, s *Store, person int64) []string {
	t.Helper()
	pins, err := s.ListFilePins(person)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, p := range pins {
		out = append(out, p.Path)
	}
	return out
}

func TestFilePins(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	a, _ := s.CreatePerson("a", "A", PersonRoleOperator)
	b, _ := s.CreatePerson("b", "B", PersonRoleOperator)

	for _, p := range []string{"Photos", "Photos/2026/beach.jpg", "Photography", "Docs/tax.pdf"} {
		if err := s.AddFilePin(a, p, "dir"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddFilePin(a, "Photos", "dir"); err != nil { // again: no-op
		t.Fatal(err)
	}
	if err := s.AddFilePin(b, "Photos", "dir"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pinPaths(t, s, a)); got != "[Photos Photos/2026/beach.jpg Photography Docs/tax.pdf]" {
		t.Errorf("a's pins = %s", got)
	}

	// A rename carries the folder's pins and those beneath it, for everyone,
	// and leaves a sibling that merely shares the prefix alone.
	if err := s.RenamePath("Photos", "Pictures"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pinPaths(t, s, a)); got != "[Pictures Pictures/2026/beach.jpg Photography Docs/tax.pdf]" {
		t.Errorf("after rename, a's pins = %s", got)
	}
	if got := fmt.Sprint(pinPaths(t, s, b)); got != "[Pictures]" {
		t.Errorf("after rename, b's pins = %s", got)
	}

	// Moving onto a path the person already pins drops the duplicate.
	if err := s.AddFilePin(a, "Archive/tax.pdf", "file"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenamePath("Docs/tax.pdf", "Archive/tax.pdf"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pinPaths(t, s, a)); got != "[Pictures Pictures/2026/beach.jpg Photography Archive/tax.pdf]" {
		t.Errorf("after colliding move, a's pins = %s", got)
	}

	if err := s.RemoveFilePin(a, "Photography"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pinPaths(t, s, a)); got != "[Pictures Pictures/2026/beach.jpg Archive/tax.pdf]" {
		t.Errorf("after unpin, a's pins = %s", got)
	}
}

func TestFilePinLimit(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	a, _ := s.CreatePerson("a", "A", PersonRoleOperator)
	for i := 0; i < MaxFilePins; i++ {
		if err := s.AddFilePin(a, fmt.Sprintf("f%d", i), "file"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddFilePin(a, "one-more", "file"); !errors.Is(err, ErrFilePinLimit) {
		t.Errorf("pin past the limit: err = %v", err)
	}
	if err := s.AddFilePin(a, "f0", "file"); err != nil { // re-pinning an existing one is fine
		t.Errorf("re-pin at the limit: %v", err)
	}
}
