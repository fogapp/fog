package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestWithTxCommitsOrRollsBack(t *testing.T) {
	s := testStore(t)
	if err := s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('kept', '1', '')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('dropped', '1', '')`); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if v, _ := s.GetSetting("kept"); v != "1" {
		t.Error("committed write missing")
	}
	if v, _ := s.GetSetting("dropped"); v != "" {
		t.Error("rolled-back write survived")
	}
}

func TestPruneBatchedCrossesBatches(t *testing.T) {
	s := testStore(t)
	n := pruneBatch + 7
	if err := s.withTx(func(tx *sql.Tx) error {
		for i := range n + 3 {
			at := "2020-01-01T00:00:00Z"
			if i >= n {
				at = "2030-01-01T00:00:00Z"
			}
			if _, err := tx.Exec(`INSERT INTO audit_log (at, actor, action, subject, detail) VALUES (?, 'a', 'x', 's', '')`, at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.pruneBatched("audit_log", "at < ?", "2025-01-01T00:00:00Z")
	if err != nil || got != int64(n) {
		t.Fatalf("pruned %d, %v; want %d", got, err, n)
	}
	var left int
	s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&left)
	if left != 3 {
		t.Errorf("%d rows left, want 3", left)
	}
}

// pruneExpired runs each rule against the current time and counts every
// table's rows; an OR in a rule stays inside that rule.
func TestPruneExpiredCountsEveryTable(t *testing.T) {
	s := testStore(t)
	for _, q := range []string{
		`INSERT INTO audit_log (at, actor, action, subject, detail) VALUES ('2020-01-01T00:00:00Z', 'a', 'x', 's', '')`,
		`INSERT INTO audit_log (at, actor, action, subject, detail) VALUES ('2999-01-01T00:00:00Z', 'a', 'used', 's', '')`,
		`INSERT INTO audit_log (at, actor, action, subject, detail) VALUES ('2999-01-01T00:00:00Z', 'a', 'x', 's', '')`,
		`INSERT INTO settings (key, value, updated_at) VALUES ('old', '1', '2020-01-01T00:00:00Z')`,
		`INSERT INTO settings (key, value, updated_at) VALUES ('new', '1', '2999-01-01T00:00:00Z')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.pruneExpired(
		expiry{"audit_log", "at <= ? OR action = 'used'"},
		expiry{"settings", "updated_at <= ?"},
	)
	if err != nil || got != 3 {
		t.Fatalf("pruned %d, %v; want 3", got, err)
	}
	var audit, settings int
	s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audit)
	s.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key IN ('old', 'new')`).Scan(&settings)
	if audit != 1 || settings != 1 {
		t.Errorf("left %d audit rows and %d settings, want 1 and 1", audit, settings)
	}
}
