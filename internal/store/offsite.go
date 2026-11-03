package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ---- offsite targets ----

// OffsiteTarget is one bucket a locker is written to. The credential pair
// lives in the vault as system secrets (offsite.KeyIDSecret / SecretSecret).
type OffsiteTarget struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Provider   string  `json:"provider"`
	Endpoint   string  `json:"endpoint"`
	Region     string  `json:"region"`
	Bucket     string  `json:"bucket"`
	Prefix     string  `json:"prefix"`
	PathStyle  bool    `json:"pathStyle"`
	LockerID   string  `json:"lockerId"`
	Role       string  `json:"role" ts:"\"active\" | \"retired\""`
	Grade      string  `json:"grade" ts:"\"A\" | \"B\" | \"C\" | \"D\" | \"\""`
	Probe      string  `json:"probe,omitempty"`
	ProbedAt   string  `json:"probedAt,omitempty"`
	PricePerTB float64 `json:"pricePerTb"`
	MaxMbps    int     `json:"maxMbps"`
	Window     string  `json:"window"`
	LastSyncAt string  `json:"lastSyncAt,omitempty"`
	LastError  string  `json:"lastError,omitempty"`
	CreatedAt  string  `json:"createdAt"`
}

const offsiteTargetCols = `id, name, provider, endpoint, region, bucket, prefix, path_style, locker_id, role, grade, probe,
	COALESCE(probed_at, ''), price_per_tb, max_mbps, window, COALESCE(last_sync_at, ''), last_error, created_at`

func scanOffsiteTarget(sc scanner) (OffsiteTarget, error) {
	var t OffsiteTarget
	var pathStyle int
	err := sc.Scan(&t.ID, &t.Name, &t.Provider, &t.Endpoint, &t.Region, &t.Bucket, &t.Prefix, &pathStyle, &t.LockerID,
		&t.Role, &t.Grade, &t.Probe, &t.ProbedAt, &t.PricePerTB, &t.MaxMbps, &t.Window, &t.LastSyncAt, &t.LastError, &t.CreatedAt)
	t.PathStyle = pathStyle == 1
	return t, err
}

