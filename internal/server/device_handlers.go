// Device authorization (RFC 8628, docs/signin-contract.md): the app
// server asks for a code, its television shows it, the person approves it on
// their phone at fog.<domain>/device, and the app's polling turns into
// tokens. fog's page says which app and which device is asking, because the
// client is the app.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// handleDeviceAuthorization starts a device sign-in for an authenticated
// client that registered for the grant.
func (s *Server) handleDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	iss, ok := s.requireIssuer(w)
	if !ok {
		return
	}
	limitForm(w, r)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "malformed body"})
		return
	}
	sc, oerr := s.authClient(r)
	if oerr != nil {
		writeOAuthError(w, oerr)
		return
	}
	if !sc.DeviceGrant {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrUnauthorizedClient, Description: "this client is not registered for device sign-in"})
		return
	}
	scope, perr := oidc.ParseScope(r.PostForm.Get("scope"), toOIDCClient(sc))
	if perr != nil {
		writeOAuthError(w, perr)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("device_name"))
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}
	deviceCode := randx.Token()
	userCode := ""
	for range 5 {
		userCode = oidc.NewUserCode()
		err := s.store.CreateDeviceCode(auth.DigestToken(deviceCode), userCode, sc.ClientID, name, mustMarshal(scope), oidc.DeviceCodeTTL)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, err)
			return
		}
		userCode = ""
	}
	if userCode == "" {
		writeError(w, errors.New("could not draw an unused user code"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          iss + "/device",
		"verification_uri_complete": iss + "/device?" + url.Values{"user_code": {userCode}}.Encode(),
		"expires_in":                int(oidc.DeviceCodeTTL.Seconds()),
		"interval":                  int(oidc.DevicePollPeriod.Seconds()),
	})
}

// tokenForDevice is the token endpoint's device-code grant (RFC 8628 §3.4).
func (s *Server) tokenForDevice(w http.ResponseWriter, iss string, sc *store.OIDCClient, form url.Values) {
	for k, v := range form {
		if len(v) > 1 {
			writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "parameter " + k + " repeated"})
			return
		}
	}
	code := form.Get("device_code")
	if code == "" {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "device_code is required"})
		return
	}
	digest := auth.DigestToken(code)
	d, err := s.store.PollDeviceCode(digest, sc.ClientID)
	if err != nil {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "unknown device code"})
		return
	}
	switch {
	case time.Now().After(d.ExpiresAt):
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrExpiredToken, Description: "the code expired; start again"})
		return
	case d.Status == store.DeviceStatusDenied:
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrAccessDenied, Description: "the person said no"})
		return
	case d.Status == store.DeviceStatusUsed:
		// A device code redeemed twice: what the first redemption minted is
		// revoked, as a replayed authorization code's is.
		s.store.RevokeOIDCAccessTokensByCode(digest)
		s.auditAs("app:"+sc.ClientID, "oidc.device.replayed", sc.ClientID, nil)
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "this code was already used"})
		return
	case d.Status == store.DeviceStatusPending:
		if !d.LastPollAt.IsZero() && time.Since(d.LastPollAt) < oidc.DevicePollPeriod {
			writeOAuthError(w, &oidc.Error{Code: oidc.ErrSlowDown, Description: "poll no faster than the interval"})
			return
		}
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrAuthorizationPending, Description: "waiting for the person to approve"})
		return
	}
	if err := s.store.SpendDeviceCode(digest); err != nil {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "this code was already used"})
		return
	}
	var grant oidc.Grant
	if json.Unmarshal([]byte(d.Grant), &grant) != nil {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "unreadable approval"})
		return
	}
	s.issueTokens(w, iss, sc, d.SessionID, grant, digest)
}

// deviceRequest is a pending device sign-in as the approval page shows it.
type deviceRequest struct {
	UserCode string `json:"userCode"`
	App      string `json:"app"`
	Device   string `json:"device"`
}

// deviceMissKey is the bucket a person's wrong user codes are charged to,
// on the sign-in limiter: ten in fifteen minutes. A user code is short by
// design (RFC 8628 §5.1), so guessing is bounded by misses, not requests.
func deviceMissKey(p *store.Person) string { return "device-miss:" + p.Handle }

