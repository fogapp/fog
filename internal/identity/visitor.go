package identity

import (
	"slices"
	"time"
)

// Visitor is the host's rule for one app and one paired household
// (docs/signin-contract.md): the home has already decided who; this is
// what. Granted is the operator's visitor grant for that peer ("" = none);
// Cap is the most the app lets a visitor be (its manifest's visitors.role,
// "" = the app hosts no visitors). Roles are the app's vocabulary, most
// privileged first.
type Visitor struct {
	Granted string
	Cap     string
	Roles   []string
	// ACR is the assurance the app always requires.
	ACR string
}

// DecideVisitor is sign-in policy for a visitor. The role is the grant,
// never above the app's cap and never the app's most privileged role: a
// visitor is never an admin. Assurance is the home's word for how the
// person proved themselves, judged as a person's would be.
func DecideVisitor(v Visitor, a Assurance, req Request, now time.Time) Decision {
	if v.Cap == "" || v.Granted == "" {
		return Decision{Outcome: Deny, Reason: ReasonNotInAudience}
	}
	if !VisitorRoleAllowed(v.Roles, v.Cap, v.Granted) {
		return Decision{Outcome: Deny, Reason: ReasonNoRole}
	}
	want := strongest(v.ACR, req.ACR)
	have := Level(a.AMR)
	if want == ACRPhishingResistant && have != ACRPhishingResistant {
		return Decision{Outcome: StepUp, Role: v.Granted, ACR: want}
	}
	if req.MaxAge != nil && (a.AuthTime.IsZero() || now.Sub(a.AuthTime) > *req.MaxAge) {
		return Decision{Outcome: StepUp, Role: v.Granted, ACR: strongest(want, ACRAny)}
	}
	return Decision{Outcome: Allow, Role: v.Granted, ACR: have}
}

// VisitorRoleAllowed says whether role may be granted to visitors of an app
// whose vocabulary is roles (most privileged first) and whose cap is cap:
// in the vocabulary, no more privileged than the cap, and not the first
// role of a vocabulary with more than one.
func VisitorRoleAllowed(roles []string, cap, role string) bool {
	ri, ci := slices.Index(roles, role), slices.Index(roles, cap)
	if ri < 0 || ci < 0 || ri < ci {
		return false
	}
	return len(roles) == 1 || ri > 0
}
