package filesfed

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
)

// Relay tickets.
//
// A contribution's bytes cross only over its author's own peerings: another
// subscriber of the origin sees a stub and fills it from the author. Whether
// that subscriber may see the file at all is the origin's to say — it is the
// origin's share, under the origin's lens — so the author serves the bytes
// only against a ticket the origin signed: this holder, this file of this
// share, for the next few minutes. The origin issues one only when the
// holder's own share from it carries the file with detail beyond a listing,
// not on the floor, and the contribution not withdrawn.

// HeaderRelayTicket carries a relay ticket to the author.
const HeaderRelayTicket = "Fog-Relay-Ticket"

// relayTicketTTL bounds a ticket: long enough to start a download, short
// enough that a narrowed lens takes effect at once.
const relayTicketTTL = 5 * time.Minute

const relayLabel = "fogfed1-relay"

// RelayTicket is what the origin vouches for.
type RelayTicket struct {
	Origin string `json:"origin"` // the origin's instance
	Author string `json:"author"` // the contribution's author
	Holder string `json:"holder"` // the subscriber it was issued to
	Share  int64  `json:"share"`  // the author's share at the origin
	Path   string `json:"path"`   // the file, inside that share
	Exp    string `json:"exp"`
}

// ErrNoRelay: the origin will not vouch for this file to this holder.
var ErrNoRelay = errors.New("the origin does not share this file's bytes with you")

// IssueRelayTicket is the origin's side: holder asks, through its share sh,
// for a ticket to fetch the contribution at inner from its author.
func (s *Service) IssueRelayTicket(holder *store.FedPeer, sh *store.FileFedShare, inner string) (string, *RelayTicket, error) {
	if sh.Detail == DetailListing {
		return "", nil, ErrNoRelay
	}
	if _, ok := s.InLens(sh, inner); !ok {
		return "", nil, ErrNoRelay
	}
	rel := files.Join(sh.Path, inner)
	o, err := s.originFor(rel)
	if err != nil || o.Withdrawn || o.InstanceID == holder.InstanceID {
		return "", nil, ErrNoRelay
	}
	contributed, err := s.Store.FileFedShareByID(o.ShareID)
	if err != nil || !under(rel, contributed.Path) || rel == contributed.Path {
		return "", nil, ErrNoRelay
	}
	id, err := s.Fed.Identity()
	if err != nil {
		return "", nil, err
	}
	t := &RelayTicket{Origin: id.InstanceID, Author: o.InstanceID, Holder: holder.InstanceID, Share: o.ShareID,
		Path: strings.TrimPrefix(rel, contributed.Path+"/"), Exp: time.Now().UTC().Add(relayTicketTTL).Format(time.RFC3339)}
	doc, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(doc) + "." + id.SignTranscript(relayLabel, t.Exp, doc), t, nil
}

// CheckRelayTicket is the author's side: holder presents ticket for the file
// inner of the origin's share shareID. The ticket must be signed by that
// origin — a peer of ours — for exactly this holder, file and author, and
// unexpired.
func (s *Service) CheckRelayTicket(holder *store.FedPeer, originInstance string, shareID int64, inner, ticket string) error {
	docB64, sig, ok := strings.Cut(ticket, ".")
	if !ok {
		return ErrNoRelay
	}
	doc, err := base64.RawURLEncoding.Strict().DecodeString(docB64)
	if err != nil {
		return ErrNoRelay
	}
	var t RelayTicket
	if json.Unmarshal(doc, &t) != nil {
		return ErrNoRelay
	}
	origin, err := s.Store.FedPeerByInstanceID(originInstance)
	if err != nil {
		return ErrNoRelay
	}
	pub, err := fed.ParsePublicKey(origin.PublicKey)
	if err != nil || !fed.VerifyTranscript(pub, relayLabel, t.Exp, doc, sig) {
		return ErrNoRelay
	}
	exp, err := time.Parse(time.RFC3339, t.Exp)
	now := time.Now()
	if err != nil || !exp.After(now) || exp.After(now.Add(relayTicketTTL+fed.MaxSkew)) {
		return ErrNoRelay
	}
	if t.Origin != originInstance || t.Author != s.Fed.InstanceID() || t.Holder != holder.InstanceID ||
		t.Share != shareID || t.Path != inner {
		return ErrNoRelay
	}
	return nil
}
