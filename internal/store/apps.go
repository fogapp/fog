package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/fogports"
)

type App struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	DesiredState   string `json:"desiredState"`
	CurrentRev     int64  `json:"currentRev"`
	HostPort       int    `json:"hostPort"`
	RepoNotes      string `json:"repoNotes"`
	DeployedCommit string `json:"deployedCommit"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

type AppRevision struct {
	ID        int64  `json:"id"`
	AppID     int64  `json:"appId"`
	Rev       int64  `json:"rev"`
	Spec      string `json:"spec"` // AppSpec JSON
	Comment   string `json:"comment"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"createdAt"`
}

type Deploy struct {
	ID         int64  `json:"id"`
	AppID      int64  `json:"appId"`
	RevisionID int64  `json:"revisionId"`
	Rev        int64  `json:"rev"`
	Status     string `json:"status"`
	Log        string `json:"log"`
	Actor      string `json:"actor"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// LogTruncated marks a Log that carries only the tail of what was
	// recorded, so the UI can say so rather than presenting a partial log as
	// the whole story.
	LogTruncated bool `json:"logTruncated,omitempty"`
}

func (s *Store) CreateApp(name string) (*App, error) {
	n := now()
	res, err := s.db.Exec(`INSERT INTO apps (name, desired_state, created_at, updated_at) VALUES (?, 'running', ?, ?)`, name, n, n)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &App{ID: id, Name: name, DesiredState: "running", CreatedAt: n, UpdatedAt: n}, nil
}

const appCols = `id, name, desired_state, current_rev, host_port, repo_notes, deployed_commit, created_at, updated_at`

func scanApp(sc scanner) (App, error) {
	var a App
	err := sc.Scan(&a.ID, &a.Name, &a.DesiredState, &a.CurrentRev, &a.HostPort, &a.RepoNotes, &a.DeployedCommit, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (s *Store) AppByName(name string) (*App, error) {
	return ptr(queryOne(s.rdb, scanApp, `SELECT `+appCols+` FROM apps WHERE name = ?`, name))
}

// AppByID resolves an app row by primary key — for callers holding a
// foreign key (a backup's app_id; the error ingest path's project id, the
// DSN's last path segment), not a name.
func (s *Store) AppByID(id int64) (*App, error) {
	return ptr(queryOne(s.rdb, scanApp, `SELECT `+appCols+` FROM apps WHERE id = ?`, id))
}

func (s *Store) ListApps() ([]App, error) {
	return queryAll(s.rdb, scanApp, `SELECT `+appCols+` FROM apps ORDER BY name`)
}

// Host ports handed to routed apps. A bounded window, reused from the bottom:
// MAX+1 climbed forever and never reclaimed the ports of deleted apps, so a
// server that churned through apps would eventually allocate past the end of
// the port space and hand out nonsense.
// The window is fogports' (the one list of fog's own host ports, which apps
// are refused).
const (
	HostPortMin = fogports.DirectMin
	HostPortMax = fogports.DirectMax
)

// AllocateHostPort assigns the app a stable host port, reusing the lowest free
// one in the window. Returns the existing port if the app already has one.
// Read-pick-write runs in one transaction, and the partial unique index on
// host_port backstops it: a concurrent claim of the same port is a constraint
// violation to retry against the new taken set, never a silent double booking.
func (s *Store) AllocateHostPort(appID int64) (int, error) {
	for attempt := 0; ; attempt++ {
		port, err := s.allocateHostPort(appID)
		if err != nil && attempt < 3 && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			continue
		}
		return port, err
	}
}

func (s *Store) allocateHostPort(appID int64) (port int, err error) {
	err = s.withTx(func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT host_port FROM apps WHERE id = ?`, appID).Scan(&port); err != nil {
			return err
		}
		if port != 0 {
			return nil // already has one; the commit writes nothing
		}
		taken := map[int]bool{}
		err := queryEach(tx, func(sc scanner) error {
			p, err := scanValue[int](sc)
			taken[p] = true
			return err
		}, `SELECT host_port FROM apps WHERE host_port != 0 ORDER BY host_port`)
		if err != nil {
			return err
		}
		for p := HostPortMin; p <= HostPortMax; p++ {
			if !taken[p] {
				port = p
				break
			}
		}
		if port == 0 {
			return fmt.Errorf("no free host port in %d–%d", HostPortMin, HostPortMax)
		}
		_, err = tx.Exec(`UPDATE apps SET host_port = ?, updated_at = ? WHERE id = ?`, port, now(), appID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return port, nil
}

func (s *Store) SetAppRepoNotes(appID int64, notes string) error {
	_, err := s.db.Exec(`UPDATE apps SET repo_notes = ?, updated_at = ? WHERE id = ?`, notes, now(), appID)
	return err
}

// Contracts an app declared via its repo fog.yaml, recorded at build time.
// Kept off the main App scan — only the federation paths ask.

func (s *Store) SetAppContracts(appID int64, contracts []string) error {
	if contracts == nil {
		contracts = []string{}
	}
	raw, err := json.Marshal(contracts)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE apps SET contracts = ?, updated_at = ? WHERE id = ?`, string(raw), now(), appID)
	return err
}

func (s *Store) AppContracts(appID int64) ([]string, error) {
	raw, err := queryOne(s.db, scanValue[string], `SELECT contracts FROM apps WHERE id = ?`, appID)
	if err != nil {
		return nil, err
	}
	var out []string
	// Corrupt JSON must surface, not read as "no contracts" — that would
	// quietly make the app refuse every connection.
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("app %d contracts: %w", appID, err)
	}
	return out, nil
}

