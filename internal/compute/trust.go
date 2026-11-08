package compute

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"hash"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/scrypt"
)

// A machine and fog prove themselves to each other, over plain http on a
// LAN anyone may be on. No secret ever crosses the wire in the clear:
//
//   - Pairing. The one-time code is a secret both sides hold and neither
//     sends. Each code has a public random salt; the machine proves it
//     knows the code with an HMAC under the code stretched with that salt
//     (JoinKey) over a nonce of its own, one attempt per live code's salt.
//     It makes its own credential, and sends fog only the credential's
//     digest (what fog keeps, and the key every later MAC is made with),
//     sealed under a key derived from JoinKey and the nonce. fog proves it
//     holds the code too, over its answer, which carries fog's ed25519
//     instance key; the machine pins that key.
//   - Rotation. The machine's first poll after pairing swaps that digest for
//     one derived from an X25519 exchange (RotatedDigest), so cracking the
//     code of a recorded pairing, later, yields a credential fog no longer
//     takes.
//   - Polls. The machine MACs each request with the credential's digest
//     (RequestMAC) over its headers and its body's declared type and
//     digest, with a fresh nonce; fog checks it all before reading the
//     body, and signs its answer, bound to the machine and that nonce,
//     with its instance key. The machine reads nothing that isn't signed
//     so. Only a signed "unknown machine" unpairs it; a request fog
//     couldn't authenticate (tampered with, replayed, or from a clock
//     that's off) is a signed refusal the machine retries.
//   - A run's data. Every request on the data route is MACed by the run's
//     machine (DataMAC); the run's token only names the run, and appears
//     in no process's arguments (the node's data proxy adds it).
//   - Inference. fog MACs each request it relays to a machine's model
//     server (InferenceMAC), checked before its body is read. No bearer is
//     ever sent.
//   - Replays. Each side takes a nonce once, and nothing stamped before
//     it started (Fresh), since it kept no record from before.
//   - Updates. A fog-node build is installed only if its digest is in a
//     release manifest signed with fog's release key, which the build
//     itself pins (internal/selfupdate), whoever served it, and only if
//     it is newer than the build running.

// Headers on a poll: the machine's nonce, and fog's signature over the
// answer.
const (
	HeaderNodeNonce    = "Fog-Node-Nonce"
	HeaderFogSignature = "Fog-Compute-Signature"
	// HeaderNodeID names the machine asking, so a 401 can be signed for
	// it (its credential, not this, is what authenticates it).
	HeaderNodeID = "Fog-Node"
)

// Labels keep each signed or MACed message from being taken for another.
const (
	joinRequestLabel = "fog-compute/1 join request"
	joinAnswerLabel  = "fog-compute/1 join answer"
	credentialLabel  = "fog-compute/1 credential"
	// AnswerLabel is the transcript label of a signed poll answer.
	AnswerLabel = "fog-compute/1 poll answer"
)

// joinCost is the scrypt cost that stretches a pairing code into its key:
// about a tenth of a second, once per side per pairing (per live code, for
// the machine). A variable only so a test binary can lower it
// (CheapJoinKeyForTests).
var joinCost = 1 << 15

// CheapJoinKeyForTests makes stretching a pairing code cost ~1ms instead of
// ~250ms under -race, for the rest of this test binary. Both sides of a
// pairing must stretch alike, so it panics outside a test. Call it from a
// test file's init.
func CheapJoinKeyForTests() {
	if !testing.Testing() {
		panic("compute.CheapJoinKeyForTests called outside a test binary")
	}
	joinCost = 1 << 10
}

// NewSalt is a pairing code's own salt, made with the code and public: no
// two codes stretch alike, so nothing computed against one recorded
// pairing helps against another.
func NewSalt() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hexString(b)
}

// JoinKey is the key a pairing code stretches to under its salt; fog keeps
// only this.
func JoinKey(code, salt string) []byte {
	k, err := scrypt.Key([]byte(NormalizeCode(code)), []byte("fog-compute/1 pairing code\n"+salt), joinCost, 8, 1, 32)
	if err != nil {
		panic(err) // only for parameters out of range, which these aren't
	}
	return k
}

// NormalizeCode reads a code however it was typed: case, dashes, spaces.
func NormalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

func hmacOf(key []byte, parts ...string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(strings.Join(parts, "\n")))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// JoinProof is the machine's proof that it holds the code, over its nonce.
func JoinProof(key []byte, nonce string) string { return hmacOf(key, joinRequestLabel, nonce) }

// JoinConfirm is fog's proof, over everything its answer says and the
// credential it took, that it holds the code too.
func JoinConfirm(key []byte, nonce, credential string, a JoinAnswer) string {
	return hmacOf(key, joinAnswerLabel, nonce, credential, a.Node, a.FogKey)
}

// credentialKey seals a credential's digest at pairing: only the code's
// holders can derive it, and it's new for every nonce.
func credentialKey(key []byte, nonce string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(credentialLabel + "\n" + nonce))
	return h.Sum(nil)
}

