package signin

import (
	"time"

	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/store"
)

// fullAssurance stands in for a proof when the question is who, not how
// well: provisioning and narrowing ask what someone may do at all, and a
// passkey requirement is met at sign-in.
var fullAssurance = identity.Assurance{AMR: []string{"pop", "user"}}

// Access is what policy lets p do at client c's app, whatever their proof:
// identity.Decide with the app's vocabulary and the operator's policy.
func Access(p store.Person, pol store.SigninPolicy, c *store.OIDCClient) identity.Decision {
	now := time.Now()
	a := fullAssurance
	a.AuthTime = now
	return identity.Decide(identity.Person{ID: p.ID, Role: p.Role, Status: p.Status},
		identity.Policy{Audience: pol.Audience, People: pol.People, RoleMap: c.RoleMap, Roles: c.Roles},
		a, identity.Request{}, now)
}

// LogoutWhereReduced owes the app every person it has seen whose access
// shrank between before and after (identity.Reduced): their sessions there
// were decided under the old answer, so they end. roles is the app's
// vocabulary afterwards. It returns how many people the app will hear about.
func LogoutWhereReduced(st *store.Store, clientID string, roles []string, before, after func(store.Person) identity.Decision) (int, error) {
	linked, err := st.LinkedPersonUIDs(clientID)
	if err != nil {
		return 0, err
	}
	n := 0
	for id, uid := range linked {
		p, err := st.PersonByID(id)
		if err != nil || uid == "" {
			continue // deleted people were told by their row's trigger
		}
		if !identity.Reduced(before(*p), after(*p), roles) {
			continue
		}
		if err := st.EnqueueLogout(clientID, uid); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// LogoutReduced is LogoutWhereReduced for an app whose declaration changed
// from before to after (a deploy): the same policy, read through each.
func LogoutReduced(st *store.Store, appID int64, before, after *store.OIDCClient) (int, error) {
	pol, err := st.SigninPolicyFor(appID)
	if err != nil {
		return 0, err
	}
	return LogoutWhereReduced(st, after.ClientID, after.Roles,
		func(p store.Person) identity.Decision { return Access(p, *pol, before) },
		func(p store.Person) identity.Decision { return Access(p, *pol, after) })
}
