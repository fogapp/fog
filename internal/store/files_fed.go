package store

import (
	"database/sql"
	"slices"
	"strings"
)

// Files across peers: shares (origin side), subscriptions and replicas
// (receiving side), the keep-on-this-box floor, and contributed origins.
// See migration 0028.

// FileFedShare is one folder shared with one peer through one lens.
type FileFedShare struct {
	ID       int64  `json:"id"`
	PeerID   int64  `json:"peerId"`
	Path     string `json:"path"`
	Title    string `json:"title"`
	Mode     string `json:"mode" ts:"\"read\" | \"contribute\""`
	Scope    string `json:"scope" ts:"\"all\" | \"picked\" | \"filtered\""`
	Detail   string `json:"detail" ts:"\"full\" | \"previews\" | \"listing\""`
	Since    string `json:"since,omitempty"`
	Apps     string `json:"apps" ts:"\"allowed\" | \"denied\""`
	Revision int64  `json:"revision"`
	// ContributeQuotaBytes caps total contributed bytes on this share; 0 is
	// unlimited. Only meaningful for a contribute-mode share.
	ContributeQuotaBytes int64  `json:"contributeQuotaBytes"`
	Dirty                bool   `json:"-"`
	BytesServed          int64  `json:"bytesServed"`
	LastFetchedAt        string `json:"lastFetchedAt,omitempty"`
	CreatedAt            string `json:"createdAt"`
	UpdatedAt            string `json:"updatedAt"`
	// Loaded with the row.
	Picks   []string `json:"picks"`
	Filters []string `json:"filters"`
}

const fileFedShareCols = `id, peer_id, path, title, mode, scope, detail, since, apps, revision, contribute_quota_bytes, dirty, bytes_served, last_fetched_at, created_at, updated_at`

func scanFileFedShare(sc scanner) (FileFedShare, error) {
	var sh FileFedShare
	var dirty int
	err := sc.Scan(&sh.ID, &sh.PeerID, &sh.Path, &sh.Title, &sh.Mode, &sh.Scope, &sh.Detail, &sh.Since, &sh.Apps, &sh.Revision, &sh.ContributeQuotaBytes, &dirty, &sh.BytesServed, &sh.LastFetchedAt, &sh.CreatedAt, &sh.UpdatedAt)
	sh.Dirty = dirty != 0
	return sh, err
}

func (s *Store) loadShareLists(sh *FileFedShare) error {
	var err error
	if sh.Picks, err = orEmpty(queryAll(s.db, scanValue[string], `SELECT rel FROM file_fed_share_picks WHERE share_id = ? ORDER BY rel`, sh.ID)); err != nil {
		return err
	}
	sh.Filters, err = orEmpty(queryAll(s.db, scanValue[string], `SELECT filter FROM file_fed_share_filters WHERE share_id = ? ORDER BY filter`, sh.ID))
	return err
}

// CreateFileFedShare inserts a share with its picks and filters.
func (s *Store) CreateFileFedShare(sh *FileFedShare) (*FileFedShare, error) {
	var id int64
	err := s.withTx(func(tx *sql.Tx) error {
		n := now()
		res, err := tx.Exec(`INSERT INTO file_fed_shares (peer_id, path, title, mode, scope, detail, since, apps, revision, contribute_quota_bytes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`, sh.PeerID, sh.Path, sh.Title, sh.Mode, sh.Scope, sh.Detail, sh.Since, sh.Apps, sh.ContributeQuotaBytes, n, n)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyExists
			}
			return err
		}
		id, _ = res.LastInsertId()
		return setShareLists(tx, id, sh.Picks, sh.Filters)
	})
	if err != nil {
		return nil, err
	}
	return s.FileFedShareByID(id)
}

func setShareLists(tx *sql.Tx, id int64, picks, filters []string) error {
	if _, err := tx.Exec(`DELETE FROM file_fed_share_picks WHERE share_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM file_fed_share_filters WHERE share_id = ?`, id); err != nil {
		return err
	}
	for _, p := range picks {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO file_fed_share_picks (share_id, rel) VALUES (?, ?)`, id, p); err != nil {
			return err
		}
	}
	for _, f := range filters {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO file_fed_share_filters (share_id, filter) VALUES (?, ?)`, id, f); err != nil {
			return err
		}
	}
	return nil
}