// SealCredential is the credential's digest as the machine sends it at
// pairing: AES-GCM under credentialKey, bound to the nonce.
func SealCredential(key []byte, nonce, tokenDigest string) string {
	aead := gcm(credentialKey(key, nonce))
	iv := make([]byte, aead.NonceSize())
	rand.Read(iv)
	return base64.RawURLEncoding.EncodeToString(aead.Seal(iv, iv, []byte(tokenDigest), []byte(nonce)))
}

// OpenCredential is fog's half: the digest a machine sealed, or an error.
func OpenCredential(key []byte, nonce, sealed string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	aead := gcm(credentialKey(key, nonce))
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errors.New("a sealed credential that isn't one")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(nonce))
	if err != nil || len(plain) != 64 || strings.Trim(string(plain), "0123456789abcdef") != "" {
		return "", errors.New("a sealed credential that isn't one")
	}
	return string(plain), nil
}

func gcm(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // a 32-byte key always makes one
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
}

// macEqual compares two MACs in constant time.
func macEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// CheckJoinAnswer verifies fog's answer to a pairing that sent credential,
// and answers fog's key to pin.
func CheckJoinAnswer(key []byte, nonce, credential string, a JoinAnswer) (ed25519.PublicKey, error) {
	if !macEqual(a.Confirm, JoinConfirm(key, nonce, credential, a)) {
		return nil, errors.New("the answer to pairing isn't from the fog that made the code (someone between this machine and fog?)")
	}
	pub, err := ParseFogKey(a.FogKey)
	if err != nil {
		return nil, err
	}
	return pub, nil
}

// ParseFogKey reads fog's instance key, base64 of its 32 bytes.
func ParseFogKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("fog's key isn't one")
	}
	return ed25519.PublicKey(raw), nil
}

// NewNonce is a fresh random nonce.
func NewNonce() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AnswerNonce is what a poll answer's signature is bound to: the machine
// asking, and its nonce for this one poll.
func AnswerNonce(node, nonce string) string { return node + "\n" + nonce }

// VerifyAnswer checks fog's signature over a poll answer's exact bytes.
func VerifyAnswer(pub ed25519.PublicKey, node, nonce string, body []byte, sigB64 string) bool {
	if len(pub) != ed25519.PublicKeySize || nonce == "" || sigB64 == "" {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, transcript(AnswerLabel, AnswerNonce(node, nonce), body), sig)
}

// transcript is what fed's SignTranscript signs, so fog's instance identity
// can sign an answer as it signs any transcript.
func transcript(label, nonce string, doc []byte) []byte {
	sum := sha256.Sum256(doc)
	return []byte(label + "\n" + nonce + "\n" + hexString(sum[:]))
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// Machine requests. A machine never sends its credential, nor its digest
// in the clear: it MACs each poll with the digest over who it is, when, a
// nonce, and the body. fog takes each nonce once inside the skew window.
// Someone answering in fog's place learns nothing it can use as the
// machine.
const (
	HeaderNodeTime = "Fog-Node-Time"
	HeaderNodeAuth = "Fog-Node-Auth"
	requestLabel   = "fog-compute/1 request"
	dataLabel      = "fog-compute/1 data"
	rotateLabel    = "fog-compute/1 rotate"
	inferenceLabel = "fog-compute/1 inference"
	// RequestSkew is how far a machine's clock may be from fog's.
	RequestSkew = 5 * time.Minute
)

// TokenDigest is what fog keeps of a machine's credential, hex.
func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hexString(sum[:])
}

// HeaderBodySHA256 declares a request's body digest, which its MAC covers:
// a receiver checks the MAC, nonce and time from the headers before it
// reads a byte of the body, and the body against the digest after.
const HeaderBodySHA256 = "Fog-Body-SHA256"

// BodySHA256 is a body's digest as HeaderBodySHA256 carries it.
func BodySHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hexString(sum[:])
}

// RequestMAC is a machine's proof of its credential over one poll: who,
// when, its nonce, and its body's type and digest.
func RequestMAC(tokenDigest, node, nonce, at, contentType, bodySHA string) string {
	return hmacOf([]byte(tokenDigest), requestLabel, node, nonce, at, contentType, bodySHA)
}

// DataMAC is a machine's proof of its credential over one request on a
// run's data route: who, when, its nonce, the method, the target (the
// run's token and the app's path in it), and an upload's declared body
// digest ("" for a read). fog checks an upload's bytes against it as they
// stream to the app, and cuts the upload off if they differ. Inputs (what
// the app sends back) aren't covered: they're ranges of files read as
// they arrive, and anything checked at the end would come after ffmpeg
// had used them.
func DataMAC(tokenDigest, node, nonce, at, method, target, bodySHA string) string {
	return hmacOf([]byte(tokenDigest), dataLabel, node, nonce, at, method, target, bodySHA)
}

