package offsite

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/objstore"
)

// The upload engine: a plaintext source becomes one sealed object in the
// bucket, in parts, resumably.
//
// Parts are 64 MiB, staged one at a time — in memory when small, else
// spooled to real disk (never /tmp, which is tmpfs on the target OS) — and
// hashed while they are staged, so the SigV4 payload hash and Content-MD5
// exist before the part is sent and a failed part is re-sent from the
// stage without being read again to hash it. Sealing is deterministic for
// a nonce, so a resumed upload re-seals the same source with the same
// nonce, asks the bucket which parts it already holds, and sends only the
// rest — the bytes it skips are byte-identical to the bytes already there.
//
// A file uploaded as it is (kits, a locker copy) needs no stage at all:
// its parts are sections of the file, hashed in place and then sent.

// PartSize is the multipart part size. A var so tests can exercise
// resume with small parts; objects that fit in one part go up in a
// single PUT.
var PartSize int64 = 64 << 20

// memPartMax is the largest part staged in memory rather than spooled —
// most Files blobs, and every small document.
var memPartMax int64 = 8 << 20

const spoolPrefix = "offsite-spool-"

// Source is a re-openable plaintext stream.
type Source struct {
	Open func() (io.ReadCloser, error)
	// Size is the plaintext size when known (a file) — progress only.
	Size int64
	// path is set for a FileSource: an unsealed upload of it sends
	// sections of the file instead of staging copies.
	path string
}

// FileSource streams one file.
func FileSource(path string) Source {
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	return Source{Open: func() (io.ReadCloser, error) { return os.Open(path) }, Size: size, path: path}
}

// DirSource streams a directory's regular files as a flat tar, in name
// order — the shape of a backup snapshot directory (docs/locker-contract.md
// §3.2). Deterministic, so a resumed upload produces the same bytes.
func DirSource(dir string) (Source, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Source{}, nil, err
	}
	var names []string
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return Source{}, nil, err
		}
		names = append(names, e.Name())
		total += info.Size() + 512
	}
	sort.Strings(names)
	open := func() (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			tw := tar.NewWriter(pw)
			err := func() error {
				for _, name := range names {
					path := filepath.Join(dir, name)
					info, err := os.Stat(path)
					if err != nil {
						return err
					}
					// Fixed mode and a truncated mtime keep the stream identical
					// across attempts even if the directory was touched.
					hdr := &tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime().Truncate(time.Second), Format: tar.FormatPAX}
					if err := tw.WriteHeader(hdr); err != nil {
						return err
					}
					f, err := os.Open(path)
					if err != nil {
						return err
					}
					n, err := io.Copy(tw, f)
					f.Close()
					if err != nil {
						return err
					}
					if n != info.Size() {
						return fmt.Errorf("%s changed while being read", name)
					}
				}
				return tw.Close()
			}()
			pw.CloseWithError(err)
		}()
		return pr, nil
	}
	return Source{Open: open, Size: total}, names, nil
}

// UploadState is what survives a crash: the multipart id and the seal
// nonce, both in the copy row.
type UploadState struct {
	UploadID string
	Nonce    []byte
}

func (s *UploadState) encodeNonce() string { return base64.StdEncoding.EncodeToString(s.Nonce) }

func decodeNonce(v string) []byte {
	if v == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil
	}
	return b
}

// Uploader carries what every upload to one target shares.
type Uploader struct {
	Client *objstore.Client
	// Keys seals the stream; nil uploads the bytes as they are (kits).
	Keys *Keyring
	// Spool is the directory parts are staged in.
	Spool string
	// Limit caps the send rate; nil is unlimited.
	Limit *RateLimiter
	// Lock is applied to every object written (grade A targets).
	Lock objstore.PutOptions
	// Progress hears sealed bytes as they are sent (or skipped on resume).
	Progress func(n int64)
}

// Result is what an upload reports back.
type Result struct {
	// SHA256 is the plaintext digest; PlainBytes its length; Bytes the
	// object's size in the bucket.
	SHA256     string
	PlainBytes int64
	Bytes      int64
	ETag       string
}

// errSourceChanged: a resumed upload found a part in the bucket whose bytes
// differ from what the source produces now. The nonce is per object, so
// re-sending the differing part would reuse that nonce across two different
// plaintexts — fatal for XChaCha20-Poly1305. The attempt is thrown away and
// restarted with a fresh nonce.
var errSourceChanged = errors.New("upload source changed under a resume")