// UpdateFileFedShare rewrites the lens. bump says whether the change is a
// lens change (revision moves, the peer re-projects) or a pick edit.
func (s *Store) UpdateFileFedShare(sh *FileFedShare, bump bool) error {
	return s.withTx(func(tx *sql.Tx) error {
		rev := "revision"
		if bump {
			rev = "revision + 1"
		}
		if _, err := tx.Exec(`UPDATE file_fed_shares SET title = ?, mode = ?, scope = ?, detail = ?, since = ?, apps = ?, contribute_quota_bytes = ?, revision = `+rev+`, dirty = 1, updated_at = ? WHERE id = ?`,
			sh.Title, sh.Mode, sh.Scope, sh.Detail, sh.Since, sh.Apps, sh.ContributeQuotaBytes, now(), sh.ID); err != nil {
			return err
		}
		if err := setShareLists(tx, sh.ID, sh.Picks, sh.Filters); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) FileFedShareByID(id int64) (*FileFedShare, error) {
	sh, err := queryOne(s.db, scanFileFedShare, `SELECT `+fileFedShareCols+` FROM file_fed_shares WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if err := s.loadShareLists(&sh); err != nil {
		return nil, err
	}
	return &sh, nil
}

// ListFileFedShares returns shares, all or for one peer, newest first.
func (s *Store) ListFileFedShares(peerID int64) ([]FileFedShare, error) {
	q := `SELECT ` + fileFedShareCols + ` FROM file_fed_shares ORDER BY created_at DESC, id DESC`
	var args []any
	if peerID > 0 {
		q = `SELECT ` + fileFedShareCols + ` FROM file_fed_shares WHERE peer_id = ? ORDER BY created_at DESC, id DESC`
		args = append(args, peerID)
	}
	out, err := orEmpty(queryAll(s.db, scanFileFedShare, q, args...))
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.loadShareLists(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// FileFedCountsByPeer counts shares offered to and subscriptions taken
// from each peer, for list views that show only the numbers.
func (s *Store) FileFedCountsByPeer() (shares, subs map[int64]int, err error) {
	count := func(table string) (map[int64]int, error) {
		out := map[int64]int{}
		err := queryEach(s.db, func(sc scanner) error {
			var id int64
			var n int
			if err := sc.Scan(&id, &n); err != nil {
				return err
			}
			out[id] = n
			return nil
		}, `SELECT peer_id, COUNT(*) FROM `+table+` GROUP BY peer_id`)
		return out, err
	}
	if shares, err = count("file_fed_shares"); err != nil {
		return nil, nil, err
	}
	if subs, err = count("file_fed_subscriptions"); err != nil {
		return nil, nil, err
	}
	return shares, subs, nil
}

func (s *Store) DeleteFileFedShare(id int64) error {
	res, err := s.db.Exec(`DELETE FROM file_fed_shares WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkFileFedSharesDirty flags every share whose folder contains rel, or
// is inside rel (a folder rename above a share). Returns every share rel
// touches, already dirty or not: the manifests a change there made stale.
// Only the shares not yet flagged are written, in one statement — a busy
// folder flags the same shares on every change until the nudge goes out.
func (s *Store) MarkFileFedSharesDirty(rel string) ([]int64, error) {
	var hits, clean []int64
	err := queryEach(s.db, func(sc scanner) error {
		var id int64
		var path string
		var dirty int
		if err := sc.Scan(&id, &path, &dirty); err != nil {
			return err
		}
		if path == "" || rel == path || strings.HasPrefix(rel, path+"/") || strings.HasPrefix(path, rel+"/") || rel == "" {
			hits = append(hits, id)
			if dirty == 0 {
				clean = append(clean, id)
			}
		}
		return nil
	}, `SELECT id, path, dirty FROM file_fed_shares`)
	if err != nil {
		return nil, err
	}
	for chunk := range slices.Chunk(clean, inChunk) {
		ph, args := inList(chunk)
		if _, err := s.db.Exec(`UPDATE file_fed_shares SET dirty = 1 WHERE id IN (`+ph+`) AND dirty = 0`, args...); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

// TakeDirtyFileFedShares returns dirty share ids grouped by peer and
// clears them, in one statement so a share flagged meanwhile is either
// taken now or left for the next nudge, never cleared unsent.
func (s *Store) TakeDirtyFileFedShares() (map[int64][]int64, error) {
	out := map[int64][]int64{}
	err := queryEach(s.db, func(sc scanner) error {
		var id, peer int64
		if err := sc.Scan(&id, &peer); err != nil {
			return err
		}
		out[peer] = append(out[peer], id)
		return nil
	}, `UPDATE file_fed_shares SET dirty = 0 WHERE dirty = 1 RETURNING id, peer_id`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) AddFileFedShareServed(id, bytes int64) {
	s.db.Exec(`UPDATE file_fed_shares SET bytes_served = bytes_served + ?, last_fetched_at = ? WHERE id = ?`, bytes, now(), id)
}

// ---- keep on this box ----

func (s *Store) ListFileShareExcludes() ([]string, error) {
	return orEmpty(queryAll(s.db, scanValue[string], `SELECT path FROM file_share_excludes ORDER BY path`))
}

func (s *Store) AddFileShareExclude(path string) error {
	_, err := s.db.Exec(`INSERT INTO file_share_excludes (path, created_at) VALUES (?, ?) ON CONFLICT(path) DO NOTHING`, path, now())
	return err
}

func (s *Store) RemoveFileShareExclude(path string) error {
	_, err := s.db.Exec(`DELETE FROM file_share_excludes WHERE path = ?`, path)
	return err
}

// ---- subscriptions ----

// FileFedSubscription is the receiving half of one share.
type FileFedSubscription struct {
	ID                  int64  `json:"id"`
	PeerID              int64  `json:"peerId"`
	RemoteShareID       int64  `json:"remoteShareId"`
	Title               string `json:"title"`
	Folder              string `json:"folder"`
	Mode                string `json:"mode" ts:"\"read\" | \"contribute\""`
	DetailOffered       string `json:"detailOffered" ts:"\"full\" | \"previews\" | \"listing\""`
	DetailWanted        string `json:"detailWanted" ts:"\"full\" | \"previews\""`
	AppsAllowed         bool   `json:"appsAllowed"`
	Status              string `json:"status" ts:"\"active\" | \"paused\" | \"revoked\" | \"error\" | \"offer_changed\""`
	AcceptedImpact      string `json:"acceptedImpact"`
	CurrentImpact       string `json:"currentImpact"`
	RevisionSeen        int64  `json:"revisionSeen"`
	ManifestETag        string `json:"-"`
	LastSyncedAt        string `json:"lastSyncedAt,omitempty"`
	LastError           string `json:"lastError,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures,omitempty"`
	NextAttemptAt       string `json:"nextAttemptAt,omitempty"`
	QuotaBytes          int64  `json:"quotaBytes"`
	Backup              bool   `json:"backup"`
	Files               int64  `json:"files"`
	Bytes               int64  `json:"bytes"`
	CreatedAt           string `json:"createdAt"`
	// Transfer stats: bytes pulled over the life of the subscription and
	// the last time any landed.
	BytesFetched  int64  `json:"bytesFetched"`
	LastFetchedAt string `json:"lastFetchedAt,omitempty"`
	// What of it this box actually holds (previews subscriptions fetch on
	// demand); computed from the replica rows on read.
	HydratedFiles int64 `json:"hydratedFiles"`
	HydratedBytes int64 `json:"hydratedBytes"`
	// Stamped by the handlers, not stored: the peer's display name and the
	// apps mounting the folder.
	Peer      string   `json:"peer,omitempty"`
	MountedBy []string `json:"mountedBy,omitempty"`
}

const fileFedSubCols = `id, peer_id, remote_share_id, title, folder, mode, detail_offered, detail_wanted, apps_allowed, status, accepted_impact, current_impact, revision_seen, manifest_etag, last_synced_at, last_error, consecutive_failures, next_attempt_at, quota_bytes, backup, files, bytes, created_at, bytes_fetched, last_fetched_at,
	(SELECT COUNT(*) FROM file_fed_replicas r WHERE r.subscription_id = file_fed_subscriptions.id AND r.kind = 'file' AND r.hydrated = 1),
	(SELECT COALESCE(SUM(r.size), 0) FROM file_fed_replicas r WHERE r.subscription_id = file_fed_subscriptions.id AND r.kind = 'file' AND r.hydrated = 1)`

func scanFileFedSub(sc scanner) (FileFedSubscription, error) {
	var sub FileFedSubscription
	var apps, backup int
	err := sc.Scan(&sub.ID, &sub.PeerID, &sub.RemoteShareID, &sub.Title, &sub.Folder, &sub.Mode, &sub.DetailOffered, &sub.DetailWanted, &apps, &sub.Status,
		&sub.AcceptedImpact, &sub.CurrentImpact, &sub.RevisionSeen, &sub.ManifestETag, &sub.LastSyncedAt, &sub.LastError, &sub.ConsecutiveFailures,
		&sub.NextAttemptAt, &sub.QuotaBytes, &backup, &sub.Files, &sub.Bytes, &sub.CreatedAt, &sub.BytesFetched, &sub.LastFetchedAt, &sub.HydratedFiles, &sub.HydratedBytes)
	sub.AppsAllowed, sub.Backup = apps != 0, backup != 0
	return sub, err
}

func (s *Store) CreateFileFedSubscription(sub *FileFedSubscription) (*FileFedSubscription, error) {
	res, err := s.db.Exec(`INSERT INTO file_fed_subscriptions (peer_id, remote_share_id, title, folder, mode, detail_offered, detail_wanted, apps_allowed, status, accepted_impact, current_impact, revision_seen, quota_bytes, backup, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?)`,
		sub.PeerID, sub.RemoteShareID, sub.Title, sub.Folder, sub.Mode, sub.DetailOffered, sub.DetailWanted, boolToInt(sub.AppsAllowed),
		sub.AcceptedImpact, sub.CurrentImpact, sub.RevisionSeen, sub.QuotaBytes, boolToInt(sub.Backup), now())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadyExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.FileFedSubscriptionByID(id)
}

func (s *Store) FileFedSubscriptionByID(id int64) (*FileFedSubscription, error) {
	return ptr(queryOne(s.db, scanFileFedSub, `SELECT `+fileFedSubCols+` FROM file_fed_subscriptions WHERE id = ?`, id))
}

func (s *Store) ListFileFedSubscriptions(peerID int64) ([]FileFedSubscription, error) {
	q := `SELECT ` + fileFedSubCols + ` FROM file_fed_subscriptions ORDER BY created_at DESC, id DESC`
	var args []any
	if peerID > 0 {
		q = `SELECT ` + fileFedSubCols + ` FROM file_fed_subscriptions WHERE peer_id = ? ORDER BY created_at DESC, id DESC`
		args = append(args, peerID)
	}
	return orEmpty(queryAll(s.db, scanFileFedSub, q, args...))
}

// UpdateFileFedSubscription rewrites the operator-editable and sync
// bookkeeping fields.
func (s *Store) UpdateFileFedSubscription(sub *FileFedSubscription) error {
	_, err := s.db.Exec(`UPDATE file_fed_subscriptions SET title = ?, mode = ?, detail_offered = ?, detail_wanted = ?, apps_allowed = ?, status = ?,
		accepted_impact = ?, current_impact = ?, revision_seen = ?, manifest_etag = ?, last_synced_at = ?, last_error = ?, consecutive_failures = ?,
		next_attempt_at = ?, quota_bytes = ?, backup = ?, files = ?, bytes = ? WHERE id = ?`,
		sub.Title, sub.Mode, sub.DetailOffered, sub.DetailWanted, boolToInt(sub.AppsAllowed), sub.Status, sub.AcceptedImpact, sub.CurrentImpact,
		sub.RevisionSeen, sub.ManifestETag, sub.LastSyncedAt, sub.LastError, sub.ConsecutiveFailures, sub.NextAttemptAt, sub.QuotaBytes,
		boolToInt(sub.Backup), sub.Files, sub.Bytes, sub.ID)
	return err
}

// AddFileFedSubscriptionFetched records bytes that landed for a subscription.
func (s *Store) AddFileFedSubscriptionFetched(id int64, n int64) error {
	_, err := s.db.Exec(`UPDATE file_fed_subscriptions SET bytes_fetched = bytes_fetched + ?, last_fetched_at = ? WHERE id = ?`, n, now(), id)
	return err
}

func (s *Store) DeleteFileFedSubscription(id int64) error {
	res, err := s.db.Exec(`DELETE FROM file_fed_subscriptions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- replicas ----

type FileFedReplica struct {
	SubscriptionID int64  `json:"subscriptionId"`
	Rel            string `json:"rel"`
	Kind           string `json:"kind"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	Mtime          int64  `json:"mtime"`
	Hydrated       bool   `json:"hydrated"`
	Origin         string `json:"origin,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
	// Contribute mode: our own contribution (Origin is this instance) —
	// when it last went up and whether the origin turned it away; or a
	// third party's entry known only as a stub.
	PushedAt int64 `json:"pushedAt,omitempty"`
	Rejected bool  `json:"rejected,omitempty"`
	Stub     bool  `json:"stub,omitempty"`
	// OriginShare: for a third party's entry, the share they contributed
	// through at the origin — how their fog finds it for us.
	OriginShare int64 `json:"originShare,omitempty"`
	// Dev and Ino: the device and inode the placed file landed as (0: not
	// recorded yet).
	Dev uint64 `json:"-"`
	Ino uint64 `json:"-"`
}

func (s *Store) ListFileFedReplicas(subID int64) (map[string]FileFedReplica, error) {
	out := map[string]FileFedReplica{}
	err := queryEach(s.db, func(sc scanner) error {
		r, err := scanFileFedReplica(sc)
		if err != nil {
			return err
		}
		out[r.Rel] = r
		return nil
	}, `SELECT `+fileFedReplicaCols+` FROM file_fed_replicas WHERE subscription_id = ?`, subID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FileFedReplicasFor returns the replica rows of subID among rels, keyed by
// rel: what a listing page needs, without loading the whole subscription.
func (s *Store) FileFedReplicasFor(subID int64, rels []string) (map[string]FileFedReplica, error) {
	out := map[string]FileFedReplica{}
	const chunk = 500
	for len(rels) > 0 {
		n := min(chunk, len(rels))
		args := make([]any, 0, n+1)
		args = append(args, subID)
		for _, r := range rels[:n] {
			args = append(args, r)
		}
		err := queryEach(s.db, func(sc scanner) error {
			r, err := scanFileFedReplica(sc)
			if err != nil {
				return err
			}
			out[r.Rel] = r
			return nil
		}, `SELECT `+fileFedReplicaCols+` FROM file_fed_replicas WHERE subscription_id = ? AND rel IN (?`+strings.Repeat(", ?", n-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		rels = rels[n:]
	}
	return out, nil
}

const fileFedReplicaCols = `subscription_id, rel, kind, sha256, size, mtime, hydrated, origin, pinned, pushed_at, rejected, stub, origin_share, dev, ino`

func scanFileFedReplica(sc scanner) (FileFedReplica, error) {
	var r FileFedReplica
	var hyd, pinned, rejected, stub int
	var dev, ino int64
	err := sc.Scan(&r.SubscriptionID, &r.Rel, &r.Kind, &r.SHA256, &r.Size, &r.Mtime, &hyd, &r.Origin, &pinned, &r.PushedAt, &rejected, &stub, &r.OriginShare, &dev, &ino)
	r.Dev, r.Ino = uint64(dev), uint64(ino)
	r.Hydrated, r.Pinned, r.Rejected, r.Stub = hyd != 0, pinned != 0, rejected != 0, stub != 0
	return r, err
}

// FileFedReplica returns one replica row.
func (s *Store) FileFedReplica(subID int64, rel string) (*FileFedReplica, error) {
	return ptr(queryOne(s.db, scanFileFedReplica, `SELECT `+fileFedReplicaCols+` FROM file_fed_replicas WHERE subscription_id = ? AND rel = ?`, subID, rel))
}

// SetFileFedReplicaContribution records the push state of our own
// contribution.
func (s *Store) SetFileFedReplicaContribution(subID int64, rel string, pushedAt int64, rejected bool) error {
	_, err := s.db.Exec(`UPDATE file_fed_replicas SET pushed_at = ?, rejected = ? WHERE subscription_id = ? AND rel = ?`, pushedAt, boolToInt(rejected), subID, rel)
	return err
}

// SetFileFedReplicaState records hydration and pinning for one replica.
func (s *Store) SetFileFedReplicaState(subID int64, rel string, hydrated, pinned bool) error {
	_, err := s.db.Exec(`UPDATE file_fed_replicas SET hydrated = ?, pinned = ? WHERE subscription_id = ? AND rel = ?`, boolToInt(hydrated), boolToInt(pinned), subID, rel)
	return err
}

const upsertReplicaSQL = `INSERT INTO file_fed_replicas (subscription_id, rel, kind, sha256, size, mtime, hydrated, origin, pinned, pushed_at, rejected, stub, origin_share, dev, ino) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(subscription_id, rel) DO UPDATE SET kind = excluded.kind, sha256 = excluded.sha256, size = excluded.size, mtime = excluded.mtime, hydrated = excluded.hydrated, origin = excluded.origin, stub = excluded.stub, origin_share = excluded.origin_share, dev = excluded.dev, ino = excluded.ino`

func replicaArgs(r FileFedReplica) []any {
	return []any{r.SubscriptionID, r.Rel, r.Kind, r.SHA256, r.Size, r.Mtime, boolToInt(r.Hydrated), r.Origin, boolToInt(r.Pinned), r.PushedAt, boolToInt(r.Rejected), boolToInt(r.Stub), r.OriginShare, int64(r.Dev), int64(r.Ino)}
}

func (s *Store) UpsertFileFedReplica(r FileFedReplica) error {
	_, err := s.db.Exec(upsertReplicaSQL, replicaArgs(r)...)
	return err
}

// UpsertFileFedReplicas records many replica rows in one transaction with
// one prepared statement: a sync listing thousands of entries.
func (s *Store) UpsertFileFedReplicas(rows []FileFedReplica) error {
	if len(rows) == 0 {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		st, err := tx.Prepare(upsertReplicaSQL)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, r := range rows {
			if _, err := st.Exec(replicaArgs(r)...); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteFileFedReplica(subID int64, rel string) error {
	_, err := s.db.Exec(`DELETE FROM file_fed_replicas WHERE subscription_id = ? AND rel = ?`, subID, rel)
	return err
}

// ---- contributed origins ----

type FileFedOrigin struct {
	Path          string `json:"path"`
	PeerID        int64  `json:"peerId"`
	ShareID       int64  `json:"shareId"`
	InstanceID    string `json:"instanceId"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	ContributedAt string `json:"contributedAt"`
	// Withdrawn: the peer unpaired; the bytes left, the listing stays.
	Withdrawn bool `json:"withdrawn,omitempty"`
}

func (s *Store) UpsertFileFedOrigin(o FileFedOrigin) error {
	_, err := s.db.Exec(`INSERT INTO file_fed_origins (path, peer_id, share_id, instance_id, sha256, size, contributed_at, withdrawn) VALUES (?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(path) DO UPDATE SET peer_id = excluded.peer_id, share_id = excluded.share_id, instance_id = excluded.instance_id, sha256 = excluded.sha256, size = excluded.size, contributed_at = excluded.contributed_at, withdrawn = 0`,
		o.Path, o.PeerID, o.ShareID, o.InstanceID, o.SHA256, o.Size, now())
	return err
}

// ContributedBytesForShare sums the sizes of files peers have contributed to
// a share and not withdrawn — the live usage a contribute quota measures
// against.
func (s *Store) ContributedBytesForShare(shareID int64) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM file_fed_origins WHERE share_id = ? AND withdrawn = 0`, shareID).Scan(&n)
	return n.Int64, err
}

func (s *Store) FileFedOriginByPath(path string) (*FileFedOrigin, error) {
	return ptr(queryOne(s.db, scanFileFedOrigin, `SELECT `+fileFedOriginCols+` FROM file_fed_origins WHERE path = ?`, path))
}

func (s *Store) ListFileFedOrigins() ([]FileFedOrigin, error) {
	return orEmpty(queryAll(s.db, scanFileFedOrigin, `SELECT `+fileFedOriginCols+` FROM file_fed_origins`))
}

const fileFedOriginCols = `path, peer_id, share_id, instance_id, sha256, size, contributed_at, withdrawn`

func scanFileFedOrigin(sc scanner) (FileFedOrigin, error) {
	var o FileFedOrigin
	var withdrawn int
	err := sc.Scan(&o.Path, &o.PeerID, &o.ShareID, &o.InstanceID, &o.SHA256, &o.Size, &o.ContributedAt, &withdrawn)
	o.Withdrawn = withdrawn != 0
	return o, err
}

// SetFileFedOriginWithdrawn marks a contribution as a stub (or fills it).
func (s *Store) SetFileFedOriginWithdrawn(path string, withdrawn bool) error {
	_, err := s.db.Exec(`UPDATE file_fed_origins SET withdrawn = ? WHERE path = ?`, boolToInt(withdrawn), path)
	return err
}

func (s *Store) DeleteFileFedOrigin(path string) error {
	_, err := s.db.Exec(`DELETE FROM file_fed_origins WHERE path = ?`, path)
	return err
}
