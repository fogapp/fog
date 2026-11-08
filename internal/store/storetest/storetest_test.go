package storetest

import (
	"path/filepath"
	"reflect"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// A store from the template must be indistinguishable from one store.Open
// migrated itself: same schema, same migrations recorded, same pragmas.
func TestTemplateStoreMatchesAFreshOne(t *testing.T) {
	dir := t.TempDir()
	fresh, err := store.Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	copied, err := Open(filepath.Join(dir, "copied.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()

	for _, q := range []string{
		`SELECT type || ' ' || name || ' ' || COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`,
		`SELECT name FROM schema_migrations ORDER BY name`,
		`PRAGMA journal_mode`,
		`PRAGMA foreign_keys`,
		`PRAGMA user_version`,
	} {
		if a, b := rows(t, fresh, q), rows(t, copied, q); !reflect.DeepEqual(a, b) {
			t.Errorf("%s\nfresh:  %q\ncopied: %q", q, a, b)
		}
	}
}

// An existing path is opened as it is, not overwritten with the template.
func TestOpenKeepsAnExistingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fog.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("kept", "yes"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, _ := st.GetSetting("kept"); v != "yes" {
		t.Fatalf("reopening lost the store's contents: kept=%q", v)
	}
}

func rows(t *testing.T, st *store.Store, q string) []string {
	t.Helper()
	r, err := st.DB().Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []string
	for r.Next() {
		var s string
		if err := r.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}
