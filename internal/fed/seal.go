package fed

import (
	"bufio"
	"bytes"
	"crypto/hpke"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Sealed bodies.
//
// Every body on the fed plane — request and answer, a file's bytes, the
// connection proof's payload — is encrypted to the receiving fog's
// encryption key, on top of being signed. The construction is HPKE
// (RFC 9180) in base mode, from Go's standard library:
//
//	KEM  DHKEM(X25519, HKDF-SHA256)   0x0020
//	KDF  HKDF-SHA256                  0x0001
//	AEAD ChaCha20-Poly1305            0x0003
//
// A body is one HPKE context, sealed as a stream of records so a file of
// any size streams through with bounded memory:
//
//	0x01 || enc (32 bytes) || record*
//	record = len (uint32 BE, ciphertext bytes) || Seal(aad = final, chunk)
//
// final is one byte, 1 on the last record and 0 on every other; the last
// record may be empty. HPKE's per-context sequence number is the nonce, so
// records can't be reordered or dropped; the final flag means a stream cut
// short is an error, not a shorter body; and nothing may follow the final
// record. The context's info binds the body to its exchange (RequestInfo,
// AnswerInfo), so a sealed body moved to any other exchange — another
// request, another target, another requester, another status — does not
// open.

const (
	sealVersion = 0x01
	sealEncLen  = 32
	// SealChunk is the plaintext carried per record.
	SealChunk = 64 << 10
	// sealMaxRecord bounds one record's ciphertext, so a hostile length
	// prefix can't make a reader allocate without limit.
	sealMaxRecord = SealChunk + 16
)

var (
	sealKEM  = hpke.DHKEM(ecdhX25519)
	sealKDF  = hpke.HKDFSHA256()
	sealAEAD = hpke.ChaCha20Poly1305()

	// ErrSealed: a body that should be sealed didn't open — tampered, cut
	// short, moved from another exchange, or not sealed at all.
	ErrSealed = errors.New("sealed body does not open")
)

// RequestInfo is what a request body's sealing binds: the method and target
// it was sent to, by whom, to whom, at the signed timestamp. The request's
// signature covers the sealed bytes, so it can't be inside them; the
// timestamp, unique per signer, stands for it.
func RequestInfo(method, target, requester, recipient, timestamp string) []byte {
	return []byte("fogfed1-request\n" + method + "\n" + target + "\n" + requester + "\n" + recipient + "\n" + timestamp)
}

// AnswerInfo is what an answer's sealing binds: the request it answers —
// the outer method and target, the requester and that request's signature
// — and who answered. The answer's status and headers are inside the seal,
// in its signed head.
func AnswerInfo(method, target, requester, requestSig, responder string) []byte {
	return []byte("fogfed1-answer\n" + method + "\n" + target + "\n" + requester + "\n" + requestSig + "\n" + responder)
}

// ParseEncKey reads a peer's encryption public key (base64 std, 32 bytes).
func ParseEncKey(b64 string) (hpke.PublicKey, error) {
	raw, err := decodeKey(b64)
	if err != nil {
		return nil, err
	}
	pk, err := sealKEM.NewPublicKey(raw)
	if err != nil {
		return nil, errors.New("invalid encryption key")
	}
	return pk, nil
}

// SealWriter seals a stream to one recipient: records of up to SealChunk
// plaintext, the last flagged final on Close.
type SealWriter struct {
	w      io.Writer
	s      *hpke.Sender
	buf    []byte
	closed bool
}

// NewSealWriter starts a sealed stream to pub, writing the header now.
func NewSealWriter(w io.Writer, pub hpke.PublicKey, info []byte) (*SealWriter, error) {
	enc, s, err := hpke.NewSender(pub, sealKDF, sealAEAD, info)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(append([]byte{sealVersion}, enc...)); err != nil {
		return nil, err
	}
	return &SealWriter{w: w, s: s, buf: make([]byte, 0, SealChunk)}, nil
}

func (sw *SealWriter) Write(p []byte) (int, error) {
	if sw.closed {
		return 0, errors.New("sealed stream already closed")
	}
	n := 0
	for len(p) > 0 {
		k := min(len(p), SealChunk-len(sw.buf))
		sw.buf = append(sw.buf, p[:k]...)
		p, n = p[k:], n+k
		if len(sw.buf) == SealChunk {
			if err := sw.record(false); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (sw *SealWriter) record(final bool) error {
	aad := []byte{0}
	if final {
		aad[0] = 1
	}
	ct, err := sw.s.Seal(aad, sw.buf)
	if err != nil {
		return err
	}
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := sw.w.Write(l[:]); err != nil {
		return err
	}
	if _, err := sw.w.Write(ct); err != nil {
		return err
	}
	sw.buf = sw.buf[:0]
	// A record is a unit the reader can use: let it go now, not when some
	// buffer below happens to fill.
	if f, ok := sw.w.(interface{ Flush() }); ok {
		f.Flush()
	}
	return nil
}

// Close seals what is buffered as the final record.
func (sw *SealWriter) Close() error {
	if sw.closed {
		return nil
	}
	sw.closed = true
	return sw.record(true)
}

// Seal seals one whole body.
func Seal(pub hpke.PublicKey, info, plaintext []byte) ([]byte, error) {
	var out bytes.Buffer
	sw, err := NewSealWriter(&out, pub, info)
	if err != nil {
		return nil, err
	}
	if _, err := sw.Write(plaintext); err != nil {
		return nil, err
	}
	if err := sw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// OpenReader opens a sealed stream as it is read. Every record is
// authenticated before any of its bytes are returned; a stream that ends
// before its final record, or carries anything after it, is ErrSealed.
type OpenReader struct {
	r     *bufio.Reader
	rc    *hpke.Recipient
	src   io.Closer
	plain []byte
	done  bool
	err   error
}

// NewOpenReader reads a sealed stream's header and is ready to open its
// records. src, when it is an io.Closer, is closed by Close.
func NewOpenReader(src io.Reader, priv hpke.PrivateKey, info []byte) (*OpenReader, error) {
	br := bufio.NewReaderSize(src, 4+sealMaxRecord)
	hdr := make([]byte, 1+sealEncLen)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != sealVersion {
		return nil, fmt.Errorf("%w: no sealed header", ErrSealed)
	}
	rc, err := hpke.NewRecipient(hdr[1:], priv, sealKDF, sealAEAD, info)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSealed, err)
	}
	or := &OpenReader{r: br, rc: rc}
	if c, ok := src.(io.Closer); ok {
		or.src = c
	}
	return or, nil
}

func (or *OpenReader) next() error {
	var l [4]byte
	if _, err := io.ReadFull(or.r, l[:]); err != nil {
		return fmt.Errorf("%w: cut short", ErrSealed)
	}
	n := binary.BigEndian.Uint32(l[:])
	if n < 16 || n > sealMaxRecord {
		return fmt.Errorf("%w: bad record length", ErrSealed)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(or.r, ct); err != nil {
		return fmt.Errorf("%w: cut short", ErrSealed)
	}
	// The final flag is authenticated as the AAD: try "more" first, then
	// "final" — exactly one can open.
	if pt, err := or.rc.Open([]byte{0}, ct); err == nil {
		or.plain = pt
		return nil
	}
	pt, err := or.rc.Open([]byte{1}, ct)
	if err != nil {
		return fmt.Errorf("%w: record does not authenticate", ErrSealed)
	}
	if _, err := or.r.Peek(1); err != io.EOF {
		return fmt.Errorf("%w: data after the final record", ErrSealed)
	}
	or.plain, or.done = pt, true
	return nil
}

func (or *OpenReader) Read(p []byte) (int, error) {
	for len(or.plain) == 0 {
		if or.err != nil {
			return 0, or.err
		}
		if or.done {
			return 0, io.EOF
		}
		if err := or.next(); err != nil {
			or.err = err
			return 0, err
		}
	}
	n := copy(p, or.plain)
	or.plain = or.plain[n:]
	return n, nil
}

func (or *OpenReader) Close() error {
	if or.src != nil {
		return or.src.Close()
	}
	return nil
}

// Open opens one whole sealed body.
func Open(priv hpke.PrivateKey, info, sealed []byte) ([]byte, error) {
	or, err := NewOpenReader(bytes.NewReader(sealed), priv, info)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(or)
}
