// Federation: this instance's identity, pairing (invites out, joins in),
// per-peer capability configuration, and the machine-facing /fed/v1 plane
// peers dial. Signed-request auth on the machine surface; session auth on
// /api/federation like the rest of the control plane.
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/feedback"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/flare"
	"gitea.lan/matthew/fog/internal/store"
)

const inviteTTL = 24 * time.Hour

// fedCapabilities is the registry: every capability a peer row can hold, in
// either direction. Unknown names are rejected at the API edge.
var fedCapabilities = map[string]bool{
	store.CapFlareIngest:     true,
	store.CapFeedbackIngest:  true,
	store.CapAppsDirectory:   true,
	store.CapAppsIntroduce:   true,
	filesfed.CapFilesShare:   true,
	chat.CapChat:             true,
	store.CapPeopleDirectory: true,
	store.CapPeopleVisit:     true,
}

// defaultDirectoryConfig is what a new pairing shares: every active member
// (docs/signin-contract.md — friction is the bigger risk between
// households that chose to pair). The operator narrows it per peer.
const defaultDirectoryConfig = `{"people":"members"}`

// grantDirectoryByDefault shares the household's people with a new peer,
// unless this pairing already says otherwise.
func (s *Server) grantDirectoryByDefault(peerID int64) error {
	if _, granted, _ := s.store.FedCapabilityConfig(peerID, store.CapPeopleDirectory, store.DirGrant); granted {
		return nil
	}
	return s.store.SetFedCapability(peerID, store.CapPeopleDirectory, store.DirGrant, defaultDirectoryConfig)
}

// ---- /api/federation (session-authed) ----

type fedPeerView struct {
	store.FedPeer
	Online       bool                  `json:"online"`
	Capabilities []store.FedCapability `json:"capabilities"`
	// DialError is set while our dials toward this peer keep failing —
	// which can be true at the same time as Online (they still reach us).
	// Two directions, two facts; hiding one behind the other is how a
	// moved peer stayed green for days.
	DialError        string `json:"dialError,omitempty"`
	DialFailingSince string `json:"dialFailingSince,omitempty"`
	// Files across peers, for the map's flow labels.
	FilesShared   int `json:"filesShared"`
	FilesReceived int `json:"filesReceived"`
	// PersonAddresses says whether every person this peer advertises
	// carries a person address (docs/signin-contract.md): chat keeps
	// accepting handle-form addresses until every peer does.
	PersonAddresses bool `json:"personAddresses"`
	// Fingerprint is the peer's key as a person reads it (see
	// fed.Fingerprint); FingerprintFull is the whole digest.
	Fingerprint     string `json:"fingerprint,omitempty"`
	FingerprintFull string `json:"fingerprintFull,omitempty"`
	// The link as this side's heartbeats measured it. RttMs is absent until
	// a dial has been answered since fogd started; LinkAnswered of
	// LinkAttempts is how many of the recent dials got any answer. Peers
	// we never dial (no address) carry none of these.
	RttMs        *int   `json:"rttMs,omitempty"`
	RttAt        string `json:"rttAt,omitempty"`
	LinkAnswered int    `json:"linkAnswered"`
	LinkAttempts int    `json:"linkAttempts"`
}

// peerFacts is what the peer list shows per peer but reads in bulk: one
// query per table for every peer, not three per peer.
type peerFacts struct {
	caps         map[int64][]store.FedCapability
	shares, subs map[int64]int
}

// peerFacts loads them. A failed read shows as empty, as a failed per-peer
// read always did: the list is still worth showing.
func (s *Server) peerFacts() peerFacts {
	var f peerFacts
	f.caps, _ = s.store.AllFedCapabilities()
	f.shares, f.subs, _ = s.store.FileFedCountsByPeer()
	return f
}

func (s *Server) fedPeerView(p store.FedPeer, facts peerFacts) fedPeerView {
	caps := facts.caps[p.ID]
	if caps == nil {
		caps = []store.FedCapability{}
	}
	view := fedPeerView{FedPeer: p, Online: fed.Online(&p, time.Now()), Capabilities: caps,
		FilesShared: facts.shares[p.ID], FilesReceived: facts.subs[p.ID]}
	if errMsg, since, failing := s.fed.DialStatus(p.Name); failing {
		view.DialError = errMsg
		view.DialFailingSince = since.UTC().Format(time.RFC3339)
	}
	view.Fingerprint, view.FingerprintFull = fed.Fingerprint(p.PublicKey)
	view.PersonAddresses = true
	for _, dp := range chat.PeerPeople(&p) {
		if dp.Address == "" {
			view.PersonAddresses = false
		}
	}
	if ls := s.fed.LinkStatus(p.ID); ls.Attempts > 0 {
		view.LinkAnswered, view.LinkAttempts = ls.Answered, ls.Attempts
		if ls.Measured {
			ms := int(ls.RTT.Round(time.Millisecond) / time.Millisecond)
			view.RttMs = &ms
			view.RttAt = ls.RTTAt.UTC().Format(time.RFC3339)
		}
	}
	return view
}

// peerFromPath resolves the {id} path segment to a paired peer: 404 when
// there is no such peer, 500 when the lookup itself failed.
func (s *Server) peerFromPath(w http.ResponseWriter, r *http.Request) (*store.FedPeer, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil, false
	}
	peer, err := s.store.FedPeerByID(id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "peer not found")
		return nil, false
	}
	if err != nil {
		writeError(w, err)
		return nil, false
	}
	return peer, true
}

// handleGetFederation is the Connect page's single read. Opening the page
// is what mints the instance identity — federation costs nothing until the
// operator first looks at it.
func (s *Server) handleGetFederation(w http.ResponseWriter, r *http.Request) {
	doc, err := s.fed.InstanceDoc()
	if err != nil {
		writeError(w, err)
		return
	}
	peers, err := s.store.ListFedPeers()
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]fedPeerView, 0, len(peers))
	facts := s.peerFacts()
	for _, p := range peers {
		views = append(views, s.fedPeerView(p, facts))
	}
	invites, err := s.store.ListOpenFedInvites()
	if err != nil {
		writeError(w, err)
		return
	}
	if invites == nil {
		invites = []store.FedInvite{}
	}
	writeJSON(w, http.StatusOK, federationInfo{
		Identity:     identityView(doc),
		Invites:      invites,
		Peers:        views,
		Reachability: s.computeReachability(),
	})
}

// federationInfo is the Connect page's one read. Its fields are in
// alphabetical order, as they came out when this was built as a map.
type federationInfo struct {
	Identity     fedIdentityView   `json:"identity"`
	Invites      []store.FedInvite `json:"invites"`
	Peers        []fedPeerView     `json:"peers"`
	Reachability fedReachability   `json:"reachability"`
}

// fedIdentityView is the instance document as the page shows it: the wire
// document plus the key's fingerprint, which is derived rather than
// exchanged (the document's bytes are what pairing signs, so it stays as
// it is on the wire).
type fedIdentityView struct {
	*fed.InstanceDoc
	Fingerprint     string `json:"fingerprint,omitempty"`
	FingerprintFull string `json:"fingerprintFull,omitempty"`
	// KeyBound: the instance ID is derived from the identity key. A fog
	// whose ID predates that pairs with no new household until it renews
	// (POST /api/federation/identity/renew).
	KeyBound bool `json:"keyBound"`
}

func identityView(doc *fed.InstanceDoc) fedIdentityView {
	short, full := fed.Fingerprint(doc.PublicKey)
	return fedIdentityView{InstanceDoc: doc, Fingerprint: short, FingerprintFull: full, KeyBound: fed.IsDerivedInstanceID(doc.InstanceID)}
}

// renewImpact is what renewing this fog's identity would cut off: the
// households paired with it, which know it by the old ID and key, and the
// chat rooms, whose members are named by it.
type renewImpact struct {
	Error      string   `json:"error"`
	Households []string `json:"households"`
	ChatRooms  int      `json:"chatRooms"`
}

