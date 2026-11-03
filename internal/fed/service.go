// Service is the running federation plane: it owns the cached instance
// identity, dials peers for heartbeats, and performs the joining side of
// the pairing handshake. Wired in main like every other subsystem.
package fed

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
	"golang.org/x/sync/errgroup"
)

const (
	// HeartbeatInterval is the dial cadence toward each active peer; a peer
	// is shown online while its last exchange is within OnlineWindow.
	HeartbeatInterval = 2 * time.Minute
	OnlineWindow      = 3 * HeartbeatInterval

	// UnpairThreshold is how many consecutive "unknown peer" answers make an
	// unpairing rather than a wobble. Three at the heartbeat cadence is
	// roughly six minutes — long enough to ride out a restart or a restore,
	// short enough that the operator learns before wondering.
	UnpairThreshold = 3

	// SettingAdvertisedURL overrides the derived https://fog.<base_domain>
	// address peers dial (for tunnel-fronted or otherwise unusual setups).
	SettingAdvertisedURL = "fed_advertised_url"

	pairPath      = "/fed/v1/pair"
	heartbeatPath = "/fed/v1/heartbeat"
)

// Online reports whether p answered within OnlineWindow of now — the one
// rule the Connect page, the apps directory and chat all show.
func Online(p *store.FedPeer, now time.Time) bool {
	if p.LastSeenAt == "" {
		return false
	}
	seen, err := time.Parse(time.RFC3339, p.LastSeenAt)
	return err == nil && now.Sub(seen) <= OnlineWindow
}

// DirectoryApp is one exposed app in a heartbeat's directory.
type DirectoryApp struct {
	App       string   `json:"app"`
	Title     string   `json:"title,omitempty"`
	URL       string   `json:"url,omitempty"`
	Contracts []string `json:"contracts,omitempty"`
}

// HeartbeatPayload is what each side of a heartbeat exchange carries: its
// version, the capabilities it grants the other side, and the app directory
// it publishes to them. Display and discovery data — never an enforcement
// input.
type HeartbeatPayload struct {
	FogVersion string `json:"fogVersion"`
	// People is this household's chat directory as published to this peer
	// (docs/chat-contract.md): handles, identity keys, devices. Empty
	// unless the peer holds the chat grant.
	People []DirectoryPerson `json:"people,omitempty"`
	// URL is the sender's current advertised address. A non-empty value
	// refreshes the stored peer URL on the receiving side, so address
	// changes propagate without a re-pair; empty means "no address yet"
	// and never clears a known one.
	URL       string         `json:"url,omitempty"`
	Grants    []string       `json:"grants"`
	Directory []DirectoryApp `json:"directory"`
	// Feedback is the sender's current status of every report this peer
	// relayed to it and it has since closed or reopened, newest change
	// first. The origin mirrors each onto the row the person who wrote it
	// can see. State rather than events: it rides every exchange and is
	// applied idempotently, so a closure is not lost while the origin is
	// away. Absent from an older Fog.
	Feedback []store.FeedbackStatus `json:"feedback,omitempty"`
}

// DirectoryPerson is one person in a household's advertised directory
// (people.directory). Address is the person address that outlives a rename
// (docs/signin-contract.md); identity keys and devices ride along only
// when chat is granted too. The type has no field for email, sessions, app
// roles or presence, so none can be sent by accident.
type DirectoryPerson struct {
	Address     string            `json:"address,omitempty"`
	Handle      string            `json:"handle"`
	DisplayName string            `json:"displayName"`
	Role        string            `json:"role"`
	IdentityKey string            `json:"identityKey,omitempty"`
	Devices     []DirectoryDevice `json:"devices"`
}

// Names reports whether ref — the second half of an address — is this
// person: their handle, or the person ULID their address carries.
func (p DirectoryPerson) Names(ref string) bool {
	if ref == "" {
		return false
	}
	if p.Handle == ref {
		return true
	}
	i := strings.LastIndexByte(p.Address, '/')
	return i >= 0 && p.Address[i+1:] == ref
}

// DirectoryDevice is one of a person's live devices: its key and whether the
// person attested it. Key packages are never in the heartbeat; they are
// fetched on demand so each is consumed once.
type DirectoryDevice struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DeviceKey   string `json:"deviceKey"`
	Attestation string `json:"attestation,omitempty"`
	Verified    bool   `json:"verified"`
}

// InstanceDoc is the identity document exchanged during pairing.
type InstanceDoc struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
	URL        string `json:"url,omitempty"`
	PublicKey  string `json:"publicKey"`
	FogVersion string `json:"fogVersion,omitempty"`
	// EncryptionKey is the key bodies to this instance are sealed to;
	// EncryptionKeySig is PublicKey's signature binding it (VerifyEncBinding).
	EncryptionKey    string `json:"encryptionKey,omitempty"`
	EncryptionKeySig string `json:"encryptionKeySig,omitempty"`
}

// CheckEncBinding verifies that the document's identity key bound its
// encryption key: required of every document a pairing takes in. One
// without is a fog that doesn't seal bodies, refused as ErrNoEncryption.
func (d *InstanceDoc) CheckEncBinding() error {
	if err := CheckInstanceID(d.InstanceID, d.PublicKey); err != nil {
		return err
	}
	if d.EncryptionKey == "" {
		return ErrNoEncryption
	}
	pub, err := ParsePublicKey(d.PublicKey)
	if err != nil {
		return err
	}
	if err := VerifyEncBinding(pub, d.InstanceID, d.EncryptionKey, d.EncryptionKeySig); err != nil {
		return fmt.Errorf("%w: %v", ErrNoEncryption, err)
	}
	return nil
}

// DialFailThreshold: consecutive failed dials before a peer's
// unreachability is raised as a persistent notice — provided the streak has
// also lasted DialFailWindow. Every signed request counts, not only
// heartbeats, so a count alone would say "unreachable" after a few seconds
// of queued chat or a burst of typing indicators toward a peer that is
// merely restarting. The window is exactly how long five heartbeats take to
// fail on their own, so a peer only heartbeats reach is judged as before;
// busier traffic can't raise it sooner.
const (
	DialFailThreshold = 5
	DialFailWindow    = (DialFailThreshold - 1) * HeartbeatInterval
)