func (s *Store) CreateOffsiteTarget(t OffsiteTarget) (int64, error) {
	ps := 0
	if t.PathStyle {
		ps = 1
	}
	if t.Role == "" {
		t.Role = "active"
	}
	res, err := s.db.Exec(`INSERT INTO offsite_targets
		(name, provider, endpoint, region, bucket, prefix, path_style, locker_id, role, grade, probe, probed_at, price_per_tb, max_mbps, window, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Provider, t.Endpoint, t.Region, t.Bucket, t.Prefix, ps, t.LockerID, t.Role, t.Grade, t.Probe,
		nullIfEmpty(t.ProbedAt), t.PricePerTB, t.MaxMbps, t.Window, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateOffsiteTarget rewrites the reachability and policy fields of a
// target. Name, locker id and role are identity and lifecycle — they have
// their own setters.
func (s *Store) UpdateOffsiteTarget(name string, t OffsiteTarget) error {
	ps := 0
	if t.PathStyle {
		ps = 1
	}
	_, err := s.db.Exec(`UPDATE offsite_targets SET provider = ?, endpoint = ?, region = ?, bucket = ?, prefix = ?, path_style = ?,
		price_per_tb = ?, max_mbps = ?, window = ? WHERE name = ?`,
		t.Provider, t.Endpoint, t.Region, t.Bucket, t.Prefix, ps, t.PricePerTB, t.MaxMbps, t.Window, name)
	return err
}

func (s *Store) SetOffsiteTargetProbe(name, grade, probe string) error {
	_, err := s.db.Exec(`UPDATE offsite_targets SET grade = ?, probe = ?, probed_at = ? WHERE name = ?`, grade, probe, now(), name)
	return err
}

func (s *Store) SetOffsiteTargetLocker(name, lockerID string) error {
	_, err := s.db.Exec(`UPDATE offsite_targets SET locker_id = ? WHERE name = ?`, lockerID, name)
	return err
}

func (s *Store) SetOffsiteTargetRole(name, role string) error {
	_, err := s.db.Exec(`UPDATE offsite_targets SET role = ? WHERE name = ?`, role, name)
	return err
}

// SetOffsiteTargetSync records the outcome of a sync pass: a success
// stamps last_sync_at and clears the error; a failure keeps the last
// success time and records why.
func (s *Store) SetOffsiteTargetSync(name string, ok bool, errText string) error {
	if ok {
		_, err := s.db.Exec(`UPDATE offsite_targets SET last_sync_at = ?, last_error = '' WHERE name = ?`, now(), name)
		return err
	}
	_, err := s.db.Exec(`UPDATE offsite_targets SET last_error = ? WHERE name = ?`, errText, name)
	return err
}

func (s *Store) ListOffsiteTargets() ([]OffsiteTarget, error) {
	return orEmpty(queryAll(s.db, scanOffsiteTarget, `SELECT `+offsiteTargetCols+` FROM offsite_targets ORDER BY id`))
}

func (s *Store) OffsiteTargetByName(name string) (*OffsiteTarget, error) {
	return ptr(queryOne(s.db, scanOffsiteTarget, `SELECT `+offsiteTargetCols+` FROM offsite_targets WHERE name = ?`, name))
}

func (s *Store) DeleteOffsiteTarget(name string) error {
	_, err := s.db.Exec(`DELETE FROM offsite_targets WHERE name = ?`, name)
	return err
}

// ---- offsite copies ----

// The vocabulary of offsite_copies' kind and state columns. The offsite
// package owns what is done with a copy; the store owns the words its rows
// are written in, so the packages that only read them (Files, backups,
// the cost meter) spell them the same way.
const (
	// Kinds: what a copy row is a copy of.
	OffsiteKindKit           = "kit"            // an encrypted recovery kit
	OffsiteKindApp           = "app"            // an app backup's snapshot directory
	OffsiteKindFilesSnapshot = "files-snapshot" // a Files snapshot's manifest
	OffsiteKindBlob          = "files-blob"     // one unique Drive file's content

	// States: where the object is in its life at the target.
	OffsiteUploading = "uploading" // started, not finished; resumable
	OffsiteStored    = "stored"    // landed and current
	OffsiteHidden    = "hidden"    // delete-marked; billed until the provider expires it

	// What OffsiteStates reports beyond the column states: a stored copy
	// that has been read back, and a subject an active target lacks.
	OffsiteVerified = "verified"
	OffsitePending  = "pending"
)

// OffsiteCopy records one object's presence in one target.
type OffsiteCopy struct {
	ID       int64  `json:"id"`
	TargetID int64  `json:"targetId"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	Key      string `json:"key"`
	Bytes    int64  `json:"bytes"`
	State    string `json:"state"`
	UploadID string `json:"uploadId,omitempty"`
	Nonce    string `json:"nonce,omitempty"`
	// Doc is the row or manifest document the object seals, kept in the
	// clear here so the nightly index needs no round trips.
	Doc string `json:"-"`
	// Summary is, for a Files snapshot, the index's line for it (stamp,
	// creation time, file count, bytes) as JSON, so the index is written
	// without reading Doc; "" for other kinds.
	Summary     string `json:"-"`
	UploadedAt  string `json:"uploadedAt,omitempty"`
	VerifiedAt  string `json:"verifiedAt,omitempty"`
	HiddenAt    string `json:"hiddenAt,omitempty"`
	RetainUntil string `json:"retainUntil,omitempty"`
}

const offsiteCopyCols = `id, target_id, kind, subject, key, bytes, state, upload_id, nonce, doc, summary,
	COALESCE(uploaded_at, ''), COALESCE(verified_at, ''), COALESCE(hidden_at, ''), COALESCE(retain_until, '')`

// offsiteCopySlimCols is offsiteCopyCols with doc left empty.
const offsiteCopySlimCols = `id, target_id, kind, subject, key, bytes, state, upload_id, nonce, '', summary,
	COALESCE(uploaded_at, ''), COALESCE(verified_at, ''), COALESCE(hidden_at, ''), COALESCE(retain_until, '')`

func scanOffsiteCopy(sc scanner) (OffsiteCopy, error) {
	var c OffsiteCopy
	err := sc.Scan(&c.ID, &c.TargetID, &c.Kind, &c.Subject, &c.Key, &c.Bytes, &c.State, &c.UploadID, &c.Nonce, &c.Doc, &c.Summary,
		&c.UploadedAt, &c.VerifiedAt, &c.HiddenAt, &c.RetainUntil)
	return c, err
}

// UpsertOffsiteCopy writes a copy row keyed by (target, kind, subject),
// replacing every mutable field.
func (s *Store) UpsertOffsiteCopy(c OffsiteCopy) error {
	_, err := s.db.Exec(upsertOffsiteCopySQL, offsiteCopyArgs(c)...)
	return err
}

// UpsertOffsiteCopies records several copies in one transaction — a batch
// of Files blobs that landed.
func (s *Store) UpsertOffsiteCopies(cs []OffsiteCopy) error {
	if len(cs) == 0 {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(upsertOffsiteCopySQL)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, c := range cs {
			if _, err := stmt.Exec(offsiteCopyArgs(c)...); err != nil {
				return err
			}
		}
		return nil
	})
}

