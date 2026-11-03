package store

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
)

// Feedback is one report written from the header's bug button — here, or
// on a peer that relayed it. See migration 0042.
type Feedback struct {
	ID         int64  `json:"id"`
	UID        string `json:"uid"`
	PeerID     int64  `json:"peerId,omitempty"`
	Author     string `json:"author"`
	Body       string `json:"body"`
	Page       string `json:"page"`
	FogVersion string `json:"fogVersion"`
	// Dossier is the JSON context captured with the report; empty when the
	// origin sent the note alone. Lists leave it out (HasDossier says
	// whether there is one) — it is the heavy part of the row.
	Dossier    string `json:"dossier,omitempty"`
	HasDossier bool   `json:"hasDossier"`
	Status     string `json:"status" ts:"\"open\" | \"closed\""`
	CreatedAt  string `json:"createdAt"`
	ReceivedAt string `json:"receivedAt"`
	ClosedAt   string `json:"closedAt,omitempty"`
	// StatusAt is when the status last changed; empty until it does. On a
	// relayed row it is what this instance reports back to the origin.
	StatusAt string `json:"statusAt,omitempty"`
	// StatusByPeerID names the peer whose word set the current status; zero
	// when it was set here.
	StatusByPeerID int64 `json:"statusByPeerId,omitempty"`
	// PeerStatusAt is the stamp of the last peer notice applied to this row
	// (see migration 0043). It is bookkeeping, never shown.
	PeerStatusAt string `json:"-"`
}

// FeedbackStatus is one report's status as its receiver reports it back
// to the origin, in the heartbeat. ChangedAt is the receiver's status_at:
// the moment shown, and the stamp that keeps a notice from applying twice.
type FeedbackStatus struct {
	UID       string `json:"uid"`
	Status    string `json:"status"`
	ChangedAt string `json:"changedAt"`
}

// FeedbackDelivery is one peer a local report is going to, and how far it
// got.
type FeedbackDelivery struct {
	PeerID        int64  `json:"peerId"`
	Mode          string `json:"mode" ts:"\"note\" | \"full\""`
	State         string `json:"state" ts:"\"queued\" | \"delivered\" | \"refused\""` // queued | delivered | refused
	Attempts      int    `json:"attempts"`
	LastError     string `json:"lastError,omitempty"`
	NextAttemptAt string `json:"nextAttemptAt,omitempty"`
	DeliveredAt   string `json:"deliveredAt,omitempty"`
}

// DueFeedback is a queued delivery joined to the report it carries.
type DueFeedback struct {
	Feedback Feedback
	Mode     string
	Attempts int
}

const feedbackListCols = `id, uid, peer_id, author, body, page, fog_version, '', length(dossier) > 0,
	status, created_at, received_at, closed_at, status_at, status_by_peer_id, peer_status_at`
const feedbackFullCols = `id, uid, peer_id, author, body, page, fog_version, dossier, length(dossier) > 0,
	status, created_at, received_at, closed_at, status_at, status_by_peer_id, peer_status_at`

func scanFeedback(sc scanner) (Feedback, error) {
	var f Feedback
	var peerID, statusBy sql.NullInt64
	var closedAt, statusAt, peerStatusAt sql.NullString
	if err := sc.Scan(&f.ID, &f.UID, &peerID, &f.Author, &f.Body, &f.Page, &f.FogVersion, &f.Dossier,
		&f.HasDossier, &f.Status, &f.CreatedAt, &f.ReceivedAt, &closedAt, &statusAt, &statusBy, &peerStatusAt); err != nil {
		return f, err
	}
	f.PeerID, f.StatusByPeerID = peerID.Int64, statusBy.Int64
	f.ClosedAt, f.StatusAt, f.PeerStatusAt = closedAt.String, statusAt.String, peerStatusAt.String
	return f, nil
}