type Service struct {
	Store   *store.Store
	Vault   *vault.Vault
	Log     *slog.Logger
	Version string
	// HTTP is swappable for tests; nil gets a sane default.
	HTTP *http.Client
	// DirectoryFor computes the app directory published to one peer; nil
	// (or a nil return) means an empty directory.
	DirectoryFor func(peer *store.FedPeer) []DirectoryApp
	// PeopleFor computes the chat directory published to one peer; nil (or
	// a nil return) means none.
	PeopleFor func(peer *store.FedPeer) []DirectoryPerson
	// FeedbackFor is what the heartbeat tells a peer about the reports it
	// relayed here; OnFeedback applies what a peer's verified payload says
	// about the reports written here. Either may be nil.
	FeedbackFor func(peer *store.FedPeer) []store.FeedbackStatus
	OnFeedback  func(peerID int64, statuses []store.FeedbackStatus)
	// SelfURLFallback supplies a last-resort dialable address (typically
	// http://<current LAN IP>:<port>) used in heartbeats when no domain or
	// advertised URL is configured. Because it is derived live, a peer
	// paired by LAN address hears the *new* address after a move instead of
	// holding the old one forever.
	SelfURLFallback func() string

	mu sync.Mutex
	id *Identity

	// dialFail counts consecutive failed dials; see dialFails.
	dialFailOnce sync.Once
	dialFail     *notify.FailStreak

	dialMu sync.Mutex
	link   map[int64]*linkState // peer ID -> what our recent heartbeats measured

	payloadMu sync.Mutex
	payloadAt map[int64]time.Time // peer ID -> signing time of the last payload applied

	connsMu sync.Mutex
	conns   map[int64]*peerConns // peer ID -> its proven connections
}

// LinkWindow is how many of our most recent heartbeats toward a peer count
// toward its link quality: at the two-minute cadence, the last forty
// minutes.
const LinkWindow = 20

// linkState is what this side's own dials measured: the round trip of the
// last exchange that got an answer, and which of the recent dials were
// answered at all. Kept in memory only — a fogd restart forgets it, and
// the page says "not measured yet" rather than inventing a figure.
type linkState struct {
	rtt     time.Duration
	rttAt   time.Time
	history []bool // newest last, at most LinkWindow entries
}

// noteLink records one heartbeat's outcome toward a peer. answered means
// the peer replied over HTTP at all — a rejection is still an answer, and
// still a round trip.
func (s *Service) noteLink(peerID int64, rtt time.Duration, answered bool) {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if s.link == nil {
		s.link = map[int64]*linkState{}
	}
	st := s.link[peerID]
	if st == nil {
		st = &linkState{}
		s.link[peerID] = st
	}
	st.history = append(st.history, answered)
	if len(st.history) > LinkWindow {
		st.history = st.history[len(st.history)-LinkWindow:]
	}
	if answered {
		st.rtt = rtt
		st.rttAt = time.Now()
	}
}

// LinkStatus is the link as this side measured it: the last round trip
// (zero and !measured until a heartbeat has been answered since fogd
// started) and how many of the recent dials were answered, out of how many
// were made. Peers we never dial (no address) report nothing.
type LinkStatus struct {
	Measured bool
	RTT      time.Duration
	RTTAt    time.Time
	Answered int
	Attempts int
}

func (s *Service) LinkStatus(peerID int64) LinkStatus {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	st := s.link[peerID]
	if st == nil {
		return LinkStatus{}
	}
	ls := LinkStatus{Measured: !st.rttAt.IsZero(), RTT: st.rtt, RTTAt: st.rttAt, Attempts: len(st.history)}
	for _, ok := range st.history {
		if ok {
			ls.Answered++
		}
	}
	return ls
}

// dialFails is the per-peer dial-failure streak, keyed by peer name — the
// notice's key — and built on first use so a Service stays a plain struct
// literal.
func (s *Service) dialFails() *notify.FailStreak {
	s.dialFailOnce.Do(func() {
		s.dialFail = &notify.FailStreak{Store: s.Store, Kind: "fed-peer-unreachable",
			Threshold: DialFailThreshold, Window: DialFailWindow, Once: true}
	})
	return s.dialFail
}

// noteDialResult tracks whether a peer answers our dials at all — transport
// failures only; an answer that rejects us is still an answer. Every signed
// request lands here through do. A streak past both DialFailThreshold and
// DialFailWindow raises one persistent notice; the next successful dial
// clears it. Before this, every failed dial was a Debug line and a moved
// fleet just quietly went grey.
//
// One raise per streak (FailStreak's Once), not a re-raise at every
// multiple: failures arrive at whatever cadence the fed plane's callers
// dial, so a multiple of them marks no particular stretch of time, and a
// dismissed notice would be back within minutes of a busy chat.
func (s *Service) noteDialResult(peer *store.FedPeer, err error) {
	if peer.ID == 0 {
		return // an address being paired with has no row to report on
	}
	if err == nil {
		if s.dialFails().Success(peer.Name) {
			s.Log.Info("fed peer reachable again", "peer", peer.Name)
		}
		return
	}
	if streak, raise := s.dialFails().FailErr(peer.Name, err); raise {
		s.Log.Warn("fed peer unreachable", "peer", peer.Name, "url", peer.URL, "streak", streak, "err", err)
		s.Store.CreateNotice("fed-peer-unreachable", peer.Name,
			"Can't reach "+peer.Name,
			"Dialing "+peer.URL+" has failed "+fmt.Sprint(streak)+" times in a row: "+err.Error()+"\n\n"+
				"If that server (or this one) moved networks, its stored address is stale — edit the peer's address on the "+
				"Connect page, or re-pair. Heartbeats the peer sends this way still work; only this side's dials are failing.")
	}
}

// DialStatus reports a peer's current dial health: the last error and when
// the failing streak began, once it has raised its notice. Healthy (or
// merely wobbling) peers return ok=false.
func (s *Service) DialStatus(peerName string) (lastErr string, since time.Time, failing bool) {
	if st := s.dialFails().State(peerName); st.Raised {
		return st.LastErr, st.Since, true
	}
	return "", time.Time{}, false
}

// InstanceID is this fog's instance id, or "" when the identity cannot be
// loaded — for callers that only compare against it.
func (s *Service) InstanceID() string {
	if id, err := s.Identity(); err == nil {
		return id.InstanceID
	}
	return ""
}

// RequireInstanceID is InstanceID for callers that must fail, not carry
// on with "", when there is no identity.
func (s *Service) RequireInstanceID() (string, error) {
	id, err := s.Identity()
	if err != nil {
		return "", err
	}
	return id.InstanceID, nil
}

// Identity returns the cached instance identity, minting it on first use.
func (s *Service) Identity() (*Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != nil {
		return s.id, nil
	}
	id, err := EnsureIdentity(s.Store, s.Vault)
	if err != nil {
		return nil, err
	}
	s.id = id
	return id, nil
}