const upsertOffsiteCopySQL = `INSERT INTO offsite_copies
		(target_id, kind, subject, key, bytes, state, upload_id, nonce, doc, summary, uploaded_at, verified_at, hidden_at, retain_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(target_id, kind, subject) DO UPDATE SET key = excluded.key, bytes = excluded.bytes, state = excluded.state,
			upload_id = excluded.upload_id, nonce = excluded.nonce, doc = excluded.doc, summary = excluded.summary, uploaded_at = excluded.uploaded_at,
			verified_at = excluded.verified_at, hidden_at = excluded.hidden_at, retain_until = excluded.retain_until`

func offsiteCopyArgs(c OffsiteCopy) []any {
	return []any{c.TargetID, c.Kind, c.Subject, c.Key, c.Bytes, c.State, c.UploadID, c.Nonce, c.Doc, c.Summary,
		nullIfEmpty(c.UploadedAt), nullIfEmpty(c.VerifiedAt), nullIfEmpty(c.HiddenAt), nullIfEmpty(c.RetainUntil)}
}

func (s *Store) OffsiteCopy(targetID int64, kind, subject string) (*OffsiteCopy, error) {
	return ptr(queryOne(s.db, scanOffsiteCopy, `SELECT `+offsiteCopyCols+` FROM offsite_copies WHERE target_id = ? AND kind = ? AND subject = ?`,
		targetID, kind, subject))
}

// ListOffsiteCopies returns a target's copies of one kind (or every kind
// when kind is ""), newest key first. Given states, only copies in one of
// them are listed — worth asking for here, where every row carries its
// whole document.
func (s *Store) ListOffsiteCopies(targetID int64, kind string, states ...string) ([]OffsiteCopy, error) {
	return s.listOffsiteCopies(offsiteCopyCols, targetID, kindList(kind), states)
}

// ListOffsiteCopiesSlim is ListOffsiteCopies without Doc — for a Files
// snapshot the whole per-file manifest — for the passes that only need a
// copy's key, state and stamps.
func (s *Store) ListOffsiteCopiesSlim(targetID int64, kind string, states ...string) ([]OffsiteCopy, error) {
	return s.listOffsiteCopies(offsiteCopySlimCols, targetID, kindList(kind), states)
}

