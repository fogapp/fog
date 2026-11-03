package offsite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// Manager owns the targets: their credentials, their lockers, and (in
// later files) the loop that fills them.
type Manager struct {
	DataDir string
	Store   *store.Store
	// Vault wraps root keys for nightly use and holds the credentials.
	Vault vault.Cipher
	Log   *slog.Logger
	// Notify is ntfy; nil is quiet.
	Notify func(title, msg string, ok bool)
	// Passphrase returns the recovery-kit passphrase — the one secret that
	// opens a locker on a fresh machine — or "" when unset.
	Passphrase func() string
	// ServerName names new lockers; nil or "" falls back to "fog".
	ServerName func() string
	// HTTPClient overrides the bucket transport (tests).
	HTTPClient *http.Client
	// B2AuthURL overrides Backblaze's sign-in endpoint and B2Settle the
	// wait for a new key to reach the S3 edge (tests).
	B2AuthURL string
	B2Settle  time.Duration
	// BackupRoot is the local backup directory (the backup manager owns
	// the setting); PathAvailable says whether a path's share is usable.
	BackupRoot    func() string
	PathAvailable func(path string) bool
	// HoldBackupDir holds the backup directory while a sync reads it or a
	// fetch writes into it, so a move of it waits; optional (tests).
	HoldBackupDir func() (release func(), err error)
	// Jobs is the progress registry; nil gets one on first use.
	Jobs *jobs.Jobs
	// Now is the clock (tests).
	Now func() time.Time
	// The restore doors, injected from main (the backup manager and the
	// engine own them): RestoreApp restores a recorded backup through its
	// normal door; Deployed says whether an app has been deployed here;
	// DeployRestored deploys a delegated app with an artifact staged for
	// its first boot; Deploy deploys an app as it is.
	RestoreApp func(ctx context.Context, backupID int64) error
	Deployed   func(name string) bool
	// TestRestore proves a delegated artifact's dump restores (the backup
	// manager's scratch-database check); nil skips that half of a verify.
	TestRestore    func(ctx context.Context, appName, artifactPath, manifest string) error
	DeployRestored func(ctx context.Context, name, artifactPath string) error
	Deploy         func(ctx context.Context, name string) error
	// FilesRoot is the Files root; FilesJobs, when set, is where Files
	// restores run so the Files tray shows them; FilesRestored is called
	// after files land so the index catches up.
	FilesRoot     func() string
	FilesJobs     *jobs.Jobs
	FilesRestored func()

	ringMu  sync.Mutex
	rings   map[string]*Keyring
	alerts  notify.Throttle
	indexes indexCache
	// stalenessAt is when SyncLoop last ran the staleness check; only the
	// loop's goroutine touches it.
	stalenessAt time.Time
}

// The credential pair lives in the vault as system secrets, keyed by the
// target's name — the NAS password pattern.
func KeyIDSecret(target string) string  { return "offsite_" + target + "_key_id" }
func SecretSecret(target string) string { return "offsite_" + target + "_secret" }

var (
	ErrNoPassphrase = errors.New("set a recovery-kit passphrase first (Settings → Storage → Recovery kit) — it is the key to the locker")
	ErrLockerLocked = errors.New("a locker already exists at this prefix but does not open with this passphrase — choose a different prefix, or the passphrase that locker was created with")
)

// TargetSpec is what the operator supplies about a target.
type TargetSpec struct {
	Name       string  `json:"name"`
	Provider   string  `json:"provider"`
	Endpoint   string  `json:"endpoint"`
	Region     string  `json:"region"`
	Bucket     string  `json:"bucket"`
	Prefix     string  `json:"prefix"`
	PathStyle  bool    `json:"pathStyle"`
	PricePerTB float64 `json:"pricePerTb"`
	MaxMbps    int     `json:"maxMbps"`
	Window     string  `json:"window"`
	// LockerID picks one locker when the prefix holds several; "" adopts
	// the only one or creates a new one.
	LockerID string `json:"lockerId"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	windowRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$`)
)

