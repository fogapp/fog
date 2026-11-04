// Package chunkstore is fog's content-addressed backup store
// (docs/store-contract.md). Every backup is a complete snapshot; what makes
// backups incremental is that a snapshot's bytes are cut into chunks by
// content, each chunk is stored once under a keyed hash of its plaintext,
// and every later snapshot that holds the same bytes refers to the chunk
// already there. Nothing chains: a snapshot never depends on an earlier
// snapshot, only on chunks, so deleting any snapshot leaves every other one
// whole, and a restore reads exactly one snapshot.
package chunkstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// Params bound the chunker. Avg must be a power of two; Min < Avg < Max.
type Params struct {
	Min, Avg, Max int
}

// DefaultParams suit backups: ~1 MiB chunks keep the index small (a
// terabyte is about a million chunks) while a one-row change in a dump or
// a one-block change in a big file costs about a megabyte, not the file.
var DefaultParams = Params{Min: 256 << 10, Avg: 1 << 20, Max: 4 << 20}

func (p Params) valid() error {
	if p.Min <= 0 || p.Avg <= p.Min || p.Max <= p.Avg {
		return errors.New("chunker: want 0 < min < avg < max")
	}
	if p.Avg&(p.Avg-1) != 0 {
		return errors.New("chunker: avg must be a power of two")
	}
	if p.Max > 64<<20 {
		return errors.New("chunker: max past 64 MiB")
	}
	return nil
}

// Gear is the per-store table the rolling hash is built from. It is
// derived from a secret seed, so where a store cuts its chunks — and with
// it the sizes an observer could fingerprint content by — is not public
// (the 2025 chunking-attack papers recover unkeyed and weakly keyed
// parameters from chunk sizes; the store also hides sizes inside packs).
type Gear [256]uint64

// NewGear derives the table from a seed.
func NewGear(seed []byte) *Gear {
	var g Gear
	mac := hmac.New(sha256.New, seed)
	var ctr [4]byte
	for i := 0; i < 256; i += 4 {
		binary.BigEndian.PutUint32(ctr[:], uint32(i))
		mac.Reset()
		mac.Write([]byte("fog-store/1 gear"))
		mac.Write(ctr[:])
		sum := mac.Sum(nil)
		for j := 0; j < 4; j++ {
			g[i+j] = binary.BigEndian.Uint64(sum[j*8:])
		}
	}
	return &g
}

// Chunker cuts a stream into content-defined chunks with FastCDC's
// normalized chunking: below the average size a cut needs two more hash
// bits to be zero, above it two fewer, so sizes cluster near the average
// without the long tail a single mask gives. Boundaries depend only on
// the bytes near them, so an insertion early in a stream moves the cut
// points around it and leaves every later chunk byte-identical.
type Chunker struct {
	r            io.Reader
	gear         *Gear
	p            Params
	maskS, maskL uint64
	buf          []byte
	start, end   int
	eof          bool
	err          error
}

// NewChunker reads r in chunks. A nil gear uses an all-public table
// derived from an empty seed — fine for measuring, not for a store.
func NewChunker(r io.Reader, gear *Gear, p Params) (*Chunker, error) {
	if err := p.valid(); err != nil {
		return nil, err
	}
	if gear == nil {
		gear = NewGear(nil)
	}
	bits := 0
	for 1<<bits < p.Avg {
		bits++
	}
	// Masks over the hash's top bits: with a left-shifting gear hash the
	// high bits mix the most recent 64 bytes, the low bits only the last
	// few.
	top := func(n int) uint64 { return ^uint64(0) << (64 - n) }
	return &Chunker{
		r: r, gear: gear, p: p,
		maskS: top(bits + 2), maskL: top(bits - 2),
		buf: make([]byte, 2*p.Max),
	}, nil
}

// fill tops the buffer up so at least Max bytes (or what remains) are
// available from start.
func (c *Chunker) fill() {
	if c.eof || c.err != nil {
		return
	}
	if c.start > 0 {
		n := copy(c.buf, c.buf[c.start:c.end])
		c.start, c.end = 0, n
	}
	for c.end-c.start < c.p.Max && c.end < len(c.buf) {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if err == io.EOF {
			c.eof = true
			return
		}
		if err != nil {
			c.err = err
			return
		}
	}
}

// Next returns the next chunk, valid until the following call, or io.EOF
// once the stream is consumed. Copy it to keep it.
func (c *Chunker) Next() ([]byte, error) {
	if c.end-c.start < c.p.Max {
		c.fill()
	}
	if c.err != nil {
		return nil, c.err
	}
	n := c.end - c.start
	if n == 0 {
		return nil, io.EOF
	}
	cut := c.cut(c.buf[c.start:c.end])
	out := c.buf[c.start : c.start+cut]
	c.start += cut
	return out, nil
}

func (c *Chunker) cut(b []byte) int {
	n := len(b)
	if n <= c.p.Min {
		return n
	}
	if n > c.p.Max {
		n = c.p.Max
	}
	norm := c.p.Avg
	if norm > n {
		norm = n
	}
	var h uint64
	i := c.p.Min
	for ; i < norm; i++ {
		h = h<<1 + c.gear[b[i]]
		if h&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		h = h<<1 + c.gear[b[i]]
		if h&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}
