// Who may sign in to an app (docs/signin-contract.md): the operator's
// side of internal/identity's policy, and the Sign-in tab's view.
package server

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

type signinPolicyPerson struct {
	Handle string `json:"handle"`
	Role   string `json:"role,omitempty"`
}

type signinClientView struct {
	ClientID       string            `json:"clientId"`
	RedirectURIs   []string          `json:"redirectUris"`
	BackchannelURI string            `json:"backchannelUri"`
	SCIMBaseURL    string            `json:"scimBaseUrl"`
	Roles          []string          `json:"roles"`
	RoleMap        map[string]string `json:"roleMap"`
	RequiredACR    string            `json:"requiredAcr"`
	// Email: the app is registered for the email scope, so it learns a
	// person's address when they have one.
	Email bool `json:"email"`
	// SecretWaiting: a rotated secret is waiting for the app's next deploy;
	// the running one works until the app uses the new one.
	SecretWaiting bool `json:"secretWaiting"`
}

type signinHouseholdPerson struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

// signinView is the Sign-in tab: how the app signs people in, who may, and
// who has.
type signinView struct {
	// Mode: "oidc" (declares fog-signin/1), "gate" (an auth: true route),
	// or "none" (the app's own accounts decide).
	Mode      string                  `json:"mode"`
	Issuer    string                  `json:"issuer"`
	Audience  string                  `json:"audience"`
	People    []signinPolicyPerson    `json:"people"`
	Household []signinHouseholdPerson `json:"household"`
	Client    *signinClientView       `json:"client"`
	Linked    []store.OIDCLink        `json:"linked"`
}

func (s *Server) appByPath(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	a, err := s.store.AppByName(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "no such app")
		return nil, false
	}
	if err != nil {
		writeError(w, err)
		return nil, false
	}
	return a, true
}

