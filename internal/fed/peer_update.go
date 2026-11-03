package fed

import (
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// PeerUpdate is an operator's edit of one peer. Nil Label and URL leave
// them alone; Label "" goes back to the advertised name, URL "" makes the
// peer dial-in only. Capabilities are the full desired state of the rows
// they name: enabled upserts, disabled removes.
type PeerUpdate struct {
	Name         string
	Label, URL   *string
	Capabilities []CapabilityChange
}

// CapabilityChange sets or removes one capability row.
type CapabilityChange struct {
	Capability, Direction, Config string
	Enabled                       bool
}

// CapabilityRules is what the caller knows that fed cannot: which
// capabilities exist (chat, files and feedback register theirs above
// this package) and how to check one's config. Check may be nil.
type CapabilityRules struct {
	Known func(capability string) bool
	Check func(c CapabilityChange) error
}

// PeerUpdated says what an update actually changed, for the caller's
// audit records and side effects.
type PeerUpdated struct {
	Peer         *store.FedPeer
	Renamed      bool
	LabelChanged bool
	URLChanged   bool
	// Capabilities are the changes applied, in request order.
	Capabilities []CapabilityChange
}

// InvalidUpdateError is an update refused before anything was written.
type InvalidUpdateError struct{ Msg string }

func (e *InvalidUpdateError) Error() string { return e.Msg }

func invalid(msg string) error { return &InvalidUpdateError{Msg: msg} }

// UpdatePeer validates u whole, then applies it in one transaction: a
// refused or failed update leaves the peer exactly as it was.
func (s *Service) UpdatePeer(peer *store.FedPeer, u PeerUpdate, rules CapabilityRules) (*PeerUpdated, error) {
	var edit store.FedPeerEdit
	res := &PeerUpdated{}
	if u.Label != nil {
		label := strings.TrimSpace(*u.Label)
		if len(label) > 64 {
			return nil, invalid("label is too long (64 characters max)")
		}
		if label != peer.Label {
			edit.Label, res.LabelChanged = &label, true
		}
	}
	if u.URL != nil {
		addr := strings.TrimSpace(*u.URL)
		if addr != "" && !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
			return nil, invalid("the peer address must be an http(s) URL, like https://fog.example.com or http://192.168.4.20:8080")
		}
		if addr != peer.URL {
			edit.URL, res.URLChanged = &addr, true
		}
	}
	for _, c := range u.Capabilities {
		if rules.Known == nil || !rules.Known(c.Capability) {
			return nil, invalid("unknown capability " + c.Capability)
		}
		if c.Direction != store.DirGrant && c.Direction != store.DirUse {
			return nil, invalid("capability direction must be grant or use")
		}
		if c.Config == "" {
			c.Config = "{}"
		}
		if c.Enabled && rules.Check != nil {
			if err := rules.Check(c); err != nil {
				return nil, invalid(err.Error())
			}
		}
		edit.Capabilities = append(edit.Capabilities, store.FedCapabilityEdit{
			Capability: c.Capability, Direction: c.Direction, Config: c.Config, Enabled: c.Enabled,
		})
		res.Capabilities = append(res.Capabilities, c)
	}
	if u.Name != "" && u.Name != peer.Name {
		name := SlugifyName(u.Name)
		edit.Name, res.Renamed = &name, true
	}

	if err := s.Store.EditFedPeer(peer.ID, edit); err != nil {
		return nil, err
	}
	updated, err := s.Store.FedPeerByID(peer.ID)
	if err != nil {
		return nil, err
	}
	res.Peer = updated
	return res, nil
}
