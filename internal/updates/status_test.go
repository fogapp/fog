package updates

import (
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func TestFleetStatus(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	auto, _ := st.CreateApp("auto")
	st.CreateRevision(auto.ID, `{"source":{"type":"image","image":"x:1"},"updatePolicy":"auto"}`, "", "test")
	st.SetAppUpdate(auto.ID, "sha256:aaaaaaaaaaaaaaaa", "sha256:bbbbbbbbbbbbbbbb", true)

	plain, _ := st.CreateApp("plain")
	st.CreateRevision(plain.ID, `{"source":{"type":"git","repo":"git@x:y.git"}}`, "", "test")

	rows, err := FleetStatus(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want two rows, got %+v", rows)
	}
	a := rows[0]
	if a.App != "auto" || a.Policy != "auto" || !a.UpdateAvailable || !a.Comparable || a.CheckedAt == "" || a.Current == "" {
		t.Errorf("auto: %+v", a)
	}
	// Never checked: no verdict either way, and the policy reads as the
	// engine's default rather than an empty string.
	p := rows[1]
	if p.App != "plain" || p.Policy != "manual" || p.Source != "git" || p.UpdateAvailable || p.Comparable || p.CheckedAt != "" {
		t.Errorf("plain: %+v", p)
	}
}