func (s *Server) handleGetSigninPolicy(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appByPath(w, r)
	if !ok {
		return
	}
	pol, err := s.store.SigninPolicyFor(a.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	view := signinView{Mode: "none", Issuer: s.issuer(), Audience: pol.Audience,
		People: []signinPolicyPerson{}, Household: []signinHouseholdPerson{}, Linked: []store.OIDCLink{}}
	for id, role := range pol.People {
		if p, err := s.store.PersonByID(id); err == nil {
			view.People = append(view.People, signinPolicyPerson{Handle: p.Handle, Role: role})
		}
	}
	everyone, err := s.store.ListPeople()
	if err != nil {
		writeError(w, err)
		return
	}
	for _, p := range everyone {
		if p.Status == store.PersonStatusActive && (p.Role == store.PersonRoleOperator || p.Role == store.PersonRoleMember) {
			view.Household = append(view.Household, signinHouseholdPerson{Handle: p.Handle, DisplayName: p.DisplayName, Role: p.Role})
		}
	}
	if spec, err := apps.LatestSpec(s.store, a.ID); err == nil {
		for _, rt := range spec.Routes {
			if rt.Auth {
				view.Mode = "gate"
			}
		}
	}
	if c, err := s.store.OIDCClientForApp(a.ID); err == nil {
		view.Mode = "oidc"
		view.Client = &signinClientView{ClientID: c.ClientID, RedirectURIs: c.RedirectURIs, BackchannelURI: c.BackchannelURI,
			SCIMBaseURL: c.SCIMBaseURL, Roles: c.Roles, RoleMap: c.RoleMap, RequiredACR: c.RequiredACR,
			Email: slices.Contains(c.Scopes, oidc.ScopeEmail), SecretWaiting: c.NextSecretDigest != ""}
		if links, err := s.store.OIDCLinksForClient(c.ClientID); err == nil {
			view.Linked = links
		}
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePutSigninPolicy(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appByPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Audience string               `json:"audience"`
		People   []signinPolicyPerson `json:"people"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := identity.ValidAudience(req.Audience); err != nil {
		badRequest(w, err.Error())
		return
	}
	var roles []string
	if c, err := s.store.OIDCClientForApp(a.ID); err == nil {
		roles = c.Roles
	}
	pol := store.SigninPolicy{AppID: a.ID, Audience: req.Audience, People: map[int64]string{}}
	for _, pp := range req.People {
		p, err := s.store.PersonByHandle(pp.Handle)
		if err != nil {
			badRequest(w, "no person "+pp.Handle)
			return
		}
		if pp.Role != "" && len(roles) > 0 && !slices.Contains(roles, pp.Role) {
			badRequest(w, pp.Role+" is not a role "+a.Name+" has")
			return
		}
		pol.People[p.ID] = pp.Role
	}
	before, err := s.store.SigninPolicyFor(a.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.PutSigninPolicy(pol); err != nil {
		writeError(w, err)
		return
	}
	if err := s.logoutNoLongerAllowed(a, before, &pol); err != nil {
		writeError(w, err)
		return
	}
	s.kickLogouts()
	s.audit(r, "app.signin.policy", a.Name, map[string]any{"audience": req.Audience, "people": len(req.People)})
	s.handleGetSigninPolicy(w, r)
}

// handleRotateSigninSecret mints a new client secret. By default the next
// deploy carries it, and the running secret works until the app first uses
// the new one (and a few minutes after, for a blue-green swap). With
// {"now": true} — a leaked secret — the old one stops at once, the app is
// redeployed with the new one, and it cannot sign anyone in until that
// deploy is done.
func (s *Server) handleRotateSigninSecret(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appByPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Now bool `json:"now"`
	}
	if !readOptionalJSON(w, r, &req) {
		return
	}
	if _, err := s.store.OIDCClientForApp(a.ID); err != nil {
		notFound(w, a.Name+" does not sign in with fog")
		return
	}
	if !req.Now {
		if err := s.signin.Rotate(a); err != nil {
			writeError(w, err)
			return
		}
		s.audit(r, "app.signin.rotate", a.Name, nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redeploying": false})
		return
	}
	if err := s.signin.RevokeNow(a); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "app.signin.revoke_secret", a.Name, nil)
	redeploying := s.engine != nil
	if redeploying {
		s.deployAsync("signin secret revoke", a.Name, 0, s.actor(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redeploying": redeploying})
}

// DeliverLogouts sends the back-channel logouts the outbox owes. fogd's
// loop calls it; so does anything that just ended a session, to save the
// wait.
func (s *Server) DeliverLogouts(ctx context.Context) { s.logouts.Run(ctx) }

func (s *Server) kickLogouts() {
	s.goSafe("signin logouts", func() { s.logouts.Run(s.bgContext()) })
	// Whatever ended or narrowed someone's access may also change who an
	// app should have provisioned.
	s.kickProvisioning()
}

// Provision reconciles every app that takes SCIM (docs/signin-contract.md).
// fogd's loop calls it; so do changes to people and policy.
func (s *Server) Provision(ctx context.Context) { s.scim.Run(ctx) }

func (s *Server) kickProvisioning() {
	s.goSafe("signin provisioning", func() { s.scim.Run(s.bgContext()) })
}

// handleSignEveryoneOut tells an app that every person it has seen through
// fog is signed out — the Sign-in tab's "Sign everyone out of Manor".
func (s *Server) handleSignEveryoneOut(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appByPath(w, r)
	if !ok {
		return
	}
	c, err := s.store.OIDCClientForApp(a.ID)
	if err != nil {
		notFound(w, a.Name+" does not sign in with fog")
		return
	}
	people, err := s.store.LinkedPersonUIDs(c.ClientID)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, uid := range people {
		if err := s.store.EnqueueLogout(c.ClientID, uid); err != nil {
			writeError(w, err)
			return
		}
	}
	s.audit(r, "app.signin.signout_all", a.Name, map[string]any{"people": len(people)})
	s.kickLogouts()
	writeJSON(w, http.StatusOK, map[string]any{"people": len(people)})
}

// logoutNoLongerAllowed tells an app about everyone it has seen whose access
// the new policy reduces — refused now, or a lesser role: narrowing an
// audience or lowering someone's role ends their sessions there too, not
// only their next sign-in.
func (s *Server) logoutNoLongerAllowed(app *store.App, before, after *store.SigninPolicy) error {
	c, err := s.store.OIDCClientForApp(app.ID)
	if err != nil {
		return nil // not an OIDC app: the gate re-checks every request itself
	}
	_, err = signin.LogoutWhereReduced(s.store, c.ClientID, c.Roles,
		func(p store.Person) identity.Decision { return signin.Access(p, *before, c) },
		func(p store.Person) identity.Decision { return signin.Access(p, *after, c) })
	return err
}

// endReducedAccess follows a change to one person — a new role, a
// suspension, reinstatement — to every app that has seen them: where they
// may now do less, the app is owed a logout (identity.Reduced), and their
// visits to other households that the change no longer allows end there.
func (s *Server) endReducedAccess(before, after *store.Person) error {
	clients, err := s.store.OIDCClientsLinkedTo(after.ID)
	if err != nil {
		return err
	}
	for i := range clients {
		c := &clients[i]
		pol, err := s.store.SigninPolicyFor(c.AppID)
		if err != nil {
			return err
		}
		if identity.Reduced(signin.Access(*before, *pol, c), signin.Access(*after, *pol, c), c.Roles) {
			if err := s.store.EnqueueLogout(c.ClientID, after.UID); err != nil {
				return err
			}
		}
	}
	peers, err := s.store.ListFedPeers()
	if err != nil {
		return err
	}
	for _, peer := range peers {
		s.recallVisits(peer.ID)
	}
	s.kickLogouts()
	return nil
}
