package pg

import (
	"strings"
	"testing"
)

// A schema an app named with a newline, a semicolon or a quote in it is
// legal Postgres. Reading pg_restore's script line by line, such a name
// split its CREATE SCHEMA across lines and the parser refused the dump —
// one app's schema name failing every restore of its database, and with it
// the fleet's major upgrade. Read statement by statement, each is one name.
func TestParseSchemaNamesWithNewlinesAndSemicolons(t *testing.T) {
	script := strings.Join([]string{
		"--",
		"-- PostgreSQL database dump",
		"--",
		"SET statement_timeout = 0;",
		"SELECT pg_catalog.set_config('search_path', '', false);",
		"SET client_encoding = 'UTF8';",
		"--",
		"-- Name: we\"ird; Type: SCHEMA; Schema: -; Owner: app",
		"--",
		"CREATE SCHEMA \"we\nird\";",
		"CREATE SCHEMA \"a;b\";",
		"CREATE SCHEMA \"q\"\"uote\";",
		"CREATE SCHEMA plain;",
		"CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA \"we\nird\";",
		"CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;",
		"CREATE SCHEMA pg_temp_9;",
		"",
	}, "\n")
	schemas, exts, err := parseSchemaExtensionSQL(script)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"we\nird", "a;b", "q\"uote", "plain"}
	if strings.Join(schemas, "|") != strings.Join(want, "|") {
		t.Fatalf("schemas = %q, want %q", schemas, want)
	}
	if len(exts) != 2 || exts[0] != (extTarget{Name: "citext", Schema: "we\nird"}) || exts[1] != (extTarget{Name: "pgcrypto", Schema: "public"}) {
		t.Fatalf("exts = %q", exts)
	}
	// Something that is not one of pg_dump's own shapes is still refused.
	for _, bad := range []string{
		"CREATE SCHEMA x AUTHORIZATION postgres;",
		"CREATE SCHEMA x; CREATE SCHEMA \"y\" ; ",
		"CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA x CASCADE;",
	} {
		if _, _, err := parseSchemaExtensionSQL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// An extension is never put into a system schema, wherever the dump says
// it lives: pg_catalog is searched first by every role in the database.
func TestSystemSchemas(t *testing.T) {
	for s, want := range map[string]bool{"pg_catalog": true, "pg_toast": true, "pg_temp_3": true, "information_schema": true, "public": false, "ext": false, "pgx": false} {
		if systemSchema(s) != want {
			t.Errorf("systemSchema(%q) = %v", s, !want)
		}
	}
}
