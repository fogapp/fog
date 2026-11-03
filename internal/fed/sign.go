// Signed requests: every fed-plane request after pairing is authenticated
// by an ed25519 signature over method, the full request target (path AND
// query), timestamp, and body digest — no shared secrets stored on either
// side, revocation = deleting the peer row, tamper-evident end to end.
// Signing the whole target, not just the path, means a parameter that names
// what a request acts on — the file behind DELETE …/contributions?path=X —
// is covered too. A receiver refuses any request signature it has already
// accepted inside the skew window (its replay record), and every answer is
// signed bound to the request it answers.
package fed

import (
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	HeaderInstance  = "Fog-Instance"
	HeaderTimestamp = "Fog-Timestamp"
	HeaderSignature = "Fog-Signature"

	// MaxSkew bounds how far a request timestamp may drift from this
	// instance's clock, in either direction.
	MaxSkew = 5 * time.Minute

	sigVersion = "fogfed1"
)

func signingString(method, path, timestamp string, body []byte) []byte {
	digest := sha256.Sum256(body)
	return []byte(sigVersion + "\n" + method + "\n" + path + "\n" + timestamp + "\n" + hex.EncodeToString(digest[:]))
}

// Sign produces the base64url signature value for one request.
func (i *Identity) Sign(method, path, timestamp string, body []byte) string {
	sig := ed25519.Sign(i.private, signingString(method, path, timestamp, body))
	return base64.RawURLEncoding.EncodeToString(sig)
}

// SignRequest stamps the three federation headers onto an outbound request.
// The body must be exactly what will be sent. The timestamp carries
// nanoseconds and is unique within this process, so no two requests this
// fog signs are byte-identical: a receiver can refuse any signature it has
// already seen (its replay record) without ever refusing a genuine request.
func (i *Identity) SignRequest(req *http.Request, body []byte) {
	i.signRequestAt(req, body, uniqueStamp())
}

// signRequestAt signs with a timestamp already taken — the one a sealed
// body was bound to. It also carries this instance's encryption key and its
// binding, so a peer that hasn't pinned it yet can.
func (i *Identity) signRequestAt(req *http.Request, body []byte, ts string) {
	req.Header.Set(HeaderInstance, i.InstanceID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, i.Sign(req.Method, req.URL.RequestURI(), ts, body))
	// The body's digest, declared up front, lets the receiver check the
	// signature from the headers alone, before it reads a byte of the body.
	sum := sha256.Sum256(body)
	req.Header.Set(HeaderContentSHA256, hex.EncodeToString(sum[:]))
	if len(i.EncPublic) > 0 {
		req.Header.Set(HeaderEncKey, i.EncPublicB64())
		req.Header.Set(HeaderEncBinding, i.EncBinding)
	}
}

// SealTo seals a body to a peer's encryption key: what fogd's own fed
// handlers' answers go through.
func (i *Identity) SealTo(pub hpke.PublicKey, info, body []byte) ([]byte, error) {
	return Seal(pub, info, body)
}

// openBody opens a sealed body sent to this instance.
func (i *Identity) openBody(info, sealed []byte) ([]byte, error) {
	if i.encPriv == nil {
		return nil, ErrNoEncryption
	}
	return Open(i.encPriv, info, sealed)
}

// OpenBody is openBody for fogd's handlers: a request body sealed to this
// instance.
func (i *Identity) OpenBody(info, sealed []byte) ([]byte, error) { return i.openBody(info, sealed) }

// openStream opens a sealed stream sent to this instance as it is read.
func (i *Identity) openStream(src io.Reader, info []byte) (*OpenReader, error) {
	if i.encPriv == nil {
		return nil, ErrNoEncryption
	}
	return NewOpenReader(src, i.encPriv, info)
}

var lastStamp atomic.Int64

// uniqueStamp is now as RFC 3339 with nanoseconds, strictly later than any
// stamp it returned before.
func uniqueStamp() string {
	for {
		last := lastStamp.Load()
		now := time.Now().UnixNano()
		if now <= last {
			now = last + 1
		}
		if lastStamp.CompareAndSwap(last, now) {
			return time.Unix(0, now).UTC().Format(time.RFC3339Nano)
		}
	}
}

// HeaderContentSHA256 declares the full body's digest on a streamed,
// possibly partial (Range) answer. The signature covers this digest, not
// the bytes of the range; the receiver verifies the whole file against it
// once it has all of it.
const HeaderContentSHA256 = "Fog-Content-SHA256"

// ErrUnsignedResponse: the answer carries no federation signature —
// something that isn't the peer's fog answering at that address.
var ErrUnsignedResponse = errors.New("response carries no federation signature")

