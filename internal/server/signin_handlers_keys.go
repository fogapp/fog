// Emergency signing-key rotation (docs/signin-contract.md): the operator's
// way out when fog's signing key may have leaked. Every key is retired at
// once (signing.Keyring.RetireAll), a fresh one signs from that moment, and
// everything the old keys signed — ID tokens, gate assertions, proofs,
// logout tokens — stops verifying, here and, as soon as they refetch the key
// set, at every app. A leaked key could have minted an ID token for anyone —
// a person the app never saw, a visitor from anywhere — so by default every
// app is told: a logout for each of the household's people and each visitor
// session it hosts, and a logout that ends every session it started from a
// sign-in before now (oidc.EventSessionsRevoked), for the subjects nobody
// can list.
package server

import (
	"net/http"
	"time"
)

// signingKeyView is one key as Settings shows it: when it signs, and when
// it stops being published.
type signingKeyView struct {
	KID         string `json:"kid"`
	PublishAt   string `json:"publishAt"`
	SignFrom    string `json:"signFrom"`
	SignUntil   string `json:"signUntil"`
	UnpublishAt string `json:"unpublishAt"`
	// Signing: this is the key fog signs with now.
	Signing bool `json:"signing"`
}

// retireConfirm is the word a retire request must carry: the API's own
// confirm step, so no stray POST rotates every key.
const retireConfirm = "retire every key"

func (s *Server) handleListSigningKeys(w http.ResponseWriter, r *http.Request) {
	if err := s.keys.Reconcile(); err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListSigningKeys()
	if err != nil {
		writeError(w, err)
		return
	}
	now := time.Now()
	out := []signingKeyView{}
	for _, k := range rows {
		out = append(out, signingKeyView{
			KID: k.KID, PublishAt: k.PublishAt.UTC().Format(time.RFC3339), SignFrom: k.SignFrom.UTC().Format(time.RFC3339),
			SignUntil: k.SignUntil.UTC().Format(time.RFC3339), UnpublishAt: k.UnpublishAt.UTC().Format(time.RFC3339),
			Signing: !now.Before(k.SignFrom) && now.Before(k.SignUntil),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// handleRetireSigningKeys is the emergency rotation. Body:
// {"confirm": "retire every key", "signOutApps": true}.
func (s *Server) handleRetireSigningKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm     string `json:"confirm"`
		SignOutApps *bool  `json:"signOutApps"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != retireConfirm {
		badRequest(w, `to retire every signing key, send "confirm": "`+retireConfirm+`"`)
		return
	}
	signOut := req.SignOutApps == nil || *req.SignOutApps
	before, err := s.store.ListSigningKeys()
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.keys.RetireAll(); err != nil {
		writeError(w, err)
		return
	}
	people := 0
	if signOut {
		if people, err = s.signOutEveryApp(time.Now()); err != nil {
			writeError(w, err)
			return
		}
		s.kickLogouts()
	}
	s.audit(r, "signin.keys.retire", "", map[string]any{"retired": len(before), "signedOutApps": signOut, "logouts": people})
	s.handleListSigningKeys(w, r)
}

// signOutEveryApp owes every app that takes back-channel logout the end of
// every session it may hold from a sign-in before before: by subject for
// each of the household's people (seen by the app or not), by sid for each
// visitor session it hosts, and the sessions-revoked logout for the rest. It
// returns how many logouts it owed.
func (s *Server) signOutEveryApp(before time.Time) (int, error) {
	clients, err := s.store.AllOIDCClients()
	if err != nil {
		return 0, err
	}
	everyone, err := s.store.ListPeople()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range clients {
		if c.BackchannelURI == "" {
			continue
		}
		for _, p := range everyone {
			if p.UID == "" {
				continue
			}
			if err := s.store.EnqueueLogout(c.ClientID, p.UID); err != nil {
				return n, err
			}
			n++
		}
		v, err := s.store.EnqueueVisitorLogouts(c.ClientID)
		if err != nil {
			return n, err
		}
		n += v
		if err := s.store.EnqueueLogoutAll(c.ClientID, before); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