// AdvertisedURL is the address peers dial: the operator override when set,
// otherwise https://fog.<base_domain>, otherwise empty (dial-out only).
func (s *Service) AdvertisedURL() string {
	if u, _ := s.Store.GetSetting(SettingAdvertisedURL); u != "" {
		return u
	}
	if domain, _ := s.Store.GetSetting(store.SettingBaseDomain); domain != "" {
		return "https://fog." + domain
	}
	return ""
}

// advertisedOrFallback is what heartbeats carry: the configured address, or
// the live LAN fallback for domainless setups. The fallback is never stored
// — it is re-derived on every call, which is exactly what lets a peer
// paired by LAN IP learn the new address after this box moves networks.
func (s *Service) advertisedOrFallback() string {
	if u := s.AdvertisedURL(); u != "" {
		return u
	}
	if s.SelfURLFallback != nil {
		return s.SelfURLFallback()
	}
	return ""
}

// InstanceDoc describes this instance for pairing exchanges.
func (s *Service) InstanceDoc() (*InstanceDoc, error) {
	id, err := s.Identity()
	if err != nil {
		return nil, err
	}
	name, _ := s.Store.GetSetting(store.SettingServerName)
	if name == "" {
		name = "fog"
	}
	return &InstanceDoc{
		InstanceID: id.InstanceID,
		Name:       name,
		URL:        s.AdvertisedURL(),
		PublicKey:  id.PublicKeyB64(),
		FogVersion: s.Version,

		EncryptionKey:    id.EncPublicB64(),
		EncryptionKeySig: id.EncBinding,
	}, nil
}

// PayloadFor builds the heartbeat payload sent to one peer: our version,
// what we grant them, and the directory we publish to them.
func (s *Service) PayloadFor(peer *store.FedPeer) (*HeartbeatPayload, error) {
	caps, err := s.Store.ListFedCapabilities(peer.ID)
	if err != nil {
		return nil, err
	}
	grants := []string{}
	for _, c := range caps {
		if c.Direction == store.DirGrant {
			grants = append(grants, c.Capability)
		}
	}
	dir := []DirectoryApp{}
	if s.DirectoryFor != nil {
		if d := s.DirectoryFor(peer); d != nil {
			dir = d
		}
	}
	var people []DirectoryPerson
	if s.PeopleFor != nil {
		people = s.PeopleFor(peer)
	}
	var fb []store.FeedbackStatus
	if s.FeedbackFor != nil {
		fb = s.FeedbackFor(peer)
	}
	return &HeartbeatPayload{FogVersion: s.Version, URL: s.advertisedOrFallback(), Grants: grants, Directory: dir,
		People: people, Feedback: fb}, nil
}

// ErrStalePayload: a heartbeat payload older than one already applied from
// the same peer — a replay inside the signature window, or two exchanges
// crossing. Nothing is recorded from it.
var ErrStalePayload = errors.New("heartbeat payload is older than the one already applied")

// claimPayload admits a payload stamped signedAt (the peer's own
// Fog-Timestamp, from either a request it signed or an answer it signed)
// unless one stamped later has already been applied for that peer. A
// signature is good for five minutes either side, so without this a
// captured exchange could be replayed to roll the peer's address, directory
// or people back. Kept in memory: a restart re-opens at most one window.
func (s *Service) claimPayload(peerID int64, signedAt string) error {
	at, err := time.Parse(time.RFC3339, signedAt)
	if err != nil {
		return fmt.Errorf("heartbeat payload carries no usable timestamp: %w", err)
	}
	s.payloadMu.Lock()
	defer s.payloadMu.Unlock()
	if s.payloadAt == nil {
		s.payloadAt = map[int64]time.Time{}
	}
	if last, ok := s.payloadAt[peerID]; ok && at.Before(last) {
		return ErrStalePayload
	}
	s.payloadAt[peerID] = at
	return nil
}

// RecordPeerPayload persists what a heartbeat exchange said about the peer.
// The caller must have verified p against the peer's key — an inbound
// request fedAuthed accepted, or an answer verifyPeerResponse accepted — and
// passes the Fog-Timestamp that signature covered. There is no unverified
// path in: an answer that can't be proven the peer's (an unsigned one, or a
// stranger squatting on a stale address) changes nothing at all, not the
// address, directory, people, version, nor when the peer was last seen.
func (s *Service) RecordPeerPayload(peerID int64, p *HeartbeatPayload, signedAt string) error {
	if err := s.claimPayload(peerID, signedAt); err != nil {
		return err
	}
	if p.URL != "" {
		if err := s.Store.UpdateFedPeerURL(peerID, p.URL); err != nil {
			return err
		}
	}
	grants, _ := json.Marshal(p.Grants)
	dir, _ := json.Marshal(p.Directory)
	people := "[]"
	if p.People != nil {
		if b, err := json.Marshal(p.People); err == nil {
			people = string(b)
		}
	}
	if err := s.Store.UpdateFedPeerHeartbeat(peerID, p.FogVersion, string(grants), string(dir), people); err != nil {
		return err
	}
	// A report is closed on the peer's word, which is provably the peer's.
	if s.OnFeedback != nil && len(p.Feedback) > 0 {
		s.OnFeedback(peerID, p.Feedback)
	}
	return nil
}

// Loop dials every active, addressable peer on a fixed cadence. Peers
// without a URL are dial-in only: their freshness comes from the heartbeats
// they send us.
func (s *Service) Loop(ctx context.Context, interval time.Duration) {
	// Leading exchange: a restart must not leave peers stale for a full
	// interval.
	loop.Every(ctx, 0, interval, s.HeartbeatAll)
}

func (s *Service) HeartbeatAll(ctx context.Context) {
	peers, err := s.Store.ListFedPeers()
	if err != nil {
		return
	}
	// A few at a time, as the outbox drains: one dead peer costs its own
	// timeout, not a slot in a queue every other peer waits behind.
	var g errgroup.Group
	g.SetLimit(outboxFanout)
	for _, p := range peers {
		if p.URL == "" {
			continue
		}
		g.Go(func() error {
			if err := s.HeartbeatPeer(ctx, &p); err != nil {
				s.Log.Debug("fed heartbeat", "peer", p.Name, "err", err)
			}
			return nil
		})
	}
	g.Wait()
}

