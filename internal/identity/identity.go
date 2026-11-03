// Package identity is household sign-in's policy (docs/signin-contract.md):
// whether a person may use an app, in which role, and whether their
// session is proven well enough. Decide is the whole of it — a pure, total
// function of its arguments — so the gate, the OIDC authorization endpoint
// and every test ask the same question the same way.
package identity

import (
	"fmt"
	"slices"
	"time"
)

// Fog roles and statuses, as people rows carry them.
const (
	RoleOperator = "operator"
	RoleMember   = "member"
	RoleGuest    = "guest"
	RoleBot      = "bot"

	StatusActive = "active"
)

// Audiences: who an app is open to.
const (
	AudienceOperator  = "operator"
	AudienceHousehold = "household"
	AudiencePeople    = "people"
)

// Assurance levels an app may require.
const (
	ACRAny = "urn:fog:acr:pwd"
	// ACRPhishingResistant is a passkey: proof of a key with the user
	// verified on the device.
	ACRPhishingResistant = "urn:fog:acr:phr"
)

// Person is what policy needs to know about someone.
type Person struct {
	ID     int64
	Role   string
	Status string
}

// Policy is one app's rule.
type Policy struct {
	Audience string
	// People: the chosen people (audience people) and per-person role
	// overrides under any audience; "" = no override.
	People map[int64]string
	// RoleMap maps a fog role to the app's role; a fog role absent here
	// passes through unchanged (gated apps with no vocabulary of their own).
	RoleMap map[string]string
	// Roles is the app's vocabulary. When set, a decided role outside it is
	// refused rather than handed to an app that would not understand it.
	Roles []string
	// ACR is the assurance the app always requires ("" = any sign-in).
	ACR string
}

// Assurance is how and when the session was proven.
type Assurance struct {
	AMR      []string
	AuthTime time.Time
}

// Request is what one sign-in asks for on top of the app's policy.
type Request struct {
	// ACR raises the bar for this sign-in only (acr_values).
	ACR string
	// MaxAge, when set, demands a proof no older than it (max_age).
	MaxAge *time.Duration
}

type Outcome int

const (
	Deny Outcome = iota
	Allow
	// StepUp: the person may, but must prove themselves again first —
	// with a passkey when Decision.ACR says so.
	StepUp
)

type Reason int

const (
	ReasonNone Reason = iota
	ReasonInactive
	ReasonGuest
	ReasonBot
	ReasonNotInAudience
	ReasonNoRole
)

func (r Reason) String() string {
	switch r {
	case ReasonInactive:
		return "inactive"
	case ReasonGuest:
		return "guests cannot sign in to apps yet"
	case ReasonBot:
		return "bots cannot sign in to apps"
	case ReasonNotInAudience:
		return "not shared with this person"
	case ReasonNoRole:
		return "no role in this app"
	}
	return ""
}

type Decision struct {
	Outcome Outcome
	// Role is the app role, when Allow or StepUp.
	Role string
	// ACR is the assurance the sign-in carries (Allow) or must reach
	// (StepUp).
	ACR    string
	Reason Reason
}

// Decide is sign-in policy. Order matters and is fixed: who the person is
// (status, then kind) before whether the app is theirs, before what role,
// before how well they proved it. A refusal for who someone is can never be
// overridden by anything that comes after.
func Decide(p Person, pol Policy, a Assurance, req Request, now time.Time) Decision {
	switch {
	case p.Status != StatusActive:
		return Decision{Outcome: Deny, Reason: ReasonInactive}
	case p.Role == RoleGuest:
		return Decision{Outcome: Deny, Reason: ReasonGuest}
	case p.Role == RoleBot:
		return Decision{Outcome: Deny, Reason: ReasonBot}
	case p.Role != RoleOperator && p.Role != RoleMember:
		return Decision{Outcome: Deny, Reason: ReasonNoRole}
	}

	override, chosen := pol.People[p.ID]
	switch pol.Audience {
	case AudienceHousehold:
	case AudiencePeople:
		if p.Role != RoleOperator && !chosen {
			return Decision{Outcome: Deny, Reason: ReasonNotInAudience}
		}
	default: // operator, and anything unrecognised: fail closed
		if p.Role != RoleOperator {
			return Decision{Outcome: Deny, Reason: ReasonNotInAudience}
		}
	}

	role := override
	if role == "" {
		role = p.Role
		if mapped, ok := pol.RoleMap[p.Role]; ok {
			role = mapped
		}
	}
	if role == "" || (len(pol.Roles) > 0 && !slices.Contains(pol.Roles, role)) {
		return Decision{Outcome: Deny, Reason: ReasonNoRole}
	}

	want := strongest(pol.ACR, req.ACR)
	have := Level(a.AMR)
	if want == ACRPhishingResistant && have != ACRPhishingResistant {
		return Decision{Outcome: StepUp, Role: role, ACR: want}
	}
	if req.MaxAge != nil && (a.AuthTime.IsZero() || now.Sub(a.AuthTime) > *req.MaxAge) {
		return Decision{Outcome: StepUp, Role: role, ACR: strongest(want, ACRAny)}
	}
	return Decision{Outcome: Allow, Role: role, ACR: have}
}

// Reduced says whether going from before to after takes something away
// from someone at an app: they are refused now and were not, or their role
// is less privileged. roles is the app's vocabulary, most privileged first;
// without one — or for a role outside it — any change of role counts, since
// fog cannot tell up from down. How well someone proved themselves is not
// access, so a step-up on either side reads as its role.
//
// Whoever's access is reduced must be signed out of the app: what it holds
// was decided under the old answer (docs/signin-contract.md).
func Reduced(before, after Decision, roles []string) bool {
	if before.Outcome == Deny {
		return false
	}
	if after.Outcome == Deny {
		return true
	}
	if before.Role == after.Role {
		return false
	}
	bi, ai := slices.Index(roles, before.Role), slices.Index(roles, after.Role)
	if bi < 0 || ai < 0 {
		return true
	}
	return ai > bi
}

// Level is the assurance a set of authentication methods reaches.
func Level(amr []string) string {
	if slices.Contains(amr, "pop") && slices.Contains(amr, "user") {
		return ACRPhishingResistant
	}
	return ACRAny
}

func strongest(a, b string) string {
	if a == ACRPhishingResistant || b == ACRPhishingResistant {
		return ACRPhishingResistant
	}
	if a != "" || b != "" {
		return ACRAny
	}
	return ""
}

// ValidAudience accepts the audiences an operator may choose.
func ValidAudience(a string) error {
	switch a {
	case AudienceOperator, AudienceHousehold, AudiencePeople:
		return nil
	}
	return fmt.Errorf("audience must be %s, %s or %s", AudienceOperator, AudienceHousehold, AudiencePeople)
}

// Address is a person's household-independent name:
// person:<instance ULID>/<person ULID>.
func Address(instance, uid string) string { return "person:" + instance + "/" + uid }
