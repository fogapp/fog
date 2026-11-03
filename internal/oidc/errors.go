// Package oidc is fog's OpenID Connect provider protocol: the narrow,
// standards-exact profile of docs/signin-contract.md, as pure functions.
// Requests are parsed into typed values in which every invariant already
// holds, or into the RFC error to send; nothing here touches HTTP
// writers, the database or the clock, so every rule is tested directly.
package oidc

import "fmt"

// Code is an error code from the OAuth 2.0 / OIDC registries.
type Code string

const (
	ErrInvalidRequest          Code = "invalid_request"
	ErrInvalidClient           Code = "invalid_client"
	ErrInvalidGrant            Code = "invalid_grant"
	ErrUnauthorizedClient      Code = "unauthorized_client"
	ErrUnsupportedGrantType    Code = "unsupported_grant_type"
	ErrUnsupportedResponseType Code = "unsupported_response_type"
	ErrInvalidScope            Code = "invalid_scope"
	ErrAccessDenied            Code = "access_denied"
	ErrLoginRequired           Code = "login_required"
	ErrInteractionRequired     Code = "interaction_required"
	ErrInvalidRequestURI       Code = "invalid_request_uri"
	ErrInvalidToken            Code = "invalid_token"
	ErrServerError             Code = "server_error"
)

// Error is a protocol error. RedirectSafe says whether it may be returned to
// the client's redirect URI: never when the client or the redirect URI
// itself is in doubt, or fog would become an open redirector (RFC 6749
// §4.1.2.1).
type Error struct {
	Code         Code
	Description  string
	RedirectSafe bool
	// Status is the HTTP status for errors answered directly (the token
	// endpoint, a page), not by redirect.
	Status int
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Description) }

func errorf(code Code, status int, redirectSafe bool, format string, args ...any) *Error {
	return &Error{Code: code, Description: fmt.Sprintf(format, args...), RedirectSafe: redirectSafe, Status: status}
}
