// Flare's storage: error groups (one per distinct failure, keyed by
// fingerprint) and their stored occurrences. See clients/flare/README.md.
package store

import (
	"database/sql"
	"errors"
)

type ErrorGroup struct {
	ID int64 `json:"id"`
	// Exactly one of AppID/PeerID is set (schema CHECK): a group belongs to
	// a local app or to a peer instance's app, never both. AppName is
	// denormalized so peer groups — whose apps may not exist here — still
	// have a name to show.
	AppID        int64  `json:"appId,omitempty"`
	PeerID       int64  `json:"peerId,omitempty"`
	AppName      string `json:"appName"`
	Fingerprint  string `json:"fingerprint"`
	ErrorClass   string `json:"errorClass"`
	Title        string `json:"title"`
	Culprit      string `json:"culprit"`
	Level        string `json:"level"`
	Status       string `json:"status" ts:"\"open\" | \"resolved\" | \"regressed\" | \"ignored\""`
	EventsCount  int64  `json:"eventsCount"`
	Unhandled    bool   `json:"unhandled"`
	FirstSeen    string `json:"firstSeen"`
	LastSeen     string `json:"lastSeen"`
	FirstRelease string `json:"firstRelease"`
	LastRelease  string `json:"lastRelease"`
	ResolvedAt   string `json:"resolvedAt,omitempty"`
	ResolvedRel  string `json:"resolvedRelease,omitempty"`
	IgnoredUntil string `json:"ignoredUntil,omitempty"`
	IgnoreCount  int64  `json:"ignoreUntilCount,omitempty"`
	LastNotified string `json:"lastNotifiedAt,omitempty"`
}

type ErrorEvent struct {
	ID          int64  `json:"id"`
	GroupID     int64  `json:"groupId"`
	EventID     string `json:"eventId"`
	ReceivedAt  string `json:"receivedAt"`
	Release     string `json:"release"`
	Environment string `json:"environment"`
	Txn         string `json:"transaction"`
	Payload     string `json:"payload,omitempty"`
}

// ErrorIngest is what one occurrence contributes to its group.
type ErrorIngest struct {
	Fingerprint string
	ErrorClass  string
	Title       string
	Culprit     string
	Level       string
	Unhandled   bool
	EventID     string
	Release     string
	Environment string
	Txn         string
	Payload     string
}

// IngestResult reports the state transition an occurrence caused, so the
// caller can notify on exactly the two moments that matter (new, regressed)
// and stay silent on the rest.
type IngestResult struct {
	Group     *ErrorGroup
	New       bool // first occurrence of this fingerprint
	Regressed bool // arrived on a resolved group with a different release
	Stored    bool // false when deduped by event_id
}

// eventsPerGroupCap bounds stored payloads per group. Counters are never
// capped — a crash loop keeps counting, it just stops accumulating disk.
const eventsPerGroupCap = 50

// IngestError upserts a local app's group for one occurrence and stores its
// payload, in a single transaction (the store holds one connection; two
// round-trips would interleave with every other writer for no benefit).
func (s *Store) IngestError(appID int64, appName string, in ErrorIngest) (*IngestResult, error) {
	return s.ingest(groupOwner{appID: appID, appName: appName}, in)
}

// IngestPeerError upserts a peer-owned group. Deliberately never merged
// with local groups even on matching fingerprints: "broken for me" and
// "broken for them" are different facts on different releases.
func (s *Store) IngestPeerError(peerID int64, appName string, in ErrorIngest) (*IngestResult, error) {
	return s.ingest(groupOwner{peerID: peerID, appName: appName}, in)
}

type groupOwner struct {
	appID   int64
	peerID  int64
	appName string
}

func (s *Store) ingest(owner groupOwner, in ErrorIngest) (*IngestResult, error) {
	res := &IngestResult{}
	var groupID int64
	err := s.withTx(func(tx *sql.Tx) error {
		var err error
		groupID, err = ingestTx(tx, owner, in, res)
		return err
	})
	if err != nil {
		return nil, err
	}
	full, err := s.ErrorGroupByID(groupID)
	if err != nil {
		return nil, err
	}
	res.Group = full
	return res, nil
}