func (spec *TargetSpec) normalize() error {
	spec.Name = strings.TrimSpace(strings.ToLower(spec.Name))
	spec.Endpoint = strings.TrimRight(strings.TrimSpace(spec.Endpoint), "/")
	spec.Region = strings.TrimSpace(spec.Region)
	spec.Bucket = strings.TrimSpace(spec.Bucket)
	spec.Prefix = strings.Trim(strings.TrimSpace(spec.Prefix), "/")
	spec.Window = strings.TrimSpace(spec.Window)
	spec.LockerID = strings.TrimSpace(spec.LockerID)
	if !nameRe.MatchString(spec.Name) {
		return errors.New("name: lowercase letters, digits and dashes, up to 40 characters")
	}
	if spec.Provider == "" {
		spec.Provider = "s3"
	}
	if PresetByID(spec.Provider) == nil {
		return fmt.Errorf("unknown provider %q", spec.Provider)
	}
	u, err := url.Parse(spec.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.Contains(spec.Endpoint, "<") {
		return errors.New("endpoint must be the service URL, like https://s3.us-west-004.backblazeb2.com")
	}
	if spec.Bucket == "" || strings.ContainsAny(spec.Bucket, "/ ") {
		return errors.New("bucket name is required")
	}
	if strings.Contains(spec.Prefix, "//") || strings.ContainsAny(spec.Prefix, " \\") {
		return errors.New("prefix: a path inside the bucket, like backups/house")
	}
	if spec.PricePerTB < 0 || spec.PricePerTB > 1000 {
		return errors.New("price per TB must be between 0 and 1000")
	}
	if spec.MaxMbps < 0 || spec.MaxMbps > 100000 {
		return errors.New("bandwidth cap must be 0 (off) or a number of Mbit/s")
	}
	if spec.Window != "" && !windowRe.MatchString(spec.Window) {
		return errors.New("upload window must look like 01:00-07:00 (UTC), or be empty")
	}
	if spec.LockerID != "" && len(spec.LockerID) != 2*lockerIDLen {
		return errors.New("locker id must be 32 hex characters")
	}
	return nil
}

func (m *Manager) serverName() string {
	if m.ServerName != nil {
		if n := strings.TrimSpace(m.ServerName()); n != "" {
			return n
		}
	}
	return "fog"
}

func (m *Manager) passphrase() (string, error) {
	if m.Passphrase == nil {
		return "", ErrNoPassphrase
	}
	p := m.Passphrase()
	if p == "" {
		return "", ErrNoPassphrase
	}
	return p, nil
}

func (m *Manager) secrets() vault.Secrets { return vault.Secrets{Store: m.Store, Vault: m.Vault} }

// credentials reads a target's key pair from the vault.
func (m *Manager) credentials(name string) (keyID, secret string, err error) {
	if keyID, err = m.secrets().Get(0, KeyIDSecret(name)); err == nil {
		secret, err = m.secrets().Get(0, SecretSecret(name))
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", "", fmt.Errorf("target %s has no stored credentials: %w", name, err)
	}
	if err != nil {
		return "", "", err
	}
	return keyID, secret, nil
}

func (m *Manager) storeCredentials(name, keyID, secret string) error {
	if err := m.secrets().Put(0, KeyIDSecret(name), keyID); err != nil {
		return err
	}
	return m.secrets().Put(0, SecretSecret(name), secret)
}

func (m *Manager) clientFor(spec TargetSpec, keyID, secret string) (*objstore.Client, error) {
	return objstore.New(objstore.Config{Endpoint: spec.Endpoint, Region: spec.Region, Bucket: spec.Bucket,
		AccessKey: keyID, SecretKey: secret, PathStyle: spec.PathStyle, HTTPClient: m.HTTPClient})
}

func specOf(t *store.OffsiteTarget) TargetSpec {
	return TargetSpec{Name: t.Name, Provider: t.Provider, Endpoint: t.Endpoint, Region: t.Region, Bucket: t.Bucket,
		Prefix: t.Prefix, PathStyle: t.PathStyle, PricePerTB: t.PricePerTB, MaxMbps: t.MaxMbps, Window: t.Window, LockerID: t.LockerID}
}

// Client opens a bucket client for a stored target.
func (m *Manager) Client(t *store.OffsiteTarget) (*objstore.Client, error) {
	keyID, secret, err := m.credentials(t.Name)
	if err != nil {
		return nil, err
	}
	return m.clientFor(specOf(t), keyID, secret)
}

// Layout is the target's locker address.
func LayoutOf(t *store.OffsiteTarget) Layout { return Layout{Prefix: t.Prefix, ID: t.LockerID} }

// resolveLocker finds or creates the locker a target writes to, and
// returns its opened ring. The nightly door is the vault wrap; a locker
// that lacks one (created by another machine, or restored from a kit on
// a new box) is opened with the passphrase and rewrapped so the next
// open is free.
func (m *Manager) resolveLocker(ctx context.Context, c *objstore.Client, prefix, want string) (Layout, *Keyring, error) {
	ids, err := Discover(ctx, c, prefix)
	if err != nil {
		return Layout{}, nil, fmt.Errorf("listing lockers: %w", err)
	}
	id := want
	if id == "" {
		switch len(ids) {
		case 0:
			return m.createLocker(ctx, c, prefix)
		case 1:
			id = ids[0]
		default:
			return Layout{}, nil, fmt.Errorf("this prefix holds %d lockers (%s) — pick one by id, or use a prefix of its own", len(ids), strings.Join(ids, ", "))
		}
	}
	layout := Layout{Prefix: prefix, ID: id}
	lk, err := FetchLocker(ctx, c, layout)
	if err != nil {
		if want != "" && errors.Is(err, objstore.ErrNotFound) {
			return Layout{}, nil, fmt.Errorf("no locker %s under this prefix", want)
		}
		return Layout{}, nil, err
	}
	// A locker fog has already upgraded stays bound even if the marker has
	// been deleted from locker.json: fog's own record is authoritative, so a
	// rollback to old-format wraps is refused rather than accepted.
	m.enforceBound(lk)
	kr, err := lk.OpenWithMaster(m.Vault)
	if err == nil {
		if err := m.upgradeWraps(ctx, c, layout, lk, kr); err != nil {
			m.Log.Warn("offsite: upgrading locker key wraps", "locker", lk.ID, "err", err)
		}
		return layout, kr, nil
	}
	if !errors.Is(err, ErrNoMasterWrap) {
		return Layout{}, nil, err
	}
	pass, err := m.passphrase()
	if err != nil {
		return Layout{}, nil, err
	}
	kr, err = lk.OpenWithPassphrase(pass)
	if err != nil {
		if errors.Is(err, ErrWrongPassphrase) {
			return Layout{}, nil, ErrLockerLocked
		}
		return Layout{}, nil, err
	}
	// Add this machine's wrap. Same passphrase, so the KDF salt is kept
	// stable — only the master wrap is new. Every wrap is rewritten in the
	// locker-bound form, so this also completes the format upgrade.
	if err := lk.wrapAll(kr, pass, m.Vault); err != nil {
		return Layout{}, nil, err
	}
	if err := m.putLocker(ctx, c, layout, lk); err != nil {
		return Layout{}, nil, err
	}
	m.markBound(lk.ID)
	return layout, kr, nil
}

// boundSetting is fog's own record that a locker's wraps are locker-bound.
func boundSetting(lockerID string) string { return "offsite_locker_bound_" + lockerID }

// markBound records that a locker has been upgraded to bound wraps.
func (m *Manager) markBound(lockerID string) {
	if m.Store != nil {
		m.Store.SetSetting(boundSetting(lockerID), "1")
	}
}

// enforceBound applies fog's own record to a locker.json just read: if fog
// upgraded this locker, legacy wraps are refused whatever the file now says.
// Reports whether the file had lost its marker (a rollback).
func (m *Manager) enforceBound(lk *Locker) (rolledBack bool) {
	if m.Store == nil {
		return false
	}
	if v, _ := m.Store.GetSetting(boundSetting(lk.ID)); v != "1" {
		return false
	}
	if !lk.Bound() {
		lk.WrapFormat = WrapFormatBound
		return true
	}
	return false
}

// upgradeWraps rewrites a legacy-format locker's wraps in the locker-bound
// form and records it, on the first open after fog gained the binding —
// no operator action needed, as long as fog holds the keys. The passphrase
// wraps are rewritten under the same salt and cost, so the passphrase must
// be fog's stored one and must open the existing passphrase wraps first:
// otherwise the rewrite could lock the operator out of fresh-machine
// recovery, and the upgrade waits instead. Other machines' vault wraps are
// carried over; if they are legacy they are refused afterwards, and that
// machine falls back to the passphrase once and rewraps its own.
func (m *Manager) upgradeWraps(ctx context.Context, c *objstore.Client, layout Layout, lk *Locker, kr *Keyring) error {
	if lk.Bound() {
		m.markBound(lk.ID)
		return nil
	}
	pass, err := m.passphrase()
	if err != nil {
		return nil // nothing to rewrap the passphrase door with yet
	}
	if _, err := lk.OpenWithPassphrase(pass); err != nil {
		return fmt.Errorf("the stored passphrase does not open this locker's passphrase wraps; leaving them for a passphrase change: %w", err)
	}
	var foreign []KeyWrap
	for _, w := range lk.Keys {
		if _, mine := unwrapMaster(m.Vault, lk.ID, w, true); w.WrappedBy == "master" && !mine {
			foreign = append(foreign, w)
		}
	}
	if err := lk.wrapAll(kr, pass, m.Vault); err != nil {
		return err
	}
	lk.Keys = append(lk.Keys, foreign...)
	if err := m.putLocker(ctx, c, layout, lk); err != nil {
		return err
	}
	m.markBound(lk.ID)
	m.Log.Info("offsite: locker key wraps upgraded to the locker-bound format", "locker", lk.ID)
	return nil
}

func (m *Manager) createLocker(ctx context.Context, c *objstore.Client, prefix string) (Layout, *Keyring, error) {
	pass, err := m.passphrase()
	if err != nil {
		return Layout{}, nil, err
	}
	lk, kr, err := CreateLocker(m.serverName(), pass, m.Vault)
	if err != nil {
		return Layout{}, nil, err
	}
	layout := Layout{Prefix: prefix, ID: lk.ID}
	if err := m.putLocker(ctx, c, layout, lk); err != nil {
		return Layout{}, nil, err
	}
	m.markBound(lk.ID)
	m.Log.Info("offsite locker created", "id", lk.ID, "bucket", c.Bucket(), "prefix", prefix)
	return layout, kr, nil
}

func (m *Manager) putLocker(ctx context.Context, c *objstore.Client, layout Layout, lk *Locker) error {
	raw, err := lk.Marshal()
	if err != nil {
		return err
	}
	if _, err := c.PutBytes(ctx, layout.LockerJSON(), raw, objstore.PutOptions{ContentType: "application/json"}); err != nil {
		return fmt.Errorf("writing locker.json: %w", err)
	}
	return nil
}

// NoticeRewrapFailed is up while some locker still answers only to the
// previous passphrase.
const NoticeRewrapFailed = "offsite-rewrap-failed"

// PassphraseChanged rewraps every target's locker under the current
// passphrase. The kit passphrase is the one secret that opens a locker on
// a fresh machine; without this the nightly door (the vault wrap) keeps
// working while recovery quietly needs the passphrase from before the
// change. Opens through the vault, so the old passphrase is not needed.
func (m *Manager) PassphraseChanged(ctx context.Context) error {
	pass, err := m.passphrase()
	if err != nil {
		return err
	}
	targets, err := m.Store.ListOffsiteTargets()
	if err != nil {
		return err
	}
	var errs []error
	for i := range targets {
		if err := m.rewrapTarget(ctx, &targets[i], pass); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", targets[i].Name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		m.Store.CreateNotice(NoticeRewrapFailed, "", "An offsite locker still opens only with the old passphrase",
			"Changing the recovery-kit passphrase could not rewrap every locker ("+err.Error()+"). Recovery on a new machine would need the previous passphrase; set the passphrase again once the target is reachable.")
		return err
	}
	m.Store.DismissNoticeByKind(NoticeRewrapFailed, "")
	return nil
}

