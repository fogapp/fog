package store

import (
	"database/sql"
	"strings"
)

// ComputeActivity is one thing an app asked of compute: an inference
// request, or a video run, from when fog took it to its outcome. A row is
// written as fog takes the work ("running") and finished when it ends, so
// the log shows what is in flight as well as what was.
type ComputeActivity struct {
	ID      int64  `json:"id"`
	At      string `json:"at"`
	EndedAt string `json:"endedAt,omitempty"`
	App     string `json:"app"`
	Pool    string `json:"pool"`
	// Kind is "inference" or "video".
	Kind string `json:"kind"`
	// Machine is the one it went to, by name; "" when fog refused it
	// before it reached one.
	Machine string `json:"machine,omitempty"`
	// What is the inference endpoint ("chat/completions") or the run's
	// class ("interactive", "batch").
	What  string `json:"what,omitempty"`
	Model string `json:"model,omitempty"`
	// Outcome is running, ok, failed, refused, lost, displaced or stopped.
	Outcome string `json:"outcome"`
	// Status is the HTTP status of an inference request, or a run's exit
	// status; nil when there was none.
	Status     *int  `json:"status,omitempty"`
	DurationMS int64 `json:"durationMs"`
	// TTFTMS is how long an inference request waited for its first byte.
	TTFTMS    int64 `json:"ttftMs,omitempty"`
	TokensIn  int64 `json:"tokensIn,omitempty"`
	TokensOut int64 `json:"tokensOut,omitempty"`
	// TokensEstimated: the server reported no usage, so TokensOut counts
	// the chunks it streamed.
	TokensEstimated bool `json:"tokensEstimated,omitempty"`
	// Message says why, for anything but ok: a refusal, an error, the
	// tail of a failed run's log.
	Message string `json:"message,omitempty"`
}

const computeActivityCols = `id, at, ended_at, app, pool, kind, machine, what, model, outcome, status,
	duration_ms, ttft_ms, tokens_in, tokens_out, tokens_estimated, message`

func scanComputeActivity(sc scanner) (ComputeActivity, error) {
	var a ComputeActivity
	var status sql.NullInt64
	err := sc.Scan(&a.ID, &a.At, &a.EndedAt, &a.App, &a.Pool, &a.Kind, &a.Machine, &a.What, &a.Model, &a.Outcome, &status,
		&a.DurationMS, &a.TTFTMS, &a.TokensIn, &a.TokensOut, &a.TokensEstimated, &a.Message)
	if status.Valid {
		n := int(status.Int64)
		a.Status = &n
	}
	return a, err
}

// maxActivityMessage bounds what a row keeps of a message: the tail of a
// run's log is the useful part, never all of it.
const maxActivityMessage = 2000

func clipMessage(s string) string {
	if len(s) <= maxActivityMessage {
		return s
	}
	s = s[len(s)-maxActivityMessage:]
	for len(s) > 0 && s[0]&0xC0 == 0x80 {
		s = s[1:]
	}
	return "…" + s
}

// AddComputeActivity records a, filling in its id (and At, when empty).
func (s *Store) AddComputeActivity(a *ComputeActivity) error {
	if a.At == "" {
		a.At = now()
	}
	a.Message = clipMessage(a.Message)
	res, err := s.db.Exec(`INSERT INTO compute_activity (at, ended_at, app, pool, kind, machine, what, model, outcome, status,
		duration_ms, ttft_ms, tokens_in, tokens_out, tokens_estimated, message) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.At, a.EndedAt, a.App, a.Pool, a.Kind, a.Machine, a.What, a.Model, a.Outcome, a.Status,
		a.DurationMS, a.TTFTMS, a.TokensIn, a.TokensOut, a.TokensEstimated, a.Message)
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// FinishComputeActivity records how a's work ended: everything but where
// and when it started.
func (s *Store) FinishComputeActivity(a *ComputeActivity) error {
	if a.EndedAt == "" {
		a.EndedAt = now()
	}
	a.Message = clipMessage(a.Message)
	return s.affectOne(`UPDATE compute_activity SET ended_at = ?, model = ?, outcome = ?, status = ?, duration_ms = ?,
		ttft_ms = ?, tokens_in = ?, tokens_out = ?, tokens_estimated = ?, message = ? WHERE id = ?`,
		a.EndedAt, a.Model, a.Outcome, a.Status, a.DurationMS, a.TTFTMS, a.TokensIn, a.TokensOut, a.TokensEstimated, a.Message, a.ID)
}

// LoseRunningComputeActivity marks lost every row still running: what a
// fogd that just started holds of work its last run handed out, none of
// which it can follow any more.
func (s *Store) LoseRunningComputeActivity(why string) (int64, error) {
	res, err := s.db.Exec(`UPDATE compute_activity SET outcome = 'lost', ended_at = ?, message = ? WHERE outcome = 'running'`, now(), why)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ComputeActivityFilter narrows the log; empty fields match everything.
type ComputeActivityFilter struct {
	App, Kind, Machine string
	// Problems keeps only what went wrong: failed, lost or refused, not
	// work that was displaced or stopped as asked.
	Problems bool
	// Before reads back from the row with this id; 0 is the newest.
	Before int64
	Limit  int
}

// ComputeActivityPage answers the log newest first, narrowed by f.
func (s *Store) ComputeActivityPage(f ComputeActivityFilter) ([]ComputeActivity, error) {
	var where []string
	var args []any
	for col, v := range map[string]string{"app": f.App, "kind": f.Kind, "machine": f.Machine} {
		if v != "" {
			where = append(where, col+" = ?")
			args = append(args, v)
		}
	}
	if f.Problems {
		where = append(where, "outcome IN ('failed', 'lost', 'refused')")
	}
	if f.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	q := `SELECT ` + computeActivityCols + ` FROM compute_activity`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	return orEmpty(queryAll(s.db, scanComputeActivity, q, args...))
}

// PruneComputeActivity forgets what started before cutoff, except what is
// still running.
func (s *Store) PruneComputeActivity(cutoff string) (int64, error) {
	return s.pruneBatched("compute_activity", "at < ? AND outcome != 'running'", cutoff)
}
