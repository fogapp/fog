package chunkstore

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/chacha20poly1305"

	"gitea.lan/matthew/fog/internal/fsx"
)

// Format is the store's on-disk version (docs/store-contract.md).
const Format = "fog-store/1"

// DefaultPackSize is where a pack is closed: big enough that a terabyte is
// tens of thousands of files rather than millions (NAS shares and object
// stores both prefer few large objects), small enough that a restore of one
// app pulls little it does not need.
const DefaultPackSize = 32 << 20

// DefaultGrace is how long a pack must be found unreferenced, across two
// collections, before it is deleted.
const DefaultGrace = 24 * time.Hour

var (
	// ErrLocked is an encrypted store opened without its key.
	ErrLocked = errors.New("this backup store is encrypted and the key to open it is not available")
	// ErrWrongKey is an encrypted store opened with a key that is not its own.
	ErrWrongKey = errors.New("the key given does not open this backup store")
	// ErrNotFound is a snapshot or chunk the store does not hold.
	ErrNotFound = errors.New("not in the backup store")
	// ErrCorrupt is stored bytes that do not match their name.
	ErrCorrupt = errors.New("backup store data does not match its checksum")
)

// config is store.json.
type config struct {
	Format    string `json:"format"`
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Params    struct {
		Min int `json:"min"`
		Avg int `json:"avg"`
		Max int `json:"max"`
	} `json:"params"`
	PackSize  int    `json:"pack_size"`
	Encrypted bool   `json:"encrypted"`
	Secret    string `json:"secret,omitempty"`
	KeyCheck  string `json:"key_check,omitempty"`
}

// Options open or create a store.
type Options struct {
	// IndexPath is the chunk index, a SQLite file. It belongs on local disk
	// beside fog's own database, never on the share the store lives on
	// (SQLite over NFS/SMB is not safe); it is a cache, rebuilt from the
	// packs when missing or stale.
	IndexPath string
	// Create makes a new store when dir holds none.
	Create bool
	// Key encrypts a new store and opens an encrypted one: 32 bytes the
	// caller keeps (fog wraps it in its vault). Nil creates a plain store;
	// a plain store opens whatever Key says.
	Key []byte
	// Params and PackSize apply to a new store only; an existing store
	// keeps what it was created with.
	Params   Params
	PackSize int
	// Grace overrides DefaultGrace (tests).
	Grace time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Store is one opened store. Safe for concurrent use: any number of
// readers, any number of writers (each Writer fills its own packs), and
// collection, which waits for writers and keeps new ones out while it runs.
type Store struct {
	dir   string
	cfg   config
	idKey []byte
	gear  *Gear
	aead  interface {
		NonceSize() int
		Overhead() int
		Seal(dst, nonce, plaintext, ad []byte) []byte
		Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
	}
	params   Params
	packSize int
	grace    time.Duration
	now      func() time.Time

	enc *zstd.Encoder
	dec *zstd.Decoder
	ix  *index

	// gate keeps collection and writers apart: a Writer holds it shared
	// from Begin to Commit, collection holds it exclusively. Readers need
	// nothing — a pack a reader might still use is never deleted before a
	// grace period has passed twice.
	gate sync.RWMutex
}

// Open opens the store in dir, creating it when opt.Create says so.
func Open(dir string, opt Options) (*Store, error) {
	if opt.IndexPath == "" {
		return nil, errors.New("chunkstore: an index path is required")
	}
	s := &Store{dir: dir, now: opt.Now, grace: opt.Grace}
	if s.now == nil {
		s.now = time.Now
	}
	if s.grace <= 0 {
		s.grace = DefaultGrace
	}
	raw, err := os.ReadFile(s.path("store.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist) && opt.Create:
		if err := s.create(opt); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.cfg); err != nil {
			return nil, fmt.Errorf("store.json: %w", err)
		}
		if s.cfg.Format != Format {
			return nil, fmt.Errorf("store.json declares %q; this fog reads %s", s.cfg.Format, Format)
		}
	}
	if err := s.keys(opt.Key); err != nil {
		return nil, err
	}
	s.params = Params{Min: s.cfg.Params.Min, Avg: s.cfg.Params.Avg, Max: s.cfg.Params.Max}
	if err := s.params.valid(); err != nil {
		return nil, fmt.Errorf("store.json: %w", err)
	}
	s.packSize = s.cfg.PackSize
	if s.packSize <= 0 {
		s.packSize = DefaultPackSize
	}
	for _, d := range []string{"packs", "snapshots", "tmp"} {
		if err := os.MkdirAll(s.path(d), 0o700); err != nil {
			return nil, err
		}
	}
	if s.enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault)); err != nil {
		return nil, err
	}
	if s.dec, err = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(uint64(s.params.Max)*2)); err != nil {
		return nil, err
	}
	if s.ix, err = openIndex(opt.IndexPath, s.cfg.ID); err != nil {
		return nil, err
	}
	if err := s.reconcile(); err != nil {
		s.ix.close()
		return nil, err
	}
	s.sweepTmp()
	return s, nil
}