// ingestTx files one event under its group inside tx, filling in res's
// flags, and returns the group's id.
func ingestTx(tx *sql.Tx, owner groupOwner, in ErrorIngest, res *IngestResult) (int64, error) {
	n := now()
	g := &ErrorGroup{}
	var resolvedRel sql.NullString
	var ignoredUntil sql.NullString
	var ignoreCount sql.NullInt64
	var row *sql.Row
	if owner.peerID != 0 {
		row = tx.QueryRow(`SELECT id, status, events_count, unhandled, resolved_release, ignored_until, ignore_until_count
			FROM error_groups WHERE peer_id = ? AND app_name = ? AND fingerprint = ?`,
			owner.peerID, owner.appName, in.Fingerprint)
	} else {
		row = tx.QueryRow(`SELECT id, status, events_count, unhandled, resolved_release, ignored_until, ignore_until_count
			FROM error_groups WHERE app_id = ? AND fingerprint = ? AND peer_id IS NULL`,
			owner.appID, in.Fingerprint)
	}
	err := row.Scan(&g.ID, &g.Status, &g.EventsCount, &g.Unhandled, &resolvedRel, &ignoredUntil, &ignoreCount)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res.New = true
		appID := sql.NullInt64{Int64: owner.appID, Valid: owner.peerID == 0}
		peerID := sql.NullInt64{Int64: owner.peerID, Valid: owner.peerID != 0}
		r, err := tx.Exec(`INSERT INTO error_groups
			(app_id, peer_id, app_name, fingerprint, error_class, title, culprit, level, status, events_count, unhandled,
			 first_seen, last_seen, first_release, last_release)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'open', 1, ?, ?, ?, ?, ?)`,
			appID, peerID, owner.appName, in.Fingerprint, in.ErrorClass, in.Title, in.Culprit, in.Level,
			boolToInt(in.Unhandled), n, n, in.Release, in.Release)
		if err != nil {
			return 0, err
		}
		g.ID, _ = r.LastInsertId()
		g.Status = "open"
		g.EventsCount = 1
	case err != nil:
		return 0, err
	default:
		// Dedupe before touching the group: a transport-retried event_id must
		// leave the group row byte-identical — no status flip, no refreshed
		// last_seen, no rewritten title — while reporting Stored:false. (When
		// the UPDATE ran first and only the counter was reverted, a stale
		// retry could regress a resolved group with no notification.) The
		// store holds one connection, so check-then-insert cannot race.
		var one int
		derr := tx.QueryRow(`SELECT 1 FROM error_events WHERE group_id = ? AND event_id = ?`,
			g.ID, in.EventID).Scan(&one)
		if derr == nil {
			return g.ID, nil // Stored, New, Regressed all false
		}
		if !errors.Is(derr, sql.ErrNoRows) {
			return 0, derr
		}
		status := g.Status
		switch status {
		case "resolved":
			// Same release as at resolve time is the fix still rolling out,
			// not a regression. An empty release on either side means we can't
			// tell releases apart, so err toward re-opening.
			if resolvedRel.String == "" || in.Release == "" || in.Release != resolvedRel.String {
				status = "regressed"
				res.Regressed = true
			}
		case "ignored":
			// Escape hatches: a count threshold un-ignores; a duration is
			// checked lazily here (no background sweep needed).
			if ignoreCount.Valid && g.EventsCount+1 >= ignoreCount.Int64 {
				status = "open"
				res.New = true // notify as if new: the operator asked to hear about it again
			} else if ignoredUntil.Valid && ignoredUntil.String != "" && ignoredUntil.String <= n {
				status = "open"
				res.New = true
			}
		}
		// Title/culprit/level track the latest occurrence: messages carry
		// current details ("timeout after 30s") and stale ones mislead.
		_, err = tx.Exec(`UPDATE error_groups SET
				events_count = events_count + 1, last_seen = ?, last_release = ?,
				title = ?, culprit = ?, level = ?, status = ?,
				unhandled = MAX(unhandled, ?)
			WHERE id = ?`,
			n, in.Release, in.Title, in.Culprit, in.Level, status, boolToInt(in.Unhandled), g.ID)
		if err != nil {
			return 0, err
		}
		g.EventsCount++
		g.Status = status
	}

	r, err := tx.Exec(`INSERT OR IGNORE INTO error_events
		(group_id, event_id, received_at, release, environment, txn, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		g.ID, in.EventID, n, in.Release, in.Environment, in.Txn, in.Payload)
	if err != nil {
		return 0, err
	}
	// The dedupe check above (and a fresh group's first event) means this
	// insert always lands; OR IGNORE stays as a belt-and-suspenders on the
	// unique index rather than a code path with meaning.
	inserted, _ := r.RowsAffected()
	res.Stored = inserted > 0

	// Payload cap: drop the oldest stored events beyond the window. first_seen
	// and first_release survive on the group row.
	if res.Stored {
		if _, err := tx.Exec(`DELETE FROM error_events WHERE group_id = ? AND id NOT IN
			(SELECT id FROM error_events WHERE group_id = ? ORDER BY id DESC LIMIT ?)`,
			g.ID, g.ID, eventsPerGroupCap); err != nil {
			return 0, err
		}
	}

	return g.ID, nil
}

const errorGroupCols = `id, COALESCE(app_id, 0), COALESCE(peer_id, 0), app_name, fingerprint, error_class, title, culprit, level, status,
	events_count, unhandled, first_seen, last_seen, first_release, last_release,
	COALESCE(resolved_at, ''), COALESCE(resolved_release, ''),
	COALESCE(ignored_until, ''), COALESCE(ignore_until_count, 0), COALESCE(last_notified_at, '')`

func scanErrorGroup(sc scanner) (ErrorGroup, error) {
	var g ErrorGroup
	var unhandled int
	err := sc.Scan(&g.ID, &g.AppID, &g.PeerID, &g.AppName, &g.Fingerprint, &g.ErrorClass, &g.Title, &g.Culprit, &g.Level, &g.Status,
		&g.EventsCount, &unhandled, &g.FirstSeen, &g.LastSeen, &g.FirstRelease, &g.LastRelease,
		&g.ResolvedAt, &g.ResolvedRel, &g.IgnoredUntil, &g.IgnoreCount, &g.LastNotified)
	g.Unhandled = unhandled != 0
	return g, err
}

func (s *Store) ErrorGroupByID(id int64) (*ErrorGroup, error) {
	return ptr(queryOne(s.db, scanErrorGroup, `SELECT `+errorGroupCols+` FROM error_groups WHERE id = ?`, id))
}

// ListErrorGroups returns a local app's groups, optionally filtered by
// status (empty = all), most recently seen first. "open" takes in regressed
// too: a regression is open again, and that's the set the badge counts.
func (s *Store) ListErrorGroups(appID int64, status string, limit int) ([]ErrorGroup, error) {
	q := `SELECT ` + errorGroupCols + ` FROM error_groups WHERE app_id = ? AND peer_id IS NULL`
	args := []any{appID}
	switch status {
	case "":
	case "open":
		q += ` AND status IN ('open', 'regressed')`
	default:
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY last_seen DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryErrorGroups(q, args...)
}

// ListOpenErrorGroupsAll is the fleet inbox: open and regressed groups across
// every app, newest first.
func (s *Store) ListOpenErrorGroupsAll(limit int) ([]ErrorGroup, error) {
	return s.queryErrorGroups(`SELECT `+errorGroupCols+` FROM error_groups
		WHERE status IN ('open', 'regressed') ORDER BY last_seen DESC, id DESC LIMIT ?`, limit)
}

// CountOpenErrorGroupsAll is how many groups ListOpenErrorGroupsAll would
// list with no limit: the Inbox badge.
func (s *Store) CountOpenErrorGroupsAll() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM error_groups WHERE status IN ('open', 'regressed')`).Scan(&n)
	return n, err
}

