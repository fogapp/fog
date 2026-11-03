package identity

import (
	"testing"
	"time"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

var (
	passkey  = Assurance{AMR: []string{"pop", "user"}, AuthTime: now.Add(-time.Hour)}
	password = Assurance{AMR: []string{"pwd"}, AuthTime: now.Add(-time.Hour)}
	unknown  = Assurance{}
)

func TestDecideExamples(t *testing.T) {
	op := Person{ID: 1, Role: RoleOperator, Status: StatusActive}
	kim := Person{ID: 2, Role: RoleMember, Status: StatusActive}
	manor := Policy{Audience: AudienceHousehold, RoleMap: map[string]string{RoleOperator: "admin", RoleMember: "member"},
		Roles: []string{"admin", "member"}, ACR: ACRPhishingResistant}
	five := 5 * time.Minute

	cases := []struct {
		name string
		p    Person
		pol  Policy
		a    Assurance
		req  Request
		want Decision
	}{
		{"default policy admits the operator", op, Policy{}, password, Request{}, Decision{Outcome: Allow, Role: RoleOperator, ACR: ACRAny}},
		{"default policy refuses a member", kim, Policy{}, password, Request{}, Decision{Outcome: Deny, Reason: ReasonNotInAudience}},
		{"household admits a member", kim, Policy{Audience: AudienceHousehold}, password, Request{}, Decision{Outcome: Allow, Role: RoleMember, ACR: ACRAny}},
		{"people refuses the unchosen", kim, Policy{Audience: AudiencePeople}, password, Request{}, Decision{Outcome: Deny, Reason: ReasonNotInAudience}},
		{"people admits the chosen", kim, Policy{Audience: AudiencePeople, People: map[int64]string{2: ""}}, password, Request{}, Decision{Outcome: Allow, Role: RoleMember, ACR: ACRAny}},
		{"people always admits the operator", op, Policy{Audience: AudiencePeople}, password, Request{}, Decision{Outcome: Allow, Role: RoleOperator, ACR: ACRAny}},
		{"role map applies", kim, Policy{Audience: AudienceHousehold, RoleMap: map[string]string{RoleMember: "viewer"}}, password, Request{}, Decision{Outcome: Allow, Role: "viewer", ACR: ACRAny}},
		{"override beats the map", kim, Policy{Audience: AudienceHousehold, RoleMap: map[string]string{RoleMember: "viewer"}, People: map[int64]string{2: "admin"}}, password, Request{}, Decision{Outcome: Allow, Role: "admin", ACR: ACRAny}},
		{"a role outside the vocabulary is refused", kim, Policy{Audience: AudienceHousehold, Roles: []string{"admin"}}, password, Request{}, Decision{Outcome: Deny, Reason: ReasonNoRole}},
		{"phr app steps a password up", kim, manor, password, Request{}, Decision{Outcome: StepUp, Role: "member", ACR: ACRPhishingResistant}},
		{"phr app admits a passkey", kim, manor, passkey, Request{}, Decision{Outcome: Allow, Role: "member", ACR: ACRPhishingResistant}},
		{"a request can raise the bar", kim, Policy{Audience: AudienceHousehold}, password, Request{ACR: ACRPhishingResistant}, Decision{Outcome: StepUp, Role: RoleMember, ACR: ACRPhishingResistant}},
		{"max_age steps a stale proof up", kim, Policy{Audience: AudienceHousehold}, passkey, Request{MaxAge: &five}, Decision{Outcome: StepUp, Role: RoleMember, ACR: ACRAny}},
		{"max_age steps an unknown proof up", op, Policy{}, unknown, Request{MaxAge: &five}, Decision{Outcome: StepUp, Role: RoleOperator, ACR: ACRAny}},
		{"unknown audience fails closed", kim, Policy{Audience: "everyone"}, password, Request{}, Decision{Outcome: Deny, Reason: ReasonNotInAudience}},
	}
	for _, c := range cases {
		if got := Decide(c.p, c.pol, c.a, c.req, now); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestDecideInvariants walks the whole product of person, audience,
// override, role map, required assurance, request and session proof, and
// checks the rules no combination may break.
func TestDecideInvariants(t *testing.T) {
	roles := []string{RoleOperator, RoleMember, RoleGuest, RoleBot, "stranger"}
	statuses := []string{StatusActive, "suspended"}
	audiences := []string{"", AudienceOperator, AudienceHousehold, AudiencePeople, "everyone"}
	overrides := []map[int64]string{nil, {7: ""}, {7: "admin"}, {7: "viewer"}}
	maps := []map[string]string{nil, {RoleMember: "viewer", RoleOperator: "admin", RoleGuest: "admin"}}
	vocabularies := [][]string{nil, {"admin", "member", "viewer"}, {"viewer"}}
	acrs := []string{"", ACRAny, ACRPhishingResistant}
	five := 5 * time.Minute
	requests := []Request{{}, {ACR: ACRPhishingResistant}, {MaxAge: &five}}
	proofs := []Assurance{passkey, password, unknown, {AMR: []string{"pop", "user"}, AuthTime: now}}

	n := 0
	for _, role := range roles {
		for _, status := range statuses {
			p := Person{ID: 7, Role: role, Status: status}
			for _, aud := range audiences {
				for _, ov := range overrides {
					for _, rm := range maps {
						for _, voc := range vocabularies {
							for _, acr := range acrs {
								pol := Policy{Audience: aud, People: ov, RoleMap: rm, Roles: voc, ACR: acr}
								for _, req := range requests {
									for _, a := range proofs {
										n++
										d := Decide(p, pol, a, req, now)
										check(t, p, pol, a, req, d)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if n < 10000 {
		t.Fatalf("only %d combinations", n)
	}
}

func check(t *testing.T, p Person, pol Policy, a Assurance, req Request, d Decision) {
	t.Helper()
	fail := func(why string) {
		t.Fatalf("%s: person %+v policy %+v proof %+v request %+v → %+v", why, p, pol, a, req, d)
	}
	if d.Outcome != Deny && p.Status != StatusActive {
		fail("an inactive person got in")
	}
	if d.Outcome != Deny && p.Role != RoleOperator && p.Role != RoleMember {
		fail("someone neither operator nor member got in (guests and bots never sign in to apps)")
	}
	if d.Outcome != Deny && p.Role == RoleMember && pol.Audience != AudienceHousehold && pol.Audience != AudiencePeople {
		fail("a member got into an operator-only app")
	}
	if d.Outcome != Deny && p.Role == RoleMember && pol.Audience == AudiencePeople {
		if _, ok := pol.People[p.ID]; !ok {
			fail("an unchosen member got in")
		}
	}
	if d.Outcome != Deny && len(pol.Roles) > 0 {
		found := false
		for _, r := range pol.Roles {
			found = found || r == d.Role
		}
		if !found {
			fail("a role outside the app's vocabulary")
		}
	}
	if d.Outcome == Allow && (pol.ACR == ACRPhishingResistant || req.ACR == ACRPhishingResistant) && Level(a.AMR) != ACRPhishingResistant {
		fail("a passkey requirement was met by something else")
	}
	if d.Outcome == Allow && req.MaxAge != nil && (a.AuthTime.IsZero() || now.Sub(a.AuthTime) > *req.MaxAge) {
		fail("max_age was not enforced")
	}
	if d.Outcome == Deny && d.Reason == ReasonNone {
		fail("a refusal with no reason")
	}
	if d.Outcome != Deny && d.Role == "" {
		fail("admitted with no role")
	}
}

// TestReducedSaysWhenAccessShrinks: a demotion, a narrower audience or a
// lesser role is a reduction an app must hear as a logout; a promotion, a
// step-up or nothing changing is not.
func TestReducedSaysWhenAccessShrinks(t *testing.T) {
	roles := []string{"admin", "member", "viewer"}
	allow := func(role string) Decision { return Decision{Outcome: Allow, Role: role} }
	deny := Decision{Outcome: Deny, Reason: ReasonGuest}
	cases := []struct {
		name          string
		before, after Decision
		roles         []string
		want          bool
	}{
		{"refused now", allow("member"), deny, roles, true},
		{"refused before and after", deny, deny, roles, false},
		{"admitted now", deny, allow("member"), roles, false},
		{"unchanged", allow("member"), allow("member"), roles, false},
		{"a lesser role", allow("admin"), allow("viewer"), roles, true},
		{"a greater role", allow("viewer"), allow("member"), roles, false},
		{"a step-up is not access", allow("member"), Decision{Outcome: StepUp, Role: "member"}, roles, false},
		{"no vocabulary: any change counts", allow("operator"), allow("member"), nil, true},
		{"a role outside the vocabulary counts", allow("member"), allow("owner"), roles, true},
	}
	for _, c := range cases {
		if got := Reduced(c.before, c.after, c.roles); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// Demotion to guest is a reduction under every audience that admitted
	// the member.
	kim := Person{ID: 2, Role: RoleMember, Status: StatusActive}
	guest := kim
	guest.Role = RoleGuest
	for _, aud := range []string{AudienceHousehold, AudiencePeople} {
		pol := Policy{Audience: aud, People: map[int64]string{2: ""}}
		if !Reduced(Decide(kim, pol, passkey, Request{}, now), Decide(guest, pol, passkey, Request{}, now), nil) {
			t.Errorf("%s: demoting a member to guest is not a reduction", aud)
		}
	}
}