// Close releases the index, after any collection or writer still running
// has finished.
func (s *Store) Close() error {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.enc.Close()
	s.dec.Close()
	return s.ix.close()
}

// ID is the store's identity, minted at creation.
func (s *Store) ID() string { return s.cfg.ID }

// Dir is where the store lives.
func (s *Store) Dir() string { return s.dir }

// Encrypted reports whether the store seals what it holds.
func (s *Store) Encrypted() bool { return s.cfg.Encrypted }

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.dir}, parts...)...)
}

func (s *Store) create(opt Options) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	// A directory already holding something that is not a store is not
	// ours to fill: an app's backups folder with the same name, say.
	if ents, err := os.ReadDir(s.dir); err == nil {
		for _, e := range ents {
			switch e.Name() {
			case "packs", "snapshots", "tmp":
			default:
				return fmt.Errorf("%s already holds %s and has no store.json — refusing to make a store there", s.dir, e.Name())
			}
		}
	}
	p := opt.Params
	if p == (Params{}) {
		p = DefaultParams
	}
	if err := p.valid(); err != nil {
		return err
	}
	var id [16]byte
	rand.Read(id[:])
	c := config{Format: Format, ID: hex.EncodeToString(id[:]), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	c.Params.Min, c.Params.Avg, c.Params.Max = p.Min, p.Avg, p.Max
	c.PackSize = opt.PackSize
	if c.PackSize <= 0 {
		c.PackSize = DefaultPackSize
	}
	if opt.Key != nil {
		if len(opt.Key) != 32 {
			return errors.New("chunkstore: the key must be 32 bytes")
		}
		c.Encrypted = true
		c.KeyCheck = keyCheck(opt.Key, c.ID)
	} else {
		secret := make([]byte, 32)
		rand.Read(secret)
		c.Secret = base64.StdEncoding.EncodeToString(secret)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteFileExclusive(s.path("store.json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	syncDir(s.dir)
	s.cfg = c
	return nil
}

func derive(secret []byte, info string) []byte {
	k, err := hkdf.Key(sha256.New, secret, nil, info, 32)
	if err != nil {
		panic(err) // only for lengths past 255*32
	}
	return k
}

func keyCheck(key []byte, id string) string {
	m := hmac.New(sha256.New, derive(key, "fog-store/1 key check"))
	m.Write([]byte(id))
	return hex.EncodeToString(m.Sum(nil))
}

// keys derives everything from the store's secret: the plain store's own
// (in store.json — whoever can read the share can read the backups anyway)
// or the caller's key for an encrypted one, which store.json never holds.
func (s *Store) keys(key []byte) error {
	var secret []byte
	if s.cfg.Encrypted {
		if key == nil {
			return ErrLocked
		}
		if !hmac.Equal([]byte(keyCheck(key, s.cfg.ID)), []byte(s.cfg.KeyCheck)) {
			return ErrWrongKey
		}
		secret = key
	} else {
		b, err := base64.StdEncoding.DecodeString(s.cfg.Secret)
		if err != nil || len(b) != 32 {
			return errors.New("store.json: secret is not 32 bytes of base64")
		}
		secret = b
	}
	s.idKey = derive(secret, "fog-store/1 chunk id")
	s.gear = NewGear(derive(secret, "fog-store/1 gear"))
	if s.cfg.Encrypted {
		a, err := chacha20poly1305.NewX(derive(secret, "fog-store/1 data"))
		if err != nil {
			return err
		}
		s.aead = a
	}
	return nil
}

// ChunkID names a chunk: HMAC-SHA256 under the store's id key, so names say
// nothing about content to anyone without the key, and a chunk read back is
// verified by recomputing its name.
type ChunkID [32]byte

func (id ChunkID) String() string { return hex.EncodeToString(id[:]) }

// ParseChunkID reads the hex form.
func ParseChunkID(s string) (ChunkID, error) {
	var id ChunkID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, fmt.Errorf("bad chunk id %q", s)
	}
	copy(id[:], b)
	return id, nil
}

func (s *Store) chunkID(b []byte) ChunkID {
	m := hmac.New(sha256.New, s.idKey)
	m.Write(b)
	var id ChunkID
	m.Sum(id[:0])
	return id
}

// seal and open encrypt a record under the data key, bound to ad.
func (s *Store) seal(plain, ad []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plain, ad)
}

func (s *Store) open(sealed, ad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, ErrCorrupt
	}
	b, err := s.aead.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return nil, ErrCorrupt
	}
	return b, nil
}

// TempDir makes a scratch directory inside the store's tmp/ — on the same
// disk as the backups, which has room for what they hold — for a caller to
// rebuild a snapshot into. Anything left behind is swept on a later open.
func (s *Store) TempDir(pattern string) (string, error) {
	return os.MkdirTemp(s.path("tmp"), pattern)
}

// sweepTmp removes what a crashed writer left in tmp/.
func (s *Store) sweepTmp() {
	ents, _ := os.ReadDir(s.path("tmp"))
	for _, e := range ents {
		if info, err := e.Info(); err == nil && s.now().Sub(info.ModTime()) > 6*time.Hour {
			os.RemoveAll(s.path("tmp", e.Name()))
		}
	}
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