// AppsWithContract lists the apps that declared contract, by name — one
// query for the notifiers that fan out to every contracted app. A row whose
// contracts JSON is corrupt is skipped, as it was when each caller ignored
// AppContracts' error, rather than failing the whole fan-out.
func (s *Store) AppsWithContract(contract string) ([]App, error) {
	return queryAll(s.db, scanApp, `SELECT `+appCols+` FROM apps
		WHERE json_valid(contracts)
		  AND EXISTS (SELECT 1 FROM json_each(apps.contracts) WHERE value = ?)
		ORDER BY name`, contract)
}

// AllAppContracts is AppContracts for every app at once; corrupt rows are
// left out, as AppsWithContract leaves them out.
func (s *Store) AllAppContracts() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT id, contracts FROM apps WHERE json_valid(contracts)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var cs []string
		if json.Unmarshal([]byte(raw), &cs) == nil {
			out[id] = cs
		}
	}
	return out, rows.Err()
}

// FederationFacts is the identity-permanence view of an app recorded from its
// fog.yaml at build time (Project J): the contracts it declares, what a host
// change costs its federation, and any contracts fog does not recognise.
type FederationFacts struct {
	Contracts        []string `json:"contracts"`
	HostChange       string   `json:"hostChange,omitempty"`
	UnknownContracts []string `json:"unknownContracts,omitempty"`
}

