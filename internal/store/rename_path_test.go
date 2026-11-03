package store

import (
	"reflect"
	"sort"
	"testing"
)

// tablePaths reads a path-keyed table's paths, sorted, straight from the
// database so the test covers tables without a listing method.
func tablePaths(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT path FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Every path-keyed table follows a rename: the entry itself and its
// descendants move; siblings that only share a prefix, names where LIKE's
// wildcards or case-folding would over-match, and unrelated paths stay put.
func TestRenamePathAllTables(t *testing.T) {
	s := testStore(t)
	peer, err := s.CreatePairedPeer("inst-1", "attic", "", "pk1")
	if err != nil {
		t.Fatal(err)
	}
	person, _ := s.CreatePerson("a", "A", PersonRoleOperator)

	// "a_%" is the renamed folder: "aX%…" and "ab…" would match an unescaped
	// LIKE, "A_%…" a case-folded one.
	paths := []string{"a_%", "a_%/b", "a_%/c/ü", "aX%", "aX%/b", "a_%b", "A_%", "A_%/b", "z"}
	for i, p := range paths {
		if err := s.AddFileExclude(p); err != nil {
			t.Fatal(err)
		}
		if err := s.AddFileShareExclude(p); err != nil {
			t.Fatal(err)
		}
		if err := s.AddFilePin(person, p, "dir"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateFileShare("d"+string(rune('0'+i)), p, "", "", 0); err != nil {
			t.Fatal(err)
		}
		sh, err := s.CreateFileFedShare(&FileFedShare{PeerID: peer.ID, Path: p, Title: p, Mode: "read", Scope: "all", Detail: "full", Apps: "allowed"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertFileFedOrigin(FileFedOrigin{Path: p, PeerID: peer.ID, ShareID: sh.ID, InstanceID: "inst-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RenamePath("a_%", "ñew"); err != nil {
		t.Fatal(err)
	}
	want := []string{"A_%", "A_%/b", "aX%", "aX%/b", "a_%b", "z", "ñew", "ñew/b", "ñew/c/ü"}
	for _, table := range []string{"file_backup_excludes", "file_share_excludes", "file_pins", "file_shares", "file_fed_shares", "file_fed_origins"} {
		if got := tablePaths(t, s, table); !reflect.DeepEqual(got, want) {
			t.Errorf("%s after rename = %q, want %q", table, got, want)
		}
	}

	// Renamed federation shares are marked dirty so peers refetch; others
	// aren't.
	shares, err := s.ListFileFedShares(peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range shares {
		moved := sh.Path == "ñew" || sh.Path == "ñew/b" || sh.Path == "ñew/c/ü"
		if sh.Dirty != moved {
			t.Errorf("share %q dirty = %v, want %v", sh.Path, sh.Dirty, moved)
		}
	}

	// Renaming to itself is a no-op, not a delete of the merged leftovers.
	if err := s.RenamePath("z", "z"); err != nil {
		t.Fatal(err)
	}
	if got := tablePaths(t, s, "file_pins"); !reflect.DeepEqual(got, want) {
		t.Errorf("pins after self-rename = %q", got)
	}
}

// An exclusion that lands on one already there merges into it.
func TestRenamePathMergesExcludes(t *testing.T) {
	s := testStore(t)
	for _, p := range []string{"a", "a/x", "b/x"} {
		if err := s.AddFileExclude(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RenamePath("a", "b"); err != nil {
		t.Fatal(err)
	}
	if got, want := tablePaths(t, s, "file_backup_excludes"), []string{"b", "b/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excludes = %q, want %q", got, want)
	}
}
