// Package offsite is the locker: fog's sealed, self-describing layout for
// object storage (docs/locker-contract.md), and the manager that fills it
// from the backups fog already makes. This file is the cryptography —
// how one object is sealed and opened.
//
// Every sealed object is a stream of authenticated frames under
// XChaCha20-Poly1305: a fixed header (format magic, key id, a random
// nonce prefix), then plaintext in 64 KiB frames, each encrypted under a
// nonce of prefix ‖ counter ‖ last-flag and bound by additional data to
// the locker format, the object's own name, and the header. The
// construction is age's STREAM: constant memory for objects of any size,
// truncation detected because the final frame carries a flag, and no
// nonce bookkeeping because the 192-bit nonce makes a random prefix per
// object safe for the life of the key.
package offsite

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// Format names the locker layout and the sealing construction. It is
	// in every frame's additional data: an object from a future format
	// cannot be misread by this one.
	Format = "fog-locker/1"

	sealMagic   = "FOGLKR1"
	sealVersion = byte(1)
	// noncePrefixSize + 4 (counter) + 1 (last flag) = the XChaCha nonce.
	noncePrefixSize = chacha20poly1305.NonceSizeX - 5
	headerSize      = len(sealMagic) + 1 + 4 + noncePrefixSize
	// FrameSize is the plaintext bytes per frame.
	FrameSize = 64 << 10
	tagSize   = chacha20poly1305.Overhead
)

var (
	ErrNotSealed  = errors.New("not a fog locker object")
	ErrTruncated  = errors.New("sealed object is truncated")
	ErrBadKey     = errors.New("sealed object does not open with this locker's keys")
	ErrUnknownKey = errors.New("sealed object was written with a key this locker does not hold")
)

// NewNonce returns a fresh random nonce prefix. Exposed so a resumed
// upload can re-seal a source with the same nonce and produce identical
// bytes — the parts already in the bucket stay valid.
func NewNonce() ([]byte, error) {
	n := make([]byte, noncePrefixSize)
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return n, nil
}

// SealedSize is the sealed length of a plaintext of n bytes.
func SealedSize(n int64) int64 {
	frames := n / FrameSize
	if n%FrameSize != 0 || n == 0 {
		frames++
	}
	return int64(headerSize) + n + frames*tagSize
}

// frameNonce writes frame counter's nonce into nonce, which already holds
// the object's prefix. Seal and Open reuse one buffer for every frame.
func frameNonce(nonce *[chacha20poly1305.NonceSizeX]byte, counter uint32, last bool) {
	binary.BigEndian.PutUint32(nonce[noncePrefixSize:], counter)
	nonce[len(nonce)-1] = 0
	if last {
		nonce[len(nonce)-1] = 1
	}
}

func aad(name string, header []byte) []byte {
	out := make([]byte, 0, len(Format)+1+len(name)+1+len(header))
	out = append(out, Format...)
	out = append(out, 0)
	out = append(out, name...)
	out = append(out, 0)
	out = append(out, header...)
	return out
}

// sealWriter frames plaintext into dst.
type sealWriter struct {
	dst  io.Writer
	aead interface {
		Seal(dst, nonce, plaintext, aad []byte) []byte
	}
	nonce   [chacha20poly1305.NonceSizeX]byte
	aad     []byte
	buf     []byte
	ct      []byte // ciphertext scratch, reused per frame
	counter uint32
	closed  bool
	err     error
}

// Seal returns a writer that seals everything written to it into dst as
// the object named name; Close writes the final frame and must be called.
// nonce is a NewNonce value or nil for a fresh one.
func (k *Keyring) Seal(dst io.Writer, name string, nonce []byte) (io.WriteCloser, error) {
	key, id := k.current()
	if key == nil {
		return nil, errors.New("locker has no data key")
	}
	if nonce == nil {
		var err error
		if nonce, err = NewNonce(); err != nil {
			return nil, err
		}
	}
	if len(nonce) != noncePrefixSize {
		return nil, fmt.Errorf("nonce must be %d bytes", noncePrefixSize)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, headerSize)
	header = append(header, sealMagic...)
	header = append(header, sealVersion)
	header = binary.BigEndian.AppendUint32(header, id)
	header = append(header, nonce...)
	if _, err := dst.Write(header); err != nil {
		return nil, err
	}
	w := &sealWriter{dst: dst, aead: aead, aad: aad(name, header), buf: make([]byte, 0, FrameSize), ct: make([]byte, 0, FrameSize+tagSize)}
	copy(w.nonce[:], nonce)
	return w, nil
}