func (m *Manager) rewrapTarget(ctx context.Context, t *store.OffsiteTarget, pass string) error {
	c, err := m.Client(t)
	if err != nil {
		return err
	}
	layout := LayoutOf(t)
	lk, err := FetchLocker(ctx, c, layout)
	if err != nil {
		return err
	}
	m.enforceBound(lk)
	kr, err := lk.OpenWithMaster(m.Vault)
	if err != nil {
		return err
	}
	// Rewrap replaces every wrap; other machines' vault wraps are still
	// good ciphertexts of the same roots, so carry them over.
	var foreign []KeyWrap
	for _, w := range lk.Keys {
		if _, mine := unwrapMaster(m.Vault, lk.ID, w, true); w.WrappedBy == "master" && !mine {
			foreign = append(foreign, w)
		}
	}
	if err := lk.Rewrap(kr, pass, m.Vault); err != nil {
		return err
	}
	lk.Keys = append(lk.Keys, foreign...)
	if err := m.putLocker(ctx, c, layout, lk); err != nil {
		return err
	}
	m.markBound(lk.ID)
	return nil
}

// Keyring returns a target's opened ring, cached per process.
func (m *Manager) Keyring(ctx context.Context, t *store.OffsiteTarget) (*Keyring, error) {
	m.ringMu.Lock()
	if kr := m.rings[t.Name]; kr != nil && kr.LockerID == t.LockerID {
		m.ringMu.Unlock()
		return kr, nil
	}
	m.ringMu.Unlock()
	c, err := m.Client(t)
	if err != nil {
		return nil, err
	}
	_, kr, err := m.resolveLocker(ctx, c, t.Prefix, t.LockerID)
	if err != nil {
		return nil, err
	}
	m.ringMu.Lock()
	if m.rings == nil {
		m.rings = map[string]*Keyring{}
	}
	m.rings[t.Name] = kr
	m.ringMu.Unlock()
	return kr, nil
}

