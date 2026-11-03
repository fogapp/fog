// Confirming it's you (docs/signin-contract.md): a signed-in person
// proves themselves again — for an app that asked for a fresh sign-in
// (prompt=login, max_age) or for a passkey (acr phr). The proof is recorded
// on the session they already have, so the sid every app was told stays
// good and nothing signs out.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// reauthKey is the sign-in limiter's bucket for one person's
// confirmations: a stolen session must not be a place to guess the
// password behind it, from however many addresses.
func reauthKey(p *store.Person) string { return "reauth:" + strconv.FormatInt(p.ID, 10) }

func (s *Server) handleReauthPassword(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	limits := s.credentialBuckets(r, reauthKey(pc.Person))
	if refuseLimited(w, "too many attempts; try again later", limits...) {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	creds, _ := s.store.PersonCredentials(pc.Person.ID)
	for _, c := range creds {
		if c.Kind != store.CredentialPassword {
			continue
		}
		caller := s.hashCaller(r, true)
		caller.Account = reauthKey(pc.Person)
		good, err := auth.CheckPasswordAs(req.Password, c.Data, caller)
		if busyHashing(w, err, limits...) {
			return
		}
		if good {
			if err := s.store.UpdateSessionAssurance(pc.Session.ID, []string{"pwd"}); err != nil {
				writeError(w, err)
				return
			}
			refund(limits...)
			s.clearAccount(reauthKey(pc.Person))
			s.audit(r, "people.reauth", pc.Person.Handle, map[string]any{"via": "password"})
			writeOK(w)
			return
		}
	}
	writeErr(w, http.StatusUnauthorized, "that isn't your password")
}

// handleReauthPasskeyBegin asks for one of this person's own passkeys, with
// user verification required: a confirmation that a tap alone could
// satisfy would not be phishing-resistant sign-in, only presence.
func (s *Server) handleReauthPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	rp, err := people.RelyingParty(r, s.baseDomain(), s.householdName())
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	user, _ := s.webAuthnUser(pc.Person)
	if len(user.WebAuthnCredentials()) == 0 {
		writeErr(w, http.StatusPreconditionFailed, "you have no passkey yet; add one from your profile")
		return
	}
	assertion, sess, err := rp.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeError(w, err)
		return
	}
	id := s.ceremonies.Put(people.CeremonyConfirm, sess, pc.Person.ID)
	writeJSON(w, http.StatusOK, passkeyCeremony{Ceremony: id, Options: assertion})
}

func (s *Server) handleReauthPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	// Its own buckets: a passkey confirmation is never held up by password
	// guesses, and its failures are charged to the person and the passkey
	// bucket alone.
	limits := append(s.passkeyBuckets(r), bucket{l: s.signinLimits().account, key: "reauth-passkey:" + strconv.FormatInt(pc.Person.ID, 10)})
	if refuseLimited(w, "too many attempts; try again later", limits...) {
		return
	}
	var req struct {
		Ceremony string          `json:"ceremony"`
		Response json.RawMessage `json:"response"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	sess, personID, ok := s.ceremonies.Take(people.CeremonyConfirm, req.Ceremony)
	if !ok || personID != pc.Person.ID {
		badRequest(w, "this confirmation expired; start again")
		return
	}
	rp, err := people.RelyingParty(r, s.baseDomain(), s.householdName())
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Response))
	if err != nil {
		badRequest(w, "malformed passkey response")
		return
	}
	user, stored := s.webAuthnUser(pc.Person)
	cred, err := rp.ValidateLogin(user, sess, parsed)
	if err != nil || !cred.Flags.UserVerified {
		writeErr(w, http.StatusUnauthorized, "that passkey wasn't accepted")
		return
	}
	refund(limits...)
	// Keep the authenticator's signature counter, as a sign-in does, so a
	// cloned passkey shows up as one.
	for _, c := range stored {
		var wc webauthn.Credential
		if c.Kind == store.CredentialPasskey && json.Unmarshal([]byte(c.Data), &wc) == nil && bytes.Equal(wc.ID, cred.ID) {
			if data, err := json.Marshal(cred); err == nil {
				s.store.UpdatePersonCredential(c.ID, string(data))
			}
		}
	}
	if err := s.store.UpdateSessionAssurance(pc.Session.ID, []string{"pop", "user"}); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "people.reauth", pc.Person.Handle, map[string]any{"via": "passkey"})
	writeOK(w)
}