func (s *Store) queryErrorGroups(q string, args ...any) ([]ErrorGroup, error) {
	return queryAll(s.db, scanErrorGroup, q, args...)
}

// CountOpenErrorGroups counts open+regressed local groups per app id, for
// the dashboard badge. Peer groups are the peer's problem to badge.
func (s *Store) CountOpenErrorGroups() (map[int64]int64, error) {
	out := map[int64]int64{}
	err := queryEach(s.db, func(sc scanner) error {
		var appID, n int64
		if err := sc.Scan(&appID, &n); err != nil {
			return err
		}
		out[appID] = n
		return nil
	}, `SELECT app_id, COUNT(*) FROM error_groups
		WHERE status IN ('open', 'regressed') AND peer_id IS NULL GROUP BY app_id`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListErrorEvents returns a group's stored occurrences without payloads
// (list view); fetch one payload at a time via ErrorEventByID.
func (s *Store) ListErrorEvents(groupID int64, limit int) ([]ErrorEvent, error) {
	return queryAll(s.db, func(sc scanner) (ErrorEvent, error) {
		var e ErrorEvent
		err := sc.Scan(&e.ID, &e.GroupID, &e.EventID, &e.ReceivedAt, &e.Release, &e.Environment, &e.Txn)
		return e, err
	}, `SELECT id, group_id, event_id, received_at, release, environment, txn
		FROM error_events WHERE group_id = ? ORDER BY id DESC LIMIT ?`, groupID, limit)
}

func (s *Store) ErrorEventByID(id int64) (*ErrorEvent, error) {
	return ptr(queryOne(s.db, func(sc scanner) (ErrorEvent, error) {
		var e ErrorEvent
		err := sc.Scan(&e.ID, &e.GroupID, &e.EventID, &e.ReceivedAt, &e.Release, &e.Environment, &e.Txn, &e.Payload)
		return e, err
	}, `SELECT id, group_id, event_id, received_at, release, environment, txn, payload
		FROM error_events WHERE id = ?`, id))
}

// ---- triage ----

// ResolveErrorGroup marks a group resolved. deployedCommit is the app's
// current release; a later event on a different release regresses the group.
func (s *Store) ResolveErrorGroup(id int64, deployedCommit string) error {
	_, err := s.db.Exec(`UPDATE error_groups SET status = 'resolved', resolved_at = ?, resolved_release = ?,
		ignored_until = NULL, ignore_until_count = NULL WHERE id = ?`, now(), deployedCommit, id)
	return err
}

// IgnoreErrorGroup silences a group. until (RFC3339) and untilCount are the
// escape hatches; zero values mean ignored until explicitly unignored.
func (s *Store) IgnoreErrorGroup(id int64, until string, untilCount int64) error {
	var u any
	if until != "" {
		u = until
	}
	var c any
	if untilCount > 0 {
		c = untilCount
	}
	_, err := s.db.Exec(`UPDATE error_groups SET status = 'ignored', ignored_until = ?, ignore_until_count = ?,
		resolved_at = NULL, resolved_release = NULL WHERE id = ?`, u, c, id)
	return err
}

// UnresolveErrorGroup reopens a group by hand.
func (s *Store) UnresolveErrorGroup(id int64) error {
	_, err := s.db.Exec(`UPDATE error_groups SET status = 'open', resolved_at = NULL, resolved_release = NULL,
		ignored_until = NULL, ignore_until_count = NULL WHERE id = ?`, id)
	return err
}

func (s *Store) TouchErrorGroupNotified(id int64) error {
	_, err := s.db.Exec(`UPDATE error_groups SET last_notified_at = ? WHERE id = ?`, now(), id)
	return err
}

// ---- retention ----

// pruneBatch keeps each DELETE small: the store runs one connection, so an
// unbounded DELETE would block every request behind it (Feed's prune job
// taught the same lesson on the Rails side).
const pruneBatch = 5000

// PruneErrorEvents deletes stored events older than the cutoff (RFC3339),
// batched. Returns rows deleted.
func (s *Store) PruneErrorEvents(cutoff string) (int64, error) {
	return s.pruneBatched("error_events", "received_at < ?", cutoff)
}

// PruneStaleErrorGroups deletes resolved/ignored groups not seen since the
// cutoff. Open groups are never pruned — an old open error is still an open
// error.
func (s *Store) PruneStaleErrorGroups(cutoff string) (int64, error) {
	return s.pruneBatched("error_groups", "status IN ('resolved', 'ignored') AND last_seen < ?", cutoff)
}
