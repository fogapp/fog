// Why a peer refused a signed request — and, specifically, how to tell
// "they no longer know us" apart from "this one request was bad".
//
// Every rejection on the fed plane is a 403, because every rejection means
// the same thing to HTTP. They do not mean the same thing to an operator: a
// clock five minutes out of skew and an unpairing look identical over the
// wire unless the answer says which it was. So the fed plane carries a
// machine-readable code alongside the human sentence, and the dialling side
// reads it.
//
// This is the whole disconnect protocol. Nothing is pushed to a peer when
// the operator unpairs it: that would mean dialling an instance that has
// just cut us off, holding a key it no longer honours, and would hand a
// hostile peer a signal it hasn't earned. The refusal already crosses the
// wire on the next heartbeat — it only had to say why.

package fed

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Rejection codes. These strings are part of the fed wire contract: a
// receiving fog matches on them, so they are additive-only.
const (
	// CodeUnknownPeer: the signature was never checked, because the caller's
	// instance ID matches no peer row. The pairing is gone from their side.
	CodeUnknownPeer = "unknown_peer"
	// CodeMissingInstance: no Fog-Instance header. A caller bug, not a peer
	// relationship fact.
	CodeMissingInstance = "missing_instance"
	// CodeKeyUnusable: their stored copy of our key won't parse. Corruption
	// on their side; the row still exists.
	CodeKeyUnusable = "key_unusable"
	// CodeBadSignature: the row and key are fine and the signature failed —
	// clock skew, a tampered request, or a rotated key.
	CodeBadSignature = "bad_signature"
	// CodeReplayed: the signature verified, and has been seen before inside
	// its window — the same signed request, sent twice.
	CodeReplayed = "replayed"
	// CodeEncryptionRequired: the request presented no encryption key, or
	// one its identity doesn't bind, or a body not sealed to this instance —
	// a fog that doesn't seal bodies. There is no unsealed fallback.
	CodeEncryptionRequired = "encryption_required"
	// CodeEncKeyChanged: the request presented an encryption key other than
	// the one pinned for its instance.
	CodeEncKeyChanged = "encryption_key_changed"
	// CodeUnsealed: the request body doesn't open for this exchange —
	// tampered with, or moved from another.
	CodeUnsealed = "unsealed"
	// CodeWrongRecipient: the request's target names another recipient
	// (its ?to=), or none: signed for some other fog.
	CodeWrongRecipient = "wrong_recipient"
)

// RemoteError is a non-2xx answer from a peer's fed plane, with the code
// parsed out when the peer sent one. Peers older than this code send none,
// which is why Code is allowed to be empty and never assumed.
type RemoteError struct {
	Status  int
	Code    string
	Message string
	// Verified: the refusal carried a valid ANSWER signature from the
	// peer's stored key, bound to this very request and its status.
	// Only a verified refusal is a statement by the peer; anything else may
	// have come from whoever answers at its address now.
	Verified bool
}

func (e *RemoteError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("peer answered %s", http.StatusText(e.Status))
	}
	return fmt.Sprintf("peer answered %d: %s", e.Status, e.Message)
}

// newRemoteError reads a rejection body, which is a JSON object with error
// and code fields when a fog wrote it and anything at all when a proxy did.
func newRemoteError(status int, body []byte) *RemoteError {
	e := &RemoteError{Status: status, Message: strings.TrimSpace(string(body))}
	var parsed struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &parsed) == nil && (parsed.Error != "" || parsed.Code != "") {
		e.Code, e.Message = parsed.Code, parsed.Error
	}
	return e
}

// Unpaired reports whether err is a peer stating it has no record of this
// instance — the one rejection that means the relationship ended rather
// than that a request went wrong.
//
// A gateway's own 403 is deliberately not enough, and neither is a body that
// merely says the code: only a refusal signed by the peer's own key for this
// request counts. A misconfigured reverse proxy in front of a healthy peer,
// or a stranger answering at an address the peer left, can never be read as
// an unpairing.
func Unpaired(err error) bool {
	var re *RemoteError
	return errors.As(err, &re) && re.Verified && re.Code == CodeUnknownPeer
}
