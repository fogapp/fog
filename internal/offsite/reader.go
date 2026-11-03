package offsite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/objstore"
)

// Reader is the read side of a locker: what the restore doors, the CLI
// and the verifier share. It needs a bucket client, a layout, and an
// opened key ring — no fog database, no local state.
type Reader struct {
	Client *objstore.Client
	Layout Layout
	Keys   *Keyring
	// LatestIndex is the index key this fog last wrote, when it is the
	// fog reading (Manager.Reader): tried first, so a bucket writer who
	// deletes or spoils it is noticed (IndexInfo.FellBack) rather than
	// quietly served an older one. Empty on a fresh machine.
	LatestIndex string
}

// maxDocument bounds the sealed JSON documents (rows, manifests, index)
// a reader will hold in memory. A Files manifest of a million files is
// ~200 MB; anything past this is not a document, it is a mistake.
const maxDocument = 512 << 20

// Discover lists the locker ids under a prefix.
func Discover(ctx context.Context, c *objstore.Client, prefix string) ([]string, error) {
	ids, err := c.ListPrefixes(ctx, LockersRoot(prefix))
	// The probe's scratch space sits beside the lockers; a test object
	// the key could not hide must not pass for one.
	return slices.DeleteFunc(ids, func(id string) bool { return id == "probe" }), err
}

// FetchLocker reads and parses locker.json.
func FetchLocker(ctx context.Context, c *objstore.Client, l Layout) (*Locker, error) {
	data, _, err := c.GetBytes(ctx, l.LockerJSON(), 1<<20)
	if err != nil {
		if errors.Is(err, objstore.ErrNotFound) {
			return nil, fmt.Errorf("no locker at %s: %w", l.Root(), err)
		}
		return nil, err
	}
	lk, err := ParseLocker(data)
	if err != nil {
		return nil, err
	}
	if lk.ID != l.ID {
		return nil, fmt.Errorf("locker.json at %s claims id %s", l.Root(), lk.ID)
	}
	return lk, nil
}

// Kits lists the recovery kits in the locker, newest first.
func (r *Reader) Kits(ctx context.Context) ([]IndexKit, error) {
	var kits []IndexKit
	err := r.Client.List(ctx, r.Layout.KitsPrefix(), func(o objstore.Object) error {
		name := strings.TrimPrefix(o.Key, r.Layout.KitsPrefix())
		// Only well-formed kit names. A planted "zzzz.enc" would otherwise
		// sort newest and replay; it is ignored, never trusted.
		if !backup.KitObjectName(name) {
			return nil
		}
		kits = append(kits, IndexKit{Name: name, Bytes: o.Size, Stamp: StampOf(name), Received: o.LastModified})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(kits, func(i, j int) bool { return kits[i].Name > kits[j].Name })
	return kits, nil
}

// MaxKitBytes caps a recovery kit read from the bucket (backup.MaxKitBytes:
// sized from measured kits). A var so tests can shrink it.
var MaxKitBytes int64 = backup.MaxKitBytes

// kitReadStall and kitFetchCap bound one kit download; vars for tests.
var (
	kitReadStall = 30 * time.Second
	kitFetchCap  = 10 * time.Minute
)

type progressReader struct {
	r io.Reader
	n *atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n.Add(int64(n))
	return n, err
}

// FetchKit copies a kit's sealed bytes — the same .enc the box wrote — to
// dst, up to MaxKitBytes. Kits are not locker-sealed; DecryptKit opens them.
func (r *Reader) FetchKit(ctx context.Context, name string, dst io.Writer) (int64, error) {
	// Bounded in time as well as size: a bucket that trickles a kit must
	// not hold whoever is waiting on it. No byte for kitReadStall, or the
	// whole read past kitFetchCap, and it is given up.
	ctx, cancel := context.WithTimeout(ctx, kitFetchCap)
	defer cancel()
	rc, _, err := r.Client.Get(ctx, r.Layout.Kit(name), "")
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	var got atomic.Int64
	go func() {
		last, since := int64(-1), time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(kitReadStall / 4):
			}
			if n := got.Load(); n != last {
				last, since = n, time.Now()
			} else if time.Since(since) >= kitReadStall {
				cancel()
				return
			}
		}
	}()
	n, err := io.Copy(dst, &progressReader{r: io.LimitReader(rc, MaxKitBytes+1), n: &got})
	if err != nil {
		return n, err
	}
	if n > MaxKitBytes {
		return n, fmt.Errorf("recovery kit %q is larger than the %d-byte cap — refusing it", name, MaxKitBytes)
	}
	return n, nil
}

// Index reads the newest valid index document (IndexInfo, without the
// account of how it was chosen).
func (r *Reader) Index(ctx context.Context) (*Index, error) {
	ix, _, err := r.IndexInfo(ctx)
	return ix, err
}