func (m *Manager) forgetRing(name string) {
	m.ringMu.Lock()
	delete(m.rings, name)
	m.ringMu.Unlock()
}

// Reader is the read side of a stored target.
func (m *Manager) Reader(ctx context.Context, t *store.OffsiteTarget) (*Reader, error) {
	c, err := m.Client(t)
	if err != nil {
		return nil, err
	}
	kr, err := m.Keyring(ctx, t)
	if err != nil {
		return nil, err
	}
	latest, _ := m.Store.GetSetting(latestIndexSetting(t))
	return &Reader{Client: c, Layout: LayoutOf(t), Keys: kr, LatestIndex: latest}, nil
}

// AddTarget validates, probes, finds or creates the locker, and stores
// the target. A bucket that cannot be written or read is refused outright
// (ErrProbeFailed); a low grade is stored and reported, never refused.
func (m *Manager) AddTarget(ctx context.Context, spec TargetSpec, keyID, secret string) (*store.OffsiteTarget, *ProbeResult, error) {
	if err := spec.normalize(); err != nil {
		return nil, nil, err
	}
	if keyID == "" || secret == "" {
		return nil, nil, errors.New("key id and secret are required")
	}
	if _, err := m.passphrase(); err != nil {
		return nil, nil, err
	}
	if _, err := m.Store.OffsiteTargetByName(spec.Name); err == nil {
		return nil, nil, fmt.Errorf("a target named %s already exists", spec.Name)
	}
	c, err := m.clientFor(spec, keyID, secret)
	if err != nil {
		return nil, nil, err
	}
	// Probe before the locker: a key that cannot write has no business
	// creating one.
	probe, err := Probe(ctx, c, Layout{Prefix: spec.Prefix, ID: "probe"}, IsB2(spec.Endpoint))
	if err != nil {
		return nil, probe, err
	}
	layout, kr, err := m.resolveLocker(ctx, c, spec.Prefix, spec.LockerID)
	if err != nil {
		return nil, probe, err
	}
	if err := m.storeCredentials(spec.Name, keyID, secret); err != nil {
		return nil, probe, err
	}
	pj, _ := json.Marshal(probe)
	row := store.OffsiteTarget{Name: spec.Name, Provider: spec.Provider, Endpoint: spec.Endpoint, Region: spec.Region,
		Bucket: spec.Bucket, Prefix: spec.Prefix, PathStyle: spec.PathStyle, LockerID: layout.ID, Role: "active",
		Grade: probe.Grade, Probe: string(pj), ProbedAt: probe.At, PricePerTB: spec.PricePerTB, MaxMbps: spec.MaxMbps, Window: spec.Window}
	if _, err := m.Store.CreateOffsiteTarget(row); err != nil {
		m.Store.DeleteSecret(0, KeyIDSecret(spec.Name))
		m.Store.DeleteSecret(0, SecretSecret(spec.Name))
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, probe, fmt.Errorf("a target named %s already exists", spec.Name)
		}
		return nil, probe, err
	}
	m.ringMu.Lock()
	if m.rings == nil {
		m.rings = map[string]*Keyring{}
	}
	m.rings[spec.Name] = kr
	m.ringMu.Unlock()
	t, err := m.Store.OffsiteTargetByName(spec.Name)
	return t, probe, err
}

