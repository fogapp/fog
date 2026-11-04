package chunkstore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/klauspost/compress/zstd"
)

// Analyze measures what a backups directory would cost in a store,
// without writing anything: every snapshot fog holds is chunked as the
// store would chunk it, and only bytes not seen before count. It answers
// the question the store is built on — how much of each night is new —
// from the real backups rather than a guess.
//
// Two figures per series. AsIs chunks the files exactly as they are, which
// is what today's producers' artifacts would cost (their inner payloads are
// compressed, so little dedupes). Expanded opens what fog can open — the
// zstd and gzip archives inside artifacts and volume snapshots — and
// chunks the plain bytes, which approximates producers that stop
// compressing (fog-backup/2). pg_dump's custom format compresses inside
// its own container and is counted as is in both; a -Z0 dump dedupes
// better than this shows.

// AnalyzeOptions tunes a run.
type AnalyzeOptions struct {
	Params Params
	// Progress, when set, is told each snapshot directory as it starts.
	Progress func(series, stamp string)
}

// Figures is one way of counting a series.
type Figures struct {
	// Logical is every byte of every snapshot: what restoring all of them
	// would produce.
	Logical int64 `json:"logical"`
	// Unique is the plaintext of the chunks seen for the first time here.
	Unique int64 `json:"unique"`
	// Stored is Unique after the store's per-chunk zstd.
	Stored int64 `json:"stored"`
	// DBStored is the part of Stored that came from database dumps.
	DBStored int64 `json:"dbStored"`
	// Nightly is Stored added by each snapshot after the first, oldest
	// first — how much a night costs once the history exists.
	Nightly []int64 `json:"nightly"`
}

// SeriesReport is one app (or Drive) across its snapshots.
type SeriesReport struct {
	Series    string `json:"series"`
	Snapshots int    `json:"snapshots"`
	// OnDisk is what the snapshots occupy today, hardlinked files counted
	// once.
	OnDisk   int64   `json:"onDisk"`
	AsIs     Figures `json:"asIs"`
	Expanded Figures `json:"expanded"`
}

// Report is the whole directory.
type Report struct {
	Root   string         `json:"root"`
	Params Params         `json:"params"`
	Series []SeriesReport `json:"series"`
}

// Totals sums the series.
func (r *Report) Totals() SeriesReport {
	var t SeriesReport
	t.Series = "total"
	for _, s := range r.Series {
		t.Snapshots += s.Snapshots
		t.OnDisk += s.OnDisk
		add := func(dst *Figures, src Figures) {
			dst.Logical += src.Logical
			dst.Unique += src.Unique
			dst.Stored += src.Stored
			dst.DBStored += src.DBStored
		}
		add(&t.AsIs, s.AsIs)
		add(&t.Expanded, s.Expanded)
	}
	return t
}