// maxIndexTries bounds how many index documents IndexInfo opens: the sync
// keeps the newest few (indexKeep), so a reader never needs more, and a
// bucket writer cannot make every reader download and try a thousand.
const maxIndexTries = 6

// maxIndexDocument bounds one index document. An index lists app rows
// without their manifests and one line per Files snapshot — kilobytes for
// a busy fleet; this is room for tens of thousands of rows.
const maxIndexDocument = 32 << 20

// minSealedDocument is smaller than any sealed document: header, nonce and
// tag alone outweigh it.
const minSealedDocument = 32

// IndexChoice says which index IndexInfo chose and what it passed over.
type IndexChoice struct {
	Key string `json:"key"`
	// Newest is the newest well-formed index name the bucket lists.
	Newest string `json:"newest"`
	// FellBack is set when the chosen index is not the newest listed, or
	// not the one this fog recorded writing last: a newer one would not
	// open, or the recorded one is gone. What the reader shows may be out
	// of date, and someone may have tampered with the bucket.
	FellBack bool     `json:"fellBack"`
	Skipped  []string `json:"skipped,omitempty"`
}

// IndexInfo reads the newest index that opens, parses and names this
// locker. The index this fog recorded writing last (LatestIndex) is a
// floor, not a ceiling: a newer valid one is preferred (a machine restored
// from a kit carries the record of the night before the kit, and the night
// after it is newer), the recorded one is always tried whatever junk was
// planted ahead of it, and choosing anything older than it is a fall-back.
// A planted junk object ("index/zzzz", or one that will not open) must not
// break recovery, the setup preview or `fog-locker ls`: an object too small
// or too large is passed over unread, at most maxIndexTries are opened
// besides the recorded one, and the choice says whether it fell back.
// ErrNoIndex when none is usable.
func (r *Reader) IndexInfo(ctx context.Context) (*Index, IndexChoice, error) {
	var keys []string
	sizes := map[string]int64{}
	err := r.Client.List(ctx, r.Layout.IndexPrefix(), func(o objstore.Object) error {
		name := strings.TrimPrefix(o.Key, r.Layout.IndexPrefix())
		if StampOf(name) != "" && strings.HasSuffix(name, ".json") {
			keys = append(keys, o.Key)
			sizes[o.Key] = o.Size
		}
		return nil
	})
	if err != nil {
		return nil, IndexChoice{}, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var choice IndexChoice
	if len(keys) > 0 {
		choice.Newest = keys[0]
	}
	tries := 0
	for _, k := range keys {
		recorded := k == r.LatestIndex
		// An object too small to be sealed, or larger than any index, is
		// passed over without a download; only real attempts count, and
		// the recorded index is always attempted.
		if sizes[k] < minSealedDocument || sizes[k] > maxIndexDocument {
			choice.Skipped = append(choice.Skipped, k)
			continue
		}
		if !recorded {
			if tries >= maxIndexTries {
				if r.LatestIndex != "" && k > r.LatestIndex {
					continue // still reach the recorded one
				}
				break
			}
			tries++
		}
		var ix Index
		if err := r.document(ctx, k, &ix, maxIndexDocument); err == nil && ix.Format == Format && (ix.LockerID == "" || ix.LockerID == r.Layout.ID) {
			choice.Key = k
			if r.LatestIndex != "" {
				choice.FellBack = k < r.LatestIndex
			} else {
				choice.FellBack = k != choice.Newest
			}
			return &ix, choice, nil
		}
		choice.Skipped = append(choice.Skipped, k)
		r.logSkip(k)
	}
	if r.LatestIndex != "" {
		choice.FellBack = true
	}
	return nil, choice, ErrNoIndex
}

// logSkip is overridable in tests; by default it is quiet (a skipped index is
// expected noise during a tamper, caught by the locker re-check).
var logSkip = func(string) {}

func (r *Reader) logSkip(key string) { logSkip(key) }

var ErrNoIndex = errors.New("the locker has no index yet")

// Document opens a sealed JSON object into v.
func (r *Reader) Document(ctx context.Context, key string, v any) error {
	return r.document(ctx, key, v, maxDocument)
}

// document is Document holding at most limit bytes.
func (r *Reader) document(ctx context.Context, key string, v any, limit int64) error {
	sealed, _, err := r.Client.GetBytes(ctx, key, limit)
	if err != nil {
		return err
	}
	plain, err := r.Keys.OpenBytes(r.Layout.Name(key), sealed, limit)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if err := json.Unmarshal(plain, v); err != nil {
		return fmt.Errorf("%s: not valid JSON: %w", key, err)
	}
	return nil
}

// Open streams a sealed object's plaintext. The caller closes it; the
// final read reports ErrTruncated or ErrBadKey if the object is not whole.
func (r *Reader) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, _, err := r.Client.Get(ctx, key, "")
	if err != nil {
		return nil, err
	}
	pr, err := r.Keys.Open(rc, r.Layout.Name(key))
	if err != nil {
		rc.Close()
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &readCloser{Reader: pr, closer: rc}, nil
}

type readCloser struct {
	io.Reader
	closer io.Closer
}

func (rc *readCloser) Close() error { return rc.closer.Close() }

// FetchTo opens a sealed object into a file, hashing the plaintext as it
// lands. Written to a sibling temp file and renamed only after the last
// frame authenticated, so a partial download never sits where a whole
// one is expected.
func (r *Reader) FetchTo(ctx context.Context, key, dst string) (sha256hex string, n int64, err error) {
	src, err := r.Open(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	err = fsx.WriteAtomic(dst, 0o600, func(w io.Writer) error {
		var cerr error
		if n, cerr = io.Copy(io.MultiWriter(w, h), src); cerr != nil {
			return fmt.Errorf("fetching %s: %w", key, cerr)
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Hash reads a sealed object end to end — every frame authenticating —
// and returns its plaintext's digest and length, without keeping a byte.
func (r *Reader) Hash(ctx context.Context, key string) (sha256hex string, n int64, err error) {
	src, err := r.Open(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	h := sha256.New()
	if n, err = io.Copy(h, src); err != nil {
		return "", 0, fmt.Errorf("fetching %s: %w", key, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// AppRows lists an app's sealed rows in the locker, newest first, by
// listing (not the index) — the truth, for callers that need it.
func (r *Reader) AppRows(ctx context.Context, app string) ([]AppRow, error) {
	var keys []string
	err := r.Client.List(ctx, r.Layout.AppPrefix(app), func(o objstore.Object) error {
		if strings.HasSuffix(o.Key, ".json") {
			keys = append(keys, o.Key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	rows := make([]AppRow, 0, len(keys))
	for _, k := range keys {
		var row AppRow
		if err := r.Document(ctx, k, &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Bounds on IndexFromListing: rows read per app and in all, the size of
// one row document (a row with its manifest; a big Files-free app's is
// kilobytes), and the bytes read in all.
const (
	listingRowsPerApp = 60
	listingRowsTotal  = 2000
	maxRowDocument    = 8 << 20
	listingBudget     = 256 << 20
)

// IndexFromListing builds what an index would say from the bucket's
// listing and the sealed rows themselves: the fallback when no index is
// readable. A row that will not open is left out, not fatal; Files
// snapshots carry their stamps alone.
func (r *Reader) IndexFromListing(ctx context.Context) (*Index, error) {
	ix := &Index{Format: Format, LockerID: r.Layout.ID, Apps: map[string][]AppRow{}, Files: []IndexFiles{}}
	apps, err := r.Apps(ctx)
	if err != nil {
		return nil, err
	}
	// Bounded: a bucket writer can list as many objects as it likes. Only
	// the newest rows per app are read, each held to a row's size, and the
	// whole read to a budget.
	read, budget := 0, int64(listingBudget)
	for _, app := range apps {
		var keys []string
		sizes := map[string]int64{}
		if err := r.Client.List(ctx, r.Layout.AppPrefix(app), func(o objstore.Object) error {
			if strings.HasSuffix(o.Key, ".json") && StampOf(o.Key) != "" {
				keys = append(keys, o.Key)
				sizes[o.Key] = o.Size
			}
			return nil
		}); err != nil {
			return nil, err
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		perApp := 0
		for _, k := range keys {
			if perApp >= listingRowsPerApp || read >= listingRowsTotal || sizes[k] > maxRowDocument || sizes[k] > budget {
				continue
			}
			read++
			perApp++
			budget -= sizes[k]
			var row AppRow
			if r.document(ctx, k, &row, maxRowDocument) == nil && row.App == app && row.Stamp != "" {
				ix.Apps[app] = append(ix.Apps[app], row)
			}
		}
		sort.Slice(ix.Apps[app], func(i, j int) bool { return ix.Apps[app][i].Stamp > ix.Apps[app][j].Stamp })
	}
	stamps, err := r.FilesSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, st := range stamps {
		ix.Files = append(ix.Files, IndexFiles{Stamp: st})
	}
	ix.Kits, _ = r.Kits(ctx)
	return ix, nil
}

// Apps lists the app names present in the locker.
func (r *Reader) Apps(ctx context.Context) ([]string, error) {
	return r.Client.ListPrefixes(ctx, r.Layout.AppsPrefix())
}

// FilesSnapshots lists the Files snapshot stamps in the locker, newest
// first.
func (r *Reader) FilesSnapshots(ctx context.Context) ([]string, error) {
	var stamps []string
	err := r.Client.List(ctx, r.Layout.FilesSnapshotsPrefix(), func(o objstore.Object) error {
		if s := StampOf(o.Key); s != "" {
			stamps = append(stamps, s)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(stamps)))
	return stamps, nil
}