func (s *Store) SetAppFederationFacts(appID int64, f FederationFacts) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE apps SET federation_facts = ?, updated_at = ? WHERE id = ?`, string(raw), now(), appID)
	return err
}

// AppFederationFacts reads them back. An app built before this column, or one
// that declares no federation, reads as the zero value, not an error.
func (s *Store) AppFederationFacts(appID int64) (FederationFacts, error) {
	raw, err := queryOne(s.db, scanValue[string], `SELECT federation_facts FROM apps WHERE id = ?`, appID)
	if err != nil {
		return FederationFacts{}, err
	}
	if raw == "" {
		return FederationFacts{}, nil
	}
	var f FederationFacts
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return FederationFacts{}, fmt.Errorf("app %d federation_facts: %w", appID, err)
	}
	return f, nil
}

func (s *Store) SetAppDesiredState(appID int64, state string) error {
	_, err := s.db.Exec(`UPDATE apps SET desired_state = ?, updated_at = ? WHERE id = ?`, state, now(), appID)
	return err
}

func (s *Store) SetAppDeployedCommit(appID int64, commit string) error {
	_, err := s.db.Exec(`UPDATE apps SET deployed_commit = ?, updated_at = ? WHERE id = ?`, commit, now(), appID)
	return err
}

func (s *Store) SetAppCurrentRev(appID, rev int64) error {
	_, err := s.db.Exec(`UPDATE apps SET current_rev = ?, updated_at = ? WHERE id = ?`, rev, now(), appID)
	return err
}

func (s *Store) DeleteApp(appID int64) error {
	_, err := s.db.Exec(`DELETE FROM apps WHERE id = ?`, appID)
	return err
}

// ---- revisions ----

func (s *Store) CreateRevision(appID int64, spec, comment, actor string) (*AppRevision, error) {
	// MAX(rev)+1 computed inside the INSERT itself, so two revisions created
	// concurrently can't both read the same max and collide on a rev number.
	n := now()
	res, err := s.db.Exec(`INSERT INTO app_revisions (app_id, rev, spec, comment, actor, created_at)
		SELECT ?, COALESCE(MAX(rev), 0) + 1, ?, ?, ?, ? FROM app_revisions WHERE app_id = ?`,
		appID, spec, comment, actor, n, appID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	var rev int64
	if err := s.db.QueryRow(`SELECT rev FROM app_revisions WHERE id = ?`, id).Scan(&rev); err != nil {
		return nil, err
	}
	return &AppRevision{ID: id, AppID: appID, Rev: rev, Spec: spec, Comment: comment, Actor: actor, CreatedAt: n}, nil
}

const revisionCols = `id, app_id, rev, spec, comment, actor, created_at`

func scanRevision(sc scanner) (AppRevision, error) {
	var r AppRevision
	err := sc.Scan(&r.ID, &r.AppID, &r.Rev, &r.Spec, &r.Comment, &r.Actor, &r.CreatedAt)
	return r, err
}

func (s *Store) RevisionByRev(appID, rev int64) (*AppRevision, error) {
	return ptr(queryOne(s.rdb, scanRevision, `SELECT `+revisionCols+` FROM app_revisions WHERE app_id = ? AND rev = ?`, appID, rev))
}

func (s *Store) LatestRevision(appID int64) (*AppRevision, error) {
	return ptr(queryOne(s.rdb, scanRevision, `SELECT `+revisionCols+` FROM app_revisions WHERE app_id = ? ORDER BY rev DESC LIMIT 1`, appID))
}

// ListRevisionSummaries lists revisions without their spec JSON — what a
// history table needs. The specs are the bulk of the rows and no list view
// reads them; fetching 50 of them per app-page load was pure weight, on the
// wire and through the store's single connection.
func (s *Store) ListRevisionSummaries(appID int64, limit int) ([]AppRevision, error) {
	return queryAll(s.db, func(sc scanner) (AppRevision, error) {
		var r AppRevision
		err := sc.Scan(&r.ID, &r.AppID, &r.Rev, &r.Comment, &r.Actor, &r.CreatedAt)
		return r, err
	}, `SELECT id, app_id, rev, comment, actor, created_at FROM app_revisions WHERE app_id = ? ORDER BY rev DESC LIMIT ?`, appID, limit)
}

// ListRevisionSpecs lists revisions newest-first with their spec JSON — for
// the few readers that must look back through the specs themselves (when
// backups were switched on, say), rather than at the newest one alone.
func (s *Store) ListRevisionSpecs(appID int64, limit int) ([]AppRevision, error) {
	return queryAll(s.db, func(sc scanner) (AppRevision, error) {
		var r AppRevision
		err := sc.Scan(&r.ID, &r.AppID, &r.Rev, &r.Spec, &r.CreatedAt)
		return r, err
	}, `SELECT id, app_id, rev, spec, created_at FROM app_revisions WHERE app_id = ? ORDER BY rev DESC LIMIT ?`, appID, limit)
}

// ---- secrets ----
// appID 0 is stored as NULL = system-level secret.

func (s *Store) SetSecret(appID int64, key, ciphertext string) error {
	if appID == 0 {
		// System secrets need the partial-index conflict target: NULL app_ids
		// never collide on the plain UNIQUE(app_id, key) constraint.
		_, err := s.db.Exec(`INSERT INTO secrets (app_id, key, ciphertext, updated_at) VALUES (NULL, ?, ?, ?)
			ON CONFLICT(key) WHERE app_id IS NULL DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
			key, ciphertext, now())
		return err
	}
	_, err := s.db.Exec(`INSERT INTO secrets (app_id, key, ciphertext, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(app_id, key) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		appID, key, ciphertext, now())
	return err
}

func (s *Store) GetSecret(appID int64, key string) (string, error) {
	if appID == 0 {
		return queryOne(s.db, scanValue[string], `SELECT ciphertext FROM secrets WHERE app_id IS NULL AND key = ?`, key)
	}
	return queryOne(s.db, scanValue[string], `SELECT ciphertext FROM secrets WHERE app_id = ? AND key = ?`, appID, key)
}

func (s *Store) DeleteSecret(appID int64, key string) error {
	if appID == 0 {
		_, err := s.db.Exec(`DELETE FROM secrets WHERE app_id IS NULL AND key = ?`, key)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM secrets WHERE app_id = ? AND key = ?`, appID, key)
	return err
}

func (s *Store) SecretKeys(appID int64) ([]string, error) {
	if appID == 0 {
		return queryAll(s.db, scanValue[string], `SELECT key FROM secrets WHERE app_id IS NULL ORDER BY key`)
	}
	return queryAll(s.db, scanValue[string], `SELECT key FROM secrets WHERE app_id = ? ORDER BY key`, appID)
}

// ---- deploys ----

