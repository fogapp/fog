package store

import (
	"errors"
	"testing"
	"time"
)

// A join that fails leaves no person behind and the invite still usable;
// one that succeeds spends it.
func TestJoinPersonIsAllOrNothing(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if _, err := s.CreatePerson("taken", "Taken", PersonRoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePersonInvite("digest", "", "", PersonRoleMember, "op", time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.JoinPerson("digest", "taken", "Again", "hash"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("taken handle: %v", err)
	}
	if _, err := s.PersonInviteByDigest("digest"); err != nil {
		t.Fatalf("invite spent by a failed join: %v", err)
	}

	id, inv, err := s.JoinPerson("digest", "ann", "Ann", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Role != PersonRoleMember {
		t.Fatalf("role %q", inv.Role)
	}
	creds, _ := s.PersonCredentials(id)
	if len(creds) != 1 || creds[0].Kind != CredentialPassword || creds[0].Data != "hash" {
		t.Fatalf("credentials: %+v", creds)
	}
	if _, _, err := s.JoinPerson("digest", "bob", "Bob", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("spent invite: %v", err)
	}
	if _, err := s.PersonByHandle("bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("person created on a spent invite: %v", err)
	}
}

func TestCreateFirstOperatorOnlyOnce(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	id, err := s.CreateFirstOperator("op", "Op", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if creds, _ := s.PersonCredentials(id); len(creds) != 1 {
		t.Fatalf("credentials: %+v", creds)
	}
	if _, err := s.CreateFirstOperator("op2", "Op2", "hash"); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second operator: %v", err)
	}
	if n, _ := s.CountOperators(); n != 1 {
		t.Fatalf("operators = %d", n)
	}
}