// handleRenewFedIdentity replaces an identity from before key-bound IDs
// with a key-bound one, so this fog can pair with new households. It is
// never done quietly: when anything knows this fog by its old ID — a paired
// household, a chat room — the request must carry {"breakPairings": true},
// and without it the answer is a 409 that says exactly what would stop
// working. Peer rows are kept, so pairing again refreshes each in place.
func (s *Server) handleRenewFedIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BreakPairings bool `json:"breakPairings"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.fed.LegacyIdentity() {
		writeErr(w, http.StatusConflict, fed.ErrNotLegacy.Error())
		return
	}
	var impact renewImpact
	for _, p := range mustList(s.store.ListFedPeers()) {
		if p.PublicKey != "" {
			impact.Households = append(impact.Households, p.DisplayName())
		}
	}
	rooms, err := s.store.AllChatRooms()
	if err != nil {
		writeError(w, err)
		return
	}
	impact.ChatRooms = len(rooms)
	if (len(impact.Households) > 0 || impact.ChatRooms > 0) && !req.BreakPairings {
		impact.Error = renewWarning(impact)
		if impact.Households == nil {
			impact.Households = []string{}
		}
		writeJSON(w, http.StatusConflict, impact)
		return
	}
	oldID, newID, err := s.fed.RenewIdentity()
	if err != nil {
		writeError(w, err)
		return
	}
	if len(impact.Households) > 0 || impact.ChatRooms > 0 {
		s.store.CreateNotice("fed-identity-renewed", "federation", "This fog has a new identity", renewWarning(impact)+" It is done: pair again with each household.")
	}
	s.audit(r, "federation.identity.renew", newID, map[string]any{"from": oldID, "households": len(impact.Households), "chatRooms": impact.ChatRooms})
	s.handleGetFederation(w, r)
}

// renewWarning says, in the operator's terms, what renewing cuts off.
func renewWarning(i renewImpact) string {
	msg := "Renewing gives this fog a new identity, so it can pair with new households."
	if n := len(i.Households); n > 0 {
		msg += fmt.Sprintf(" The %d household(s) paired with it — %s — know it by the old one and will stop recognising it; each must be paired again.", n, strings.Join(i.Households, ", "))
	}
	if i.ChatRooms > 0 {
		msg += fmt.Sprintf(" Chat names people by this fog's identity: the %d chat room(s) here will no longer open for anyone and must be started again.", i.ChatRooms)
	}
	return msg
}

// fedReachability is the honest answer to "can another household dial this
// instance?" — surfaced on the Connect page so a dial-out-only box is a
// stated fact with a next step, not a silent mystery a peer discovers by
// timing out. The federation design tolerates one unreachable side (a
// heartbeat *response* carries our own payload, so a reachable peer keeps us
// both fresh), which is why Reachable=false is a soft state with advice, never
// an error.
type fedReachability struct {
	Reachable bool        `json:"reachable"`
	URL       string      `json:"url,omitempty"`
	Via       string      `json:"via" ts:"\"advertised\" | \"domain\" | \"none\""` // "advertised" | "domain" | "none"
	Tunnel    tunnelReach `json:"tunnel"`
	Advice    string      `json:"advice,omitempty"`
}

// tunnelReach is the Cloudflare tunnel's bearing on reachability: fog can keep
// the connector alive, but the public hostname lives in the Zero Trust
// dashboard, so knowing the tunnel is *up* is what lets us tell the operator
// the one remaining step (advertise that hostname) rather than guess it.
type tunnelReach struct {
	Configured bool `json:"configured"` // a tunnel token is saved
	Running    bool `json:"running"`
}

func (s *Server) computeReachability() fedReachability {
	var r fedReachability
	r.URL = s.fed.AdvertisedURL()
	r.Reachable = r.URL != ""

	override, _ := s.store.GetSetting(fed.SettingAdvertisedURL)
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	switch {
	case override != "":
		r.Via = "advertised"
	case domain != "":
		r.Via = "domain"
	default:
		r.Via = "none"
	}

	if s.tunnel != nil {
		ts := s.tunnel.Status()
		r.Tunnel = tunnelReach{Configured: ts.TokenConfigured, Running: ts.Running}
	}

	if r.Reachable {
		return r
	}
	switch {
	case r.Tunnel.Running:
		r.Advice = "Your Cloudflare tunnel is running — set its public hostname as the advertised URL so peers can dial you directly."
	case r.Tunnel.Configured:
		r.Advice = "A Cloudflare tunnel is configured but not running. Start it, then advertise its hostname so peers can reach you."
	default:
		r.Advice = "No household can dial you yet. Set a domain, or run a Cloudflare tunnel and advertise its hostname. Until then you can still federate with any peer that is reachable itself — one reachable side keeps both in sync."
	}
	return r
}

func (s *Server) handlePutFederationIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
	if req.URL != "" && !strings.HasPrefix(req.URL, "http") {
		badRequest(w, "advertised URL must start with http:// or https://")
		return
	}
	if err := s.store.SetSetting(fed.SettingAdvertisedURL, req.URL); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "federation.identity.update", "", map[string]any{"urlSet": req.URL != ""})
	s.handleGetFederation(w, r)
}

// inviteGrant is one capability pre-selected at invite time, applied as a
// grant row the moment the invite is claimed.
type inviteGrant struct {
	Capability string          `json:"capability"`
	Config     json.RawMessage `json:"config,omitempty"`
}

func (s *Server) handleCreateFedInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Grants []inviteGrant `json:"grants"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	for _, g := range req.Grants {
		if !fedCapabilities[g.Capability] {
			badRequest(w, "unknown capability "+g.Capability)
			return
		}
		// An invite's grants land unseen when it is claimed, so they meet
		// the same config rules an edit does, now.
		if err := checkCapabilityConfig(fed.CapabilityChange{Capability: g.Capability, Direction: store.DirGrant,
			Config: string(g.Config), Enabled: true}); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	url := s.fed.AdvertisedURL()
	if url == "" {
		// No domain or advertised URL — fall back to the address the
		// operator reached this UI at, exactly as joins do. A LAN-only box
		// can still mint invites for LAN peers; refusing here used to leave
		// a moved, domainless instance unable to even start a re-pair.
		url = requestBaseURL(r)
	}
	if url == "" {
		badRequest(w, "this instance has no reachable URL yet — set a domain (or an advertised URL) so the other side can dial it")
		return
	}
	if _, err := s.fed.Identity(); err != nil {
		writeError(w, err)
		return
	}
	if s.fed.LegacyIdentity() {
		writeErr(w, http.StatusConflict, fed.ErrLegacySelf.Error())
		return
	}
	secret, digest := auth.NewToken()
	grantsJSON, _ := json.Marshal(req.Grants)
	expires := time.Now().UTC().Add(inviteTTL).Format(time.RFC3339)
	invite, err := s.store.CreateFedInvite(digest, string(grantsJSON), expires)
	if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "federation.invite.create", "", map[string]any{"grants": req.Grants})
	// The code appears exactly once, here. Only the secret's digest survives.
	// The name rides along so the joiner sees a household, not a blob — it is a
	// label they can preview and override, never an identity claim.
	name, _ := s.store.GetSetting(store.SettingServerName)
	writeJSON(w, http.StatusOK, map[string]any{
		"invite": invite,
		"code":   fed.InviteCode(url, secret, name, mustEncKey(s)),
	})
}

// handleFedJoinPreview decodes a pasted invite without pairing, so the joiner
// can confirm who and where before committing. Decode-only: no network, no
// secret spent — a garbled or non-FOG paste is a plain 400.
func (s *Server) handleFedJoinPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	inv, err := fed.ParseInvite(req.Code)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": inv.Name,
		"url":  inv.URL,
	})
}

// capLabels renders each federation capability as the thing a peer can do with
// it, phrased from the receiving side — the words the impersonation mirror puts
// in the peer's mouth.
var capLabels = map[string]string{
	store.CapFlareIngest:     "receive the error reports this household forwards",
	store.CapFeedbackIngest:  "receive the feedback this household's people write",
	store.CapAppsDirectory:   "see this household's shared apps",
	store.CapAppsIntroduce:   "connect its apps to this household's apps",
	filesfed.CapFilesShare:   "pull this household's shared folders",
	chat.CapChat:             "bring its people into rooms with this household's",
	store.CapPeopleDirectory: "see who is in this household — names and handles, never email or what apps they use",
	store.CapPeopleVisit:     "sign this household's people in to its apps as visitors, on this household's word",
}

// capOrder fixes the sentence order so the mirror reads the same every time.
var capOrder = []string{
	store.CapPeopleDirectory, store.CapPeopleVisit, store.CapAppsDirectory, store.CapAppsIntroduce, filesfed.CapFilesShare, chat.CapChat,
	store.CapFlareIngest, store.CapFeedbackIngest,
}

// handleFedPeerMirror answers "what can this peer see, receiving from me?" —
// the impersonation mirror. It renders exactly what our heartbeat sends this
// peer (the granted capabilities and the app directory we publish to them),
// splits the capability registry into what they can and cannot see, and states
// it in the shared consent grammar from the peer's point of view. Read-only:
// it grants nothing and sends nothing, it just shows the operator the other
// side of a grant they already made.
func (s *Server) handleFedPeerMirror(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.peerFromPath(w, r)
	if !ok {
		return
	}
	payload, err := s.fed.PayloadFor(peer)
	if err != nil {
		writeError(w, err)
		return
	}
	granted := map[string]bool{}
	for _, g := range payload.Grants {
		granted[g] = true
	}
	self, _ := s.store.GetSetting(store.SettingServerName)
	if self == "" {
		self = "this household"
	}
	peerName := peer.DisplayName()

	type capView struct {
		Capability string `json:"capability"`
		Label      string `json:"label"`
		Granted    bool   `json:"granted"`
	}
	caps := make([]capView, 0, len(capOrder))
	var sees, hidden []string
	for _, key := range capOrder {
		label := capLabels[key]
		g := granted[key]
		caps = append(caps, capView{Capability: key, Label: label, Granted: g})
		if g {
			sees = append(sees, label)
		} else {
			hidden = append(hidden, label)
		}
	}

	sentence := fed.ImpactSentence(peerName, "what "+self+" federates to it", sees, hidden, "", "")

	writeJSON(w, http.StatusOK, map[string]any{
		"as":           peerName,
		"from":         self,
		"url":          payload.URL,
		"capabilities": caps,
		"directory":    payload.Directory,
		"sentence":     sentence,
	})
}

// ledgerEntry is one boundary crossing: the raw audit row plus the plain
// reading Project K puts in front of a person, and the axis it belongs to.
type ledgerEntry struct {
	store.AuditEntry
	Category string `json:"category"` // "trust" | "discovery" | "sharing"
	Summary  string `json:"summary"`
	// Source names the app a row came from (Project K's cross-app
	// aggregation, app_ledger.go); empty for fog's own rows.
	Source string `json:"source,omitempty"`
}

// ledgerVerbs maps a boundary audit action to its axis and a plain phrase. An
// action absent here still shows (raw), so a new federation verb never goes
// silently missing from the ledger.
var ledgerVerbs = map[string]struct{ cat, phrase string }{
	"federation.pair":             {"trust", "paired"},
	"federation.pair.refused":     {"trust", "refused a pairing that claimed an already-paired household under another key"},
	"federation.unpair":           {"trust", "unpaired"},
	"federation.capability":       {"trust", "changed what a peer may do"},
	"federation.identity.update":  {"trust", "changed this household's address"},
	"federation.url":              {"trust", "corrected a peer's address"},
	"federation.label":            {"trust", "relabelled a peer"},
	"federation.invite.create":    {"discovery", "created an invite"},
	"federation.invite.delete":    {"discovery", "withdrew an invite"},
	"connect.request":             {"discovery", "asked to connect an app to a peer's"},
	"connect.serve":               {"discovery", "connected a peer's app to one here"},
	"files.share.peer":            {"sharing", "shared a folder with a peer"},
	"files.share.peer.update":     {"sharing", "changed a folder share"},
	"files.share.peer.revoke":     {"sharing", "stopped sharing a folder"},
	"files.subscribe":             {"sharing", "subscribed to a peer's folder"},
	"files.subscription.update":   {"sharing", "changed a subscription"},
	"files.contribution":          {"sharing", "received a contribution"},
	"files.contribution.reject":   {"sharing", "rejected a contribution"},
	"files.contribution.withdraw": {"sharing", "withdrew a contribution"},
	"chat.keys.served":            {"sharing", "handed a peer a person's key material"},
	"chat.invite.received":        {"sharing", "a peer invited someone here into a room"},
	"chat.commit":                 {"sharing", "changed a room's membership"},
	"chat.household.cut":          {"sharing", "a household left every room it shared"},
}

