package store

import "encoding/json"

// Integration is one app connected to another's offer
// (docs/integrations-contract.md).
type Integration struct {
	ID        int64    `json:"id"`
	AppID     int64    `json:"appId"`
	Provider  string   `json:"provider"`
	Offer     string   `json:"offer"`
	RemoteID  string   `json:"-"`
	Env       []string `json:"env"`
	Secrets   []string `json:"secrets"`
	CreatedBy string   `json:"createdBy"`
	CreatedAt string   `json:"createdAt"`
	// State is pending while a person has yet to approve the offer at the
	// provider, then connected.
	State        string `json:"state"`
	ReturnDigest string `json:"-"`
	Deploy       bool   `json:"-"`
}

// Integration states.
const (
	IntegrationPending   = "pending"
	IntegrationConnected = "connected"
)

const integrationCols = `id, app_id, provider, offer, remote_id, env, secrets, created_by, created_at, state, return_digest, deploy`

func scanIntegration(sc scanner) (Integration, error) {
	var in Integration
	var env, secrets string
	if err := sc.Scan(&in.ID, &in.AppID, &in.Provider, &in.Offer, &in.RemoteID, &env, &secrets, &in.CreatedBy, &in.CreatedAt, &in.State, &in.ReturnDigest, &in.Deploy); err != nil {
		return in, err
	}
	in.Env, in.Secrets = []string{}, []string{}
	_ = json.Unmarshal([]byte(env), &in.Env)
	_ = json.Unmarshal([]byte(secrets), &in.Secrets)
	return in, nil
}

// CreateIntegration records a connection; its ID is filled in.
func (s *Store) CreateIntegration(in *Integration) error {
	env, _ := json.Marshal(nonNil(in.Env))
	secrets, _ := json.Marshal(nonNil(in.Secrets))
	in.CreatedAt = now()
	if in.State == "" {
		in.State = IntegrationConnected
	}
	res, err := s.db.Exec(`INSERT INTO app_integrations (app_id, provider, offer, remote_id, env, secrets, created_by, created_at, state, return_digest, deploy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.AppID, in.Provider, in.Offer, in.RemoteID, string(env), string(secrets), in.CreatedBy, in.CreatedAt,
		in.State, in.ReturnDigest, in.Deploy)
	if err != nil {
		return err
	}
	in.ID, err = res.LastInsertId()
	return err
}

// ListIntegrations answers an app's connections, oldest first.
func (s *Store) ListIntegrations(appID int64) ([]Integration, error) {
	return orEmpty(queryAll(s.db, scanIntegration,
		`SELECT `+integrationCols+` FROM app_integrations WHERE app_id = ? ORDER BY id`, appID))
}

// IntegrationsWithProvider answers every connection to provider's offers.
func (s *Store) IntegrationsWithProvider(provider string) ([]Integration, error) {
	return orEmpty(queryAll(s.db, scanIntegration,
		`SELECT `+integrationCols+` FROM app_integrations WHERE provider = ? ORDER BY id`, provider))
}

// DeleteIntegration forgets a connection.
func (s *Store) DeleteIntegration(id int64) error {
	_, err := s.db.Exec(`DELETE FROM app_integrations WHERE id = ?`, id)
	return err
}

// IntegrationByReturnDigest answers the pending connection a provider sent
// a person back to fog about.
func (s *Store) IntegrationByReturnDigest(digest string) (*Integration, error) {
	return ptr(queryOne(s.db, scanIntegration,
		`SELECT `+integrationCols+` FROM app_integrations WHERE state = ? AND return_digest = ? AND return_digest != ''`,
		IntegrationPending, digest))
}

// MarkIntegrationConnected records that fog claimed a pending connection's
// credential and set it under env and secrets.
func (s *Store) MarkIntegrationConnected(id int64, env, secrets []string) error {
	e, _ := json.Marshal(nonNil(env))
	sc, _ := json.Marshal(nonNil(secrets))
	_, err := s.db.Exec(`UPDATE app_integrations SET state = ?, return_digest = '', env = ?, secrets = ? WHERE id = ?`,
		IntegrationConnected, string(e), string(sc), id)
	return err
}

// SetAppIntegrations records what an app's repo declares about
// integrations, as JSON; "" is none.
func (s *Store) SetAppIntegrations(appID int64, declared string) error {
	_, err := s.db.Exec(`UPDATE apps SET integrations = ?, updated_at = ? WHERE id = ?`, declared, now(), appID)
	return err
}

// AllAppIntegrations answers every app's recorded declaration by id,
// leaving out the apps that declare none.
func (s *Store) AllAppIntegrations() (map[int64]string, error) {
	out := map[int64]string{}
	err := queryEach(s.db, func(sc scanner) error {
		var id int64
		var raw string
		if err := sc.Scan(&id, &raw); err != nil {
			return err
		}
		out[id] = raw
		return nil
	}, `SELECT id, integrations FROM apps WHERE integrations != ''`)
	return out, err
}

// AppIntegrations answers one app's recorded declaration; "" is none.
func (s *Store) AppIntegrations(appID int64) (string, error) {
	return queryOne(s.db, scanValue[string], `SELECT integrations FROM apps WHERE id = ?`, appID)
}