// HeartbeatPeer performs one signed exchange with a peer and records what
// came back. Both sides refresh from a single dial: we learn from the
// response, they learn from the request. The response must carry a valid
// signature from the peer's stored key; an unsigned or wrongly signed one
// fails the exchange and records nothing — an answer is only the peer's
// when it can prove it.
func (s *Service) HeartbeatPeer(ctx context.Context, peer *store.FedPeer) error {
	if peer.URL == "" {
		return errors.New("peer has no reachable URL")
	}
	payload, err := s.PayloadFor(peer)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(payload)
	started := time.Now()
	r, err := s.exchange(ctx, peer, "POST", heartbeatPath, body, reqOpts{contentType: "application/json"})
	s.noteLink(peer.ID, time.Since(started), answeredByPeer(err))
	if err != nil {
		// A peer telling us it has no record of this instance is the only
		// disconnect signal federation gives us, and it only arrives here,
		// on our own dial. Counting it is what turns a silently stale peer
		// into a stated one.
		if Unpaired(err) {
			if e := s.Store.RecordFedPeerRejection(peer.ID, UnpairThreshold); e != nil {
				s.Log.Warn("fed record rejection", "peer", peer.Name, "err", e)
			}
		}
		return err
	}
	var theirs HeartbeatPayload
	if err := json.Unmarshal(r.body, &theirs); err != nil {
		return fmt.Errorf("heartbeat response: %w", err)
	}
	// An answer is bound to this request, so it is never a replay; it can
	// only be older than a heartbeat the peer sent us meanwhile, whose
	// payload then stands. The exchange itself worked.
	if err := s.RecordPeerPayload(peer.ID, &theirs, r.header.Get(HeaderTimestamp)); err != nil && !errors.Is(err, ErrStalePayload) {
		return err
	}
	return nil
}

// answeredByPeer is what link quality counts as an answer: one the peer
// proved was its own — a verified answer, or a refusal it signed for the
// request. A rejection is still an answer and still a round trip; an
// unsigned page from whatever now holds the address is not the peer.
func answeredByPeer(err error) bool {
	if err == nil {
		return true
	}
	var re *RemoteError
	return errors.As(err, &re) && re.Verified
}

// ---- the signed-request core ----

// maxReplyBody caps a buffered answer — a POST's acknowledgement, a JSON
// GET's document (a share's manifest can run to megabytes). Past it the
// exchange fails rather than handing a caller a truncated body to parse or
// verify. maxErrorBody caps what a non-2xx answer contributes to its
// RemoteError; a proxy's error page needn't be held in full.
const (
	maxReplyBody = 64 << 20
	maxErrorBody = 1 << 20
)

// Headers carrying an instance's encryption key and its binding — the
// identity key's signature over it — on every fed request and answer, so a
// peer paired before bodies were sealed learns the key over the signed
// channel it already trusts. Public material; the binding is what makes it
// usable.
const (
	HeaderEncKey     = "Fog-Encryption-Key"
	HeaderEncBinding = "Fog-Encryption-Key-Sig"
)

// ErrNoEncryption: the other side doesn't seal bodies — a fog from before
// sealed federation, or not a fog. Nothing is sent to it and nothing taken
// from it: there is no unsealed fallback.
var ErrNoEncryption = errors.New("the peer does not seal federation bodies; it needs to update before anything crosses")

// ErrEncKeyChanged: a peer presented an encryption key other than the one
// pinned for it. Refused, never re-keyed, and the operator told.
var ErrEncKeyChanged = errors.New("the peer presented a different encryption key than the one pinned for it")

// NoticeEncKeyConflict is the notice a refused encryption re-key raises.
const NoticeEncKeyConflict = "fed-enc-key-conflict"

// reqOpts is everything that varies between signed requests; the sequence
// itself — identity, seal, sign, send, record the dial, verify, open — is
// do's alone.
type reqOpts struct {
	contentType string
	header      http.Header // extra headers, unsigned (an upload offset, a range)
	ifNoneMatch string      // conditional GET; a 304 is then an answer, not an error
	// stream: the body may be gigabytes either way, so no total timeout —
	// the streaming client plus an idle watchdog — and the response body is
	// the caller's to read and close.
	stream bool
	// sealTo and recipient stand in for the peer's encryption key and
	// instance ID before there is a row to read them from: the pair hop,
	// sealed to the key the invite carried.
	sealTo    hpke.PublicKey
	recipient string
}

// reply is a buffered, verified, opened answer.
type reply struct {
	status int
	header http.Header
	body   []byte
	target string
}

// sent is what this side signed for one request: the outer target and
// binding its answer must be signed and sealed for, and who it went to.
type sent struct {
	target    string
	bound     Bound
	recipient string
}

func (sn sent) answerInfo() []byte {
	return AnswerInfo("POST", sn.target, sn.bound.Instance, sn.bound.Sig, sn.recipient)
}