func (s *Store) CreateDeploy(appID, revisionID int64, actor string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO deploys (app_id, revision_id, status, actor, started_at) VALUES (?, ?, 'running', ?, ?)`,
		appID, revisionID, actor, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishDeploy(deployID int64, status, log string) error {
	_, err := s.db.Exec(`UPDATE deploys SET status = ?, log = ?, finished_at = ? WHERE id = ?`, status, log, now(), deployID)
	return err
}

// FailInterruptedDeploys closes out deploys a dead fogd left behind. Called
// once at boot: FinishDeploy only ever runs in the goroutine that started the
// deploy, so any row still 'running' when fogd starts is an orphan the UI
// would otherwise report as in-flight forever. Returns how many were closed.
func (s *Store) FailInterruptedDeploys() (int64, error) {
	res, err := s.db.Exec(`UPDATE deploys SET status = 'failed', finished_at = ?,
		log = log || char(10) || 'interrupted by fogd restart' WHERE status = 'running'`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneDeploys trims each app's deploy history to its most recent keep rows.
// A superseded deploy is pure history — nothing references it — but each row
// can carry a build log up to the 256 KB live-log cap, so unbounded history
// is real disk on a small box. Returns rows deleted.
func (s *Store) PruneDeploys(keep int) (int64, error) {
	r, err := s.db.Exec(`DELETE FROM deploys WHERE id NOT IN
		(SELECT d2.id FROM deploys d2 WHERE d2.app_id = deploys.app_id ORDER BY d2.id DESC LIMIT ?)`, keep)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ClearOldDeployLogs empties the log column on deploys older than each app's
// most recent keep rows. The rows themselves stay — who deployed what, when,
// and whether it worked is cheap history worth holding long after the build
// log stopped being useful. A running deploy has no log yet (FinishDeploy
// writes it), so in-flight rows are untouched by construction. Returns rows
// cleared.
func (s *Store) ClearOldDeployLogs(keep int) (int64, error) {
	r, err := s.db.Exec(`UPDATE deploys SET log = '' WHERE log != '' AND id NOT IN
		(SELECT d2.id FROM deploys d2 WHERE d2.app_id = deploys.app_id ORDER BY d2.id DESC LIMIT ?)`, keep)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ListDeploySummaries lists deploys carrying only the last tailBytes of each
// log. The history view renders logs in a small scrolling box, so the tail is
// what's actually readable there — and shipping twenty full build logs (which
// ran to megabytes before they were capped) on every app-page load was the
// single heaviest thing that endpoint did.
func (s *Store) ListDeploySummaries(appID int64, limit, tailBytes int) ([]Deploy, error) {
	return queryAll(s.rdb, func(sc scanner) (Deploy, error) {
		var d Deploy
		err := sc.Scan(&d.ID, &d.AppID, &d.RevisionID, &d.Rev, &d.Status, &d.Log, &d.LogTruncated,
			&d.Actor, &d.StartedAt, &d.FinishedAt)
		return d, err
	}, `SELECT d.id, d.app_id, d.revision_id, r.rev, d.status,
			substr(d.log, -?), length(d.log) > ?, d.actor, d.started_at, COALESCE(d.finished_at, '')
		FROM deploys d JOIN app_revisions r ON r.id = d.revision_id
		WHERE d.app_id = ? ORDER BY d.id DESC LIMIT ?`, tailBytes, tailBytes, appID, limit)
}

// DeployLog returns one deploy's full build log, for the reader that needs
// all of it (failure diagnosis) after a summary query served the rest.
func (s *Store) DeployLog(deployID int64) (string, error) {
	return queryOne(s.db, func(sc scanner) (string, error) {
		var log string
		err := sc.Scan(&log)
		return log, err
	}, `SELECT log FROM deploys WHERE id = ?`, deployID)
}

// DeployTiming is what the shell's deploy tray estimates progress from: when
// the app's running deploy began ("" while none has — an update's snapshot
// and pull come before its deploy row), and how long its recent successful
// deploys took, the median of the last five (0 before the first).
func (s *Store) DeployTiming(appID int64) (startedAt string, typical time.Duration, err error) {
	type span struct{ started, finished string }
	rows, err := queryAll(s.rdb, func(sc scanner) (span, error) {
		var sp span
		err := sc.Scan(&sp.started, &sp.finished)
		return sp, err
	}, `SELECT started_at, COALESCE(finished_at, '') FROM deploys
		WHERE app_id = ? AND status IN ('running', 'succeeded') ORDER BY id DESC LIMIT 6`, appID)
	if err != nil {
		return "", 0, err
	}
	var took []time.Duration
	for i, sp := range rows {
		if i == 0 && sp.finished == "" {
			startedAt = sp.started
			continue
		}
		a, aerr := time.Parse(time.RFC3339, sp.started)
		b, berr := time.Parse(time.RFC3339, sp.finished)
		if aerr == nil && berr == nil && b.After(a) {
			took = append(took, b.Sub(a))
		}
	}
	if len(took) > 5 {
		took = took[:5]
	}
	if len(took) > 0 {
		slices.Sort(took)
		typical = took[len(took)/2]
	}
	return startedAt, typical, nil
}