// CreateFeedback stores a local report and queues it for every peer named
// in deliveries (peer id → mode), in one transaction: a report is never on
// record without the sends it was promised, or the other way round.
func (s *Store) CreateFeedback(in Feedback, deliveries map[int64]string) (*Feedback, error) {
	n := now()
	if in.CreatedAt == "" {
		in.CreatedAt = n
	}
	var id int64
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO feedback (uid, author, body, page, fog_version, dossier, created_at, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, in.UID, in.Author, in.Body, in.Page, in.FogVersion, in.Dossier, in.CreatedAt, n)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		for peerID, mode := range deliveries {
			if _, err := tx.Exec(`INSERT INTO feedback_deliveries (feedback_id, peer_id, mode, next_attempt_at)
			VALUES (?, ?, ?, ?)`, id, peerID, mode, n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.FeedbackByID(id)
}

// IngestPeerFeedback stores a report a peer relayed. A uid this peer has
// already delivered is a retry: nothing changes and inserted is false.
func (s *Store) IngestPeerFeedback(peerID int64, in Feedback) (inserted bool, err error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO feedback
		(uid, peer_id, author, body, page, fog_version, dossier, created_at, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UID, peerID, in.Author, in.Body, in.Page, in.FogVersion, in.Dossier, in.CreatedAt, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListFeedback lists reports newest first, without their dossiers. status
// is "open", "closed", or "" for both.
func (s *Store) ListFeedback(status string, limit int) ([]Feedback, error) {
	q := `SELECT ` + feedbackListCols + ` FROM feedback`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	return orEmpty(queryAll(s.db, scanFeedback, q, append(args, limit)...))
}

func (s *Store) FeedbackByID(id int64) (*Feedback, error) {
	// No row is sql.ErrNoRows here, not ErrNotFound: callers check for it.
	return ptr(scanFeedback(s.db.QueryRow(`SELECT `+feedbackFullCols+` FROM feedback WHERE id = ?`, id)))
}

// statusStamp is now, to the millisecond: a status change's stamp is also
// what tells one change from the next on the origin's side, and a close
// and reopen can land inside the same second. They can land inside the
// same millisecond too, so each stamp is strictly later than the last one
// this process issued — the origin would otherwise take the second change
// for a repeat of the first.
func statusStamp() string {
	stampMu.Lock()
	defer stampMu.Unlock()
	t := time.Now().UTC().Truncate(time.Millisecond)
	if !t.After(lastStamp) {
		t = lastStamp.Add(time.Millisecond)
	}
	lastStamp = t
	return t.Format(statusStampLayout)
}

// statusStampLayout is the one fixed-width form every status stamp is
// stored in, ours and a peer's alike, so they order as strings.
const statusStampLayout = "2006-01-02T15:04:05.000Z07:00"

var (
	stampMu   sync.Mutex
	lastStamp time.Time
)

// SetFeedbackStatus opens or closes a report, on this instance's word:
// the change is stamped now and attributed to nobody else. On a relayed
// report the stamp is what the origin will be told.
func (s *Store) SetFeedbackStatus(id int64, status string) error {
	n := statusStamp()
	var closedAt any
	if status == "closed" {
		closedAt = n
	}
	res, err := s.db.Exec(`UPDATE feedback SET status = ?, closed_at = ?, status_at = ?, status_by_peer_id = NULL
		WHERE id = ?`, status, closedAt, n, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// PeerFeedbackStatuses is what a heartbeat tells one peer about the
// reports it relayed here: every one whose status has been changed here,
// newest change first. It is state, not events — sent every time, applied
// idempotently — so nothing is lost while the peer is away.
func (s *Store) PeerFeedbackStatuses(peerID int64, limit int) ([]FeedbackStatus, error) {
	return orEmpty(queryAll(s.db, func(sc scanner) (FeedbackStatus, error) {
		var st FeedbackStatus
		err := sc.Scan(&st.UID, &st.Status, &st.ChangedAt)
		return st, err
	}, `SELECT uid, status, status_at FROM feedback
		WHERE peer_id = ? AND status_at IS NOT NULL ORDER BY status_at DESC, id DESC LIMIT ?`, peerID, limit))
}

// ApplyPeerFeedbackStatus mirrors a receiver's word onto the local report
// it is about. It applies only to a report this instance wrote and the
// peer holds — one with a delivered row to that peer — and only once per
// stamp: a notice already applied, even if the status was changed here
// since, changes nothing. The updated report and the status it had before
// come back; nil when nothing was applied.
// maxPeerClockAhead is how far ahead of this clock a peer's status stamp
// may be: the federation signature window.
const maxPeerClockAhead = 5 * time.Minute

func (s *Store) ApplyPeerFeedbackStatus(peerID int64, n FeedbackStatus) (f *Feedback, prev string, err error) {
	// The stamp is the peer's clock's word, so it is held to ours: a time
	// that doesn't parse, or one more than the signature window ahead of
	// now, is refused outright — one far in the future would outrank every
	// later status — and anything ahead of now is taken as now. Stored in
	// the one fixed-width form (statusStampLayout), so stamps keep ordering
	// as strings.
	changed, perr := time.Parse(time.RFC3339, n.ChangedAt)
	received := time.Now().UTC()
	if perr != nil || changed.After(received.Add(maxPeerClockAhead)) {
		return nil, "", nil
	}
	if changed.After(received) {
		changed = received
	}
	n.ChangedAt = changed.UTC().Truncate(time.Millisecond).Format(statusStampLayout)
	var id int64 // 0: nothing applied
	err = s.withTx(func(tx *sql.Tx) error {
		var rowID int64
		var seen sql.NullString
		// Only while this side still sends that peer its reports (the
		// feedback.ingest use row): withdrawing it ends the peer's say over
		// them, as well as what is queued.
		err := tx.QueryRow(`SELECT f.id, f.status, f.peer_status_at FROM feedback f
		JOIN feedback_deliveries d ON d.feedback_id = f.id AND d.peer_id = ? AND d.state = 'delivered'
		JOIN fed_capabilities c ON c.peer_id = d.peer_id AND c.capability = 'feedback.ingest' AND c.direction = 'use'
		WHERE f.peer_id IS NULL AND f.uid = ?`, peerID, n.UID).Scan(&rowID, &prev, &seen)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// Not newer than the word already applied: a repeat, or an older
		// heartbeat overtaken by a newer one (two status changes in quick
		// succession each kick a dial, and they race). Stamps share one
		// fixed-width format, so they order as strings.
		if seen.Valid && n.ChangedAt <= seen.String {
			return nil
		}
		var closedAt any
		if n.Status == "closed" {
			closedAt = n.ChangedAt
		}
		if _, err := tx.Exec(`UPDATE feedback SET status = ?, closed_at = ?, status_at = ?, status_by_peer_id = ?,
		peer_status_at = ? WHERE id = ?`, n.Status, closedAt, n.ChangedAt, peerID, n.ChangedAt, rowID); err != nil {
			return err
		}
		id = rowID
		return nil
	})
	if err != nil || id == 0 {
		return nil, "", err
	}
	f, err = s.FeedbackByID(id)
	return f, prev, err
}

// DeleteFeedback removes a report and anything still queued to send it.
func (s *Store) DeleteFeedback(id int64) error {
	res, err := s.db.Exec(`DELETE FROM feedback WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountOpenFeedback counts open reports: all of them, and the ones that
// arrived from peers (someone else's report, waiting on this household).
func (s *Store) CountOpenFeedback() (all, fromPeers int64, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COUNT(peer_id) FROM feedback WHERE status = 'open'`).Scan(&all, &fromPeers)
	return
}

// FeedbackDeliveries lists where one local report is being sent.
func (s *Store) FeedbackDeliveries(feedbackID int64) ([]FeedbackDelivery, error) {
	return orEmpty(queryAll(s.db, func(sc scanner) (FeedbackDelivery, error) {
		var d FeedbackDelivery
		err := sc.Scan(&d.PeerID, &d.Mode, &d.State, &d.Attempts, &d.LastError, &d.NextAttemptAt, &d.DeliveredAt)
		if d.State != "queued" {
			d.NextAttemptAt = ""
		}
		return d, err
	}, `SELECT peer_id, mode, state, attempts, last_error, next_attempt_at, COALESCE(delivered_at, '')
		FROM feedback_deliveries WHERE feedback_id = ? ORDER BY peer_id`, feedbackID))
}

// PeersWithDueFeedback names the peers holding at least one delivery that
// is due now. With onlyFeedback non-zero, only that report's peers count
// (the send-right-now path after a report is written).
func (s *Store) PeersWithDueFeedback(onlyFeedback int64) ([]int64, error) {
	q := `SELECT DISTINCT peer_id FROM feedback_deliveries WHERE state = 'queued' AND next_attempt_at <= ?`
	args := []any{now()}
	if onlyFeedback != 0 {
		q += ` AND feedback_id = ?`
		args = append(args, onlyFeedback)
	}
	return queryAll(s.db, scanValue[int64], q, args...)
}

// DueFeedbackDeliveries returns one peer's due deliveries, oldest report
// first, each with the report it carries.
func (s *Store) DueFeedbackDeliveries(peerID int64, limit int) ([]DueFeedback, error) {
	return queryAll(s.db, func(sc scanner) (DueFeedback, error) {
		var d DueFeedback
		var peer sql.NullInt64
		var closedAt sql.NullString
		f := &d.Feedback
		err := sc.Scan(&f.ID, &f.UID, &peer, &f.Author, &f.Body, &f.Page, &f.FogVersion, &f.Dossier,
			&f.HasDossier, &f.Status, &f.CreatedAt, &f.ReceivedAt, &closedAt, &d.Mode, &d.Attempts)
		f.PeerID, f.ClosedAt = peer.Int64, closedAt.String
		return d, err
	}, `SELECT f.id, f.uid, f.peer_id, f.author, f.body, f.page, f.fog_version, f.dossier,
			length(f.dossier) > 0, f.status, f.created_at, f.received_at, f.closed_at, d.mode, d.attempts
		FROM feedback_deliveries d JOIN feedback f ON f.id = d.feedback_id
		WHERE d.peer_id = ? AND d.state = 'queued' AND d.next_attempt_at <= ?
		ORDER BY f.id LIMIT ?`, peerID, now(), limit)
}

func idPlaceholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// SettleFeedbackDeliveries records a peer's answer for a batch: delivered
// reports get their receipt, refused ones the reason.
func (s *Store) SettleFeedbackDeliveries(peerID int64, delivered, refused []int64, reason string) error {
	n := now()
	if len(delivered) > 0 {
		ph, args := idPlaceholders(delivered)
		if _, err := s.db.Exec(`UPDATE feedback_deliveries SET state = 'delivered', delivered_at = ?, last_error = '',
			attempts = attempts + 1 WHERE peer_id = ? AND feedback_id IN (`+ph+`)`,
			append([]any{n, peerID}, args...)...); err != nil {
			return err
		}
	}
	if len(refused) > 0 {
		ph, args := idPlaceholders(refused)
		if _, err := s.db.Exec(`UPDATE feedback_deliveries SET state = 'refused', last_error = ?,
			attempts = attempts + 1 WHERE peer_id = ? AND feedback_id IN (`+ph+`)`,
			append([]any{reason, peerID}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

// DeferFeedbackDeliveries records a failed attempt and when to try again.
func (s *Store) DeferFeedbackDeliveries(peerID int64, ids []int64, nextAttempt, errText string) error {
	if len(ids) == 0 {
		return nil
	}
	ph, args := idPlaceholders(ids)
	_, err := s.db.Exec(`UPDATE feedback_deliveries SET attempts = attempts + 1, next_attempt_at = ?, last_error = ?
		WHERE peer_id = ? AND state = 'queued' AND feedback_id IN (`+ph+`)`,
		append([]any{nextAttempt, errText, peerID}, args...)...)
	return err
}