// do sends one request to peer in the envelope (envelope.go) and returns
// its answer, verified as the peer's and opened, with the answer's own
// status and headers. Every request on the fed plane goes through here.
//
// The inner request — method, target, the headers that say what is asked,
// and the body — is sealed to the peer's encryption key (learned first, over
// a signed hello, when it isn't pinned yet) and sent as a POST to
// SealedPath, signed. The answer must be the envelope's 200, open with this
// instance's key for this exchange, and carry a head signed by the peer's
// identity key for this request; its body must match the head's digest by
// its end. Anything else is an error, whatever it says.
//
// The peer's dial streak (noteDialResult) moves only on what proves
// something: a transport failure counts against the peer; an answer the
// peer signed — refusals included — counts for it; anything else against.
//
// An answer whose inner status is not 2xx (other than the 304 a conditional
// request asked for) is consumed and returned as a RemoteError. A peer with
// ID 0 is an address, not a row yet — the joining side of a pairing — and
// is not tracked.
func (s *Service) do(ctx context.Context, peer *store.FedPeer, method, path string, body []byte, o reqOpts) (*http.Response, sent, error) {
	if peer.URL == "" {
		return nil, sent{}, errors.New("peer has no reachable URL")
	}
	// A row whose instance ID is another key's stands for nobody
	// (CheckPeerRow): nothing is sent to it.
	if peer.PublicKey != "" {
		if err := CheckPeerRow(peer); err != nil {
			return nil, sent{}, err
		}
	}
	id, err := s.Identity()
	if err != nil {
		return nil, sent{}, err
	}
	sealTo, recipient := o.sealTo, o.recipient
	if recipient == "" {
		recipient = peer.InstanceID
	}
	if sealTo == nil {
		if sealTo, err = s.peerEncKey(ctx, peer); err != nil {
			// Learning the key is a dial like any other: its outcome counts.
			var re *RemoteError
			switch {
			case errors.As(err, &re) && re.Verified:
				s.noteDialResult(peer, nil)
			case !callerGaveUp(ctx):
				s.noteDialResult(peer, err)
			}
			return nil, sent{}, err
		}
	}
	parent := ctx
	stop := func() {}
	var dog *time.Timer
	// A large request body goes the streaming way too: no total timeout,
	// which a big upload to a busy peer can outlast, but the watchdog.
	bigUpload := len(body) > 1<<20
	if o.stream || bigUpload {
		// No total timeout: a body can be gigabytes. The caller's ctx bounds
		// the transfer, and the watchdog cancels one that stops making
		// progress.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		dog = time.AfterFunc(streamIdle, cancel)
		stop = func() { dog.Stop(); cancel() }
	}
	innerHdr := http.Header{}
	if o.contentType != "" {
		innerHdr.Set("Content-Type", o.contentType)
	}
	for k, vs := range o.header {
		for _, v := range vs {
			innerHdr.Add(k, v)
		}
	}
	if o.ifNoneMatch != "" {
		innerHdr.Set("If-None-Match", o.ifNoneMatch)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(peer.URL, "/")+SealedTarget(recipient), nil)
	if err != nil {
		stop()
		return nil, sent{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	target := req.URL.RequestURI()
	ts := uniqueStamp()
	wire, err := Seal(sealTo, RequestInfo("POST", target, id.InstanceID, recipient, ts), EncodeInnerRequest(method, path, innerHdr, body))
	if err != nil {
		stop()
		return nil, sent{}, err
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(wire)), nil }
	id.signRequestAt(req, wire, ts)
	sn := sent{target: target, bound: BoundTo(req.Header), recipient: recipient}
	client := s.clientFor(peer, o.stream || bigUpload)
	if dog != nil {
		req.Body = &progressBody{ReadCloser: req.Body, dog: dog}
	}
	resp, err := client.Do(req)
	if err != nil {
		stop()
		// The connection's proof can itself be the peer's signed refusal —
		// "unknown peer" arrives this way — and is then an answer like any
		// other.
		var re *RemoteError
		if errors.As(err, &re) {
			if re.Verified {
				s.noteDialResult(peer, nil)
			} else {
				s.noteDialResult(peer, err)
			}
			return nil, sn, re
		}
		// The caller giving up (shutdown, a cancelled page) says nothing
		// about the peer.
		if !callerGaveUp(parent) {
			s.noteDialResult(peer, err)
		}
		return nil, sn, err
	}
	pub, _ := ParsePublicKey(peer.PublicKey) // nil only for an address being paired with
	fail := func(err error) (*http.Response, sent, error) {
		stop()
		resp.Body.Close()
		s.noteDialResult(peer, fmt.Errorf("%w (%v)", errUnprovenAnswer, err))
		return nil, sn, err
	}
	if resp.StatusCode != http.StatusOK {
		// Not the envelope: a proxy's page, a redirect, a fog from before
		// it. Unproven, so it says nothing about the peer.
		re := &RemoteError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		fail(fmt.Errorf("%w: %s", errNotEnvelope, re.Error()))
		return nil, sn, re
	}
	var src io.ReadCloser = resp.Body
	if dog != nil {
		src = &progressBody{ReadCloser: resp.Body, dog: dog, stop: stop}
	}
	or, err := id.openStream(src, sn.answerInfo())
	if err != nil {
		return fail(err)
	}
	// Only the pair hop has no key to verify against: it is proven by the
	// pair signature in its body.
	if pub == nil && o.sealTo == nil {
		return fail(errors.New("no usable key to verify against"))
	}
	head, err := ReadAnswerHead(or, pub, sn.target, sn.bound)
	if err != nil {
		return fail(err)
	}
	if peer.ID != 0 {
		s.noteDialResult(peer, nil)
		s.learnEncKey(peer, head.EncKey, head.EncBinding)
	}
	body2 := newDigestReader(or, head.Digest, src)
	switch {
	case head.Status == http.StatusNotModified && o.ifNoneMatch != "":
		rest, err := io.ReadAll(io.LimitReader(body2, maxErrorBody))
		stop()
		src.Close()
		if err != nil || len(rest) != 0 || head.Header.Get("ETag") != o.ifNoneMatch {
			return nil, sn, fmt.Errorf("not-modified answer from %s doesn't confirm the version asked about", peer.URL)
		}
		return answered(resp, head, http.NoBody), sn, nil

	case head.Status < 200 || head.Status >= 300:
		raw, _ := io.ReadAll(io.LimitReader(body2, maxErrorBody))
		stop()
		src.Close()
		re := newRemoteError(head.Status, raw)
		re.Verified = pub != nil
		return nil, sn, re

	case o.stream:
		return answered(resp, head, body2), sn, nil

	default:
		plain, err := io.ReadAll(io.LimitReader(body2, maxReplyBody+1))
		stop()
		src.Close()
		if err != nil {
			s.noteDialResult(peer, fmt.Errorf("%w (%v)", errUnprovenAnswer, err))
			return nil, sn, err
		}
		if len(plain) > maxReplyBody {
			return nil, sn, fmt.Errorf("peer's answer to %s exceeds %d bytes", path, maxReplyBody)
		}
		return answered(resp, head, io.NopCloser(bytes.NewReader(plain))), sn, nil
	}
}

// callerGaveUp reports whether ctx ended — cancelled, or past its deadline
// (a proof's connection times out on the deadline itself, a moment before
// ctx says so): that says nothing about the peer.
func callerGaveUp(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	d, ok := ctx.Deadline()
	return ok && !time.Now().Before(d.Add(-50*time.Millisecond))
}

// answered dresses the outer response as the answer it carried: the
// inner status and headers, verified, and its body.
func answered(resp *http.Response, head *AnswerHead, body io.ReadCloser) *http.Response {
	resp.StatusCode = head.Status
	resp.Status = fmt.Sprintf("%d %s", head.Status, http.StatusText(head.Status))
	resp.Header = head.Header
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	resp.Header.Set(HeaderContentSHA256, head.Digest)
	// When the peer signed it: a heartbeat orders payloads by it.
	resp.Header.Set(HeaderTimestamp, head.Time)
	resp.ContentLength = -1
	resp.Body = body
	return resp
}

// DeclaredDigest is the sha256hex of an answer's body as its signed head
// declared it — and its reader checked, by the end of the body.
func DeclaredDigest(resp *http.Response) string { return resp.Header.Get(HeaderContentSHA256) }

// errUnprovenAnswer is a dial answered by something that couldn't prove it
// was the peer: an unsigned or wrongly signed answer, one that doesn't open,
// a proxy's error page, a stranger now holding the address. For
// reachability it is a failure — the peer itself was not reached.
var errUnprovenAnswer = errors.New("answered without the peer's signature")

// peerEncKey is the key a body for peer is sealed to: the pinned one, or —
// for a pairing from before sealed bodies — the one its signed hello
// presents now, which is then pinned.
//
// peer is the caller's and is only read: what is learned is pinned in the
// store, never written into a struct someone else may be holding.
func (s *Service) peerEncKey(ctx context.Context, peer *store.FedPeer) (hpke.PublicKey, error) {
	key := peer.EncPublicKey
	if key == "" && peer.ID != 0 {
		// A connection's proof carries the peer's bound key and pins it
		// (dial.go): proving one is learning it.
		if err := s.proveNow(ctx, peer); err != nil {
			return nil, err
		}
		fresh, err := s.Store.FedPeerByID(peer.ID)
		if err != nil {
			return nil, err
		}
		key = fresh.EncPublicKey
	}
	if key == "" {
		return nil, ErrNoEncryption
	}
	return ParseEncKey(key)
}

// learnEncKey pins peer's encryption key from an answer already verified
// as the peer's, when none is pinned yet. The key must carry the binding of
// the peer's identity key; a different key from the pinned one is refused
// and raised.
func (s *Service) learnEncKey(peer *store.FedPeer, key, binding string) error {
	if key == "" {
		if peer.EncPublicKey == "" {
			return ErrNoEncryption
		}
		return nil
	}
	pub, err := ParsePublicKey(peer.PublicKey)
	if err != nil {
		return err
	}
	if err := VerifyEncBinding(pub, peer.InstanceID, key, binding); err != nil {
		return fmt.Errorf("%w: %v", ErrNoEncryption, err)
	}
	return s.PinEncKey(peer, key)
}

// PinEncKey records key as peer's encryption key — its binding already
// checked — refusing (and telling the operator) a key other than one
// already pinned.
func (s *Service) PinEncKey(peer *store.FedPeer, key string) error {
	err := s.Store.SetFedPeerEncKey(peer.ID, key)
	if errors.Is(err, store.ErrPeerEncKeyMismatch) {
		if s.Log != nil {
			s.Log.Warn("fed: peer presented another encryption key", "peer", peer.Name)
		}
		s.Store.CreateNotice(NoticeEncKeyConflict, peer.Name,
			"Refused a changed encryption key from "+peer.DisplayName(),
			peer.DisplayName()+" presented an encryption key other than the one pinned for it. Fog never changes a "+
				"paired household's keys, so nothing was sent to it under the new one. If it really reinstalled, "+
				"remove it on the Connect page and pair again.")
		return ErrEncKeyChanged
	}
	return err
}

// exchange is do for an answer small enough to hold.
func (s *Service) exchange(ctx context.Context, peer *store.FedPeer, method, path string, body []byte, o reqOpts) (*reply, error) {
	o.stream = false
	resp, sn, err := s.do(ctx, peer, method, path, body, o)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	plain, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	r := &reply{status: resp.StatusCode, header: resp.Header, body: plain, target: sn.target}
	if r.status == http.StatusNotModified {
		return r, ErrNotModified
	}
	return r, nil
}

// SignedPost sends one sealed, signed request to a peer and returns the
// opened answer: verified as the peer's, for this request. 2xx only;
// anything else is an error with the status.
func (s *Service) SignedPost(ctx context.Context, peer *store.FedPeer, path, contentType string, body []byte) ([]byte, error) {
	r, err := s.exchange(ctx, peer, "POST", path, body, reqOpts{contentType: contentType})
	if err != nil {
		return nil, err
	}
	return r.body, nil
}

// SignedPostVerified is SignedPost. Every answer is verified now; the name
// stays for the callers that asked for it when that was a choice.
func (s *Service) SignedPostVerified(ctx context.Context, peer *store.FedPeer, path, contentType string, body []byte) ([]byte, error) {
	return s.SignedPost(ctx, peer, path, contentType, body)
}

// SignedDo sends one sealed, signed request of any method and returns the
// answer for the caller to stream and close: verified as the peer's, its
// body open. A file arrives signed by its digest (Fog-Content-SHA256), which
// the caller checks the bytes against. Non-2xx is returned as a RemoteError
// with the response consumed. Used for the files plane, where a body can be
// gigabytes and a GET carries the request.
func (s *Service) SignedDo(ctx context.Context, peer *store.FedPeer, method, path, contentType string, body []byte) (*http.Response, error) {
	return s.SignedDoHeaders(ctx, peer, method, path, contentType, body, nil)
}

// SignedDoHeaders is SignedDo with extra request headers (an upload
// offset, a range). Headers are not signed; the body and path are, and a
// Range is bound into the answer's sealing.
func (s *Service) SignedDoHeaders(ctx context.Context, peer *store.FedPeer, method, path, contentType string, body []byte, extra http.Header) (*http.Response, error) {
	resp, _, err := s.do(ctx, peer, method, path, body, reqOpts{contentType: contentType, header: extra, stream: true})
	return resp, err
}

// streamIdle is how long a streaming exchange may go without progress: no
// request bytes taken, no response yet, or no response bytes arriving. A
// transfer that keeps moving may take as long as it needs.
var streamIdle = 60 * time.Second

// progressBody resets the idle watchdog on every read that moves bytes, and
// on Close releases the request's context.
type progressBody struct {
	io.ReadCloser
	dog  *time.Timer
	stop func()
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.dog != nil {
		b.dog.Reset(streamIdle)
	}
	return n, err
}

func (b *progressBody) Close() error {
	if b.stop != nil {
		b.stop()
	}
	return b.ReadCloser.Close()
}

// SignedGetJSON is a GET whose answer, verified and opened, is decoded into
// out. ifNoneMatch, when set, is sent as If-None-Match; a 304 returns
// ErrNotModified.
func (s *Service) SignedGetJSON(ctx context.Context, peer *store.FedPeer, path, ifNoneMatch string, out any) (etag string, err error) {
	r, err := s.exchange(ctx, peer, "GET", path, nil, reqOpts{ifNoneMatch: ifNoneMatch})
	if errors.Is(err, ErrNotModified) {
		return ifNoneMatch, err
	}
	if err != nil {
		return "", err
	}
	if out != nil {
		if err := json.Unmarshal(r.body, out); err != nil {
			return "", err
		}
	}
	return r.header.Get("ETag"), nil
}

// ErrNotModified: the peer answered 304 to a conditional GET.
var ErrNotModified = errors.New("not modified")

// ---- pairing ----

// Invite is everything the one pasted string carries: where to dial, the
// single-use secret, and — so the joiner sees who they are pairing with before
// they commit — the inviter's chosen name. Name is advisory (a label the
// joiner can preview and override), never trusted: identity is still the key
// exchanged over the secret-authenticated pair hop.
type Invite struct {
	URL    string
	Secret string
	Name   string
	// EncKey is the inviter's encryption key: the pair request — the secret
	// and the joiner's identity — is sealed to it, so it crosses even a bare
	// LAN address unread. The code is the trust root for it, as for the URL.
	EncKey string
}

// pairRecipient names the inviter in the pair hop's sealing, before its
// instance ID is known: by the encryption key the invite carried.
func pairRecipient(encKey string) string { return "pair:" + encKey }

// PairRecipient is pairRecipient for the inviter's own handler.
func PairRecipient(encKey string) string { return pairRecipient(encKey) }

// InviteCode wraps an invite into the one string a human pastes:
// FOG-<base64url({"u":…,"s":…,"n":…,"k":…})>. "k" is the inviter's
// encryption key; a code without one is from a fog that doesn't seal
// bodies, and is refused when used.
func InviteCode(url, secret, name, encKey string) string {
	m := map[string]string{"u": url, "s": secret, "k": encKey}
	if name != "" {
		m["n"] = name
	}
	raw, _ := json.Marshal(m)
	return "FOG-" + base64.RawURLEncoding.EncodeToString(raw)
}

// ParseInvite decodes a pasted code without dialing anyone — a joiner can
// preview who and where before pairing.
func ParseInvite(code string) (Invite, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, "FOG-") {
		return Invite{}, errors.New("invite codes start with FOG-")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(code, "FOG-"))
	if err != nil {
		return Invite{}, errors.New("invite code is garbled")
	}
	var v struct{ U, S, N, K string }
	if err := json.Unmarshal(raw, &v); err != nil || v.U == "" || v.S == "" {
		return Invite{}, errors.New("invite code is garbled")
	}
	return Invite{URL: v.U, Secret: v.S, Name: v.N, EncKey: v.K}, nil
}

