package fed

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"
)

// The envelope.
//
// On the wire every fed exchange is one shape: a POST to SealedPath whose
// body is the real request, sealed, and a 200 whose body is the real
// answer, sealed. Nothing that names what is being done crosses outside the
// seal — not the method, the path or query (a file's name, a handle, a room),
// not a Range or an If-None-Match, not the answer's status or headers. What
// an observer sees is that two fogs talk, the requester's instance ID and
// public keys (the receiver needs them to find the key that verifies the
// request and the key its answer is sealed to), sizes and timing.
//
//	outer request:  POST /fed/v1/sealed
//	                Fog-Instance, Fog-Timestamp, Fog-Signature,
//	                Fog-Encryption-Key, Fog-Encryption-Key-Sig
//	                body = Seal(RequestInfo, u32 len(R) || R || inner body)
//	                R    = {"method", "target", "headers"}
//
//	outer answer:   200, application/octet-stream
//	                body = Seal(AnswerInfo, u32 len(H) || H || u16 len(S) || S || inner body)
//	                H    = {"status", "headers", "digest", "instance", "ts",
//	                        "encryptionKey", "encryptionKeySig"}
//	                S    = Sign(ANSWER, target, ts, AnswerMaterial(status, request, H))
//
// The outer request is signed as any request: its signature covers the
// sealed bytes, and the replay record holds it. An empty body is the
// connection proof (the hello). The answer's head is signed by the
// responder, bound to the request (its instance and signature) and to the
// status, and carries the digest of the inner body, checked as the body is
// read to its end: so a file streams, and a body cut, changed or swapped
// fails at the end where it can no longer be mistaken for the whole.

// SealedPath is the fed plane's one endpoint on the wire.
const SealedPath = "/fed/v1/sealed"

// SealedTarget is the outer target of a request to recipient: the
// envelope's path naming its recipient (an instance ID, or a pair
// recipient) in ?to=. The signature covers the target, so a request's
// signed headers are good only at the fog they were made for.
func SealedTarget(recipient string) string {
	return SealedPath + "?to=" + url.QueryEscape(recipient)
}

// maxHead bounds an envelope's head (and the inner request's): headers, not
// bodies.
const maxHead = 64 << 10

// innerRequest is what the outer request carries, sealed.
type innerRequest struct {
	Method string      `json:"method"`
	Target string      `json:"target"`
	Header http.Header `json:"headers,omitempty"`
}

// EncodeInnerRequest frames a request for sealing.
func EncodeInnerRequest(method, target string, h http.Header, body []byte) []byte {
	head, _ := json.Marshal(innerRequest{Method: method, Target: target, Header: h})
	out := make([]byte, 4, 4+len(head)+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(head)))
	return append(append(out, head...), body...)
}

// DecodeInnerRequest reads a framed request back.
func DecodeInnerRequest(plain []byte) (method, target string, h http.Header, body []byte, err error) {
	if len(plain) < 4 {
		return "", "", nil, nil, fmt.Errorf("%w: no inner request", ErrSealed)
	}
	n := binary.BigEndian.Uint32(plain)
	if n > maxHead || int(n) > len(plain)-4 {
		return "", "", nil, nil, fmt.Errorf("%w: bad inner request head", ErrSealed)
	}
	var ir innerRequest
	if err := json.Unmarshal(plain[4:4+n], &ir); err != nil || ir.Method == "" || len(ir.Target) == 0 || ir.Target[0] != '/' {
		return "", "", nil, nil, fmt.Errorf("%w: bad inner request head", ErrSealed)
	}
	return ir.Method, ir.Target, ir.Header, plain[4+n:], nil
}

// AnswerHead is what an answer says about itself, signed.
type AnswerHead struct {
	Status     int         `json:"status"`
	Header     http.Header `json:"headers,omitempty"`
	Digest     string      `json:"digest"` // sha256hex of the inner body
	Instance   string      `json:"instance"`
	Time       string      `json:"ts"`
	EncKey     string      `json:"encryptionKey,omitempty"`
	EncBinding string      `json:"encryptionKeySig,omitempty"`
}

