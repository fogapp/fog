package store

import (
	"path/filepath"
	"testing"
)

func TestAppFederationFactsRoundTrip(t *testing.T) {
	t.Parallel()

	s, err := openTest(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApp("feed")
	if err != nil {
		t.Fatal(err)
	}

	// A freshly built app reads as the zero value, not an error.
	if f, err := s.AppFederationFacts(app.ID); err != nil || len(f.Contracts) != 0 || f.HostChange != "" {
		t.Fatalf("fresh app: %+v %v", f, err)
	}

	want := FederationFacts{
		Contracts:        []string{"fog-connect/1"},
		HostChange:       "Peers must re-pair.",
		UnknownContracts: []string{"fog-bogus/9"},
	}
	if err := s.SetAppFederationFacts(app.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.AppFederationFacts(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostChange != want.HostChange || len(got.Contracts) != 1 || got.Contracts[0] != "fog-connect/1" ||
		len(got.UnknownContracts) != 1 || got.UnknownContracts[0] != "fog-bogus/9" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}