// ParseInviteCode is the URL+secret view of ParseInvite, kept for callers that
// only need to dial.
func ParseInviteCode(code string) (url, secret string, err error) {
	inv, err := ParseInvite(code)
	return inv.URL, inv.Secret, err
}

// PairSign binds a pairing response to both the invite secret and the
// responder's key: sig over "fogfed1-pair\n<secret>\n<sha256hex(instanceJSON)>".
// The joiner verifies with the public key inside the doc — proof the
// responder holds the key it claims and knew the secret.
func PairSign(id *Identity, secret string, instanceJSON []byte) string {
	return id.SignTranscript("fogfed1-pair", secret, instanceJSON)
}

func PairVerify(pub ed25519.PublicKey, secret string, instanceJSON []byte, sigB64 string) bool {
	return VerifyTranscript(pub, "fogfed1-pair", secret, instanceJSON, sigB64)
}

// SignTranscript signs a labelled statement: "<label>\n<nonce>\n<sha256hex(doc)>".
// The label names what is being said (fogfed1-pair, fogfed1-relay), so a
// signature made for one statement never stands for another.
func (i *Identity) SignTranscript(label, nonce string, doc []byte) string {
	sum := sha256.Sum256(doc)
	msg := []byte(label + "\n" + nonce + "\n" + hex.EncodeToString(sum[:]))
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(i.private, msg))
}