// Median is the middle nightly cost, 0 with no nights.
func (f Figures) Median() int64 {
	if len(f.Nightly) == 0 {
		return 0
	}
	s := append([]int64(nil), f.Nightly...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// seen is one dedup universe: the whole directory shares it, as the store
// shares chunks across apps. Keys are truncated digests — this is a
// measurement, and 128 bits is plenty to count with.
type seen struct {
	set map[[16]byte]struct{}
	enc *zstd.Encoder
}

func newSeen() (*seen, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &seen{set: map[[16]byte]struct{}{}, enc: enc}, nil
}

// feed chunks r into f, counting what is new.
func (s *seen) feed(r io.Reader, p Params, f *Figures, db bool) (stored int64, err error) {
	c, err := NewChunker(r, nil, p)
	if err != nil {
		return 0, err
	}
	var dst []byte
	for {
		b, err := c.Next()
		if err == io.EOF {
			return stored, nil
		}
		if err != nil {
			return stored, err
		}
		f.Logical += int64(len(b))
		sum := sha256.Sum256(b)
		var k [16]byte
		copy(k[:], sum[:16])
		if _, ok := s.set[k]; ok {
			continue
		}
		s.set[k] = struct{}{}
		f.Unique += int64(len(b))
		dst = s.enc.EncodeAll(b, dst[:0])
		n := int64(min(len(dst), len(b)+1)) // stored raw when zstd doesn't help
		f.Stored += n
		stored += n
		if db {
			f.DBStored += n
		}
	}
}

// Analyze walks root, the backups directory: <app>/<stamp>/ for each app
// and files/<stamp>/ for Drive. Reserved names other than files are skipped.
func Analyze(ctx context.Context, root string, skip func(name string) bool, opt AnalyzeOptions) (*Report, error) {
	if opt.Params == (Params{}) {
		opt.Params = DefaultParams
	}
	if err := opt.Params.valid(); err != nil {
		return nil, err
	}
	asIs, err := newSeen()
	if err != nil {
		return nil, err
	}
	expanded, err := newSeen()
	if err != nil {
		return nil, err
	}
	rep := &Report{Root: root, Params: opt.Params}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		if name != "files" && skip != nil && skip(name) {
			continue
		}
		sr := SeriesReport{Series: name}
		if name == "files" {
			sr.Series = "Drive"
		}
		stamps, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		var dirs []string
		for _, s := range stamps {
			if s.IsDir() && s.Type()&fs.ModeSymlink == 0 && !strings.HasPrefix(s.Name(), ".") {
				dirs = append(dirs, s.Name())
			}
		}
		sort.Strings(dirs)
		inodes := map[[2]uint64]bool{}
		for i, stamp := range dirs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if opt.Progress != nil {
				opt.Progress(sr.Series, stamp)
			}
			dir := filepath.Join(root, name, stamp)
			var newAsIs, newExp int64
			err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				if st, ok := info.Sys().(*syscall.Stat_t); ok {
					k := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
					if !inodes[k] {
						inodes[k] = true
						sr.OnDisk += info.Size()
					}
				} else {
					sr.OnDisk += info.Size()
				}
				base := filepath.Base(p)
				db := strings.HasSuffix(base, ".dump")
				n, err := feedFile(p, func(r io.Reader) (int64, error) { return asIs.feed(r, opt.Params, &sr.AsIs, db) })
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				newAsIs += n
				n, err = feedExpanded(p, opt.Params, expanded, &sr.Expanded)
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				newExp += n
				return nil
			})
			if err != nil {
				return nil, err
			}
			if i > 0 {
				sr.AsIs.Nightly = append(sr.AsIs.Nightly, newAsIs)
				sr.Expanded.Nightly = append(sr.Expanded.Nightly, newExp)
			}
			sr.Snapshots++
		}
		if sr.Snapshots > 0 {
			rep.Series = append(rep.Series, sr)
		}
	}
	return rep, nil
}

func feedFile(p string, fn func(io.Reader) (int64, error)) (int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return fn(f)
}

// feedExpanded chunks a file with what fog can decompress opened up.
func feedExpanded(p string, params Params, s *seen, f *Figures) (int64, error) {
	base := filepath.Base(p)
	switch {
	case base == "artifact.tar":
		return feedFile(p, func(r io.Reader) (int64, error) {
			var total int64
			tr := tar.NewReader(r)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					return total, nil
				}
				if err != nil {
					return total, err
				}
				if hdr.Typeflag != tar.TypeReg {
					continue
				}
				n, err := feedMaybeCompressed(tr, hdr.Name, params, s, f)
				total += n
				if err != nil {
					return total, err
				}
			}
		})
	default:
		return feedFile(p, func(r io.Reader) (int64, error) { return feedMaybeCompressed(r, base, params, s, f) })
	}
}

func feedMaybeCompressed(r io.Reader, name string, params Params, s *seen, f *Figures) (int64, error) {
	db := strings.HasSuffix(name, ".dump")
	switch {
	case strings.HasSuffix(name, ".zst"):
		d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return 0, err
		}
		defer d.Close()
		return s.feed(d, params, f, db)
	case strings.HasSuffix(name, ".gz"):
		g, err := gzip.NewReader(r)
		if err != nil {
			return 0, err
		}
		defer g.Close()
		return s.feed(g, params, f, db)
	}
	return s.feed(r, params, f, db)
}
