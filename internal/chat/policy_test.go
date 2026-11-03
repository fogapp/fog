package chat

import (
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// The "code" join rule and the "listed" households rule were accepted but
// never enforced: a room set to them behaved as invite-only among the hub's
// peers. A client may no longer set them — at creation or by commit — and a
// room that stored them reads as what it actually did, keeping the rest of
// its policy.
func TestPolicyRefusesUnenforcedRules(t *testing.T) {
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Join = "code" },
		func(p *Policy) { p.Households = "listed" },
	} {
		p := DefaultPolicy(KindGroup)
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("Validate accepted join %q households %q", p.Join, p.Households)
		}
		if _, err := parseNewPolicy(p.JSON()); err == nil {
			t.Errorf("a committed policy with join %q households %q was accepted", p.Join, p.Households)
		}
	}

	stored := DefaultPolicy(KindGroup)
	stored.Join, stored.Households = "code", "listed"
	stored.Retention.Days = 30
	raw := stored.JSON()
	if !strings.Contains(raw, `"code"`) {
		t.Fatalf("test policy lost its legacy rule: %s", raw)
	}
	got, err := ParsePolicy(raw)
	if err != nil {
		t.Fatalf("a stored policy with the old rules no longer loads: %v", err)
	}
	if got.Join != JoinInvite || got.Households != HouseholdsHubPeers || got.Retention.Days != 30 {
		t.Errorf("stored policy read as join %q households %q retention %d", got.Join, got.Households, got.Retention.Days)
	}
	s := &Service{}
	if p := s.policyOf(&store.ChatRoom{Policy: raw, Kind: KindGroup}); p.Retention.Days != 30 {
		t.Errorf("policyOf fell back to the default for a stored legacy policy: %+v", p)
	}
}

// A policy change that keeps the roles' look but pads or reorders their
// capabilities is still a change of roles.
func TestSameRolesIsExact(t *testing.T) {
	base := DefaultPolicy(KindGroup).Roles
	clone := func() []Role {
		out := make([]Role, len(base))
		for i, r := range base {
			out[i] = Role{Index: r.Index, Name: r.Name, Capabilities: append([]string(nil), r.Capabilities...)}
		}
		return out
	}
	if !sameRoles(base, clone()) {
		t.Fatal("identical roles differ")
	}
	padded := clone()
	caps := padded[2].Capabilities // admin
	padded[2].Capabilities = append(caps[:len(caps)-1], caps[0])
	if sameRoles(base, padded) {
		t.Error("an admin capability swapped for a repeat counted as the same roles")
	}
	renamed := clone()
	renamed[1].Name = "owner-in-all-but-name"
	if sameRoles(base, renamed) {
		t.Error("a renamed role counted as the same")
	}
	dup := clone()
	dup = append(dup, dup[1])
	if sameRoles(base, dup) {
		t.Error("a duplicated role counted as the same")
	}
}