// deviceCodeFor finds the waiting device sign-in for a typed code, charging
// a miss to the person and refusing once they have missed too often —
// before looking, so a guesser at the limit learns nothing.
func (s *Server) deviceCodeFor(w http.ResponseWriter, p *store.Person, typed string) (*store.DeviceCode, bool) {
	if !limit(w, s.appAPILimiter, "device:"+p.Handle, "too many codes tried; wait a minute") {
		return nil, false
	}
	misses := bucket{l: s.signinLimits().account, key: deviceMissKey(p)}
	if refuseLimited(w, "too many wrong codes; try again later", misses) {
		return nil, false
	}
	code := oidc.NormalizeUserCode(typed)
	var d *store.DeviceCode
	err := store.ErrNotFound
	if code != "" {
		d, err = s.store.DeviceCodeByUserCode(code)
	}
	if err != nil {
		if code == "" {
			badRequest(w, "that isn't a code fog shows")
		} else {
			notFound(w, "no device is waiting for that code — it may have expired")
		}
		return nil, false
	}
	refund(misses)
	return d, true
}

// fromFogsPage refuses a request that doesn't carry an Origin naming fog's
// own host. The global check refuses a mismatched Origin; this also refuses
// none, for actions a page on a sibling host must not be able to start.
func fromFogsPage(w http.ResponseWriter, r *http.Request) bool {
	if o := r.Header.Get("Origin"); o == "" || !sameHost(o, r.Host) {
		forbidden(w, "this must come from fog's own page")
		return false
	}
	return true
}

// handleDeviceLookup is the /device page asking what a typed code is for.
//
// A POST, from fog's own page only: a sibling host's page shares fog's site,
// so the browser sends fog's cookie with its requests, and a GET it could
// fire would spend the person's wrong-code budget for them.
func (s *Server) handleDeviceLookup(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	if !fromFogsPage(w, r) {
		return
	}
	var req struct {
		UserCode string `json:"userCode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	d, ok := s.deviceCodeFor(w, pc.Person, req.UserCode)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, deviceRequest{UserCode: d.UserCode, App: d.ClientID, Device: d.DeviceName})
}

// handleDeviceDecide records the person's yes or no. A yes is the same
// policy decision a sign-in in the browser gets: who may use the app, in
// what role, with what proof.
func (s *Server) handleDeviceDecide(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	if !fromFogsPage(w, r) {
		return
	}
	var req struct {
		UserCode string `json:"userCode"`
		Approve  bool   `json:"approve"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	d, ok := s.deviceCodeFor(w, pc.Person, req.UserCode)
	if !ok {
		return
	}
	code := d.UserCode
	if !req.Approve {
		if err := s.store.DecideDeviceCode(code, false, 0, ""); err != nil {
			writeError(w, err)
			return
		}
		writeOK(w)
		return
	}
	sc, client := s.lookupClient(d.ClientID)
	if client == nil {
		notFound(w, "that app no longer signs in with fog")
		return
	}
	dec, err := s.decideFor(client.AppID, pc.Person, pc.Session, identity.Request{}, client)
	if err != nil {
		writeError(w, err)
		return
	}
	switch dec.Outcome {
	case identity.Deny:
		forbidden(w, d.ClientID+" isn't shared with you")
		return
	case identity.StepUp:
		writeJSON(w, http.StatusConflict, map[string]any{"error": d.ClientID + " asks for your passkey first", "code": "step_up"})
		return
	}
	var scope []string
	json.Unmarshal([]byte(d.Scope), &scope)
	grant := oidc.Grant{Request: oidc.AuthorizeRequest{ClientID: sc.ClientID, Scope: scope}, Role: dec.Role,
		SID: pc.Session.SID, AMR: pc.Session.AMR, ACR: dec.ACR, AuthTime: pc.Session.AuthTime}
	if err := s.store.DecideDeviceCode(code, true, pc.Session.ID, mustMarshal(grant)); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "oidc.device.approve", d.ClientID, map[string]any{"device": d.DeviceName})
	writeOK(w)
}