// ListOffsiteCopiesOfKinds is ListOffsiteCopiesSlim for several kinds at
// once. The passes that look only at snapshots and kits ask for those by
// name, so they never load the blob rows — one per unique Drive file, by
// far the most of any target's copies.
func (s *Store) ListOffsiteCopiesOfKinds(targetID int64, kinds []string, states ...string) ([]OffsiteCopy, error) {
	if len(kinds) == 0 {
		return []OffsiteCopy{}, nil
	}
	return s.listOffsiteCopies(offsiteCopySlimCols, targetID, kinds, states)
}

func kindList(kind string) []string {
	if kind == "" {
		return nil
	}
	return []string{kind}
}

// OffsiteCopiesFingerprint summarises a target's copy rows in one cheap
// query. It moves when a copy lands, is hidden or unhidden, or a row is
// forgotten — the events after which the index and retention have work to
// do. A hide and an unhide that cancel out leave it unchanged; the daily
// housekeeping pass catches those.
func (s *Store) OffsiteCopiesFingerprint(targetID int64) (string, error) {
	var n, stored, hidden, maxID int64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(state = '`+OffsiteStored+`'), 0), COALESCE(SUM(state = '`+OffsiteHidden+`'), 0), COALESCE(MAX(id), 0)
		FROM offsite_copies WHERE target_id = ?`, targetID).Scan(&n, &stored, &hidden, &maxID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d/%d/%d/%d", n, stored, hidden, maxID), nil
}

// listOffsiteCopies lists a target's copies, narrowed to states and kinds
// when given (idx_offsite_copies_state serves both).
func (s *Store) listOffsiteCopies(cols string, targetID int64, kinds []string, states []string) ([]OffsiteCopy, error) {
	q := `SELECT ` + cols + ` FROM offsite_copies WHERE target_id = ?`
	args := []any{targetID}
	if len(states) > 0 {
		ph, a := inList(states)
		q += ` AND state IN (` + ph + `)`
		args = append(args, a...)
	}
	if len(kinds) > 0 {
		ph, a := inList(kinds)
		q += ` AND kind IN (` + ph + `)`
		args = append(args, a...)
	}
	return orEmpty(queryAll(s.db, scanOffsiteCopy, q+` ORDER BY key DESC`, args...))
}

func (s *Store) SetOffsiteCopyState(id int64, state string) error {
	col := ""
	switch state {
	case OffsiteStored:
		col = ", uploaded_at = " + sqlNow() + ", upload_id = '', nonce = ''"
	case OffsiteHidden:
		col = ", hidden_at = " + sqlNow()
	}
	_, err := s.db.Exec(`UPDATE offsite_copies SET state = ?`+col+` WHERE id = ?`, state, id)
	return err
}

func (s *Store) SetOffsiteCopyVerified(id int64) error {
	_, err := s.db.Exec(`UPDATE offsite_copies SET verified_at = ? WHERE id = ?`, now(), id)
	return err
}

func (s *Store) SetOffsiteCopyRetention(id int64, until string) error {
	_, err := s.db.Exec(`UPDATE offsite_copies SET retain_until = ? WHERE id = ?`, until, id)
	return err
}

// DeleteOffsiteCopy forgets a copy row. Only for a hidden copy the
// provider has since expired: a stored one's row is the only record fog
// has that the object exists.
func (s *Store) DeleteOffsiteCopy(id int64) error {
	_, err := s.db.Exec(`DELETE FROM offsite_copies WHERE id = ?`, id)
	return err
}

// OffsiteCopyTotals is the byte accounting behind the cost meter: what is
// stored and what is hidden but (until the provider expires it) still
// billed.
type OffsiteCopyTotals struct {
	Stored      int64 `json:"stored"`
	StoredBytes int64 `json:"storedBytes"`
	Hidden      int64 `json:"hidden"`
	HiddenBytes int64 `json:"hiddenBytes"`
	Uploading   int64 `json:"uploading"`
}

func (s *Store) OffsiteCopyTotals(targetID int64) (OffsiteCopyTotals, error) {
	var t OffsiteCopyTotals
	err := queryEach(s.db, func(sc scanner) error {
		var state string
		var n, b int64
		if err := sc.Scan(&state, &n, &b); err != nil {
			return err
		}
		t.add(state, n, b)
		return nil
	}, `SELECT state, COUNT(*), COALESCE(SUM(bytes), 0) FROM offsite_copies WHERE target_id = ? GROUP BY state`, targetID)
	return t, err
}

// AllOffsiteCopyTotals is OffsiteCopyTotals for every target in one query,
// keyed by target id. A target with no copies has no entry, which reads as
// the zero totals OffsiteCopyTotals would return for it.
func (s *Store) AllOffsiteCopyTotals() (map[int64]OffsiteCopyTotals, error) {
	out := map[int64]OffsiteCopyTotals{}
	err := queryEach(s.db, func(sc scanner) error {
		var id int64
		var state string
		var n, b int64
		if err := sc.Scan(&id, &state, &n, &b); err != nil {
			return err
		}
		t := out[id]
		t.add(state, n, b)
		out[id] = t
		return nil
	}, `SELECT target_id, state, COUNT(*), COALESCE(SUM(bytes), 0) FROM offsite_copies GROUP BY target_id, state`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// add folds one state's count and bytes into the totals.
func (t *OffsiteCopyTotals) add(state string, n, b int64) {
	switch state {
	case OffsiteStored:
		t.Stored, t.StoredBytes = n, b
	case OffsiteHidden:
		t.Hidden, t.HiddenBytes = n, b
	case OffsiteUploading:
		t.Uploading = n
	}
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// sqlNow is the RFC3339 UTC timestamp expression SQLite evaluates.
func sqlNow() string { return `strftime('%Y-%m-%dT%H:%M:%SZ', 'now')` }

// offsiteRank orders the states OffsiteStates reports, worst first.
var offsiteRank = map[string]int{"": 0, OffsitePending: 1, OffsiteUploading: 2, OffsiteStored: 3, OffsiteVerified: 4}

// copyState is what one copy row says of its subject.
func copyState(state, verifiedAt string) string {
	switch state {
	case OffsiteStored:
		if verifiedAt != "" {
			return OffsiteVerified
		}
		return OffsiteStored
	case OffsiteUploading:
		return OffsiteUploading
	}
	return OffsitePending
}

// OffsiteStates is the best state each subject of one kind has across the
// active targets, read at once: "" (no targets), "verified", "stored",
// "uploading", or "pending". One copy that landed is a copy that landed.
type OffsiteStates struct {
	targets bool // any active target at all
	best    map[string]string
}

// Of is one subject's state.
func (o OffsiteStates) Of(subject string) string {
	if !o.targets {
		return ""
	}
	if st, ok := o.best[subject]; ok {
		return st
	}
	return OffsitePending
}

// OffsiteStatesOf reads the offsite state of every subject of a kind in
// one query.
func (s *Store) OffsiteStatesOf(kind string) (OffsiteStates, error) {
	out := OffsiteStates{best: map[string]string{}}
	err := queryEach(s.db, func(sc scanner) error {
		var subject, state, verifiedAt sql.NullString
		if err := sc.Scan(&subject, &state, &verifiedAt); err != nil {
			return err
		}
		out.targets = true
		if !subject.Valid {
			return nil // an active target holding nothing of this kind
		}
		if st := copyState(state.String, verifiedAt.String); offsiteRank[st] > offsiteRank[out.best[subject.String]] {
			out.best[subject.String] = st
		}
		return nil
	}, `SELECT c.subject, c.state, COALESCE(c.verified_at, '')
		FROM offsite_targets t LEFT JOIN offsite_copies c ON c.target_id = t.id AND c.kind = ?
		WHERE t.role = 'active'`, kind)
	if err != nil {
		return OffsiteStates{}, err
	}
	return out, nil
}