// Upload sends src to key. name is the object's locker-relative name
// (the seal's additional data). state is consulted for a resume and
// updated through save as the upload proceeds, so a crash mid-way leaves
// enough to continue.
//
// A resume reuses the saved nonce so the parts already in the bucket stay
// valid — sound only while the source is byte-for-byte what it was. If the
// source has changed, a reused part no longer matches and the attempt
// restarts under a fresh nonce rather than reusing the old one across
// differing data (which would both leak plaintext and record an object that
// will not decrypt). Each reused part is verified against the freshly-staged
// bytes before it is trusted, so what completes is one coherent object.
func (u *Uploader) Upload(ctx context.Context, key, name string, src Source, state *UploadState, save func(*UploadState) error) (*Result, error) {
	for {
		res, err := u.attempt(ctx, key, name, src, state, save)
		if errors.Is(err, errSourceChanged) {
			// attempt has already aborted the old multipart and cleared the
			// nonce; the next pass mints a fresh one and starts clean.
			continue
		}
		return res, err
	}
}

func (u *Uploader) attempt(ctx context.Context, key, name string, src Source, state *UploadState, save func(*UploadState) error) (*Result, error) {
	if u.Keys != nil && state.Nonce == nil {
		// Saved with the multipart id, before any part is sent: a single
		// PUT has nothing to resume, and the nonce rides in the object's
		// own header.
		nonce, err := NewNonce()
		if err != nil {
			return nil, err
		}
		state.Nonce = nonce
	}
	// What the bucket already holds of a previous attempt.
	existing := map[int]objstore.Part{}
	if state.UploadID != "" {
		parts, err := u.Client.ListParts(ctx, key, state.UploadID)
		switch {
		case err == nil:
			for _, p := range parts {
				existing[p.Number] = p
			}
		case errors.Is(err, objstore.ErrNotFound):
			// The provider expired or lost the upload; start over — and with a
			// fresh nonce, not the saved one. The lost parts may have been
			// observed, so reusing the nonce on a now-possibly-changed source
			// would reuse it across differing data.
			state.UploadID = ""
			if u.Keys != nil {
				nonce, nerr := NewNonce()
				if nerr != nil {
					return nil, nerr
				}
				state.Nonce = nonce
			}
			if err := save(state); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}

	var parts *partSource
	var err error
	if u.Keys == nil && src.path != "" {
		parts, err = u.fileParts(src.path)
	} else {
		parts, err = u.streamParts(name, src, state.Nonce)
	}
	if err != nil {
		return nil, err
	}
	defer parts.close()

	// restart aborts the half-done upload and clears both the multipart id
	// and the nonce, so the caller's next attempt starts wholly fresh. Used
	// the moment a resumed part fails to match the current source.
	restart := func() error {
		if state.UploadID != "" {
			u.Client.AbortMultipart(ctx, key, state.UploadID)
		}
		state.UploadID = ""
		state.Nonce = nil
		if err := save(state); err != nil {
			return err
		}
		return errSourceChanged
	}

	var (
		sent   []objstore.Part
		number = 0
		total  int64
		etag   string
	)
	for {
		number++
		p, err := parts.next()
		if err != nil {
			return nil, err
		}
		if p.n == 0 && number > 1 {
			// The stream ended exactly on a part boundary: the previous
			// part was the last one, and an empty part is never sent.
			p.release()
			break
		}
		last := p.n < PartSize
		if number == 1 && last {
			// Fits in one part: a single PUT, no multipart bookkeeping.
			etag, err = u.Client.Put(ctx, key, u.body(ctx, p), p.n, u.Lock)
			p.release()
			if err != nil {
				return nil, err
			}
			total = p.n
			break
		}
		if state.UploadID == "" {
			id, err := u.Client.CreateMultipart(ctx, key, u.Lock)
			if err != nil {
				p.release()
				return nil, err
			}
			state.UploadID = id
			if err := save(state); err != nil {
				p.release()
				return nil, err
			}
		}
		if have, ok := existing[number]; ok {
			// Reuse a part the bucket already holds only when its bytes are
			// exactly what the source produces now. A size-only match is not
			// enough: a changed part of the same length under the same nonce
			// is the nonce-reuse bug.
			if !partMatches(have, p) {
				p.release()
				return nil, restart()
			}
			sent = append(sent, have)
			if u.Progress != nil {
				u.Progress(p.n)
			}
		} else {
			part, err := u.Client.UploadPart(ctx, key, state.UploadID, number, u.body(ctx, p), p.n)
			if err != nil {
				p.release()
				return nil, err
			}
			sent = append(sent, part)
		}
		p.release()
		total += p.n
		if last {
			break
		}
	}
	if state.UploadID != "" {
		var err error
		etag, err = u.Client.CompleteMultipart(ctx, key, state.UploadID, sent)
		if err != nil {
			// A completion the bucket refuses (a part that no longer
			// matches — the source changed under a resume) is not resumable;
			// abandon the upload so the next attempt starts clean.
			u.Client.AbortMultipart(ctx, key, state.UploadID)
			state.UploadID = ""
			save(state)
			return nil, err
		}
		state.UploadID = ""
	}
	sum, plainBytes := parts.result()
	return &Result{SHA256: sum, PlainBytes: plainBytes, Bytes: total, ETag: etag}, nil
}

// partMatches reports whether a part the bucket already holds is byte-for-byte
// the freshly-staged part: same length, and the same content. The S3 ETag of
// a part uploaded in one PUT is the hex MD5 of its bytes, which is the MD5 the
// stage already computed. When the ETag is not a plain MD5 (an unusual
// provider), the bytes cannot be confirmed identical, so the part is treated
// as changed and the upload restarts under a fresh nonce — correctness over a
// resumed part.
func partMatches(have objstore.Part, p *stagedPart) bool {
	if have.Size != p.n {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(p.d.MD5)
	if err != nil {
		return false
	}
	return strings.EqualFold(have.ETag, hex.EncodeToString(raw))
}

// stagedPart is one part ready to send: its bytes (a memory buffer, a
// spool file, or a section of the source file), its length, and the
// digests computed as it was staged.
type stagedPart struct {
	r     io.ReadSeeker
	spool *os.File // removed on release; nil when the bytes are not ours
	n     int64
	d     objstore.Digests
}

func (p *stagedPart) release() {
	if p.spool != nil {
		p.spool.Close()
		os.Remove(p.spool.Name())
		p.spool = nil
	}
}

// partSource yields an upload's parts in order and, at the end, the
// plaintext's digest and length.
type partSource struct {
	next   func() (*stagedPart, error)
	result func() (sha string, n int64)
	close  func()
}

// streamParts seals (or copies) src into a pipe and stages it a part at
// a time; a pipe keeps sealing and part-sending decoupled without
// buffering more than one part.
func (u *Uploader) streamParts(name string, src Source, nonce []byte) (*partSource, error) {
	if err := os.MkdirAll(u.Spool, 0o700); err != nil {
		return nil, err
	}
	plain, err := src.Open()
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	counted := &countingReader{r: io.TeeReader(plain, hash)}
	pr, pw := io.Pipe()
	go func() {
		var err error
		if u.Keys == nil {
			_, err = io.Copy(pw, counted)
		} else {
			var w io.WriteCloser
			w, err = u.Keys.Seal(pw, name, nonce)
			if err == nil {
				if _, err = io.Copy(w, counted); err == nil {
					err = w.Close()
				}
			}
		}
		pw.CloseWithError(err)
	}()
	return &partSource{
		next:   func() (*stagedPart, error) { return stagePart(pr, u.Spool) },
		result: func() (string, int64) { return hex.EncodeToString(hash.Sum(nil)), counted.n },
		close: func() {
			pr.Close()
			plain.Close()
		},
	}, nil
}

// fileParts sends a file as it is: each part is a section of the file,
// hashed in place (the whole-file digest accumulating as the sections go
// by in order) and then sent from the file itself.
func (u *Uploader) fileParts(path string) (*partSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := info.Size()
	whole := sha256.New()
	var off int64
	return &partSource{
		next: func() (*stagedPart, error) {
			n := min(PartSize, size-off)
			sec := io.NewSectionReader(f, off, n)
			sha, md := sha256.New(), md5.New()
			if _, err := io.Copy(io.MultiWriter(sha, md, whole), sec); err != nil {
				return nil, err
			}
			if _, err := sec.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			off += n
			return &stagedPart{r: sec, n: n, d: digests(sha, md)}, nil
		},
		result: func() (string, int64) { return hex.EncodeToString(whole.Sum(nil)), size },
		close:  func() { f.Close() },
	}, nil
}

// stagePart reads up to PartSize bytes from r, hashing them as they go:
// into memory when the part is no bigger than memPartMax, else into a
// fresh spool file. The part comes back positioned at its start.
func stagePart(r io.Reader, dir string) (*stagedPart, error) {
	sha, md := sha256.New(), md5.New()
	var buf bytes.Buffer
	limit := min(PartSize, memPartMax)
	n, err := io.CopyN(io.MultiWriter(&buf, sha, md), r, limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n < limit || limit == PartSize {
		return &stagedPart{r: bytes.NewReader(buf.Bytes()), n: n, d: digests(sha, md)}, nil
	}
	// A big part: what is in memory goes to the spool first, then the
	// rest of the part.
	f, err := os.CreateTemp(dir, spoolPrefix+"*")
	if err != nil {
		return nil, err
	}
	p := &stagedPart{r: f, spool: f}
	if _, err := f.Write(buf.Bytes()); err != nil {
		p.release()
		return nil, err
	}
	rest, err := io.CopyN(io.MultiWriter(f, sha, md), r, PartSize-n)
	if err != nil && !errors.Is(err, io.EOF) {
		p.release()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		p.release()
		return nil, err
	}
	p.n, p.d = n+rest, digests(sha, md)
	return p, nil
}

func digests(sha, md hash.Hash) objstore.Digests {
	return objstore.Digests{SHA256: hex.EncodeToString(sha.Sum(nil)), MD5: base64.StdEncoding.EncodeToString(md.Sum(nil))}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// body is what the client sends for a part: the staged bytes, carrying
// their digests so the client does not read them to hash, through the
// rate limiter.
func (u *Uploader) body(ctx context.Context, p *stagedPart) *sendBody {
	return &sendBody{r: p.r, d: p.d, limit: u.Limit, ctx: ctx, progress: u.Progress}
}

// sendBody counts what it hands out. Every pass over it is a real send —
// the digests came with it — so every pass is rate-limited, but progress
// reports each byte once: a retry that re-sends from the start reports
// only what goes beyond the furthest point an earlier attempt reached.
type sendBody struct {
	r        io.ReadSeeker
	d        objstore.Digests
	limit    *RateLimiter
	ctx      context.Context
	progress func(int64)
	pos      int64
	reported int64
}

func (b *sendBody) Digests() objstore.Digests { return b.d }

func (b *sendBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		if b.limit != nil {
			if werr := b.limit.Wait(b.ctx, n); werr != nil {
				return n, werr
			}
		}
		b.pos += int64(n)
		if b.pos > b.reported {
			if b.progress != nil {
				b.progress(b.pos - b.reported)
			}
			b.reported = b.pos
		}
	}
	return n, err
}

func (b *sendBody) Seek(offset int64, whence int) (int64, error) {
	pos, err := b.r.Seek(offset, whence)
	if err == nil {
		b.pos = pos
	}
	return pos, err
}

// RateLimiter is a token bucket in bytes per second with one second of
// burst — enough to keep a home uplink usable while a seed runs.
type RateLimiter struct {
	mu        sync.Mutex
	bps       float64
	allowance float64
	last      time.Time
}

// NewRateLimiter caps at mbps megabits per second; 0 returns nil (no cap).
func NewRateLimiter(mbps int) *RateLimiter {
	if mbps <= 0 {
		return nil
	}
	bps := float64(mbps) * 1e6 / 8
	return &RateLimiter{bps: bps, allowance: bps, last: time.Now()}
}

// Wait blocks until n bytes may be sent.
func (l *RateLimiter) Wait(ctx context.Context, n int) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.allowance += now.Sub(l.last).Seconds() * l.bps
		l.last = now
		if l.allowance > l.bps {
			l.allowance = l.bps
		}
		if l.allowance >= float64(n) {
			l.allowance -= float64(n)
			l.mu.Unlock()
			return nil
		}
		need := (float64(n) - l.allowance) / l.bps
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(need * float64(time.Second))):
		}
	}
}

// SweepSpool removes spool files older than age — what a crash mid-part
// leaves behind. The janitor calls it.
func SweepSpool(dir string, age time.Duration) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-age)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