// Answers. Every answer on the fed plane — a 2xx, a 304, a refusal — is
// signed under a pseudo-method no request can carry, over material that
// binds it to the request it answers:
//
//	ANSWER: fogfed1\nANSWER\n<target>\n<ts>\n<sha256hex(status \n instance \n sig \n head)>
//
// instance and sig are the request's Fog-Instance and Fog-Signature; head is
// the answer's head as sent (envelope.go), which carries its status, its
// headers and its body's digest. An answer captured from one exchange
// therefore verifies for no other — not a later request, not another
// caller's request carrying a copied signature — and the status is part of
// what is signed, so a refusal can't be served as a 200.
const methodAnswer = "ANSWER"

// Bound is the request an answer answers, as the answer signs it: the
// requester's Fog-Instance and Fog-Signature headers, either possibly empty.
type Bound struct {
	Instance, Sig string
}

// BoundTo is the binding of a request, from its headers.
func BoundTo(h http.Header) Bound {
	return Bound{Instance: h.Get(HeaderInstance), Sig: h.Get(HeaderSignature)}
}

// RequestDigest names one signed request in a receiver's replay record: by
// the signature's bytes, not its text, so no other spelling of the same
// signature is a different request. ok is false for a signature that isn't
// canonical unpadded base64url of 64 bytes — which Verify refuses anyway.
func RequestDigest(req Bound) (digest string, ok bool) {
	sig, err := base64.RawURLEncoding.Strict().DecodeString(req.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", false
	}
	sum := sha256.Sum256(append([]byte(req.Instance+"\n"), sig...))
	return hex.EncodeToString(sum[:]), true
}

// AnswerMaterial is what an ANSWER signs in place of the bare body.
func AnswerMaterial(status int, req Bound, body []byte) []byte {
	return append([]byte(strconv.Itoa(status)+"\n"+req.Instance+"\n"+req.Sig+"\n"), body...)
}

func (i *Identity) stamp(h http.Header, method, target string, material []byte) {
	// Nanoseconds, like a request's: a heartbeat answer and a heartbeat
	// request from the same fog are ordered by these stamps.
	ts := uniqueStamp()
	h.Set(HeaderInstance, i.InstanceID)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderSignature, i.Sign(method, target, ts, material))
}

// SignAnswer stamps an answer's headers: status and body, bound to req, at
// the target received. A 304's body is the entity tag it confirms.
func (i *Identity) SignAnswer(h http.Header, target string, status int, req Bound, body []byte) {
	i.stamp(h, methodAnswer, target, AnswerMaterial(status, req, body))
}

// VerifyAnswer checks an answer against the peer's key and the request this
// side sent. An unsigned answer is ErrUnsignedResponse.
func VerifyAnswer(pub ed25519.PublicKey, target string, hdr http.Header, status int, req Bound, body []byte, at time.Time) error {
	if hdr.Get(HeaderSignature) == "" {
		return ErrUnsignedResponse
	}
	return Verify(pub, methodAnswer, target, hdr, AnswerMaterial(status, req, body), at)
}

// VerifyDigest is Verify for a body known by its sha256hex: one read and
// hashed as it arrived, never held whole until the signature has verified.
func VerifyDigest(pub ed25519.PublicKey, method, path string, hdr http.Header, digestHex string, at time.Time) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("no usable key to verify against")
	}
	ts := hdr.Get(HeaderTimestamp)
	sigB64 := hdr.Get(HeaderSignature)
	if ts == "" || sigB64 == "" {
		return errors.New("missing federation signature headers")
	}
	when, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return errors.New("invalid federation timestamp")
	}
	if d := at.Sub(when); d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("federation timestamp outside ±%s window", MaxSkew)
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("invalid federation signature encoding")
	}
	msg := []byte(sigVersion + "\n" + method + "\n" + path + "\n" + ts + "\n" + digestHex)
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("federation signature verification failed")
	}
	return nil
}

// Verify checks a request's federation signature against a peer's public
// key. The caller resolves the peer from the Fog-Instance header first —
// identity comes from the verified key, never from the payload.
func Verify(pub ed25519.PublicKey, method, path string, hdr http.Header, body []byte, at time.Time) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("no usable key to verify against")
	}
	ts := hdr.Get(HeaderTimestamp)
	sigB64 := hdr.Get(HeaderSignature)
	if ts == "" || sigB64 == "" {
		return errors.New("missing federation signature headers")
	}
	when, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return errors.New("invalid federation timestamp")
	}
	if d := at.Sub(when); d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("federation timestamp outside ±%s window", MaxSkew)
	}
	// Strict: base64url leaves spare bits in its last character, and a
	// lenient decoder reads several spellings as the same 64 bytes — each
	// one a "different" request to a record keyed on text. One spelling
	// only.
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("invalid federation signature encoding")
	}
	if !ed25519.Verify(pub, signingString(method, path, ts, body), sig) {
		return errors.New("federation signature verification failed")
	}
	return nil
}