// answerPrefix is the start of an answer's sealed plaintext: its head,
// signed by id for the request req at target.
func (i *Identity) answerPrefix(target string, req Bound, status int, hdr http.Header, digest string) []byte {
	ts := uniqueStamp()
	head, _ := json.Marshal(AnswerHead{Status: status, Header: hdr, Digest: digest, Instance: i.InstanceID, Time: ts,
		EncKey: i.EncPublicB64(), EncBinding: i.EncBinding})
	sig := i.Sign(methodAnswer, target, ts, AnswerMaterial(status, req, head))
	out := make([]byte, 4, 4+len(head)+2+len(sig))
	binary.BigEndian.PutUint32(out, uint32(len(head)))
	out = append(out, head...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(sig)))
	return append(out, sig...)
}

// ReadAnswerHead reads an answer's head from its opened stream and, when
// pub is given, verifies its signature for the request req at target.
func ReadAnswerHead(r io.Reader, pub ed25519.PublicKey, target string, req Bound) (*AnswerHead, error) {
	return readAnswerHeadAt(r, pub, target, req, time.Now())
}

func readAnswerHeadAt(r io.Reader, pub ed25519.PublicKey, target string, req Bound, at time.Time) (*AnswerHead, error) {
	var l4 [4]byte
	if _, err := io.ReadFull(r, l4[:]); err != nil {
		return nil, fmt.Errorf("%w: no answer head: %v", ErrSealed, err)
	}
	n := binary.BigEndian.Uint32(l4[:])
	if n > maxHead {
		return nil, fmt.Errorf("%w: answer head too large", ErrSealed)
	}
	head := make([]byte, n)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("%w: answer head cut short", ErrSealed)
	}
	var l2 [2]byte
	if _, err := io.ReadFull(r, l2[:]); err != nil {
		return nil, fmt.Errorf("%w: answer head cut short", ErrSealed)
	}
	sig := make([]byte, binary.BigEndian.Uint16(l2[:]))
	if _, err := io.ReadFull(r, sig); err != nil {
		return nil, fmt.Errorf("%w: answer head cut short", ErrSealed)
	}
	var ah AnswerHead
	if err := json.Unmarshal(head, &ah); err != nil || ah.Status < 100 || ah.Status > 599 {
		return nil, fmt.Errorf("%w: bad answer head", ErrSealed)
	}
	if pub != nil {
		if len(sig) == 0 {
			return nil, ErrUnsignedResponse
		}
		h := http.Header{}
		h.Set(HeaderTimestamp, ah.Time)
		h.Set(HeaderSignature, string(sig))
		if err := Verify(pub, methodAnswer, target, h, AnswerMaterial(ah.Status, req, head), at); err != nil {
			return nil, err
		}
	}
	return &ah, nil
}

// digestReader returns a body's bytes and, at its end, checks them against
// the digest its signed head declared.
type digestReader struct {
	r    io.Reader
	h    hash.Hash
	want string
	src  io.Closer
}

func newDigestReader(r io.Reader, want string, src io.Closer) *digestReader {
	return &digestReader{r: r, h: sha256.New(), want: want, src: src}
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.h.Write(p[:n])
	if err == io.EOF && hex.EncodeToString(d.h.Sum(nil)) != d.want {
		return n, fmt.Errorf("%w: the body does not match its signed digest", ErrSealed)
	}
	return n, err
}

func (d *digestReader) Close() error {
	if d.src != nil {
		return d.src.Close()
	}
	return nil
}

// WriteSealedAnswer writes one answer in the envelope: a 200 whose body is
// the answer — head signed by signer for the request req at target, then
// body — sealed to `to` for info. digest is the body's sha256hex, known
// before it is written (a file's, from the index; a buffered body's,
// computed). fogd's server writes every fed answer through this.
func WriteSealedAnswer(w http.ResponseWriter, signer *Identity, to hpke.PublicKey, info []byte, target string, req Bound, status int, hdr http.Header, digest string, body io.Reader) error {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h.Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	sw, err := NewSealWriter(w, to, info)
	if err != nil {
		return err
	}
	if _, err := sw.Write(signer.answerPrefix(target, req, status, hdr, digest)); err != nil {
		return err
	}
	if _, err := io.Copy(sw, body); err != nil {
		return err
	}
	return sw.Close()
}

