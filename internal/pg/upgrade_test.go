package pg

import (
	"strings"
	"testing"
)

// pg_dumpall re-creates the bootstrap superuser, which a fresh initdb
// already has; those two lines — and only those — are dropped so the
// restore can run strict.
func TestFilterDump(t *testing.T) {
	in := strings.Join([]string{
		"--",
		"CREATE ROLE feed;",
		"ALTER ROLE feed WITH LOGIN PASSWORD 'SCRAM-SHA-256$x';",
		"CREATE ROLE postgres;",
		"ALTER ROLE postgres WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS PASSWORD 'SCRAM-SHA-256$y';",
		"CREATE ROLE postgres_reader;",
		"COPY public.items (id, name) FROM stdin;",
		"1\tpostgres",
		"\\.",
	}, "\n") + "\n"
	var out strings.Builder
	if err := filterDump(&out, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, dropped := range []string{"CREATE ROLE postgres;", "ALTER ROLE postgres WITH"} {
		if strings.Contains(got, dropped) {
			t.Errorf("superuser line survived: %q", dropped)
		}
	}
	for _, kept := range []string{"CREATE ROLE feed;", "ALTER ROLE feed WITH", "CREATE ROLE postgres_reader;", "1\tpostgres\n", "COPY public.items"} {
		if !strings.Contains(got, kept) {
			t.Errorf("line dropped that must survive: %q", kept)
		}
	}
	// Bytes in, bytes out, minus exactly the two lines.
	if want := len(in) - len("CREATE ROLE postgres;\n") - len("ALTER ROLE postgres WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS PASSWORD 'SCRAM-SHA-256$y';\n"); len(got) != want {
		t.Errorf("filtered length %d, want %d", len(got), want)
	}
}

func inv(roles []string, dbs map[string]dbInventory) inventory {
	return inventory{roles: roles, dbs: dbs}
}

// The restore is trusted only when every role, database, table, and row
// count is identical and every extension is present; versions may rise.
func TestInventoryCompare(t *testing.T) {
	base := inv([]string{"feed", "hub"}, map[string]dbInventory{
		"feed_production": {rows: map[string]int64{"public.items": 10, "public.users": 2}, extensions: map[string]string{"vector": "0.8.0", "pg_trgm": "1.6"}},
		"hub_production":  {rows: map[string]int64{"public.people": 43}, extensions: map[string]string{"pg_trgm": "1.6"}},
	})
	same := inv([]string{"feed", "hub"}, map[string]dbInventory{
		"feed_production": {rows: map[string]int64{"public.items": 10, "public.users": 2}, extensions: map[string]string{"vector": "0.8.6", "pg_trgm": "1.7"}},
		"hub_production":  {rows: map[string]int64{"public.people": 43}, extensions: map[string]string{"pg_trgm": "1.7"}},
	})
	if d := base.compare(same); d != "" {
		t.Fatalf("newer extension versions must compare equal, got %q", d)
	}
	cases := map[string]inventory{
		"missing role":      inv([]string{"feed"}, same.dbs),
		"missing database":  inv(same.roles, map[string]dbInventory{"feed_production": same.dbs["feed_production"]}),
		"extra database":    inv(same.roles, map[string]dbInventory{"feed_production": same.dbs["feed_production"], "hub_production": same.dbs["hub_production"], "stray": {}}),
		"missing table":     inv(same.roles, map[string]dbInventory{"feed_production": {rows: map[string]int64{"public.items": 10}, extensions: same.dbs["feed_production"].extensions}, "hub_production": same.dbs["hub_production"]}),
		"row count off":     inv(same.roles, map[string]dbInventory{"feed_production": {rows: map[string]int64{"public.items": 9, "public.users": 2}, extensions: same.dbs["feed_production"].extensions}, "hub_production": same.dbs["hub_production"]}),
		"extension missing": inv(same.roles, map[string]dbInventory{"feed_production": {rows: same.dbs["feed_production"].rows, extensions: map[string]string{"pg_trgm": "1.7"}}, "hub_production": same.dbs["hub_production"]}),
		"table renamed":     inv(same.roles, map[string]dbInventory{"feed_production": {rows: map[string]int64{"public.item": 10, "public.users": 2}, extensions: same.dbs["feed_production"].extensions}, "hub_production": same.dbs["hub_production"]}),
	}
	for name, other := range cases {
		if d := base.compare(other); d == "" {
			t.Errorf("%s: compare must fail", name)
		}
	}
	if base.tables() != 3 || base.rows() != 55 {
		t.Fatalf("totals: %d tables, %d rows", base.tables(), base.rows())
	}
}