// VerifyTranscript checks a SignTranscript signature.
func VerifyTranscript(pub ed25519.PublicKey, label, nonce string, doc []byte, sigB64 string) bool {
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigB64)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(doc)
	msg := []byte(label + "\n" + nonce + "\n" + hex.EncodeToString(sum[:]))
	return ed25519.Verify(pub, msg, sig)
}

// PairRequest is the joiner's body to POST /fed/v1/pair.
type PairRequest struct {
	Secret   string      `json:"secret"`
	Instance InstanceDoc `json:"instance"`
}

// PairResponse carries the inviter's identity (raw, so the pair signature
// covers the exact bytes), the capability names it granted, and the binding
// signature.
type PairResponse struct {
	Instance json.RawMessage `json:"instance"`
	Grants   []string        `json:"grants"`
	Sig      string          `json:"sig"`
}

// Join runs the joining side of the handshake: dial the inviter named in
// the code, prove key possession, verify the response, store the peer.
// selfURL is a fallback address to advertise when none is configured —
// typically the address the operator reached this UI at — so the inviter
// isn't left with a dial-in-only peer.
func (s *Service) Join(ctx context.Context, code, selfURL string) (*store.FedPeer, error) {
	inv, err := ParseInvite(code)
	if err != nil {
		return nil, err
	}
	inviterURL, secret := inv.URL, inv.Secret
	// No unsealed pairing: a code without the inviter's encryption key is
	// from a fog that doesn't seal bodies.
	if inv.EncKey == "" {
		return nil, fmt.Errorf("%w (the invite carries no encryption key)", ErrNoEncryption)
	}
	sealTo, err := ParseEncKey(inv.EncKey)
	if err != nil {
		return nil, fmt.Errorf("invite code is garbled: %w", err)
	}
	// A fog whose own ID predates key-bound IDs may re-pair only with a
	// household it already holds — one whose pinned encryption key is the
	// invite's — and is told so before anything is dialed, rather than
	// after the inviter has committed.
	if s.LegacyIdentity() {
		held := false
		if peers, err := s.Store.ListFedPeers(); err == nil {
			for _, p := range peers {
				held = held || (p.EncPublicKey != "" && p.EncPublicKey == inv.EncKey)
			}
		}
		if !held {
			return nil, ErrLegacySelf
		}
	}
	doc, err := s.InstanceDoc()
	if err != nil {
		return nil, err
	}
	if doc.URL == "" {
		doc.URL = selfURL
	}
	body, _ := json.Marshal(PairRequest{Secret: secret, Instance: *doc})
	// The inviter is an address, not a peer row yet: the request is sealed
	// to the key its invite carried, and its answer — sealed to the key in
	// our document — is proven by the pair signature inside it.
	r, err := s.exchange(ctx, &store.FedPeer{URL: inviterURL}, "POST", pairPath, body,
		reqOpts{contentType: "application/json", sealTo: sealTo, recipient: pairRecipient(inv.EncKey)})
	if err != nil {
		return nil, err
	}
	var resp PairResponse
	if err := json.Unmarshal(r.body, &resp); err != nil {
		return nil, fmt.Errorf("pair response: %w", err)
	}
	var theirs InstanceDoc
	if err := json.Unmarshal(resp.Instance, &theirs); err != nil || theirs.InstanceID == "" || theirs.PublicKey == "" {
		return nil, errors.New("pair response missing the inviter's identity")
	}
	pub, err := ParsePublicKey(theirs.PublicKey)
	if err != nil {
		return nil, err
	}
	if !PairVerify(pub, secret, resp.Instance, resp.Sig) {
		return nil, errors.New("pair response signature failed — not the instance this invite came from")
	}
	// The key the invite carried is the key the inviter's identity binds.
	if theirs.EncryptionKey != inv.EncKey {
		return nil, errors.New("the inviter's encryption key is not the one its invite carried")
	}
	// The inviter's dialable address: what it advertises, or failing that
	// the URL the invite itself carried.
	theirURL := theirs.URL
	if theirURL == "" {
		theirURL = inviterURL
	}
	peer, err := s.StorePairedPeer(&theirs, theirURL)
	if err != nil {
		return nil, err
	}
	// The instance document is what the pair signature covers; the grant
	// list beside it is not, so it is not recorded. The heartbeat below
	// brings the peer's grants, signed.
	if err := s.Store.MarkFedPeerPaired(peer.ID, theirs.FogVersion); err != nil {
		return nil, err
	}
	// First real exchange right away: it fills in the directory on both
	// sides instead of leaving it empty until the ticker's next pass.
	if err := s.HeartbeatPeer(ctx, peer); err != nil {
		s.Log.Debug("fed heartbeat after pair", "peer", peer.Name, "err", err)
	}
	return s.Store.FedPeerByID(peer.ID)
}

