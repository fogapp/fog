package store

// AppVersion is one version an app has run: what it was, when it last ran,
// and how it got there. The Updates tab lists these as rollback targets, so
// every field has to be something fog watched happen — a digest it deployed,
// a commit it built — never something inferred from a tag that has since
// moved.
type AppVersion struct {
	ID     int64  `json:"id"`
	AppID  int64  `json:"appId"`
	Source string `json:"source"` // image | git
	Ref    string `json:"ref"`    // sha256:… digest, or commit sha
	Image  string `json:"image,omitempty"`
	Rev    int64  `json:"rev"`
	Actor  string `json:"actor,omitempty"`
	// CreatedAt is when this version last went live; FirstAt when it first
	// did. They differ once a version has been deployed more than once —
	// after a rollback, or a redeploy of the same image.
	CreatedAt string `json:"createdAt"`
	FirstAt   string `json:"firstAt,omitempty"`
	// Deploys counts how many times this exact ref has gone live.
	Deploys int `json:"deploys,omitempty"`
}

// RecordAppVersion appends the version a deploy just landed on. An empty ref
// records nothing: a version fog cannot name is not a version it can offer
// to go back to, and a blank row would sit in the list as an untakeable
// rollback target.
func (s *Store) RecordAppVersion(appID int64, source, ref, image string, rev int64, actor string) error {
	if ref == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO app_versions (app_id, source, ref, image, rev, actor, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, appID, source, ref, image, rev, actor, now())
	return err
}

const versionCols = `id, app_id, source, ref, image, rev, actor, created_at`
const versionColsV = `v.id, v.app_id, v.source, v.ref, v.image, v.rev, v.actor, v.created_at`

func scanVersion(sc scanner) (AppVersion, error) {
	var v AppVersion
	err := sc.Scan(versionDest(&v)...)
	return v, err
}

func versionDest(v *AppVersion) []any {
	return []any{&v.ID, &v.AppID, &v.Source, &v.Ref, &v.Image, &v.Rev, &v.Actor, &v.CreatedAt}
}

// CurrentAppVersion is the version the app most recently deployed — what is
// running now, as far as fog saw. ErrNotFound before the first deploy this
// table witnessed (apps that last deployed before it existed have no rows).
func (s *Store) CurrentAppVersion(appID int64) (*AppVersion, error) {
	return ptr(queryOne(s.db, scanVersion, `SELECT `+versionCols+` FROM app_versions WHERE app_id = ? ORDER BY id DESC LIMIT 1`, appID))
}

// ListAppVersions returns the app's distinct versions, most recently run
// first. Distinct on purpose: three redeploys of one image are one place to
// go back to, and a list that repeated them would push the version an
// operator actually wants off the end of it.
func (s *Store) ListAppVersions(appID int64, limit int) ([]AppVersion, error) {
	return queryAll(s.db, func(sc scanner) (AppVersion, error) {
		var v AppVersion
		err := sc.Scan(append(versionDest(&v), &v.FirstAt, &v.Deploys)...)
		return v, err
	}, `SELECT `+versionColsV+`, g.first_at, g.deploys
		FROM app_versions v
		JOIN (SELECT ref, MAX(id) AS last_id, MIN(created_at) AS first_at, COUNT(*) AS deploys
		      FROM app_versions WHERE app_id = ? GROUP BY ref) g ON g.last_id = v.id
		ORDER BY v.id DESC LIMIT ?`, appID, limit)
}

// AppVersionByRef finds one recorded version of an app by its ref — the
// rollback target lookup, and the check that a requested ref is one this app
// has actually run rather than an arbitrary digest handed to the API.
func (s *Store) AppVersionByRef(appID int64, ref string) (*AppVersion, error) {
	return ptr(queryOne(s.db, scanVersion, `SELECT `+versionCols+` FROM app_versions
		WHERE app_id = ? AND ref = ? ORDER BY id DESC LIMIT 1`, appID, ref))
}