// SealedRequest is an outer request opened: the inner request it carries,
// and what its answer must be bound and sealed to.
type SealedRequest struct {
	Inner     *http.Request
	Bound     Bound          // the outer request's instance and signature
	Target    string         // the outer request's target, which answers are signed for
	Requester hpke.PublicKey // the key the request presents
	Hello     bool           // an empty body: the connection proof
}

// AnswerInfo is the info for an answer to this request, from responder.
func (sr *SealedRequest) AnswerInfo(responder string) []byte {
	return AnswerInfo("POST", sr.Target, sr.Bound.Instance, sr.Bound.Sig, responder)
}

// OpenSealedRequest opens an outer request sent to id as recipient (its
// instance ID, or a pair recipient name). It checks no signature: the
// caller verifies the outer request first, as fogd's server does, or — a
// test server — doesn't need to.
func OpenSealedRequest(id *Identity, r *http.Request, wire []byte, recipient string) (*SealedRequest, []byte, error) {
	sr, err := OpenSealedRequestStream(id, r, bytes.NewReader(wire), int64(len(wire)), recipient)
	if err != nil {
		return nil, wire, err
	}
	if sr.Inner != nil {
		// Whole now: a caller holding the wire wants a body that fails here,
		// not part-way through its handler.
		body, err := io.ReadAll(sr.Inner.Body)
		if err != nil {
			return nil, wire, err
		}
		sr.Inner.Body = io.NopCloser(bytes.NewReader(body))
		sr.Inner.ContentLength = int64(len(body))
	}
	return sr, wire, nil
}

