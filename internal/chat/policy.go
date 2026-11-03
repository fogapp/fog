// Room policy, in the shape of draft-ietf-mimi-room-policy: roles with
// capabilities, a join rule, a history rule, and the three axes fog adds
// (households, hints, retention). The hub enforces what it can see —
// membership, roles, metadata; clients enforce the rest on decrypted content.
package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Role indices are fixed for the four built-in roles so a client can render
// them without the policy in hand; custom roles may follow at 5+.
const (
	RoleNone   = 0
	RoleBanned = 1
	RoleMember = 2
	RoleAdmin  = 3
	RoleOwner  = 4
)

// Capabilities the hub or a client checks. Names follow the MIMI draft where
// it has one.
const (
	CapSendMessage        = "canSendMessage"
	CapReact              = "canReact"
	CapEditOwn            = "canEditOwn"
	CapDeleteOwn          = "canDeleteOwn"
	CapAddParticipant     = "canAddParticipant"
	CapRemoveParticipant  = "canRemoveParticipant"
	CapDeleteOtherMessage = "canDeleteOtherMessage"
	CapChangeRoomName     = "canChangeRoomName"
	CapChangeRoomTopic    = "canChangeRoomTopic"
	CapChangeRoomAvatar   = "canChangeRoomAvatar"
	CapPin                = "canPin"
	CapChangePolicy       = "canChangePolicy"
	CapChangeRoles        = "canChangeRoles"
	CapShareHistory       = "canShareHistory"
	CapAll                = "*"
)

// Join rules.
const (
	JoinOpenHousehold = "open-household"
	JoinInvite        = "invite"
)

// History rules.
const (
	HistoryShared    = "shared"
	HistorySinceJoin = "since-join"
)

// Households rules.
const (
	HouseholdsHubPeers = "hub-peers"
)

// Rule values Validate once let through though nothing ever enforced them:
// a join code no endpoint accepts, and a list of households nowhere kept.
// A stored policy may still hold them; it reads as what was enforced.
const (
	legacyJoinCode         = "code"
	legacyHouseholdsListed = "listed"
)

// HintMentions is the one hint fog defines.
const HintMentions = "mentions"

