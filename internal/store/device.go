package store

import (
	"database/sql"
	"time"
)

// ---- device authorization (0056) ----

const (
	DeviceStatusPending  = "pending"
	DeviceStatusApproved = "approved"
	DeviceStatusDenied   = "denied"
	DeviceStatusUsed     = "used"
)

// DeviceCode is one device sign-in in flight.
type DeviceCode struct {
	UserCode   string
	ClientID   string
	DeviceName string
	Scope      string
	Status     string
	SessionID  int64
	Grant      string
	ExpiresAt  time.Time
	LastPollAt time.Time
}

const deviceCodeCols = `user_code, client_id, device_name, scope, status, COALESCE(session_id, 0), grant, expires_at, last_poll_at`

func scanDeviceCode(sc scanner) (DeviceCode, error) {
	var d DeviceCode
	var exp, poll string
	err := sc.Scan(&d.UserCode, &d.ClientID, &d.DeviceName, &d.Scope, &d.Status, &d.SessionID, &d.Grant, &exp, &poll)
	d.ExpiresAt, d.LastPollAt = parseTime(exp), parseTime(poll)
	return d, err
}

// CreateDeviceCode stores a new device sign-in. A user code that collides
// with a live one is ErrAlreadyExists; the caller draws another.
func (s *Store) CreateDeviceCode(digest, userCode, clientID, deviceName, scope string, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO oidc_device_codes (device_digest, user_code, client_id, device_name, scope, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		digest, userCode, clientID, deviceName, scope, stamp(time.Now().Add(ttl)))
	if isUniqueViolation(err) {
		return ErrAlreadyExists
	}
	return err
}

// DeviceCodeByUserCode finds a live, still-pending sign-in by what the
// person typed.
func (s *Store) DeviceCodeByUserCode(userCode string) (*DeviceCode, error) {
	return ptr(queryOne(s.db, scanDeviceCode, `SELECT `+deviceCodeCols+` FROM oidc_device_codes
		WHERE user_code = ? AND status = 'pending' AND expires_at > ?`, userCode, now()))
}

// DecideDeviceCode records the person's answer, once.
func (s *Store) DecideDeviceCode(userCode string, approve bool, sessionID int64, grant string) error {
	status := DeviceStatusDenied
	var sess any
	if approve {
		status, sess = DeviceStatusApproved, sessionID
	}
	res, err := s.db.Exec(`UPDATE oidc_device_codes SET status = ?, session_id = ?, grant = ? WHERE user_code = ? AND status = 'pending' AND expires_at > ?`,
		status, sess, grant, userCode, now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PollDeviceCode is the token endpoint's read: the row for a device code,
// stamping the poll so a client that polls too fast can be told to slow
// down. The previous poll time is returned in LastPollAt.
func (s *Store) PollDeviceCode(digest, clientID string) (*DeviceCode, error) {
	var d DeviceCode
	err := s.withTx(func(tx *sql.Tx) error {
		var err error
		d, err = queryOne(tx, scanDeviceCode, `SELECT `+deviceCodeCols+` FROM oidc_device_codes WHERE device_digest = ? AND client_id = ?`, digest, clientID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE oidc_device_codes SET last_poll_at = ? WHERE device_digest = ?`, time.Now().UTC().Format(time.RFC3339Nano), digest)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// SpendDeviceCode turns an approved device code into tokens exactly once.
func (s *Store) SpendDeviceCode(digest string) error {
	res, err := s.db.Exec(`UPDATE oidc_device_codes SET status = 'used' WHERE device_digest = ? AND status = 'approved'`, digest)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneDeviceCodes drops expired device sign-ins.
func (s *Store) PruneDeviceCodes() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM oidc_device_codes WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