// handleFederationLedger answers "who can see what, and what moved" — the
// unified feed of everything that has crossed a federation boundary, newest
// first, optionally narrowed to one peer with ?peer=<name>. One place, one
// vocabulary, drawn from the audit log every federation verb already writes to.
func (s *Server) handleFederationLedger(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	// A folder share records the peer by its numeric id, not its name, so
	// resolve the requested name to an id too — otherwise scoping to one peer
	// silently drops every share crossing.
	peer := r.URL.Query().Get("peer")
	peerID := ""
	// Apps know a fog peer by its instance id, so a scope to one peer is
	// carried to them as that.
	peerInstance := ""
	if peer != "" {
		if peers, perr := s.store.ListFedPeers(); perr == nil {
			for _, p := range peers {
				if p.Name == peer || p.DisplayName() == peer {
					peerID = strconv.FormatInt(p.ID, 10)
					peerInstance = p.InstanceID
					break
				}
			}
		}
	}
	rows, err := s.store.FederationLedger(limit, peer, peerID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]ledgerEntry, 0, len(rows))
	for _, e := range rows {
		cat, phrase := "trust", e.Action
		if v, ok := ledgerVerbs[e.Action]; ok {
			cat, phrase = v.cat, v.phrase
		}
		out = append(out, ledgerEntry{AuditEntry: e, Category: cat, Summary: phrase})
	}
	// The apps' own crossings beside fog's — one place, one vocabulary. A
	// peer scope an app cannot resolve (no paired instance matched the
	// name) asks the apps for nothing rather than for everything.
	if peer == "" || peerInstance != "" {
		out = mergeLedger(out, s.appLedgerEntries(r.Context(), peerInstance, limit), limit)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (s *Server) handleDeleteFedInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := s.store.DeleteFedInvite(id); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "federation.invite.delete", "", nil)
	writeOK(w)
}

func (s *Server) handleFedJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	held := map[string]bool{}
	for _, p := range mustList(s.store.ListFedPeers()) {
		held[p.InstanceID] = p.PublicKey != ""
	}
	peer, err := s.fed.Join(r.Context(), req.Code, requestBaseURL(r))
	if errors.Is(err, fed.ErrKeyConflict) || errors.Is(err, fed.ErrPairWithSelf) || errors.Is(err, fed.ErrLegacySelf) || errors.Is(err, fed.ErrLegacyPeer) {
		s.audit(r, "federation.pair.refused", "", map[string]any{"role": "joiner", "reason": err.Error()})
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	// A new peer gets the default grant; one already held keeps exactly the
	// grants its row has.
	refreshed := held[peer.InstanceID]
	if !refreshed {
		if err := s.grantDirectoryByDefault(peer.ID); err != nil {
			writeError(w, err)
			return
		}
	}
	s.audit(r, "federation.pair", peer.Name, map[string]any{"role": "joiner", "refreshed": refreshed})
	s.notifyAppsDirectoryChanged(DirChangePaired)
	writeJSON(w, http.StatusOK, s.fedPeerView(*peer, s.peerFacts()))
}

// mustEncKey is this instance's encryption key for an invite: its identity
// was loaded just above, so it is there.
func mustEncKey(s *Server) string {
	id, err := s.fed.Identity()
	if err != nil {
		return ""
	}
	return id.EncPublicB64()
}

// requestBaseURL reconstructs the address the operator reached this UI at —
// the best available guess for a peer-dialable address when no domain or
// advertised URL is configured. Loopback addresses are useless to a peer
// and yield "".
func requestBaseURL(r *http.Request) string {
	if r.Host == "" {
		return ""
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return ""
	}
	return requestOrigin(r)
}