// ErrBodyMismatch is a body that wasn't the one its MAC declared.
var ErrBodyMismatch = errors.New("the body isn't the one its sender declared")

// DigestReader passes a body of Size bytes through, checking it against
// the digest declared before it hands on the read that completes it: a
// body that isn't the one declared never arrives whole, so whatever it
// streams to is cut off short of its length.
type DigestReader struct {
	R    io.Reader
	Want string
	Size int64
	h    hash.Hash
	n    int64
}

func (d *DigestReader) Read(p []byte) (int, error) {
	if d.h == nil {
		d.h = sha256.New()
	}
	if left := d.Size - d.n; int64(len(p)) > left {
		p = p[:left]
	}
	if len(p) == 0 {
		if hexString(d.h.Sum(nil)) != d.Want {
			return 0, ErrBodyMismatch
		}
		return 0, io.EOF
	}
	n, err := d.R.Read(p)
	d.h.Write(p[:n])
	d.n += int64(n)
	if d.n == d.Size && hexString(d.h.Sum(nil)) != d.Want {
		return 0, ErrBodyMismatch
	}
	if err == io.EOF && d.n < d.Size {
		return n, io.ErrUnexpectedEOF
	}
	if d.n == d.Size {
		return n, io.EOF
	}
	return n, err
}

// RotatedDigest is the credential digest a rotation agrees on: from an
// X25519 secret only the two ends of that exchange hold, so recording the
// pairing and cracking its code later yields nothing current.
func RotatedDigest(shared []byte, node, oldDigest, machinePub, fogPub string) string {
	h := hmac.New(sha256.New, shared)
	h.Write([]byte(strings.Join([]string{rotateLabel, node, oldDigest, machinePub, fogPub}, "\n")))
	return hexString(h.Sum(nil))
}

// Refusals fog gives a machine's poll, as the JSON "code" of a signed
// answer. Only CodeUnknownMachine unpairs it; the others are transient.
const (
	// CodeUnknownMachine: fog has no machine of that id (never paired, or
	// removed). Signed with a 401.
	CodeUnknownMachine = "unknown_machine"
	// CodeBadRequest: the MAC didn't verify (a tampered request, or a
	// credential that isn't this machine's). Signed with a 403.
	CodeBadRequest = "unauthenticated"
	// CodeReplay: a nonce fog has already taken. Signed with a 403.
	CodeReplay = "replayed"
	// CodeClock: the request's time is outside RequestSkew of fog's.
	// Signed with a 403; Settings › Compute says the machine's clock is
	// off.
	CodeClock = "clock"
)

// InferenceKey is the key a machine's inference listener checks fog's MAC
// with, derived from the credential's digest (never itself sent).
func InferenceKey(tokenDigest string) string {
	sum := sha256.Sum256([]byte(inferenceLabel + "\n" + tokenDigest))
	return hexString(sum[:])
}

// Headers on a request fog relays to a machine's inference listener.
const (
	HeaderInferenceNonce = "Fog-Inference-Nonce"
	HeaderInferenceTime  = "Fog-Inference-Time"
	HeaderInferenceAuth  = "Fog-Inference-Auth"
)

// InferenceMAC is fog's proof of the inference key over one relayed
// request: its method, target (path and query), time, nonce, and its
// body's type and digest.
func InferenceMAC(key, nonce, at, method, target, contentType, bodySHA string) string {
	return hmacOf([]byte(key), inferenceLabel, nonce, at, method, target, contentType, bodySHA)
}

// Fresh reports whether a request stamped at may be taken at now: within
// RequestSkew either way, and not from before the receiver started
// (epoch), when it could have been taken already by a process that kept
// no record of it.
func Fresh(at string, now, epoch time.Time) (time.Time, bool) {
	when, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return when, false
	}
	off := when.Sub(now)
	return when, off <= RequestSkew && off >= -RequestSkew && !when.Before(epoch)
}

// MACEqual compares two MACs in constant time.
func MACEqual(a, b string) bool { return macEqual(a, b) }

// Nonces remembers nonces taken within a window, so none is taken twice.
type Nonces struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// Take records nonce at now, answering false when it was already taken
// within window.
func (n *Nonces) Take(nonce string, now time.Time, window time.Duration) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[string]time.Time{}
	}
	for k, t := range n.seen {
		if now.Sub(t) > window {
			delete(n.seen, k)
		}
	}
	if _, ok := n.seen[nonce]; ok {
		return false
	}
	n.seen[nonce] = now
	return true
}

// KeySigner signs as fed.Identity does, with a key of its own: for the
// e2e harness and tests, which have no instance identity.
type KeySigner struct{ Key ed25519.PrivateKey }

// NewKeySigner is a KeySigner with a fresh key.
func NewKeySigner() KeySigner {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return KeySigner{Key: priv}
}

func (k KeySigner) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(k.Key.Public().(ed25519.PublicKey))
}

func (k KeySigner) SignTranscript(label, nonce string, doc []byte) string {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.Key, transcript(label, nonce, doc)))
}
