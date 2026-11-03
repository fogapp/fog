package store

import (
	"reflect"
	"testing"
)

func TestAppsWithContract(t *testing.T) {
	s := testStore(t)
	mk := func(name string, contracts ...string) *App {
		t.Helper()
		a, err := s.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		if contracts != nil {
			if err := s.SetAppContracts(a.ID, contracts); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	both := mk("both", "fog-connect/1", "fog-files/1")
	fedOnly := mk("fed", "fog-connect/1")
	mk("none")
	corrupt := mk("corrupt", "fog-connect/1")
	if _, err := s.db.Exec(`UPDATE apps SET contracts = '{bad' WHERE id = ?`, corrupt.ID); err != nil {
		t.Fatal(err)
	}

	names := func(list []App) []string {
		var out []string
		for _, a := range list {
			out = append(out, a.Name)
		}
		return out
	}
	got, err := s.AppsWithContract("fog-connect/1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"both", "fed"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("federation apps = %v, want %v (corrupt row skipped, not fatal)", names(got), want)
	}
	got, err = s.AppsWithContract("fog-files/1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"both"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("files apps = %v, want %v", names(got), want)
	}
	// A contract name is matched whole, never as a substring.
	if got, _ := s.AppsWithContract("fog-files"); len(got) != 0 {
		t.Errorf("substring matched: %v", names(got))
	}

	all, err := s.AllAppContracts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all[both.ID], []string{"fog-connect/1", "fog-files/1"}) ||
		!reflect.DeepEqual(all[fedOnly.ID], []string{"fog-connect/1"}) {
		t.Errorf("AllAppContracts = %v", all)
	}
	if _, ok := all[corrupt.ID]; ok {
		t.Error("corrupt contracts row included")
	}
}
