package identity

import (
	"testing"
	"time"
)

func TestDecideVisitor(t *testing.T) {
	now := time.Now()
	roles := []string{"admin", "member", "viewer"}
	passkey := Assurance{AMR: []string{"pop", "user"}, AuthTime: now}
	password := Assurance{AMR: []string{"pwd"}, AuthTime: now}
	hour := time.Minute
	for _, c := range []struct {
		name string
		v    Visitor
		a    Assurance
		req  Request
		want Outcome
		why  Reason
	}{
		{"no grant", Visitor{Cap: "viewer", Roles: roles}, passkey, Request{}, Deny, ReasonNotInAudience},
		{"app hosts no visitors", Visitor{Granted: "viewer", Roles: roles}, passkey, Request{}, Deny, ReasonNotInAudience},
		{"granted the cap", Visitor{Granted: "viewer", Cap: "viewer", Roles: roles}, password, Request{}, Allow, ReasonNone},
		{"granted below the cap", Visitor{Granted: "viewer", Cap: "member", Roles: roles}, password, Request{}, Allow, ReasonNone},
		{"granted above the cap", Visitor{Granted: "member", Cap: "viewer", Roles: roles}, password, Request{}, Deny, ReasonNoRole},
		{"never admin", Visitor{Granted: "admin", Cap: "admin", Roles: roles}, passkey, Request{}, Deny, ReasonNoRole},
		{"outside the vocabulary", Visitor{Granted: "owner", Cap: "viewer", Roles: roles}, passkey, Request{}, Deny, ReasonNoRole},
		{"app wants a passkey, home says password", Visitor{Granted: "viewer", Cap: "viewer", Roles: roles, ACR: ACRPhishingResistant}, password, Request{}, StepUp, ReasonNone},
		{"app wants a passkey, home says passkey", Visitor{Granted: "viewer", Cap: "viewer", Roles: roles, ACR: ACRPhishingResistant}, passkey, Request{}, Allow, ReasonNone},
		{"max_age passed", Visitor{Granted: "viewer", Cap: "viewer", Roles: roles}, Assurance{AMR: []string{"pwd"}, AuthTime: now.Add(-time.Hour)}, Request{MaxAge: &hour}, StepUp, ReasonNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := DecideVisitor(c.v, c.a, c.req, now)
			if d.Outcome != c.want || d.Reason != c.why {
				t.Fatalf("got %+v, want outcome %v reason %v", d, c.want, c.why)
			}
			if d.Outcome != Deny && d.Role != c.v.Granted {
				t.Fatalf("role %q, want the grant %q", d.Role, c.v.Granted)
			}
		})
	}
}

func TestVisitorRoleAllowedSingleRole(t *testing.T) {
	if !VisitorRoleAllowed([]string{"user"}, "user", "user") {
		t.Fatal("an app with one role may give it to visitors")
	}
}
