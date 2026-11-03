package oidc

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
	"regexp"
)

// verifierRe is RFC 7636 §4.1: 43–128 characters of the unreserved set.
var verifierRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// ChallengeFor is the S256 code challenge for a verifier.
func ChallengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyPKCE checks a code verifier against the challenge the
// authorization request carried, in constant time.
func VerifyPKCE(challenge, verifier string) bool {
	if !verifierRe.MatchString(verifier) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(ChallengeFor(verifier)), []byte(challenge)) == 1
}

// TokenRequest is a validated authorization_code grant. The code itself is
// redeemed by the caller, which owns single use.
type TokenRequest struct {
	Code         string
	RedirectURI  string
	CodeVerifier string
}

// ParseTokenRequest validates a token endpoint body for an authenticated
// client. Only authorization_code is granted.
func ParseTokenRequest(form url.Values) (TokenRequest, *Error) {
	for k, v := range form {
		if len(v) > 1 {
			return TokenRequest{}, errorf(ErrInvalidRequest, 400, false, "parameter %s repeated", k)
		}
	}
	if form.Get("grant_type") != "authorization_code" {
		return TokenRequest{}, errorf(ErrUnsupportedGrantType, 400, false, "only authorization_code is supported")
	}
	t := TokenRequest{Code: form.Get("code"), RedirectURI: form.Get("redirect_uri"), CodeVerifier: form.Get("code_verifier")}
	switch {
	case t.Code == "":
		return t, errorf(ErrInvalidRequest, 400, false, "code is required")
	case t.RedirectURI == "":
		return t, errorf(ErrInvalidRequest, 400, false, "redirect_uri is required")
	case t.CodeVerifier == "":
		return t, errorf(ErrInvalidRequest, 400, false, "code_verifier is required")
	}
	return t, nil
}

// CheckRedemption is what the token endpoint asks of a redeemed code: it was
// minted for this client and this redirect URI, and the verifier matches.
// Every failure is the same invalid_grant, so a probe learns nothing about
// which part was wrong.
func CheckRedemption(t TokenRequest, clientID string, a AuthorizeRequest) *Error {
	ok := a.ClientID == clientID &&
		subtle.ConstantTimeCompare([]byte(a.RedirectURI), []byte(t.RedirectURI)) == 1 &&
		VerifyPKCE(a.CodeChallenge, t.CodeVerifier)
	if !ok {
		return errorf(ErrInvalidGrant, 400, false, "the code is invalid, expired, or was issued to another request")
	}
	return nil
}