// OpenSealedRequestStream is OpenSealedRequest over a body of size bytes
// read from src: only the inner request's head is opened here; its body is
// opened record by record as the handler reads it, so what the handler's
// own limit refuses is never decrypted, let alone held. A body that turns
// out cut, tampered or followed by anything fails the read that reaches it.
func OpenSealedRequestStream(id *Identity, r *http.Request, src io.Reader, size int64, recipient string) (*SealedRequest, error) {
	sr := &SealedRequest{Bound: BoundTo(r.Header), Target: r.URL.RequestURI()}
	sr.Requester, _ = ParseEncKey(r.Header.Get(HeaderEncKey))
	if size == 0 {
		sr.Hello = true
		return sr, nil
	}
	if id.encPriv == nil {
		return nil, ErrNoEncryption
	}
	or, err := NewOpenReader(src, id.encPriv, RequestInfo("POST", sr.Target, sr.Bound.Instance, recipient, r.Header.Get(HeaderTimestamp)))
	if err != nil {
		return nil, err
	}
	var l [4]byte
	if _, err := io.ReadFull(or, l[:]); err != nil {
		return nil, fmt.Errorf("%w: no inner request", ErrSealed)
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > maxHead {
		return nil, fmt.Errorf("%w: bad inner request head", ErrSealed)
	}
	head := make([]byte, n)
	if _, err := io.ReadFull(or, head); err != nil {
		return nil, fmt.Errorf("%w: bad inner request head", ErrSealed)
	}
	var ir innerRequest
	if err := json.Unmarshal(head, &ir); err != nil || ir.Method == "" || len(ir.Target) == 0 || ir.Target[0] != '/' {
		return nil, fmt.Errorf("%w: bad inner request head", ErrSealed)
	}
	method, target, h := ir.Method, ir.Target, ir.Header
	inner, err := http.NewRequestWithContext(r.Context(), method, target, io.NopCloser(or))
	if err != nil {
		return nil, fmt.Errorf("%w: bad inner target", ErrSealed)
	}
	inner.ContentLength = -1
	inner.RequestURI = target
	inner.Host, inner.RemoteAddr, inner.Proto = r.Host, r.RemoteAddr, r.Proto
	for k, vs := range h {
		for _, v := range vs {
			inner.Header.Add(k, v)
		}
	}
	// The outer request's identity rides along for handlers that read it.
	for _, k := range []string{HeaderInstance, HeaderTimestamp, HeaderSignature, HeaderEncKey, HeaderEncBinding} {
		inner.Header.Set(k, r.Header.Get(k))
	}
	sr.Inner = inner
	return sr, nil
}

// PeekRoute reads the inner method and target from the start of a sealed
// request body — its header and first record — opened for whichever of
// infos it was sealed under. It authenticates nothing: it only tells the
// receiver how large a body that route may carry, so the rest of the read
// can be held to it. ok is false when the head isn't in the first record.
func (i *Identity) PeekRoute(prefix []byte, infos ...[]byte) (method, target string, which int, ok bool) {
	if i.encPriv == nil {
		return "", "", 0, false
	}
	for k, info := range infos {
		or, err := NewOpenReader(bytes.NewReader(prefix), i.encPriv, info)
		if err != nil || or.next() != nil {
			continue
		}
		p := or.plain
		if len(p) < 4 {
			continue
		}
		n := binary.BigEndian.Uint32(p)
		if n > maxHead || int(n) > len(p)-4 {
			continue
		}
		var ir innerRequest
		if json.Unmarshal(p[4:4+n], &ir) != nil || ir.Method == "" || len(ir.Target) == 0 || ir.Target[0] != '/' {
			continue
		}
		u, err := url.ParseRequestURI(ir.Target)
		if err != nil {
			continue
		}
		return ir.Method, u.Path, k, true
	}
	return "", "", 0, false
}

// PeekPrefix is how much of a sealed body PeekRoute needs: its header and
// one whole record.
const PeekPrefix = 1 + sealEncLen + 4 + sealMaxRecord

// sealedLimitFor is the most a sealed body carrying maxBody bytes of
// plaintext can weigh: a header, and per record a length and a tag.
func sealedLimitFor(maxBody int64) int64 {
	return maxBody + 1 + 32 + (maxBody/SealChunk+1)*(4+16)
}

// SealedLimit is sealedLimitFor for fogd's server.
func SealedLimit(maxBody int64) int64 { return sealedLimitFor(maxBody) }

// SealedHandler serves the fed plane as the holder of id around h, for
// anything outside fogd's server that has to (tests above all): it opens
// each outer request (verifying nothing about its sender), answers the hello
// itself, hands h the inner request, and seals whatever h writes as the
// answer. h sees inner paths, methods and headers, as fogd's handlers do.
func SealedHandler(id *Identity, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, err := io.ReadAll(io.LimitReader(r.Body, sealedLimitFor(maxReplyBody)))
		if err != nil {
			return
		}
		sr, _, err := OpenSealedRequest(id, r, wire, id.InstanceID)
		if err != nil || sr.Requester == nil {
			http.Error(w, "unsealed", http.StatusForbidden)
			return
		}
		rec := httptest.NewRecorder()
		if sr.Hello {
			rec.WriteString("{}")
		} else {
			h.ServeHTTP(rec, sr.Inner)
		}
		WriteRecorded(w, id, sr, rec.Code, rec.Header(), rec.Body.Bytes())
	})
}

// WriteRecorded writes a recorded answer to sr through the envelope.
func WriteRecorded(w http.ResponseWriter, id *Identity, sr *SealedRequest, status int, hdr http.Header, body []byte) {
	sum := sha256.Sum256(body)
	hdr = hdr.Clone()
	hdr.Del("Content-Length")
	WriteSealedAnswer(w, id, sr.Requester, sr.AnswerInfo(id.InstanceID), sr.Target, sr.Bound, status, hdr, hex.EncodeToString(sum[:]), bytes.NewReader(body))
}

// errNotEnvelope: an answer that isn't the envelope's 200 — a proxy's page,
// a redirect, a fog from before the envelope.
var errNotEnvelope = errors.New("the answer is not a sealed envelope")
