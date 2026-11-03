package oidc

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AuthorizeRequest is a validated authorization request: every field below
// has already been checked against the profile and the client, so a
// handler holding one never re-checks.
type AuthorizeRequest struct {
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	Scope         []string  `json:"scope"`
	State         string    `json:"state,omitempty"`
	Nonce         string    `json:"nonce,omitempty"`
	CodeChallenge string    `json:"code_challenge"`
	Prompt        string    `json:"prompt,omitempty"` // "", "none" or "login"
	MaxAge        *Duration `json:"max_age,omitempty"`
	ACR           string    `json:"acr,omitempty"`
	LoginHint     string    `json:"login_hint,omitempty"`
	Pushed        bool      `json:"pushed,omitempty"`
	// Began is when fog first held this request (unix milliseconds): set
	// by the server, never read from a client, so "sign in again" can be
	// told apart from "signed in long ago" after the person returns.
	Began int64 `json:"began,omitempty"`
}

// Duration is a whole number of seconds, as max_age carries it.
type Duration int64

func (d Duration) Std() time.Duration { return time.Duration(d) * time.Second }

// Known ACR values.
const (
	ACRAny               = "urn:fog:acr:pwd"
	ACRPhishingResistant = "urn:fog:acr:phr"
)

// PKCE code challenges are base64url SHA-256: exactly 43 characters.
var challengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

const (
	maxStateLen = 1024
	maxNonceLen = 512
	maxHintLen  = 256
)

// ParseAuthorize validates an authorization request's parameters for
// client c — from the query of /oauth2/authorize, or the body of a pushed
// request (pushed = true). A nil client means client_id named nobody.
//
// The order is the RFC's: until the client and its redirect URI are known
// good, errors are not RedirectSafe and are shown on fog's own page.
func ParseAuthorize(q url.Values, c *Client, pushed bool) (AuthorizeRequest, *Error) {
	for k, v := range q {
		if len(v) > 1 {
			return AuthorizeRequest{}, errorf(ErrInvalidRequest, 400, false, "parameter %s repeated", k)
		}
	}
	if c == nil {
		return AuthorizeRequest{}, errorf(ErrInvalidClient, 400, false, "unknown client_id")
	}
	if q.Get("client_id") != c.ID {
		return AuthorizeRequest{}, errorf(ErrInvalidRequest, 400, false, "client_id does not match")
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" || !c.AllowsRedirect(redirect) {
		return AuthorizeRequest{}, errorf(ErrInvalidRequest, 400, false, "redirect_uri is not registered for this client")
	}
	req := AuthorizeRequest{ClientID: c.ID, RedirectURI: redirect, State: q.Get("state"), Pushed: pushed}

	// From here, errors go back to the client.
	fail := func(code Code, format string, args ...any) (AuthorizeRequest, *Error) {
		e := errorf(code, 400, true, format, args...)
		return req, e
	}
	if len(req.State) > maxStateLen {
		req.State = ""
		return fail(ErrInvalidRequest, "state too long")
	}
	if c.RequirePAR && !pushed {
		return fail(ErrInvalidRequest, "this client must use pushed authorization requests")
	}
	if q.Has("request") || q.Has("request_uri") {
		return fail(ErrInvalidRequest, "request objects are not supported; use pushed authorization requests")
	}
	if q.Get("response_type") != "code" {
		return fail(ErrUnsupportedResponseType, "only response_type=code is supported")
	}
	if rm := q.Get("response_mode"); rm != "" && rm != "query" {
		return fail(ErrInvalidRequest, "only response_mode=query is supported")
	}
	scope, err := ParseScope(q.Get("scope"), c)
	if err != nil {
		return req, err
	}
	req.Scope = scope

	if q.Get("code_challenge_method") != "S256" {
		return fail(ErrInvalidRequest, "PKCE is required, with code_challenge_method=S256")
	}
	req.CodeChallenge = q.Get("code_challenge")
	if !challengeRe.MatchString(req.CodeChallenge) {
		return fail(ErrInvalidRequest, "code_challenge must be a base64url SHA-256 digest")
	}

	req.Nonce = q.Get("nonce")
	if len(req.Nonce) > maxNonceLen {
		return fail(ErrInvalidRequest, "nonce too long")
	}

	switch p := q.Get("prompt"); p {
	case "", "none", "login":
		req.Prompt = p
	default:
		// consent and select_account mean nothing on a household's fog;
		// several values together is a request we would answer wrongly.
		return fail(ErrInvalidRequest, "prompt must be none or login")
	}

	if raw := q.Get("max_age"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return fail(ErrInvalidRequest, "max_age must be a non-negative integer")
		}
		d := Duration(n)
		req.MaxAge = &d
	}

	for _, v := range strings.Fields(q.Get("acr_values")) {
		switch v {
		case ACRPhishingResistant:
			req.ACR = ACRPhishingResistant
		case ACRAny:
			if req.ACR == "" {
				req.ACR = ACRAny
			}
		}
		// Unknown values are voluntary claims we cannot meet; OIDC says to
		// proceed and report the acr actually achieved.
	}

	req.LoginHint = q.Get("login_hint")
	if len(req.LoginHint) > maxHintLen {
		return fail(ErrInvalidRequest, "login_hint too long")
	}
	return req, nil
}

// Redirect builds the URL a successful or failed authorization sends the
// browser to: the client's redirect URI with code (or error), state, and
// iss (RFC 9207). The redirect URI's own query is kept.
func Redirect(redirectURI, issuer, state string, params url.Values) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	return u.String()
}

// ErrorRedirect is Redirect for an error.
func ErrorRedirect(redirectURI, issuer, state string, e *Error) string {
	p := url.Values{"error": {string(e.Code)}}
	if e.Description != "" {
		p.Set("error_description", e.Description)
	}
	return Redirect(redirectURI, issuer, state, p)
}