// UpdateTarget changes how a target is reached or its policy. Credentials
// are replaced only when both are given. The bucket is re-probed, and if
// the bucket or prefix moved the locker is resolved again.
func (m *Manager) UpdateTarget(ctx context.Context, name string, spec TargetSpec, keyID, secret string) (*store.OffsiteTarget, *ProbeResult, error) {
	cur, err := m.Store.OffsiteTargetByName(name)
	if err != nil {
		return nil, nil, err
	}
	spec.Name = name
	if err := spec.normalize(); err != nil {
		return nil, nil, err
	}
	if keyID == "" || secret == "" {
		if keyID, secret, err = m.credentials(name); err != nil {
			return nil, nil, err
		}
	}
	c, err := m.clientFor(spec, keyID, secret)
	if err != nil {
		return nil, nil, err
	}
	probe, err := Probe(ctx, c, Layout{Prefix: spec.Prefix, ID: "probe"}, IsB2(spec.Endpoint))
	if err != nil {
		return nil, probe, err
	}
	lockerID := cur.LockerID
	moved := spec.Bucket != cur.Bucket || spec.Prefix != cur.Prefix || spec.Endpoint != cur.Endpoint || (spec.LockerID != "" && spec.LockerID != cur.LockerID)
	if moved {
		layout, _, err := m.resolveLocker(ctx, c, spec.Prefix, spec.LockerID)
		if err != nil {
			return nil, probe, err
		}
		lockerID = layout.ID
	}
	if err := m.storeCredentials(name, keyID, secret); err != nil {
		return nil, probe, err
	}
	if err := m.Store.UpdateOffsiteTarget(name, store.OffsiteTarget{Provider: spec.Provider, Endpoint: spec.Endpoint, Region: spec.Region,
		Bucket: spec.Bucket, Prefix: spec.Prefix, PathStyle: spec.PathStyle, PricePerTB: spec.PricePerTB, MaxMbps: spec.MaxMbps, Window: spec.Window}); err != nil {
		return nil, probe, err
	}
	if lockerID != cur.LockerID {
		if err := m.Store.SetOffsiteTargetLocker(name, lockerID); err != nil {
			return nil, probe, err
		}
	}
	pj, _ := json.Marshal(probe)
	m.Store.SetOffsiteTargetProbe(name, probe.Grade, string(pj))
	m.forgetRing(name)
	t, err := m.Store.OffsiteTargetByName(name)
	return t, probe, err
}