func (w *sealWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("write after close")
	}
	n := 0
	for len(p) > 0 {
		room := FrameSize - len(w.buf)
		take := len(p)
		if take > room {
			take = room
		}
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		n += take
		if len(w.buf) == FrameSize && len(p) > 0 {
			// A full frame with more to come is never the last frame; a
			// full frame at the end of a write waits, because the next
			// call may be Close.
			if err := w.flush(false); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (w *sealWriter) flush(last bool) error {
	frameNonce(&w.nonce, w.counter, last)
	w.ct = w.aead.Seal(w.ct[:0], w.nonce[:], w.buf, w.aad)
	if _, err := w.dst.Write(w.ct); err != nil {
		w.err = err
		return err
	}
	w.counter++
	w.buf = w.buf[:0]
	return nil
}

func (w *sealWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	return w.flush(true)
}

// openReader verifies frames from src.
type openReader struct {
	src  io.Reader
	aead interface {
		Open(dst, nonce, ciphertext, aad []byte) ([]byte, error)
	}
	nonce   [chacha20poly1305.NonceSizeX]byte
	aad     []byte
	frame   []byte // ciphertext scratch
	carry   []byte // the look-ahead byte from the previous frame
	out     []byte // plaintext scratch, reused per frame
	plain   []byte // decrypted, unread (a window on out)
	counter uint32
	done    bool
}

// Open returns a reader over the plaintext of a sealed object named name.
// Every frame is authenticated before its bytes are handed out; a
// truncated or tampered object fails with ErrTruncated / ErrBadKey rather
// than yielding garbage.
func (k *Keyring) Open(src io.Reader, name string) (io.Reader, error) {
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(src, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrNotSealed
		}
		return nil, err
	}
	if string(header[:len(sealMagic)]) != sealMagic || header[len(sealMagic)] != sealVersion {
		return nil, ErrNotSealed
	}
	id := binary.BigEndian.Uint32(header[len(sealMagic)+1:])
	key := k.key(id)
	if key == nil {
		return nil, fmt.Errorf("%w (key id %d)", ErrUnknownKey, id)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	r := &openReader{src: src, aead: aead, aad: aad(name, header), frame: make([]byte, FrameSize+tagSize), out: make([]byte, 0, FrameSize)}
	copy(r.nonce[:], header[len(sealMagic)+1+4:])
	return r, nil
}

func (r *openReader) Read(p []byte) (int, error) {
	for len(r.plain) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.plain)
	r.plain = r.plain[n:]
	return n, nil
}

// next reads and authenticates one frame. The last frame is recognized by
// its size (short) or, for a payload that is an exact multiple of the
// frame size, by the stream ending right after it — which takes a
// one-byte look ahead, kept in carry for the next frame.
func (r *openReader) next() error {
	n := copy(r.frame, r.carry)
	r.carry = r.carry[:0]
	m, err := io.ReadFull(r.src, r.frame[n:])
	n += m
	switch {
	case err == nil:
		// A full frame. It is the last one only if nothing follows.
		var probe [1]byte
		p, perr := io.ReadFull(r.src, probe[:])
		if p == 0 && (errors.Is(perr, io.EOF) || errors.Is(perr, io.ErrUnexpectedEOF)) {
			return r.open(r.frame[:n], true)
		}
		if p == 0 {
			return perr
		}
		if err := r.open(r.frame[:n], false); err != nil {
			return err
		}
		r.carry = append(r.carry, probe[0])
		return nil
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF) && n > 0:
		// A short final frame.
		if n < tagSize {
			return ErrTruncated
		}
		return r.open(r.frame[:n], true)
	case errors.Is(err, io.EOF):
		// Stream ended on a frame boundary without a last frame.
		return ErrTruncated
	default:
		return err
	}
}

func (r *openReader) open(ct []byte, last bool) error {
	frameNonce(&r.nonce, r.counter, last)
	plain, err := r.aead.Open(r.out[:0], r.nonce[:], ct, r.aad)
	if err != nil {
		if last {
			// Either the key is wrong or the object was cut at a frame
			// boundary; both are "does not open", and truncation is the
			// one worth naming when a full frame is missing its flag.
			return fmt.Errorf("%w (final frame)", ErrBadKey)
		}
		return ErrBadKey
	}
	r.counter++
	r.plain = plain
	r.done = last
	return nil
}

// SealBytes seals a small in-memory object.
func (k *Keyring) SealBytes(name string, plain []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(int(SealedSize(int64(len(plain)))))
	w, err := k.Seal(&buf, name, nil)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OpenBytes opens a small sealed object, refusing plaintext over limit.
func (k *Keyring) OpenBytes(name string, sealed []byte, limit int64) ([]byte, error) {
	r, err := k.Open(bytes.NewReader(sealed), name)
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("%s: plaintext exceeds %d bytes", name, limit)
	}
	return out, nil
}