// handleUpdateFedPeer relabels or renames a peer and/or reconfigures its
// capability rows. Capabilities arrive as the full desired state for the
// rows named: enabled upserts, disabled deletes.
func (s *Server) handleUpdateFedPeer(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.peerFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		// Label is a pointer so that "" can mean "clear the label and go
		// back to the advertised name", which a plain string couldn't say
		// without also clearing it on every capability-only request.
		Label *string `json:"label"`
		// URL is the operator's override of the peer's dial address — the
		// repair path when a peer moved networks and its stored address went
		// stale (heartbeats can only refresh it while at least one direction
		// still connects). "" makes the peer dial-in only.
		URL          *string `json:"url"`
		Capabilities []struct {
			Capability string          `json:"capability"`
			Direction  string          `json:"direction"`
			Config     json.RawMessage `json:"config,omitempty"`
			Enabled    bool            `json:"enabled"`
		} `json:"capabilities"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	upd := fed.PeerUpdate{Name: req.Name, Label: req.Label, URL: req.URL}
	for _, c := range req.Capabilities {
		upd.Capabilities = append(upd.Capabilities, fed.CapabilityChange{
			Capability: c.Capability, Direction: c.Direction, Config: string(c.Config), Enabled: c.Enabled,
		})
	}
	res, err := s.fed.UpdatePeer(peer, upd, fed.CapabilityRules{
		Known: func(c string) bool { return fedCapabilities[c] },
		Check: checkCapabilityConfig,
	})
	var invalid *fed.InvalidUpdateError
	if errors.As(err, &invalid) {
		badRequest(w, invalid.Msg)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	updated := res.Peer
	// The audit records name the peer by its stable name as it was, since
	// that is what every other record about it uses.
	if res.LabelChanged {
		s.audit(r, "federation.label", peer.Name, map[string]any{"label": updated.Label})
	}
	if res.URLChanged {
		s.audit(r, "federation.url", peer.Name, map[string]any{"url": updated.URL})
	}
	for _, c := range res.Capabilities {
		if !c.Enabled && c.Capability == chat.CapChat && c.Direction == store.DirGrant {
			s.chat.Fed.PeerCut(updated, "chat revoked")
			s.audit(r, "chat.household.cut", peer.Name, map[string]any{"reason": "revoked"})
		}
		if !c.Enabled && c.Capability == filesfed.CapFilesShare && c.Direction == store.DirGrant {
			if err := s.filesFed.GrantWithdrawn(r.Context(), updated); err != nil {
				s.log.Warn("files fed: stopping shares after the grant was withdrawn", "peer", peer.Name, "err", err)
			}
		}
		s.audit(r, "federation.capability", peer.Name, map[string]any{"capability": c.Capability, "direction": c.Direction, "enabled": c.Enabled})
		if c.Direction == store.DirGrant && (c.Capability == store.CapPeopleVisit || c.Capability == store.CapPeopleDirectory) {
			s.recallVisits(updated.ID)
		}
		// Connect's credentials live in the apps that minted them; the grant
		// row only gates new connections. Withdrawing apps.introduce, or
		// narrowing its allow-list, has to reach those too, as unpairing
		// does, or a peer's apps keep every door the grant opened: each app
		// the grant no longer covers is asked to revoke what it minted.
		if c.Direction == store.DirGrant && c.Capability == store.CapAppsIntroduce {
			scope := fed.AppScope{} // withdrawn: covers nothing
			if c.Enabled {
				scope, _ = fed.ParseAppScope(c.Config)
			}
			p := *updated
			s.goSafe("connect grant revoke", func() { s.revokeAppGrantsOutside(s.bgContext(), &p, scope) })
		}
	}
	// A capability change only reaches the peer through a heartbeat
	// exchange, so kick one now rather than waiting out the ticker.
	// Dial-in-only peers (no URL) still pick it up on their next inbound
	// dial: the reply payload is computed fresh. A changed address gets the
	// same kick — the operator fixing a stale URL wants to see it work now.
	if (len(res.Capabilities) > 0 || res.URLChanged) && updated.URL != "" {
		p := *updated
		s.goSafe("federation heartbeat", func() {
			ctx, cancel := context.WithTimeout(s.bgContext(), 30*time.Second)
			defer cancel()
			if err := s.fed.HeartbeatPeer(ctx, &p); err != nil {
				s.log.Debug("fed heartbeat after capability change", "peer", p.Name, "err", err)
			}
		})
	}
	s.notifyAppsDirectoryChanged(DirChangeUpdated)
	writeJSON(w, http.StatusOK, s.fedPeerView(*updated, s.peerFacts()))
}

// checkCapabilityConfig holds the config rules of the capabilities that
// have any, so what is stored is always something enforcement reads exactly
// as the operator meant it — never a shape a reader would have to guess at,
// and so might read as "everything".
func checkCapabilityConfig(c fed.CapabilityChange) error {
	cfg := strings.TrimSpace(c.Config)
	if cfg == "" {
		cfg = "{}"
	}
	strict := func(v any) error {
		dec := json.NewDecoder(strings.NewReader(cfg))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return err
		}
		if dec.More() {
			return errors.New("trailing data")
		}
		return nil
	}
	switch {
	case c.Capability == store.CapFeedbackIngest && c.Direction == store.DirUse:
		var fc struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal([]byte(cfg), &fc) != nil || !feedback.ValidMode(fc.Mode) {
			return errors.New("sending feedback needs a mode: note or full")
		}
	case c.Capability == store.CapFlareIngest && c.Direction == store.DirUse:
		var fc struct {
			Mode string    `json:"mode"`
			Apps *[]string `json:"apps"`
		}
		if strict(&fc) != nil || (fc.Mode != "redacted" && fc.Mode != "full") {
			return errors.New(`forwarding errors needs {"mode": "redacted" | "full", "apps"?: ["name", …]}`)
		}
		if fc.Apps != nil && len(*fc.Apps) == 0 {
			return errors.New("forwarding errors from no apps is not forwarding: leave apps out for every app, or name some")
		}
	case (c.Capability == store.CapAppsDirectory || c.Capability == store.CapAppsIntroduce) && c.Direction == store.DirGrant:
		if _, err := fed.ParseAppScope(cfg); err != nil {
			return errors.New(c.Capability + ": " + err.Error())
		}
	case (c.Capability == store.CapPeopleDirectory || c.Capability == store.CapPeopleVisit) && c.Direction == store.DirGrant:
		var pc struct {
			People json.RawMessage `json:"people"`
		}
		if strict(&pc) != nil || !validPeopleScope(pc.People) {
			return errors.New(c.Capability + ` needs a people scope: {"people": "members"} for everyone, or {"people": ["handle", …]}`)
		}
	case c.Capability == chat.CapChat && c.Direction == store.DirGrant:
		var cc struct {
			AcceptThirdHouseholds *bool `json:"acceptThirdHouseholds"`
		}
		if strict(&cc) != nil {
			return errors.New(`chat needs {"acceptThirdHouseholds"?: true | false}`)
		}
	case c.Capability == filesfed.CapFilesShare && c.Direction == store.DirUse:
		var fc struct {
			BandwidthBps *int64 `json:"bandwidthBps"`
		}
		if strict(&fc) != nil || (fc.BandwidthBps != nil && *fc.BandwidthBps < 0) {
			return errors.New(`files.share needs {"bandwidthBps"?: bytes per second}`)
		}
	}
	return nil
}

// validPeopleScope accepts the two people scopes: every member ("members",
// or "all", which older configs say), or a list of handles. A config without
// one says nobody — readers fail closed on it — so it is refused here rather
// than stored as a grant that grants nothing.
func validPeopleScope(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var all string
	if json.Unmarshal(raw, &all) == nil {
		return all == "members" || all == "all"
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return false
	}
	for _, h := range list {
		if strings.TrimSpace(h) == "" {
			return false
		}
	}
	return true
}

// handleRecheckFedPeer dials one peer now and reports what actually came
// back. It exists because the alternative — a button that clears the
// unpaired stamp — would let an operator hide a fact rather than test it.
// This tests it: a peer that has really unpaired us refuses again and keeps
// the stamp, a peer that was mid-restore answers and clears it in the same
// exchange, and a peer that is merely unreachable says so.
func (s *Server) handleRecheckFedPeer(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.peerFromPath(w, r)
	if !ok {
		return
	}
	if peer.URL == "" {
		badRequest(w, "this peer has no address to dial — it only ever reaches us")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	hbErr := s.fed.HeartbeatPeer(ctx, peer)

	// Re-read rather than reusing the pre-dial row: the exchange is what
	// just changed the stamp, in either direction.
	updated, err := s.store.FedPeerByID(peer.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	res := map[string]any{"peer": s.fedPeerView(*updated, s.peerFacts()), "ok": hbErr == nil}
	if hbErr != nil {
		res["unpaired"] = fed.Unpaired(hbErr)
		res["error"] = hbErr.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

// handleUnpairFedPeer ends a federation. Signature access dies with the
// row; error groups the peer relayed cascade away; contracted apps are
// asked (best-effort) to revoke any credentials Connect minted.
func (s *Server) handleUnpairFedPeer(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.peerFromPath(w, r)
	if !ok {
		return
	}
	// Files first, while the channel still exists to say "revoked" and the
	// rows still name the folders to remove. Then chat: their people leave
	// every room hubbed here, and replicas of their rooms are archived.
	s.filesFed.PeerUnpaired(r.Context(), peer)
	s.chat.Fed.PeerCut(peer, "unpaired")
	s.audit(r, "chat.household.cut", peer.Name, map[string]any{"reason": "unpaired"})
	// Visits our people are making there end there, said while the channel
	// still exists; the visits its people are making here end with the row
	// (the cascade reaches visitor_sessions, whose trigger tells the apps).
	if err := s.store.RecallVisits(peer.ID, nil); err == nil {
		s.logouts.Run(r.Context())
	}
	if err := s.store.DeleteFedPeer(peer.ID); err != nil {
		writeError(w, err)
		return
	}
	s.kickLogouts()
	s.audit(r, "federation.unpair", peer.Name, nil)
	s.goSafe("federation grant revoke", func() { s.revokeAppGrants(s.bgContext(), peer) })
	s.notifyAppsDirectoryChanged(DirChangeUnpaired)
	writeOK(w)
}

// ---- /fed/v1 (machine plane, signed requests) ----

// fedAuthed is an inner request's handler asking who sent it: the peer the
// envelope (serveSealed) authenticated — signature, replay record, keys —
// and the inner body, opened. A request that didn't arrive in the envelope
// has no sender here.
func (s *Server) fedAuthed(w http.ResponseWriter, r *http.Request, maxBody int64) (*store.FedPeer, []byte, bool) {
	fc := fedCallOf(r)
	if fc == nil || fc.peer == nil {
		fedReject(w, fed.CodeEncryptionRequired, "fed requests travel sealed, to "+fed.SealedPath)
		return nil, nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		badRequest(w, "body too large or unreadable")
		return nil, nil, false
	}
	return fc.peer, body, true
}

// maxPairBody bounds a pair request — an instance document and a secret —
// and so everything a caller without a row may send.
const maxPairBody = 64 << 10

// Capacity. Two pools, so strangers never stand between a peer and this fog.
//
// A request whose signature verifies from its headers alone — its body's
// digest declared in Fog-Content-SHA256 — is a peer's before a byte of its
// body is read: it takes one of that peer's fedPeerSlots and its body may
// take as long as it keeps arriving. Nothing a stranger does touches that
// pool, and a peer can hold no more of it than its own slots.
//
// Anything else — a pairing, a caller this fog doesn't know, headers that
// don't verify — is read unauthenticated: one of fedUnauthSlots in all and
// fedUnauthPerAddr per client address (the outer request's, as fogd's proxy
// vouches for it), no more than a pairing's worth from an unknown caller or
// fedUnauthMaxBody from a known one, and under an absolute deadline:
// fedUnauthGrace plus that much at fedUnauthMinRate. Trickling holds a slot
// no longer than that, whatever arrives.
const (
	fedUnauthSlots   = 32
	fedUnauthPerAddr = 4
	fedUnauthMaxBody = 1 << 20
	fedUnauthMinRate = 16 << 10 // bytes a second
	fedPeerSlots     = 8
)

// fedUnauthGrace is the unauthenticated phase's fixed allowance.
const fedUnauthGrace = 10 * time.Second

// fedCapacity is one server's pools (kept beside the Server rather than in
// it, so the fed plane owns them whole).
type fedCapacity struct {
	mu     sync.Mutex
	grace  time.Duration // fedUnauthGrace; a test shortens it
	unauth chan struct{}
	byAddr map[string]int
	byPeer map[int64][]*peerHold
	// spooled is the bytes on disk in fed-spool now, against fedSpoolMax.
	spooled int64
}

// peerHold is one of a peer's slots, held by a request whose headers
// verified. Until its body is in and hashes to what those headers signed,
// it may be anybody's — headers can be copied — so it is evictable: a
// later request of the peer's takes its slot, cutting its read short.
type peerHold struct {
	verified bool
	evict    func()
	start    time.Time
	read     atomic.Int64 // body bytes in so far
}

// fedEvictGrace is how long a holder is given before its rate is judged.
const fedEvictGrace = 2 * time.Second

// stalled reports whether a hold's body is arriving slower than
// fedUnauthMinRate, once it has had fedEvictGrace to start.
func (h *peerHold) stalled(now time.Time) bool {
	d := now.Sub(h.start)
	if d < fedEvictGrace {
		return false
	}
	return h.read.Load() < int64(d.Seconds()*fedUnauthMinRate)
}

var fedCapBy sync.Map // *Server -> *fedCapacity

func (s *Server) fedCap() *fedCapacity {
	c, _ := fedCapBy.LoadOrStore(s, &fedCapacity{grace: fedUnauthGrace, unauth: make(chan struct{}, fedUnauthSlots), byAddr: map[string]int{}, byPeer: map[int64][]*peerHold{}})
	return c.(*fedCapacity)
}

// fedUnauthSlots is the unauthenticated pool, as tests watch it fill.
func (s *Server) fedUnauthSlots() chan struct{} { return s.fedCap().unauth }

// takeUnauth takes a slot in the unauthenticated pool for addr, or says
// there is none.
func (c *fedCapacity) takeUnauth(addr string) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byAddr[addr] >= fedUnauthPerAddr {
		return nil, false
	}
	select {
	case c.unauth <- struct{}{}:
	default:
		return nil, false
	}
	c.byAddr[addr]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			if c.byAddr[addr]--; c.byAddr[addr] <= 0 {
				delete(c.byAddr, addr)
			}
			c.mu.Unlock()
			<-c.unauth
		})
	}, true
}

// takePeer takes one of a peer's own slots for a request whose headers
// verified; evict cuts that request short if a later one needs the slot
// before its body has verified. When every slot is held, the oldest
// holder whose body hasn't verified gives way; only when all of them are
// verified requests of the peer's is the newcomer turned away.
func (c *fedCapacity) takePeer(id int64, verified bool, evict func()) (*peerHold, func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	holds := c.byPeer[id]
	if len(holds) >= fedPeerSlots {
		// Only a holder whose body hasn't verified and is stalling gives
		// way: one making real progress keeps its slot.
		now := time.Now()
		k := slices.IndexFunc(holds, func(h *peerHold) bool { return !h.verified && h.stalled(now) })
		if k < 0 {
			return nil, nil, false
		}
		holds[k].evict()
		holds = slices.Delete(holds, k, k+1)
	}
	h := &peerHold{verified: verified, evict: evict, start: time.Now()}
	c.byPeer[id] = append(holds, h)
	var once sync.Once
	return h, func() {
		once.Do(func() {
			c.mu.Lock()
			hs := c.byPeer[id]
			if k := slices.Index(hs, h); k >= 0 {
				hs = slices.Delete(hs, k, k+1)
			}
			if len(hs) == 0 {
				delete(c.byPeer, id)
			} else {
				c.byPeer[id] = hs
			}
			c.mu.Unlock()
		})
	}, true
}

// verify marks a hold's body verified: from now on it is the peer's.
func (c *fedCapacity) verify(h *peerHold) {
	c.mu.Lock()
	h.verified = true
	c.mu.Unlock()
}

// fedSpoolMax bounds the fed bodies spooled to disk at once, across every
// request.
const fedSpoolMax = 1 << 30

// reserveSpool takes n bytes of the spool's allowance, or says there is
// none left.
func (c *fedCapacity) reserveSpool(n int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spooled+n > fedSpoolMax {
		return false
	}
	c.spooled += n
	return true
}

func (c *fedCapacity) freeSpool(n int64) {
	c.mu.Lock()
	c.spooled -= n
	c.mu.Unlock()
}

// fedRouteCap is the most plaintext an inner route takes: the envelope
// carrying it is held to that while it is still arriving. Each is at least
// its handler's own limit, which still applies as the body is opened.
func fedRouteCap(method, p string) int64 {
	switch {
	case method == http.MethodGet || method == http.MethodDelete || method == http.MethodHead:
		return 64 << 10
	case p == "/fed/v1/pair":
		return maxPairBody
	case p == "/fed/v1/flare/envelope" || p == "/fed/v1/feedback" || p == "/fed/v1/chat/notify":
		return 8 << 20
	case method == http.MethodPatch && strings.HasPrefix(p, "/fed/v1/files/shares/"):
		return contribChunkMax
	case strings.HasPrefix(p, "/fed/v1/chat/rooms/") && strings.HasSuffix(p, "/blobs"):
		return chat.BlobChunk + 1
	case strings.HasPrefix(p, "/fed/v1/chat/rooms/") && strings.HasSuffix(p, "/submit"):
		return int64(chat.MaxPayload) * 3
	case strings.HasPrefix(p, "/fed/v1/chat/rooms/") && strings.HasSuffix(p, "/groupinfo"):
		return int64(chat.MaxPayload) * 2
	}
	return 1 << 20
}

// fedRouteGrant is the grant an inner route is refused without; a peer
// lacking it may send no more than a refusal's worth there.
func fedRouteGrant(p string) string {
	switch {
	case p == "/fed/v1/flare/envelope":
		return store.CapFlareIngest
	case p == "/fed/v1/feedback":
		return store.CapFeedbackIngest
	case strings.HasPrefix(p, "/fed/v1/chat/"):
		return chat.CapChat
	case strings.HasPrefix(p, "/fed/v1/files/shares/"):
		return filesfed.CapFilesShare
	}
	return ""
}

// fedRouteMax is the largest fedRouteCap.
const fedRouteMax = chat.BlobChunk + 1

// fedReadIdle is how long an authenticated peer's body may stall before
// the connection is cut: slow is fine, stopped is not.
const fedReadIdle = 30 * time.Second

// fedMemBody is how much of a fed body is held in memory while it is read;
// past it the rest is spooled to a temporary file until its signature
// verifies.
const fedMemBody = 1 << 20

// fedBody is an outer request's body as read: in memory, or spooled to a
// file once it outgrew fedMemBody, with the sha256 its signature covers.
type fedBody struct {
	mem    []byte
	file   *os.File
	size   int64
	digest string
	// spooled is what this body holds of the spool's allowance.
	spooled int64
	sp      *fedSpool
}

// fedSpool is where bodies past fedMemBody go: a folder under fog's data
// directory — not /tmp, which is memory on the systems fog runs on — and
// an allowance shared by every request.
type fedSpool struct {
	dir string
	cap *fedCapacity
}

// errFedSpoolFull: the spool's allowance is used up.
var errFedSpoolFull = errors.New("fed spool full")

var fedSpoolSwept sync.Map // dir -> struct{}

// fedSpoolFor is this server's spool, swept of a previous run's leftovers
// the first time it is used.
func (s *Server) fedSpoolFor() *fedSpool {
	dir := filepath.Join(s.cfg.DataDir, "fed-spool")
	if _, done := fedSpoolSwept.LoadOrStore(dir, struct{}{}); !done {
		os.RemoveAll(dir)
	}
	os.MkdirAll(dir, 0o700)
	return &fedSpool{dir: dir, cap: s.fedCap()}
}

func (b *fedBody) bytes() ([]byte, error) {
	if b.file == nil {
		return b.mem, nil
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(b.file)
}

// reader reads the body again from its start, from wherever it is held.
func (b *fedBody) reader() (io.Reader, error) {
	if b.file == nil {
		return bytes.NewReader(b.mem), nil
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return bufio.NewReaderSize(b.file, 64<<10), nil
}

func (b *fedBody) close() {
	if b.file != nil {
		b.file.Close()
		os.Remove(b.file.Name())
		b.file = nil
	}
	if b.spooled > 0 && b.sp != nil {
		b.sp.cap.freeSpool(b.spooled)
		b.spooled = 0
	}
}

// spoolWrite writes p to the spool file, against the allowance.
func (b *fedBody) spoolWrite(p []byte) error {
	if !b.sp.cap.reserveSpool(int64(len(p))) {
		return errFedSpoolFull
	}
	b.spooled += int64(len(p))
	_, err := b.file.Write(p)
	return err
}

// errFedTooLarge: the body ran past what its route takes.
var errFedTooLarge = errors.New("fed body too large")

// readFedBody reads r's body up to limit, hashing it as it comes, holding
// the first fedMemBody in memory and spooling the rest. With a deadline the
// whole body must be in by then; without one every read pushes the
// connection's deadline fedReadIdle out, so a body that keeps arriving may
// take as long as it needs and one that stops is cut off. Once the start of
// the body is in, sizeFor may lower the limit to what its inner route takes.
func readFedBody(w http.ResponseWriter, r *http.Request, limit int64, deadline time.Time, sizeFor func(prefix []byte) int64, sp *fedSpool, progress *atomic.Int64) (*fedBody, error) {
	rc := http.NewResponseController(w)
	if !deadline.IsZero() {
		rc.SetReadDeadline(deadline)
	}
	src := http.MaxBytesReader(w, r.Body, limit)
	h := sha256.New()
	b := &fedBody{sp: sp}
	buf := make([]byte, 32<<10)
	peeked := sizeFor == nil
	peek := func() {
		if peeked {
			return
		}
		peeked = true
		if l := sizeFor(b.mem); l < limit {
			limit = l
		}
	}
	for {
		if deadline.IsZero() {
			rc.SetReadDeadline(time.Now().Add(fedReadIdle))
		}
		n, err := src.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			b.size += int64(n)
			if progress != nil {
				progress.Store(b.size)
			}
			switch {
			case b.file != nil:
				if werr := b.spoolWrite(buf[:n]); werr != nil {
					b.close()
					return nil, werr
				}
			case len(b.mem)+n > fedMemBody:
				peek()
				if b.size > limit {
					return nil, errFedTooLarge
				}
				f, ferr := os.CreateTemp(sp.dir, "body-*")
				if ferr != nil {
					return nil, ferr
				}
				b.file = f
				if werr := b.spoolWrite(append(b.mem, buf[:n]...)); werr != nil {
					b.close()
					return nil, werr
				}
				b.mem = nil
			default:
				b.mem = append(b.mem, buf[:n]...)
			}
			if !peeked && len(b.mem) >= fed.PeekPrefix {
				peek()
			}
			if b.size > limit {
				b.close()
				return nil, errFedTooLarge
			}
		}
		if err == io.EOF {
			peek()
			if b.size > limit {
				b.close()
				return nil, errFedTooLarge
			}
			break
		}
		if err != nil {
			b.close()
			return nil, err
		}
	}
	rc.SetReadDeadline(time.Time{})
	b.digest = hex.EncodeToString(h.Sum(nil))
	return b, nil
}

// fedInnerPath reports whether an inner request's path is one the envelope
// may carry: a /fed/v1 route, already clean (no "..", no doubled slash — the
// mux would otherwise redirect it somewhere else), and not the envelope
// itself. The rest of fogd — the API, the UI — is never reachable this way.
func fedInnerPath(p string) bool {
	return strings.HasPrefix(p, "/fed/v1/") && path.Clean(p) == p && p != fed.SealedPath
}

// fedCall is what serveSealed hands an inner request: its authenticated
// sender, or — for the pair hop, whose sender has no row yet — the outer
// request the pair handler verifies itself.
type fedCall struct {
	peer *store.FedPeer
	pair bool
	// claimed: the outer request is already in the replay record (its
	// headers verified and were reserved), so the pair handler doesn't
	// claim it again.
	claimed bool
	// clientIP is the outer request's client address, as clientIP reads it
	// (the proxy's vouched header, or the connection's): the inner request
	// carries the proxy's own address.
	clientIP string
	// The outer request, for the pair hop.
	outer  http.Header
	target string
	wire   []byte
}

type fedCallKey struct{}

func fedCallOf(r *http.Request) *fedCall {
	fc, _ := r.Context().Value(fedCallKey{}).(*fedCall)
	return fc
}

// declaredDigest is the body digest a request declares, when it is one.
func declaredDigest(h http.Header) string {
	d := h.Get(fed.HeaderContentSHA256)
	if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
		return ""
	}
	return d
}

// serveSealed is the fed plane's one door: a POST to fed.SealedPath carrying
// a sealed inner request. It authenticates the outer request — the peer by
// Fog-Instance, its signature, the replay record, its encryption key, pinned
// or learned — opens it, and dispatches the inner request to its handler,
// whose body is opened record by record as the handler reads it. An empty
// body is the connection proof. A caller with no row may only be pairing,
// sealed to the key this fog's invite carries.
//
// A refusal to a request whose signature hasn't verified goes unsigned and
// unsealed (fedWriter.finish): bound only to headers anyone can copy, a
// signed one could be relayed as this fog's answer to another request.
func (s *Server) serveSealed(fw *fedWriter, r *http.Request, next http.Handler) {
	id, err := s.fed.Identity()
	if err != nil {
		writeError(fw, err)
		return
	}
	fw.recipient, _ = fed.ParseEncKey(r.Header.Get(fed.HeaderEncKey))
	instanceID := r.Header.Get(fed.HeaderInstance)
	if instanceID == "" {
		fedReject(fw, fed.CodeMissingInstance, "missing Fog-Instance")
		return
	}
	// The outer target names its recipient (?to=), and the signature covers
	// the target: headers signed for another household — another peer of
	// the sender's, a squatter at an old address — are refused here before
	// anything else, and can't hold anything of this fog's.
	self := fed.PairRecipient(id.EncPublicB64())
	if to := r.URL.Query().Get("to"); to != id.InstanceID && to != self {
		fedReject(fw, fed.CodeWrongRecipient, "this request is addressed to another recipient")
		return
	}
	peer, peerErr := s.store.FedPeerByInstanceID(instanceID)
	known := peerErr == nil && peer.PublicKey != ""
	var pub ed25519.PublicKey
	if known {
		if pub, err = fed.ParsePublicKey(peer.PublicKey); err != nil || fed.CheckPeerRow(peer) != nil {
			// A row whose instance ID is another key's (paired before IDs
			// were checked) stands for nobody.
			pub = nil
		}
	}
	addr, _ := s.limitAddr(r)
	capy := s.fedCap()
	target, ts := r.URL.RequestURI(), r.Header.Get(fed.HeaderTimestamp)
	capy.mu.Lock()
	grace := capy.grace
	capy.mu.Unlock()

	// The fast lane: the headers verify — the peer's own, for this fog —
	// before the body. They are reserved in the replay record at once, so
	// a copy of them is refused while this one is still arriving; the
	// reservation is given up only if the body fails, and kept past the
	// latest moment any copy could still complete. Nothing is answered
	// under this fog's signature until the body verifies, and its slot is
	// one a later request of the peer's can take if this body stalls.
	declared := declaredDigest(r.Header)
	limit := fed.SealedLimit(fedRouteMax + 4 + 64<<10)
	var release func()
	var hold *peerHold
	headersAt := time.Now()
	fast := pub != nil && declared != "" && fed.VerifyDigest(pub, r.Method, target, r.Header, declared, headersAt) == nil
	reserved := ""
	defer func() {
		if reserved != "" {
			s.store.ReleaseFedRequest(reserved)
		}
	}()
	if fast {
		digest, ok := s.reserveFedRequest(fw, r.Header)
		if !ok {
			return
		}
		reserved = digest
		rc := http.NewResponseController(fw)
		h, rel, ok := capy.takePeer(peer.ID, false, func() { rc.SetReadDeadline(time.Now()) })
		if !ok {
			writeErr(fw, http.StatusServiceUnavailable, "too many requests from this peer at once; try again")
			return
		}
		hold, release = h, rel
	} else {
		rel, ok := capy.takeUnauth(addr)
		if !ok {
			writeErr(fw, http.StatusServiceUnavailable, "too many unauthenticated fed requests; try again")
			return
		}
		release = rel
		limit = fed.SealedLimit(maxPairBody + 4 + 64<<10)
		if known {
			limit = fed.SealedLimit(fedUnauthMaxBody + 4 + 64<<10)
		}
	}
	defer func() { release() }()
	if r.ContentLength > limit {
		writeErr(fw, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	// Either way the body has an absolute deadline: a fixed allowance plus
	// what it declares, at a minimum rate. A trickle holds nothing longer.
	expect := limit
	if r.ContentLength >= 0 {
		expect = r.ContentLength
	}
	deadline := time.Now().Add(grace + time.Duration(expect/fedUnauthMinRate)*time.Second)

	// The envelope is held to what its inner route takes, read from its
	// first record while the rest is still arriving — and to a refusal's
	// worth for a route this peer isn't granted. Only the size depends on
	// it: nothing is acted on before the whole body verifies.
	infoSelf := fed.RequestInfo("POST", target, instanceID, id.InstanceID, ts)
	infoPair := fed.RequestInfo("POST", target, instanceID, self, ts)
	// Each limit leaves room for the inner request's own head.
	const head = 4 + 64<<10
	sizeFor := func(prefix []byte) int64 {
		method, p, which, ok := id.PeekRoute(prefix, infoSelf, infoPair)
		switch {
		case !ok:
			return fed.SealedLimit(1<<20 + head)
		case which == 1:
			return fed.SealedLimit(maxPairBody + head)
		}
		if g := fedRouteGrant(p); g != "" && known {
			if _, granted, _ := s.store.FedCapabilityConfig(peer.ID, g, store.DirGrant); !granted {
				return fed.SealedLimit(64<<10 + head)
			}
		}
		return fed.SealedLimit(fedRouteCap(method, p) + head)
	}
	var progress *atomic.Int64
	if hold != nil {
		progress = &hold.read
	}
	body, err := readFedBody(fw, r, limit, deadline, sizeFor, s.fedSpoolFor(), progress)
	if errors.Is(err, errFedSpoolFull) {
		writeErr(fw, http.StatusServiceUnavailable, "too much is being received at once; try again")
		return
	}
	var mbe *http.MaxBytesError
	if errors.Is(err, errFedTooLarge) || errors.As(err, &mbe) {
		writeErr(fw, http.StatusRequestEntityTooLarge, "body too large for its route")
		return
	}
	if err != nil {
		badRequest(fw, "body too slow, or unreadable")
		return
	}
	defer body.close()
	if fast {
		if body.digest != declared {
			// Not the body the headers were signed for.
			fedReject(fw, fed.CodeBadSignature, "the body is not the one its signature covers")
			return
		}
		// Complete: the request is still within its window — measured from
		// its timestamp, plus the time its own body was allowed — and its
		// reservation is still the one in the record.
		if time.Since(headersAt) > deadline.Sub(headersAt) || !s.store.FedRequestHeld(reserved) || !fedStampInWindow(r.Header, deadline.Sub(headersAt)) {
			fedReject(fw, fed.CodeBadSignature, "the request completed outside its window")
			return
		}
		// The body is the one signed: the request is the peer's, and its
		// reservation is now its claim.
		reserved = ""
		capy.verify(hold)
	}

	// A pairing is sealed to the key this fog's invites carry, under the
	// name that key gives the inviter, and is the one request a caller
	// without a row may make. It is told apart by how it opens, not by who
	// sends it: a household re-pairing while still paired here, and one
	// claiming another's instance ID, both reach the pair handler, which
	// verifies the request against the key inside it and refuses a re-key.
	if body.size <= fed.SealedLimit(maxPairBody) {
		wire, err := body.bytes()
		if err != nil {
			writeError(fw, err)
			return
		}
		if sr, _, err := fed.OpenSealedRequest(id, r, wire, self); err == nil && !sr.Hello && sr.Inner.URL.Path == "/fed/v1/pair" {
			fw.answerAs(self)
			fw.bindTo(sr)
			fw.verified = false // the pair handler says when it has verified
			ctx := context.WithValue(sr.Inner.Context(), fedCallKey{}, &fedCall{pair: true, claimed: fast, outer: r.Header.Clone(), target: sr.Target, wire: wire, clientIP: addr})
			next.ServeHTTP(fw, sr.Inner.WithContext(ctx))
			return
		}
	}
	if !known {
		// The one rejection that is a fact about the relationship rather
		// than about this request. Saying so in a code is what lets the
		// caller distinguish being unpaired from being briefly broken —
		// it is the only notice an unpaired peer ever gets. It is signed:
		// bound to an instance this fog doesn't hold, it can't stand in for
		// an answer to a request this fog would take.
		fw.signUnknown = true
		fedReject(fw, fed.CodeUnknownPeer, "unknown peer")
		return
	}
	if pub == nil {
		fedReject(fw, fed.CodeKeyUnusable, "peer key unusable")
		return
	}
	if !fast {
		if err := fed.VerifyDigest(pub, r.Method, target, r.Header, body.digest, time.Now()); err != nil {
			fedReject(fw, fed.CodeBadSignature, err.Error())
			return
		}
		// Authenticated: out of the strangers' pool, into the peer's own.
		release()
		_, rel, ok := capy.takePeer(peer.ID, true, func() {})
		if !ok {
			release = func() {}
			writeErr(fw, http.StatusServiceUnavailable, "too many requests from this peer at once; try again")
			return
		}
		release = rel
	}
	// Verified, and only now answered under this fog's signature. (The
	// fast lane's reservation is already the claim; the slow lane claims
	// now, at completion, and re-checks the window.)
	fw.verified = true
	if !fast {
		if !fedStampInWindow(r.Header, deadline.Sub(headersAt)) {
			fedReject(fw, fed.CodeBadSignature, "the request completed outside its window")
			return
		}
		if !s.claimFedRequest(fw, r.Header) {
			return
		}
	}
	// The caller's encryption key: the one pinned for it, or — for a pairing
	// from before sealed bodies — the one this request presents, bound to
	// the identity key that just verified, then pinned. A request without
	// one is a fog that doesn't seal, refused; a different one is refused
	// and raised.
	encKey := peer.EncPublicKey
	presented := r.Header.Get(fed.HeaderEncKey)
	if presented == "" {
		fedReject(fw, fed.CodeEncryptionRequired, "fed bodies are sealed here; this request presents no encryption key")
		return
	}
	// The binding first: the header is just a header until the peer's
	// identity key is shown to have signed that key. An unbound key — one
	// swapped in on the way — is refused as unusable, and is no evidence the
	// peer changed anything: no pin, no alarm.
	if err := fed.VerifyEncBinding(pub, peer.InstanceID, presented, r.Header.Get(fed.HeaderEncBinding)); err != nil {
		if encKey != "" {
			fw.recipient, _ = fed.ParseEncKey(encKey)
		}
		fedReject(fw, fed.CodeEncryptionRequired, err.Error())
		return
	}
	switch {
	case encKey == "":
		if err := s.fed.PinEncKey(peer, presented); err != nil {
			fedReject(fw, fed.CodeEncKeyChanged, err.Error())
			return
		}
		encKey = presented
	case presented != encKey:
		// A key the peer's identity really bound, and not the pinned one:
		// that is the peer changing keys, which fog never accepts.
		s.fed.PinEncKey(peer, presented) // raises the notice
		fw.recipient, _ = fed.ParseEncKey(encKey)
		fedReject(fw, fed.CodeEncKeyChanged, "this request presents an encryption key other than the one pinned for its instance")
		return
	}
	if fw.recipient, err = fed.ParseEncKey(encKey); err != nil {
		fedReject(fw, fed.CodeKeyUnusable, "peer encryption key unusable")
		return
	}
	src, err := body.reader()
	if err != nil {
		writeError(fw, err)
		return
	}
	sr, err := fed.OpenSealedRequestStream(id, r, src, body.size, id.InstanceID)
	if err != nil {
		fedReject(fw, fed.CodeUnsealed, "the request does not open for this exchange")
		return
	}
	fw.bindTo(sr)
	if sr.Hello {
		// The connection proof: a signed answer, sealed to the caller, whose
		// head carries this fog's bound encryption key.
		fw.Write([]byte("{}"))
		return
	}
	if !fedInnerPath(sr.Inner.URL.Path) {
		writeErr(fw, http.StatusNotFound, "no such fed route")
		return
	}
	ctx := context.WithValue(sr.Inner.Context(), fedCallKey{}, &fedCall{peer: peer, clientIP: addr})
	next.ServeHTTP(fw, sr.Inner.WithContext(ctx))
}

// claimFedRequest writes a verified request's signature into the replay
// record, refusing it (and answering) when it is already there: the same
// signed bytes, captured and sent again inside the window.
func (s *Server) claimFedRequest(w http.ResponseWriter, h http.Header) bool {
	_, ok := s.reserveFedRequest(w, h)
	return ok
}

// fedClaimLife is how long past its timestamp a request's claim is kept: its
// window, plus the longest a body may be allowed to arrive (fedUnauthGrace
// plus the largest envelope at fedUnauthMinRate), plus a margin — so no
// copy of it can complete after its record is swept.
var fedClaimLife = fed.MaxSkew + fedUnauthGrace + time.Duration(fed.SealedLimit(fedRouteMax+4+64<<10)/fedUnauthMinRate)*time.Second + time.Minute

// fedStampInWindow re-checks a request's timestamp at completion: within
// the skew window as its headers were, widened by the time its body was
// allowed to take.
func fedStampInWindow(h http.Header, allowed time.Duration) bool {
	at, err := time.Parse(time.RFC3339, h.Get(fed.HeaderTimestamp))
	if err != nil {
		return false
	}
	d := time.Since(at)
	return d <= fed.MaxSkew+allowed && d >= -fed.MaxSkew
}

// reserveFedRequest enters a request in the replay record — kept for
// fedClaimLife past its timestamp — and returns its key, refusing (and
// answering) when it is already there.
func (s *Server) reserveFedRequest(w http.ResponseWriter, h http.Header) (string, bool) {
	at, err := time.Parse(time.RFC3339, h.Get(fed.HeaderTimestamp))
	if err != nil {
		fedReject(w, fed.CodeBadSignature, "invalid federation timestamp")
		return "", false
	}
	digest, ok := fed.RequestDigest(fed.BoundTo(h))
	if !ok {
		fedReject(w, fed.CodeBadSignature, "invalid federation signature encoding")
		return "", false
	}
	fresh, err := s.store.ClaimFedRequest(digest, at.Add(fedClaimLife))
	if err != nil {
		writeError(w, err)
		return "", false
	}
	if !fresh {
		fedReject(w, fed.CodeReplayed, "this signed request was already received")
		return "", false
	}
	return digest, true
}

// fedReject writes one fed-plane refusal: the sentence for a human reading
// logs, the code for the fog that has to decide what the refusal means.
func fedReject(w http.ResponseWriter, code, msg string) {
	writeJSON(w, http.StatusForbidden, map[string]string{"error": msg, "code": code})
}

// handleFedPair serves the inviter's side of the handshake. The one /fed/v1
// route not authenticated by a stored peer key: the invite secret is the
// credential, and the joiner's signature (verified against the key inside
// its own request) proves possession, bootstrapping the signed channel.
func (s *Server) handleFedPair(w http.ResponseWriter, r *http.Request) {
	// serveSealed opened the envelope with the key this fog's invites
	// carry; a pair request that wasn't sealed to it never got here.
	fc := fedCallOf(r)
	if fc == nil || !fc.pair {
		fedReject(w, fed.CodeEncryptionRequired, "pair requests are sealed to the key the invite carries")
		return
	}
	plain, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPairBody))
	if err != nil {
		badRequest(w, "body too large or unreadable")
		return
	}
	// The rate limit is the invite's own for a request that names a live
	// invite, and the caller's address's for one that doesn't: guessing at
	// secrets uses up only the guesser's allowance, never the joiner's who
	// holds the real code. The address is the outer request's, as fogd's
	// proxy vouched for it — never anything the sealed request says.
	var probe struct {
		Secret string `json:"secret"`
	}
	json.Unmarshal(plain, &probe)
	digest := auth.DigestToken(probe.Secret)
	if probe.Secret != "" && s.store.FedInviteOpen(digest) {
		if !limit(w, s.limiter, "fed-pair-invite:"+digest, "rate limited") {
			return
		}
	} else if !s.stranger("fed-pair").Allow(auth.AddrKey(fc.clientIP)) {
		writeErr(w, http.StatusTooManyRequests, "rate limited") // signin_handlers_limits.go
		return
	}
	id, err := s.fed.Identity()
	if err != nil {
		writeError(w, err)
		return
	}
	var req fed.PairRequest
	if err := json.Unmarshal(plain, &req); err != nil || req.Secret == "" {
		badRequest(w, "invalid pair request")
		return
	}
	inst := req.Instance
	if inst.InstanceID == "" || inst.PublicKey == "" || inst.Name == "" {
		badRequest(w, "pair request missing instance identity")
		return
	}
	pub, err := fed.ParsePublicKey(inst.PublicKey)
	if err != nil {
		badRequest(w, "invalid instance public key")
		return
	}
	// The joiner's encryption key, bound to its identity: the answer is
	// sealed to it, and it is pinned with the peer.
	if err := inst.CheckEncBinding(); err != nil {
		fedReject(w, fed.CodeEncryptionRequired, err.Error())
		return
	}
	joinerKey, _ := fed.ParseEncKey(inst.EncryptionKey)
	fedWriterOf(w).sealTo(joinerKey)
	// The outer request is the joiner's, signed with the key inside its own
	// document, under the instance it names.
	if fc.outer.Get(fed.HeaderInstance) != inst.InstanceID {
		forbidden(w, "instance header does not match the pair request")
		return
	}
	if err := fed.Verify(pub, "POST", fc.target, fc.outer, fc.wire, time.Now()); err != nil {
		forbidden(w, err.Error())
		return
	}
	fedWriterOf(w).verified = true
	if !fc.claimed && !s.claimFedRequest(w, fc.outer) {
		return
	}
	// A new pairing needs both instance IDs key-bound; that is settled
	// before the invite is spent. (A household already held here re-pairs
	// under the rules it was paired under.)
	if prior, err := s.store.FedPeerByInstanceID(inst.InstanceID); err != nil || prior.PublicKey == "" {
		switch err := s.fed.CanPairNew(inst.InstanceID); {
		case errors.Is(err, fed.ErrLegacySelf):
			forbidden(w, "refused: the inviting fog's identity predates key-bound instance IDs; its operator must renew it before it can pair with a new household")
			return
		case errors.Is(err, fed.ErrLegacyPeer):
			forbidden(w, "refused: your fog's identity predates key-bound instance IDs; renew it on your Connect page to pair with a new household")
			return
		case err != nil:
			writeError(w, err)
			return
		}
	}
	invite, err := s.store.ClaimFedInvite(auth.DigestToken(req.Secret), inst.InstanceID)
	if err != nil {
		// Wrong, spent, and expired secrets are indistinguishable on purpose.
		forbidden(w, "invite is invalid, expired, or already used")
		return
	}
	// Only after the claim: whether an instance is paired here is not
	// something to tell a caller without a valid invite. A refused claim
	// stays spent — whoever presented it named a household it isn't.
	prior, held := s.store.FedPeerByInstanceID(inst.InstanceID)
	// Decided from this side's own records: a row here is a peer here.
	// unpaired_at is no such record — it is the peer's own word, stamped
	// from its unknown_peer answers, and a peer could stamp it at will
	// before re-pairing with a leaked invite. Only the operator removing
	// the row makes a household new again.
	existed := held == nil && prior.PublicKey != ""
	peer, err := s.fed.StorePairedPeer(&inst, inst.URL)
	if errors.Is(err, fed.ErrKeyConflict) || errors.Is(err, fed.ErrPairWithSelf) {
		s.auditAs("federation", "federation.pair.refused", inst.InstanceID, map[string]any{"role": "inviter", "reason": err.Error()})
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	// Apply the grants pre-selected when the invite was minted — to a new
	// peer only. A household already paired here keeps exactly the grants
	// its row has: an invite minted for someone else (a leaked one, or one
	// meant for another household) never widens what an existing peer may
	// do without the operator changing it on the peer itself.
	var grants []inviteGrant
	json.Unmarshal([]byte(invite.Grants), &grants)
	grantNames := []string{}
	if existed {
		if len(grants) > 0 {
			s.store.CreateNotice("fed-pair-invite-unused", peer.Name,
				peer.DisplayName()+" re-paired with an invite meant to grant more",
				peer.DisplayName()+" is already paired here and used an invite to re-pair. The grants that invite carried were "+
					"not applied: an existing peer keeps the grants it has. Change them on the peer if you meant to.")
		}
		grants = nil
	}
	for _, g := range grants {
		cfg := "{}"
		if len(g.Config) > 0 {
			cfg = string(g.Config)
		}
		if err := s.store.SetFedCapability(peer.ID, g.Capability, store.DirGrant, cfg); err != nil {
			writeError(w, err)
			return
		}
		grantNames = append(grantNames, g.Capability)
	}
	if !existed {
		if err := s.grantDirectoryByDefault(peer.ID); err != nil {
			writeError(w, err)
			return
		}
	}
	// What the handshake itself proved: they are live, and their version.
	if err := s.store.MarkFedPeerPaired(peer.ID, inst.FogVersion); err != nil {
		writeError(w, err)
		return
	}
	s.auditAs("federation", "federation.pair", peer.Name, map[string]any{"role": "inviter", "instance": inst.InstanceID, "refreshed": existed})

	doc, err := s.fed.InstanceDoc()
	if err != nil {
		writeError(w, err)
		return
	}
	docJSON, _ := json.Marshal(doc)
	// The inviter's side: a peer just joined us, and no operator here
	// clicked anything to make it happen. This is the notice that matters
	// most — without it the new household is invisible to local apps until
	// someone opens a settings page.
	s.notifyAppsDirectoryChanged(DirChangePaired)
	writeJSON(w, http.StatusOK, fed.PairResponse{
		Instance: docJSON,
		Grants:   grantNames,
		Sig:      fed.PairSign(id, req.Secret, docJSON),
	})
}

// handleFedFlareEnvelope receives a peer's error-envelope batch: same
// pipeline as local app ingest from the parser down, but the caller is
// authenticated by its request signature and must hold a flare.ingest grant
// here.
func (s *Server) handleFedFlareEnvelope(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedAuthed(w, r, 8<<20)
	if !ok {
		return
	}
	if _, granted, _ := s.store.FedCapabilityConfig(peer.ID, store.CapFlareIngest, store.DirGrant); !granted {
		forbidden(w, "this peer is not granted flare.ingest here")
		return
	}
	if !limit(w, s.flareLimiter, "peer:"+peer.Name, "rate limited") {
		return
	}
	env, err := flare.ParseEnvelope(bytes.NewReader(body))
	if err != nil {
		badRequest(w, "invalid envelope: "+err.Error())
		return
	}
	accepted := 0
	for _, item := range env.Items {
		if item.Type != "event" {
			continue
		}
		if _, err := s.flareProc.IngestPeerEvent(peer, item.Payload); err != nil {
			s.log.Warn("peer ingest", "peer", peer.Name, "err", err)
			continue
		}
		accepted++
	}
	s.store.TouchFedPeerSeen(peer.ID)
	// Signed: the sender deletes its outbox rows on this answer, so it must
	// be able to prove the answer came from us and not from whatever now
	// owns an address we used to have.
	s.writeFedSignedJSON(w, r, map[string]any{"accepted": accepted})
}

// handleFedHeartbeat answers a peer's dial: record what they sent, respond
// with our side of the same exchange.
func (s *Server) handleFedHeartbeat(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedAuthed(w, r, 1<<20)
	if !ok {
		return
	}
	var theirs fed.HeartbeatPayload
	if !fedDecode(w, body, &theirs) {
		return
	}
	// The request signature verified (fedAuthed), so the payload carries the
	// peer's own authority — unless a later one already landed, in which
	// case this is a replay or a crossing and changes nothing. It is still
	// answered: the caller is the peer, and our side of the exchange is
	// computed fresh.
	switch err := s.fed.RecordPeerPayload(peer.ID, &theirs, r.Header.Get(fed.HeaderTimestamp)); {
	case errors.Is(err, fed.ErrStalePayload):
	case err != nil:
		writeError(w, err)
		return
	default:
		// Heartbeats are frequent and mostly say nothing new; the
		// fingerprint gate decides whether this one moved anything an app
		// can see.
		s.notifyAppsDirectoryChanged(DirChangeAdvertised)
	}
	ours, err := s.fed.PayloadFor(peer)
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, ours)
}

// writeFedSignedJSON writes a fed-plane answer. It is sealed to the
// requester and signed for its request on the way out, by the fed
// middleware (fedWriter) — as every answer on /fed/v1 is.
func (s *Server) writeFedSignedJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeFedSignedBytes(w, r, body)
}

// writeFedSignedBytes writes an already-marshaled answer — a response
// served from an idempotency cache (a replayed connect) too: it is sealed
// and signed afresh for the exchange it answers.
func (s *Server) writeFedSignedBytes(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// serveFedFile streams a file as a fed answer: its head signed with the
// file's digest, its bytes sealed record by record to the requester, and
// checked against that digest by the requester as they land.
func (s *Server) serveFedFile(w http.ResponseWriter, r *http.Request, f io.ReadSeeker, size int64, sha string) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeError(w, err)
		return
	}
	if err := fedWriterOf(w).stream(s, http.StatusOK, sha, f); err != nil {
		s.log.Debug("fed file answer", "path", r.URL.Path, "err", err)
	}
}

// writeFedNotModified tags the response with etag and, when the peer
// already holds that version, answers 304 and reports true: the caller is
// done. The ETag rides in the answer's signed head, so the 304 confirms that
// version, to that request.
func (s *Server) writeFedNotModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") != etag {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}

// maxFedAnswer caps a buffered fed answer (a manifest can run to megabytes)
// and an incoming envelope. Files stream (serveFedFile) and are not held.
const maxFedAnswer = 64 << 20

// fedPlane is the fed plane's door on the wire. Every /fed/v1 exchange is
// one shape: a POST to fed.SealedPath carrying the real request sealed, and
// a 200 carrying the real answer sealed — its status and headers inside,
// signed. serveSealed authenticates and opens the request and dispatches it
// to its handler; fedWriter seals and signs whatever the handler answers.
// Handlers write plain answers as they always have; this is the one place
// requests are opened and answers sealed, so nothing crosses unsealed by an
// oversight. Any other /fed/v1 path is refused: nothing is served outside
// the envelope.
func (s *Server) fedPlane(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/fed/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != fed.SealedPath || r.Method != http.MethodPost {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "fed requests travel sealed, to " + fed.SealedPath,
				"code": fed.CodeEncryptionRequired})
			return
		}
		fw := &fedWriter{ResponseWriter: w, outer: r}
		if id, err := s.fed.Identity(); err == nil {
			fw.self = id.InstanceID
		}
		defer fw.finish(s)
		s.serveSealed(fw, r, next)
	})
}

// signFedRefusals is the earlier name of fedPlane, kept for the router.
func (s *Server) signFedRefusals(next http.Handler) http.Handler { return s.fedPlane(next) }

// fedWriter holds an answer back until the handler is done, then seals and
// signs it in the envelope; a streamed file it seals and signs as it goes.
type fedWriter struct {
	http.ResponseWriter
	outer     *http.Request
	sr        *fed.SealedRequest
	status    int
	header    http.Header // the inner answer's headers
	body      bytes.Buffer
	overflow  bool
	finished  bool
	recipient hpke.PublicKey // the requester's encryption key, once known
	self      string         // who answers, as the sealing names it
	// verified: the request's signature verified, so its answer may be
	// signed and sealed, bound to it. Until then a refusal goes out plain
	// and unsigned — the requester treats it as nobody's word.
	verified bool
	// signUnknown: unknown_peer is signed though nothing verified (see
	// serveSealed).
	signUnknown bool
}

// fedWriterOf finds the fed middleware's writer under w; a handler called
// outside it (a test calling one directly) gets a detached one.
func fedWriterOf(w http.ResponseWriter) *fedWriter {
	for {
		switch v := w.(type) {
		case *fedWriter:
			return v
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return &fedWriter{}
		}
	}
}

func (fw *fedWriter) sealTo(pk hpke.PublicKey)     { fw.recipient = pk }
func (fw *fedWriter) answerAs(self string)         { fw.self = self }
func (fw *fedWriter) bindTo(sr *fed.SealedRequest) { fw.sr = sr }

// Header is the inner answer's: none of it reaches the wire outside the seal.
func (fw *fedWriter) Header() http.Header {
	if fw.header == nil {
		fw.header = http.Header{}
	}
	return fw.header
}

func (fw *fedWriter) WriteHeader(code int) {
	if fw.status == 0 {
		fw.status = code
	}
}

func (fw *fedWriter) Write(p []byte) (int, error) {
	if fw.finished {
		return 0, errors.New("fed answer already sent")
	}
	if fw.status == 0 {
		fw.status = http.StatusOK
	}
	if fw.body.Len()+len(p) > maxFedAnswer {
		fw.overflow = true
		return len(p), nil
	}
	return fw.body.Write(p)
}

// Flush is a no-op: nothing leaves before the answer is sealed.
func (fw *fedWriter) Flush() {}

func (fw *fedWriter) Unwrap() http.ResponseWriter { return fw.ResponseWriter }

// binding is what the answer is bound and sealed to: the outer request.
func (fw *fedWriter) binding() (target string, b fed.Bound, info []byte) {
	if fw.sr != nil {
		return fw.sr.Target, fw.sr.Bound, fw.sr.AnswerInfo(fw.self)
	}
	b = fed.BoundTo(fw.outer.Header)
	target = fw.outer.URL.RequestURI()
	return target, b, fed.AnswerInfo("POST", target, b.Instance, b.Sig, fw.self)
}

// answerHeader is the inner answer's headers as they go into its head.
func (fw *fedWriter) answerHeader() http.Header {
	h := fw.Header().Clone()
	h.Del("Content-Length")
	return h
}

// stream answers with a file: its head signed with the file's digest now,
// then its bytes sealed record by record straight to the wire.
func (fw *fedWriter) stream(s *Server, status int, sha string, src io.Reader) error {
	if fw.recipient == nil || fw.outer == nil {
		return errors.New("no encryption key to seal a file to")
	}
	id, err := s.fed.Identity()
	if err != nil {
		return err
	}
	fw.finished = true
	target, b, info := fw.binding()
	return fed.WriteSealedAnswer(fw.ResponseWriter, id, fw.recipient, info, target, b, status, fw.answerHeader(), sha, src)
}

func (fw *fedWriter) finish(s *Server) {
	if fw.finished || fw.ResponseWriter == nil || fw.outer == nil {
		return
	}
	fw.finished = true
	status := fw.status
	if status == 0 {
		status = http.StatusOK
	}
	plain := fw.body.Bytes()
	if fw.overflow {
		status, plain = http.StatusInternalServerError, []byte(`{"error":"answer too large"}`)
	}
	id, err := s.fed.Identity()
	if !fw.verified && !fw.signUnknown {
		// A refusal to a request nothing has verified: plain, unsigned.
		if status < 400 {
			status, plain = http.StatusForbidden, []byte(`{"error":"not verified","code":"`+fed.CodeBadSignature+`"}`)
		}
		h := fw.ResponseWriter.Header()
		h.Set("Content-Type", "application/json")
		fw.ResponseWriter.WriteHeader(status)
		fw.ResponseWriter.Write(plain)
		return
	}
	if fw.recipient == nil || err != nil {
		// No key to seal to: not a fog asking (no key presented). It gets
		// the refusal's status and code in the clear, and nothing else — a
		// 2xx with no one to seal it to never leaves.
		if status < 400 {
			status, plain = http.StatusForbidden, []byte(`{"error":"fed requests are sealed","code":"`+fed.CodeEncryptionRequired+`"}`)
		}
		h := fw.ResponseWriter.Header()
		h.Set("Content-Type", "application/json")
		fw.ResponseWriter.WriteHeader(status)
		fw.ResponseWriter.Write(plain)
		return
	}
	sum := sha256.Sum256(plain)
	target, b, info := fw.binding()
	if err := fed.WriteSealedAnswer(fw.ResponseWriter, id, fw.recipient, info, target, b, status, fw.answerHeader(),
		hex.EncodeToString(sum[:]), bytes.NewReader(plain)); err != nil {
		s.log.Debug("fed answer", "err", err)
	}
}

// fedDecode reads a verified peer request's JSON body into v, answering
// 400 and reporting false when it isn't one.
func fedDecode(w http.ResponseWriter, body []byte, v any) bool {
	if err := json.Unmarshal(body, v); err != nil {
		badRequest(w, "invalid body")
		return false
	}
	return true
}