// ErrKeyConflict: a pairing named an instance this fog already holds, under
// a key other than the one it was paired with. Refused on either side, and
// raised to the operator as a notice.
var ErrKeyConflict = errors.New("refused: this invite names a household already paired here, but under a different key — " +
	"it is not the household it claims to be. A household that really reinstalled has a new identity: " +
	"remove its old entry here and pair again")

// ErrPairWithSelf: the other side of a pairing claimed this fog's own
// instance ID.
var ErrPairWithSelf = errors.New("refused: the other side claims this fog's own identity")

// NoticePairKeyConflict is the notice a refused re-key raises.
const NoticePairKeyConflict = "fed-pair-key-conflict"

// StorePairedPeer records the instance a pairing handshake just proved — the
// joiner storing its inviter, or the inviter its joiner — and is the only
// way a peer row comes to exist or is refreshed by pairing.
//
// An instance already held is refreshed in place only when the handshake
// proved the same key the row was paired under: the row keeps its name,
// label, grants and history, and a non-empty url becomes its address. That
// is what lets either operator re-pair after the other unpaired, and keeps a
// handshake from being stranded half-applied when the inviter has already
// committed its side.
//
// The same instance ID under a different key is refused (ErrKeyConflict),
// and the operator told: the row's key never changes. An instance ID is not
// a secret — every peer of that household knows it — and the row carries
// every grant made to it, so accepting a new key would hand all of them to
// whoever held one invite code. Fog has no key rotation; a reinstalled
// household has a new instance ID and pairs as a new peer.
func (s *Service) StorePairedPeer(doc *InstanceDoc, url string) (*store.FedPeer, error) {
	if self, err := s.RequireInstanceID(); err != nil {
		return nil, err
	} else if doc.InstanceID == self {
		return nil, ErrPairWithSelf
	}
	if err := doc.CheckEncBinding(); err != nil {
		return nil, err
	}
	if existing, err := s.Store.FedPeerByInstanceID(doc.InstanceID); err == nil {
		// A row whose ID isn't its key's is nobody's to refresh.
		if CheckPeerRow(existing) != nil {
			return nil, ErrInstanceNotBound
		}
		// Both keys follow the same rule: the ones it was paired under, or
		// none at all.
		if existing.EncPublicKey != "" && existing.EncPublicKey != doc.EncryptionKey && existing.PublicKey == doc.PublicKey {
			s.keyConflict(existing, doc, url)
			return nil, ErrKeyConflict
		}
		err := s.Store.RefreshPairedPeer(existing.ID, doc.PublicKey, url)
		if errors.Is(err, store.ErrPeerKeyMismatch) {
			s.keyConflict(existing, doc, url)
			return nil, ErrKeyConflict
		}
		if err != nil {
			return nil, err
		}
		if err := s.Store.SetFedPeerEncKey(existing.ID, doc.EncryptionKey); err != nil {
			return nil, err
		}
		return s.Store.FedPeerByID(existing.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	// A new pairing: both sides' instance IDs must be their keys'.
	if err := s.CanPairNew(doc.InstanceID); err != nil {
		return nil, err
	}
	base := SlugifyName(doc.Name)
	for i := 0; i < 20; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		peer, err := s.Store.CreatePairedPeer(doc.InstanceID, name, url, doc.PublicKey)
		if err == nil {
			if err := s.Store.SetFedPeerEncKey(peer.ID, doc.EncryptionKey); err != nil {
				return nil, err
			}
			return s.Store.FedPeerByID(peer.ID)
		}
		if !errors.Is(err, store.ErrAlreadyExists) {
			return nil, err
		}
		// Only the name collided — a different instance already holds this
		// alias — so try the next one. An instance-ID collision cannot
		// reach here: that row was refreshed above.
	}
	return nil, errors.New("couldn't find a free peer name")
}

// keyConflict tells the operator a pairing tried to re-key a peer: which
// peer, from where, and both fingerprints, so the claim can be checked with
// the household it names.
func (s *Service) keyConflict(existing *store.FedPeer, doc *InstanceDoc, url string) {
	have, _ := Fingerprint(existing.PublicKey)
	got, _ := Fingerprint(doc.PublicKey)
	if s.Log != nil {
		s.Log.Warn("fed pairing refused: instance already paired under another key",
			"peer", existing.Name, "instance", doc.InstanceID, "url", url, "have", have, "got", got)
	}
	from := url
	if from == "" {
		from = "an address it did not give"
	}
	s.Store.CreateNotice(NoticePairKeyConflict, existing.Name,
		"Refused a pairing that claimed to be "+existing.DisplayName(),
		"A pairing named "+existing.DisplayName()+"'s instance ID, but under a different key ("+got+", from "+from+
			") than the one it was paired with ("+have+"). Fog never changes a paired household's key, so nothing changed: "+
			existing.DisplayName()+" keeps its key and its grants, and the newcomer got none.\n\n"+
			"If "+existing.DisplayName()+" really reinstalled, its fog has a new identity: remove the old entry on the "+
			"Connect page and pair again. Otherwise someone holding an invite code tried to pass as them.")
}

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

// SlugifyName folds a display name into the peer-name shape the UI and
// audit log use.
func SlugifyName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = s[:32]
	}
	if s == "" {
		s = "peer"
	}
	return s
}