// Reprobe re-grades a stored target — after the operator changed the
// bucket's settings on the provider's side.
func (m *Manager) Reprobe(ctx context.Context, name string) (*store.OffsiteTarget, *ProbeResult, error) {
	t, err := m.Store.OffsiteTargetByName(name)
	if err != nil {
		return nil, nil, err
	}
	c, err := m.Client(t)
	if err != nil {
		return nil, nil, err
	}
	probe, err := Probe(ctx, c, LayoutOf(t), IsB2(t.Endpoint))
	if probe != nil {
		pj, _ := json.Marshal(probe)
		grade := probe.Grade
		if err != nil {
			grade = ""
		}
		m.Store.SetOffsiteTargetProbe(name, grade, string(pj))
	}
	if err != nil {
		return t, probe, err
	}
	t, err = m.Store.OffsiteTargetByName(name)
	return t, probe, err
}

// DeleteTarget forgets a target: the row, its copy records and its
// credentials. The bucket is untouched — fog holds no right to empty it,
// by design.
func (m *Manager) DeleteTarget(name string) error {
	if _, err := m.Store.OffsiteTargetByName(name); err != nil {
		return err
	}
	if err := m.Store.DeleteOffsiteTarget(name); err != nil {
		return err
	}
	m.Store.DeleteSecret(0, KeyIDSecret(name))
	m.Store.DeleteSecret(0, SecretSecret(name))
	m.Store.DeleteSetting(housekeepingSetting(name))
	m.forgetRing(name)
	return nil
}

// ParseProbe decodes a stored probe; nil when none.
func ParseProbe(raw string) *ProbeResult {
	if raw == "" {
		return nil
	}
	var p ProbeResult
	if json.Unmarshal([]byte(raw), &p) != nil {
		return nil
	}
	return &p
}
