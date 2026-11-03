// Issue storage for fog's configuration scanner (internal/issues): one row
// per distinct discrepancy, with auto-resolve + ack-memory lifecycle. The
// scanner re-derives the full set of findings every sweep and calls
// SyncIssues per check; this file owns the transition rules.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

type Issue struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	AppID       int64  `json:"appId,omitempty"` // 0 = host scope
	AppName     string `json:"appName,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Severity    string `json:"severity" ts:"\"critical\" | \"warning\" | \"info\""` // critical | warning | info
	Title       string `json:"title"`
	Detail      string `json:"detail,omitempty"`
	Fix         string `json:"fix,omitempty"`
	FixURL      string `json:"fixUrl,omitempty"` // in-app path the fix text points at
	Data        string `json:"data,omitempty"`
	Status      string `json:"status" ts:"\"open\" | \"acked\" | \"resolved\""` // open | acked | resolved
	DetailsHash string `json:"-"`
	FirstSeen   string `json:"firstSeen"`
	LastSeen    string `json:"lastSeen"`
	ResolvedAt  string `json:"resolvedAt,omitempty"`
	AckedAt     string `json:"ackedAt,omitempty"`
}

// IssueFinding is one observed fact from a check run.
type IssueFinding struct {
	Kind                               string
	AppID                              int64 // 0 = host scope
	AppName                            string
	Subject                            string
	Severity, Title, Detail, Fix, Data string
	FixURL                             string
}

// IssueSyncResult reports the transitions one sweep caused for a check, so
// the caller notifies on exactly the moments that matter and stays silent
// on the rest (the flare IngestResult idea, applied to a full re-scan).
type IssueSyncResult struct {
	New      []Issue // first time this identity has ever been seen
	Recurred []Issue // was resolved, reproduced again (ack cleared)
	Reopened []Issue // was acked, details changed enough to break the ack
	Resolved []Issue // open or acked last sweep, absent this sweep
}

// issueHash fingerprints the parts of a finding an acknowledgement covers.
// Data and FixURL are deliberately excluded: they are display payload, and
// refreshing them must not un-ack an issue.
func issueHash(f IssueFinding) string {
	sum := sha256.Sum256([]byte(f.Severity + "|" + f.Title + "|" + f.Detail + "|" + f.Fix))
	return hex.EncodeToString(sum[:])[:16]
}

// SyncIssues reconciles one check's findings against the stored issues, in
// a single transaction (the store holds one connection; interleaving with
// other writers buys nothing). evaluated lists the scopes (app IDs, 0 for
// host) the check actually assessed this sweep: only those scopes can
// auto-resolve. A scope the check skipped — an app mid-deploy — is absent
// from evaluated, so its issues survive untouched rather than resolving on
// missing evidence.
func (s *Store) SyncIssues(kind string, evaluated []int64, findings []IssueFinding) (res *IssueSyncResult, err error) {
	err = s.withTx(func(tx *sql.Tx) error {
		res, err = syncIssuesTx(tx, kind, evaluated, findings)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func syncIssuesTx(tx *sql.Tx, kind string, evaluated []int64, findings []IssueFinding) (*IssueSyncResult, error) {
	n := now()
	res := &IssueSyncResult{}
	touched := make(map[int64]bool)

	for _, f := range findings {
		if f.Kind != kind {
			return nil, fmt.Errorf("finding kind %q under sync of %q", f.Kind, kind)
		}
		hash := issueHash(f)
		var appID any // NULL for host scope
		if f.AppID != 0 {
			appID = f.AppID
		}
		var cur Issue
		var ackedAt sql.NullString
		err := tx.QueryRow(`SELECT id, status, details_hash, acked_at FROM issues
			WHERE kind = ? AND COALESCE(app_id, 0) = ? AND subject = ?`,
			kind, f.AppID, f.Subject).Scan(&cur.ID, &cur.Status, &cur.DetailsHash, &ackedAt)
		switch {
		case err == sql.ErrNoRows:
			r, err := tx.Exec(`INSERT INTO issues
				(kind, app_id, app_name, subject, severity, title, detail, fix, fix_url, data, status, details_hash, first_seen, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
				kind, appID, f.AppName, f.Subject, f.Severity, f.Title, f.Detail, f.Fix, f.FixURL, f.Data, hash, n, n)
			if err != nil {
				return nil, err
			}
			id, _ := r.LastInsertId()
			touched[id] = true
			res.New = append(res.New, issueFromFinding(id, f, "open", hash, n, n))
		case err != nil:
			return nil, err
		default:
			touched[cur.ID] = true
			switch {
			case cur.Status == "resolved":
				// The fact came back: a fresh generation. The old ack was
				// for the old occurrence and does not carry over.
				if _, err := tx.Exec(`UPDATE issues SET status = 'open', resolved_at = NULL, acked_at = NULL,
					app_name = ?, severity = ?, title = ?, detail = ?, fix = ?, fix_url = ?, data = ?, details_hash = ?, last_seen = ?
					WHERE id = ?`,
					f.AppName, f.Severity, f.Title, f.Detail, f.Fix, f.FixURL, f.Data, hash, n, cur.ID); err != nil {
					return nil, err
				}
				res.Recurred = append(res.Recurred, issueFromFinding(cur.ID, f, "open", hash, "", n))
			case cur.Status == "acked" && cur.DetailsHash != hash:
				// Acked, but the finding materially changed (severity is in
				// the hash, so escalation lands here too) — resurface it.
				if _, err := tx.Exec(`UPDATE issues SET status = 'open', acked_at = NULL,
					app_name = ?, severity = ?, title = ?, detail = ?, fix = ?, fix_url = ?, data = ?, details_hash = ?, last_seen = ?
					WHERE id = ?`,
					f.AppName, f.Severity, f.Title, f.Detail, f.Fix, f.FixURL, f.Data, hash, n, cur.ID); err != nil {
					return nil, err
				}
				res.Reopened = append(res.Reopened, issueFromFinding(cur.ID, f, "open", hash, "", n))
			case cur.Status == "acked":
				// Still acked, unchanged: keep the silence, refresh only the
				// display payload and the liveness stamp.
				if _, err := tx.Exec(`UPDATE issues SET data = ?, last_seen = ? WHERE id = ?`,
					f.Data, n, cur.ID); err != nil {
					return nil, err
				}
			default: // open
				if _, err := tx.Exec(`UPDATE issues SET app_name = ?, severity = ?, title = ?, detail = ?, fix = ?, fix_url = ?, data = ?, details_hash = ?, last_seen = ?
					WHERE id = ?`,
					f.AppName, f.Severity, f.Title, f.Detail, f.Fix, f.FixURL, f.Data, hash, n, cur.ID); err != nil {
					return nil, err
				}
			}
		}
	}

	// Auto-resolve: anything of this kind in an evaluated scope that the
	// sweep did not reproduce has stopped being true.
	if len(evaluated) > 0 {
		scopePh := strings.TrimSuffix(strings.Repeat("?,", len(evaluated)), ",")
		args := []any{kind}
		for _, sc := range evaluated {
			args = append(args, sc)
		}
		rows, err := tx.Query(`SELECT id, kind, COALESCE(app_id, 0), app_name, subject, severity, title, detail, fix, fix_url, data, status, details_hash, first_seen, last_seen
			FROM issues WHERE kind = ? AND status IN ('open', 'acked') AND COALESCE(app_id, 0) IN (`+scopePh+`)`, args...)
		if err != nil {
			return nil, err
		}
		var doomed []Issue
		for rows.Next() {
			var i Issue
			if err := rows.Scan(&i.ID, &i.Kind, &i.AppID, &i.AppName, &i.Subject, &i.Severity, &i.Title, &i.Detail, &i.Fix, &i.FixURL, &i.Data, &i.Status, &i.DetailsHash, &i.FirstSeen, &i.LastSeen); err != nil {
				rows.Close()
				return nil, err
			}
			if !touched[i.ID] {
				doomed = append(doomed, i)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, i := range doomed {
			if _, err := tx.Exec(`UPDATE issues SET status = 'resolved', resolved_at = ? WHERE id = ?`, n, i.ID); err != nil {
				return nil, err
			}
			i.Status = "resolved"
			i.ResolvedAt = n
			res.Resolved = append(res.Resolved, i)
		}
	}

	return res, nil
}

func issueFromFinding(id int64, f IssueFinding, status, hash, firstSeen, lastSeen string) Issue {
	return Issue{
		ID: id, Kind: f.Kind, AppID: f.AppID, AppName: f.AppName, Subject: f.Subject,
		Severity: f.Severity, Title: f.Title, Detail: f.Detail, Fix: f.Fix, FixURL: f.FixURL, Data: f.Data,
		Status: status, DetailsHash: hash, FirstSeen: firstSeen, LastSeen: lastSeen,
	}
}

const issueCols = `id, kind, COALESCE(app_id, 0), app_name, subject, severity, title, detail, fix, fix_url, data, status, details_hash, first_seen, last_seen, COALESCE(resolved_at, ''), COALESCE(acked_at, '')`

func scanIssue(sc scanner) (Issue, error) {
	var i Issue
	err := sc.Scan(&i.ID, &i.Kind, &i.AppID, &i.AppName, &i.Subject, &i.Severity, &i.Title, &i.Detail, &i.Fix, &i.FixURL, &i.Data, &i.Status, &i.DetailsHash, &i.FirstSeen, &i.LastSeen, &i.ResolvedAt, &i.AckedAt)
	return i, err
}

// ListIssues returns issues in triage order: open before acked before
// resolved, most severe first within a status, most recently seen first
// within a severity. statuses filters; empty means all.
func (s *Store) ListIssues(statuses []string, limit int) ([]Issue, error) {
	q := `SELECT ` + issueCols + ` FROM issues`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",") + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY
		CASE status WHEN 'open' THEN 0 WHEN 'acked' THEN 1 ELSE 2 END,
		CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
		last_seen DESC LIMIT ?`
	args = append(args, limit)
	return queryAll(s.rdb, scanIssue, q, args...)
}

func (s *Store) IssueByID(id int64) (*Issue, error) {
	return ptr(queryOne(s.rdb, scanIssue, `SELECT `+issueCols+` FROM issues WHERE id = ?`, id))
}

// AckIssue silences an open issue. Only open issues can be acknowledged —
// acking a resolved row would be a no-op with a misleading timestamp.
func (s *Store) AckIssue(id int64) error {
	r, err := s.db.Exec(`UPDATE issues SET status = 'acked', acked_at = ? WHERE id = ? AND status = 'open'`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("issue is not open")
	}
	return nil
}

func (s *Store) UnackIssue(id int64) error {
	r, err := s.db.Exec(`UPDATE issues SET status = 'open', acked_at = NULL WHERE id = ? AND status = 'acked'`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("issue is not acknowledged")
	}
	return nil
}

// CountOpenIssues reports open (unacknowledged) issues by severity — the
// badge's data. Acked issues are deliberately excluded: the badge exists
// to demand attention, and acked means attention was already paid.
func (s *Store) CountOpenIssues() (map[string]int64, error) {
	out := map[string]int64{}
	err := queryEach(s.rdb, func(sc scanner) error {
		var sev string
		var c int64
		if err := sc.Scan(&sev, &c); err != nil {
			return err
		}
		out[sev] = c
		return nil
	}, `SELECT severity, COUNT(*) FROM issues WHERE status = 'open' GROUP BY severity`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PruneResolvedIssues drops resolved rows not seen since cutoff — history
// is useful for a while, then dead weight.
func (s *Store) PruneResolvedIssues(cutoff string) (int64, error) {
	return s.pruneBatched("issues", "status = 'resolved' AND last_seen < ?", cutoff)
}
