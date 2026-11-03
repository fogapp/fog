package store

// LatestRevisionSpecs returns every app's newest revision spec JSON, keyed by
// app ID, in a single query. This is the batch companion to LatestRevision:
// the dashboard poll and the health tick want the latest spec of every app at
// once, and one round-trip per app was pure overhead. An app with no
// revisions has no entry — the same absence LatestRevision reports as
// ErrNotFound. The grouped MAX reads only the UNIQUE(app_id, rev) index, and
// the join then fetches one spec per app: a correlated subquery here scanned
// every revision row, spec JSON and all.
func (s *Store) LatestRevisionSpecs() (map[int64]string, error) {
	out := map[int64]string{}
	err := queryEach(s.rdb, func(sc scanner) error {
		var appID int64
		var spec string
		if err := sc.Scan(&appID, &spec); err != nil {
			return err
		}
		out[appID] = spec
		return nil
	}, `SELECT r.app_id, r.spec FROM
		(SELECT app_id, MAX(rev) AS rev FROM app_revisions GROUP BY app_id) AS latest
		JOIN app_revisions AS r ON r.app_id = latest.app_id AND r.rev = latest.rev`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PruneRevisions trims each app's revision history to its newest keep
// revisions. Nothing older is deleted while something still points at it: a
// deploy row (revision_id is a foreign key, and deploy history is pruned on
// its own schedule), a version-history row (it names the rev it ran under),
// or the app's current_rev. The newest revision always survives, so the
// MAX(rev)+1 numbering in CreateRevision never reuses a number. Returns rows
// deleted.
func (s *Store) PruneRevisions(keep int) (int64, error) {
	r, err := s.db.Exec(`DELETE FROM app_revisions WHERE id IN (
		SELECT r.id FROM app_revisions AS r
		JOIN (SELECT app_id, MAX(rev) AS rev FROM app_revisions GROUP BY app_id) AS latest
			ON latest.app_id = r.app_id
		JOIN apps AS a ON a.id = r.app_id
		WHERE r.rev <= latest.rev - ?
			AND r.rev <> a.current_rev
			AND r.id NOT IN (SELECT revision_id FROM deploys)
			AND (r.app_id, r.rev) NOT IN (SELECT app_id, rev FROM app_versions))`, keep)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