type Role struct {
	Index        int      `json:"index"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

type Retention struct {
	Days int `json:"days"`
}

type Policy struct {
	Roles      []Role    `json:"roles"`
	Join       string    `json:"join"`
	History    string    `json:"history"`
	Households string    `json:"households"`
	Hints      []string  `json:"hints"`
	Retention  Retention `json:"retention"`
}

var memberCaps = []string{CapSendMessage, CapReact, CapEditOwn, CapDeleteOwn, CapAddParticipant, CapShareHistory}
var adminCaps = append(append([]string{}, memberCaps...),
	CapRemoveParticipant, CapDeleteOtherMessage, CapChangeRoomName, CapChangeRoomTopic, CapChangeRoomAvatar, CapPin, CapChangePolicy, CapChangeRoles)

// DefaultPolicy is what a room of the given kind starts with.
func DefaultPolicy(kind string) Policy {
	p := Policy{
		Roles: []Role{
			{Index: RoleBanned, Name: "banned", Capabilities: []string{}},
			{Index: RoleMember, Name: "member", Capabilities: memberCaps},
			{Index: RoleAdmin, Name: "admin", Capabilities: adminCaps},
			{Index: RoleOwner, Name: "owner", Capabilities: []string{CapAll}},
		},
		Join:       JoinInvite,
		History:    HistorySinceJoin,
		Households: HouseholdsHubPeers,
		Hints:      []string{HintMentions},
	}
	switch kind {
	case KindChannel:
		p.Join = JoinOpenHousehold
		p.History = HistoryShared
	case KindDM:
		// Fixed membership: nobody adds anyone after creation.
		p.Roles[1].Capabilities = []string{CapSendMessage, CapReact, CapEditOwn, CapDeleteOwn, CapShareHistory}
		p.Roles[2].Capabilities = p.Roles[1].Capabilities
	}
	return p
}

// Can reports whether a role holds a capability.
func (p Policy) Can(roleIndex int, capability string) bool {
	for _, r := range p.Roles {
		if r.Index != roleIndex {
			continue
		}
		for _, c := range r.Capabilities {
			if c == CapAll || c == capability {
				return true
			}
		}
		return false
	}
	return false
}

// HasRole says whether the policy defines a role at this index.
func (p Policy) HasRole(roleIndex int) bool {
	for _, r := range p.Roles {
		if r.Index == roleIndex {
			return true
		}
	}
	return false
}

// Rank orders roles for deciding who may change whose: a role holding every
// capability ranks above all others, the rest by index, banned lowest. A
// role the policy doesn't define ranks below everything.
func (p Policy) Rank(roleIndex int) int {
	for _, r := range p.Roles {
		if r.Index != roleIndex {
			continue
		}
		for _, c := range r.Capabilities {
			if c == CapAll {
				return math.MaxInt32
			}
		}
		return r.Index
	}
	return 0
}

// HintAllowed says whether senders may declare a hint of this name.
func (p Policy) HintAllowed(name string) bool {
	for _, h := range p.Hints {
		if h == name {
			return true
		}
	}
	return false
}

// Validate normalizes a policy from a client and refuses nonsense.
func (p *Policy) Validate() error {
	switch p.Join {
	case JoinOpenHousehold, JoinInvite:
	case "":
		p.Join = JoinInvite
	default:
		return fmt.Errorf("unknown join rule %q", p.Join)
	}
	switch p.History {
	case HistoryShared, HistorySinceJoin:
	case "":
		p.History = HistorySinceJoin
	default:
		return fmt.Errorf("unknown history rule %q", p.History)
	}
	switch p.Households {
	case HouseholdsHubPeers:
	case "":
		p.Households = HouseholdsHubPeers
	default:
		return fmt.Errorf("unknown households rule %q", p.Households)
	}
	if p.Hints == nil {
		p.Hints = []string{}
	}
	for _, h := range p.Hints {
		if h != HintMentions {
			return fmt.Errorf("unknown hint %q", h)
		}
	}
	if p.Retention.Days < 0 || p.Retention.Days > 3650 {
		return errors.New("retention must be 0 (forever) to 3650 days")
	}
	if len(p.Roles) == 0 {
		p.Roles = DefaultPolicy(KindGroup).Roles
	}
	seen := map[int]bool{}
	hasOwner := false
	for i := range p.Roles {
		r := &p.Roles[i]
		if r.Index <= RoleNone || seen[r.Index] {
			return fmt.Errorf("bad role index %d", r.Index)
		}
		seen[r.Index] = true
		if r.Index == RoleBanned && len(r.Capabilities) > 0 {
			return errors.New("the banned role holds no capabilities")
		}
		if r.Capabilities == nil {
			r.Capabilities = []string{}
		}
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			return fmt.Errorf("role %d has no name", r.Index)
		}
		for _, c := range r.Capabilities {
			if c == CapAll {
				hasOwner = true
			}
		}
	}
	if !seen[RoleBanned] || !seen[RoleMember] {
		return errors.New("roles must include banned (1) and member (2)")
	}
	if !hasOwner {
		return errors.New("some role must hold every capability (*)")
	}
	return nil
}

// ParsePolicy reads a room's stored policy. One written while Validate
// still let the unenforced rules through reads as what the hub actually
// did: invite-only, households as the hub's peers.
func ParsePolicy(s string) (Policy, error) {
	if s == "" {
		return DefaultPolicy(KindGroup), nil
	}
	var p Policy
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return p, err
	}
	if p.Join == legacyJoinCode {
		p.Join = JoinInvite
	}
	if p.Households == legacyHouseholdsListed {
		p.Households = HouseholdsHubPeers
	}
	return p, p.Validate()
}

// parseNewPolicy reads a policy a member commits to the room: held to
// Validate as it stands, with no allowance for old rules.
func parseNewPolicy(s string) (Policy, error) {
	var p Policy
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

func (p Policy) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}
