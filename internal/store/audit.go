package store

import "encoding/json"

type AuditEntry struct {
	ID      int64  `json:"id"`
	At      string `json:"at"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

// Audit records a mutating action. Detail must never contain secret values.
func (s *Store) Audit(actor, action, subject, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit_log (at, actor, action, subject, detail) VALUES (?, ?, ?, ?, ?)`,
		now(), actor, action, subject, detail)
	return err
}

// AuditDetail renders an audit row's detail: the JSON object the activity
// views parse, or "" when there is nothing to record.
func AuditDetail(detail map[string]any) string {
	if len(detail) == 0 {
		return ""
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return ""
	}
	return string(b)
}

// FederationActor is the audit actor for something a paired peer did. Rows
// written before this was the one spelling may say "fed:<name>" instead.
func FederationActor(peer string) string { return "federation:" + peer }

// PruneAudit deletes audit entries older than the cutoff (RFC3339), batched:
// the store runs one connection, so one unbounded DELETE over years of rows
// would block every request behind it. Returns rows deleted.
func (s *Store) PruneAudit(cutoff string) (int64, error) {
	return s.pruneBatched("audit_log", "at < ?", cutoff)
}

func (s *Store) RecentAudit(limit int) ([]AuditEntry, error) {
	return s.scanAudit(`SELECT id, at, actor, action, subject, detail FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
}

// AuditBefore returns the page of entries older than the entry with id
// before, newest first — how the activity log reads further back than its
// first page.
func (s *Store) AuditBefore(before int64, limit int) ([]AuditEntry, error) {
	return s.scanAudit(`SELECT id, at, actor, action, subject, detail FROM audit_log WHERE id < ? ORDER BY id DESC LIMIT ?`, before, limit)
}

// AppAudit returns the entries about one app, newest first: every verb whose
// subject is the app (app.*, backup.*, app.errors.*), plus what the app did
// through fog itself (actor "app:<name>", the federation vouches). Not a
// LIKE on the subject — a path or a peer that happens to contain the name
// is not this app's business.
func (s *Store) AppAudit(app string, limit int) ([]AuditEntry, error) {
	return s.scanAudit(`SELECT id, at, actor, action, subject, detail FROM audit_log
		WHERE subject = ? OR actor = ? ORDER BY id DESC LIMIT ?`, app, "app:"+app, limit)
}

// FederationLedger returns the audit entries that represent something crossing
// a federation boundary — trust established or revoked, a folder shared to or
// pulled from a peer, a contribution — newest first. This is Project K's "what
// actually crossed the boundary" feed: every such verb, drawn from the one
// audit log so the whole fleet's boundary activity reads the same way in one
// place. subject, when set, narrows it to one peer.
func (s *Store) FederationLedger(limit int, peer, peerID string) ([]AuditEntry, error) {
	const boundary = `(action LIKE 'federation.%'
		OR action LIKE 'files.share.peer%'
		OR action LIKE 'files.subscri%'
		OR action LIKE 'files.contribution%')`
	if peer == "" {
		return s.scanAudit(`SELECT id, at, actor, action, subject, detail FROM audit_log
			WHERE `+boundary+` ORDER BY id DESC LIMIT ?`, limit)
	}
	// The peer identifier lands in a different place per verb: the name in
	// subject (federation.*) or detail's "peer" (files.subscribe), the
	// federation actor (contributions), or the numeric peer id in detail's
	// "peerId" (files.share.peer, where subject is the folder path). Match
	// all of them so a per-edge view catches every crossing, not just the
	// pairings. Rows from before details were JSON carry the bare name or id
	// as the whole detail; they match too. CASE guards json_extract, which
	// errors on those.
	args := []any{peer, peer, peer, FederationActor(peer)}
	cond := `subject = ? OR detail = ?
		OR CASE WHEN json_valid(detail) THEN json_extract(detail, '$.peer') END = ?
		OR actor = ?`
	if peerID != "" {
		cond += ` OR detail = ?
		OR CASE WHEN json_valid(detail) THEN CAST(json_extract(detail, '$.peerId') AS TEXT) END = ?`
		args = append(args, peerID, peerID)
	}
	args = append(args, limit)
	return s.scanAudit(`SELECT id, at, actor, action, subject, detail FROM audit_log
		WHERE `+boundary+` AND (`+cond+`) ORDER BY id DESC LIMIT ?`, args...)
}

func (s *Store) scanAudit(query string, args ...any) ([]AuditEntry, error) {
	return queryAll(s.db, func(sc scanner) (AuditEntry, error) {
		var e AuditEntry
		err := sc.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Subject, &e.Detail)
		return e, err
	}, query, args...)
}
