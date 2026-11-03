package compute

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// Timing.
const (
	// CodeTTL is how long a pairing code shown in Settings › Compute lasts.
	CodeTTL = 30 * time.Minute
	// Online is how long a node may go unheard before its slots leave its
	// pool. A node holds a poll open for at most HoldPoll, so a live one is
	// heard from well inside it.
	Online = 15 * time.Second
	// HoldPoll is the longest fogd keeps a node's poll waiting for work.
	HoldPoll = 8 * time.Second
	// touchEvery throttles last_seen_at writes: a node polls several times a
	// second while it has work, and the store holds one connection.
	touchEvery = 30 * time.Second
)

// TokenPrefix marks a node's credential.
const TokenPrefix = "fognode_"

// Errors the handlers map to statuses; the message is the whole answer.
var (
	ErrBadCode = errors.New("that pairing code is not valid any more — make a new one in Settings › Compute")
	ErrInvalid = errors.New("invalid")
)

// Pool families: a hardware family is a pool when its machines can run the
// same command lines (docs/compute-contract.md).
var poolFamilies = map[string]poolSpec{
	"videotoolbox": {Name: "VideoToolbox", Encoders: []string{"h264_videotoolbox"}, Filters: []string{"scale_vt"}},
	"vaapi":        {Name: "VAAPI", Encoders: []string{"h264_vaapi"}, Filters: []string{"scale_vaapi"}},
}

type poolSpec struct {
	Name     string
	Encoders []string
	Filters  []string
}

// VideoPool names the pool a video family belongs to.
func VideoPool(family string) string { return "video/" + family }

// PoolName is how a pool is shown, to the operator and to apps: "fog ·
// VideoToolbox".
func PoolName(pool string) string {
	if pool == InferencePool {
		return "fog · Inference"
	}
	if spec, ok := poolFamilies[strings.TrimPrefix(pool, "video/")]; ok {
		return "fog · " + spec.Name
	}
	return "fog · " + pool
}

// poolFor answers the video pool a machine qualifies for, or "" with why
// not: its family has no pool, or its ffmpeg lacks what the pool's command
// lines use.
func poolFor(v *VideoCaps) (string, string) {
	if v == nil {
		return "", "no ffmpeg found"
	}
	spec, ok := poolFamilies[v.Family]
	if !ok {
		return "", "no hardware encoder (" + v.Family + ")"
	}
	// A machine runs what apps hand it only inside its confinement, or with
	// its owner having said, loudly, to do without.
	if v.Confinement == "" && !v.Unconfined {
		if v.ConfinementError != "" {
			return "", "it can't confine ffmpeg: " + v.ConfinementError
		}
		return "", "its fog-node doesn't confine ffmpeg (it updates itself to one that does)"
	}
	for _, e := range spec.Encoders {
		if !slices.Contains(v.Encoders, e) {
			return "", "its ffmpeg has no " + e
		}
	}
	for _, f := range spec.Filters {
		if !slices.Contains(v.Filters, f) {
			return "", "its ffmpeg has no " + f + " filter"
		}
	}
	return VideoPool(v.Family), ""
}

// Registry is the paired machines: pairing, their credentials, what they
// reported, and whether they're heard from.
type Registry struct {
	Store *store.Store
	Log   *slog.Logger

	mu sync.Mutex
	// seen is when each node last polled; touched when the store last
	// recorded it.
	seen    map[string]time.Time
	touched map[string]time.Time
	// nonces are the request nonces taken lately, so none is taken twice.
	nonces map[string]time.Time
	// skew is how far off each machine's clock was, while it's refused for
	// it.
	skew map[string]time.Duration
	// epoch is when this fog started (processStart, unless a test sets
	// it): nothing sent before it is taken, since the nonces seen before
	// it are gone with the process that saw them.
	epoch time.Time
	// now is the clock; tests move it.
	now func() time.Time
}

func (r *Registry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// codeAlphabet leaves out what reads ambiguously aloud or on a screen.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewCode makes a one-time pairing code, shown once: ABCD-EFGH. fog keeps
// only the key it stretches to.
func (r *Registry) NewCode(by string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, c := range buf {
		if i == 4 {
			b.WriteByte('-')
		}
		b.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	code := b.String()
	salt := NewSalt()
	return code, r.Store.CreateComputeJoinCode(salt+":"+hex.EncodeToString(JoinKey(code, salt)), by, CodeTTL)
}

// Salts answers the public salts of the live codes, for a machine to pair
// against.
func (r *Registry) Salts() ([]string, error) {
	verifiers, err := r.Store.LiveComputeJoinCodes()
	if err != nil {
		return nil, err
	}
	salts := []string{}
	for _, v := range verifiers {
		if salt, _, ok := strings.Cut(v, ":"); ok {
			salts = append(salts, salt)
		}
	}
	return salts, nil
}

// Join pairs a machine that proved it holds a live code: spends the code,
// takes the digest of the credential the machine made (sealed under the
// code; fog never sees the credential, and nobody else the digest), and
// answers with fogKey (fog's instance key, for the machine to pin) and
// fog's own proof of the code.
func (r *Registry) Join(req JoinRequest, fogKey string) (*JoinAnswer, error) {
	name := strings.TrimSpace(req.Inventory.Name)
	if name == "" || len(name) > 80 {
		return nil, fmt.Errorf("%w: a machine needs a name of at most 80 characters", ErrInvalid)
	}
	if req.Nonce == "" || len(req.Nonce) > 128 || len(req.Attempts) == 0 || len(req.Attempts) > MaxJoinAttempts {
		return nil, ErrBadCode
	}
	verifiers, err := r.Store.LiveComputeJoinCodes()
	if err != nil {
		return nil, err
	}
	var key []byte
	var verifier string
	var attempt JoinAttempt
	for _, v := range verifiers {
		salt, keyHex, ok := strings.Cut(v, ":")
		raw, err := hex.DecodeString(keyHex)
		if !ok || err != nil {
			continue
		}
		for _, a := range req.Attempts {
			if a.Salt == salt && macEqual(a.Proof, JoinProof(raw, req.Nonce)) {
				key, verifier, attempt = raw, v, a
			}
		}
	}
	if key == nil {
		return nil, ErrBadCode
	}
	tokenDigest, err := OpenCredential(key, req.Nonce, attempt.Credential)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	inv, err := json.Marshal(req.Inventory)
	if err != nil {
		return nil, err
	}
	n := &store.ComputeNode{ID: randx.ULID(), Name: name, Inventory: string(inv)}
	if err := r.Store.PairComputeNode(verifier, n, tokenDigest); err != nil {
		if errors.Is(err, store.ErrCodeSpent) {
			return nil, ErrBadCode
		}
		return nil, err
	}
	r.heard(n.ID, true)
	if r.Log != nil {
		r.Log.Info("compute: machine paired", "node", n.ID, "name", name, "by", n.CreatedBy)
	}
	ans := &JoinAnswer{Node: n.ID, FogKey: fogKey}
	ans.Confirm = JoinConfirm(key, req.Nonce, attempt.Credential, *ans)
	return ans, nil
}

// processStart is when this fogd started.
var processStart = time.Now()

// Why a machine's request wasn't taken (AuthenticateRequest). Only
// ErrUnknownMachine means it isn't paired.
var (
	ErrUnknownMachine = errors.New("fog has no such machine")
	ErrBadRequestMAC  = errors.New("the request's MAC doesn't verify")
	ErrReplayed       = errors.New("the request's nonce was taken before")
	ErrClock          = errors.New("the machine's clock is too far from fog's")
)

// AuthenticateRequest resolves a machine's MACed poll (trust.go) from its
// headers alone, before its body is read: the node it names, its MAC under
// that node's credential over the request's time, nonce, and declared body
// type and digest, a time within RequestSkew and since fog started, and a
// nonce not seen before. The MAC is checked before the time, so only the
// machine itself can be told its clock is off (and the page says so). The
// caller checks the body against bodySHA once it reads it.
func (r *Registry) AuthenticateRequest(node, nonce, at, mac, contentType, bodySHA string) (*store.ComputeNode, error) {
	n, _, err := r.authenticate(node, nonce, at, mac, func(digest string) string {
		return RequestMAC(digest, node, nonce, at, contentType, bodySHA)
	})
	return n, err
}

// AuthenticateData resolves a machine's MACed request on a run's data
// route: as AuthenticateRequest, over the method, target and an upload's
// declared body digest.
func (r *Registry) AuthenticateData(node, nonce, at, mac, method, target, bodySHA string) (*store.ComputeNode, error) {
	n, _, err := r.authenticate(node, nonce, at, mac, func(digest string) string {
		return DataMAC(digest, node, nonce, at, method, target, bodySHA)
	})
	return n, err
}

// authenticate checks a request's MAC under the node's credential, or the
// one it held before rotating (until it's heard with the new), and its
// time and nonce. It answers the digest that verified.
func (r *Registry) authenticate(node, nonce, at, mac string, macFor func(digest string) string) (*store.ComputeNode, string, error) {
	if node == "" || len(node) > 64 {
		return nil, "", ErrUnknownMachine
	}
	n, err := r.Store.ComputeNode(node)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", ErrUnknownMachine
	}
	if err != nil {
		return nil, "", err
	}
	if len(nonce) < 16 || len(nonce) > 128 || mac == "" {
		return nil, "", ErrBadRequestMAC
	}
	used := ""
	switch {
	case macEqual(mac, macFor(n.TokenDigest)):
		used = n.TokenDigest
	case n.PrevTokenDigest != "" && macEqual(mac, macFor(n.PrevTokenDigest)):
		used = n.PrevTokenDigest
	default:
		return nil, "", ErrBadRequestMAC
	}
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	epoch := r.epoch
	if epoch.IsZero() {
		epoch = processStart
	}
	if r.skew == nil {
		r.skew = map[string]time.Duration{}
	}
	when, fresh := Fresh(at, now, time.Time{})
	if when.IsZero() {
		return nil, "", ErrBadRequestMAC
	}
	if !fresh {
		r.skew[node] = when.Sub(now)
		return nil, "", ErrClock
	}
	delete(r.skew, node)
	if when.Before(epoch) {
		// Sent before this fog started: it may have been taken by the one
		// before, which kept no record. The machine sends a fresh one.
		return nil, "", ErrReplayed
	}
	if r.nonces == nil {
		r.nonces = map[string]time.Time{}
	}
	for k, t := range r.nonces {
		if now.Sub(t) > 2*RequestSkew {
			delete(r.nonces, k)
		}
	}
	key := node + "\n" + nonce
	if _, seen := r.nonces[key]; seen {
		return nil, "", ErrReplayed
	}
	r.nonces[key] = now
	if used == n.TokenDigest && n.PrevTokenDigest != "" {
		// Heard with the new credential: the old one is done.
		if err := r.Store.ForgetPreviousComputeCredential(n.ID); err != nil {
			return nil, "", err
		}
		n.PrevTokenDigest = ""
	}
	return n, used, nil
}

// Rotate gives a machine a new credential, right after pairing, on its
// authenticated poll (NodeReport.Rotate): an X25519 exchange of keys made
// for it, so the new digest is known only to the two ends, whoever
// recorded the pairing. It answers fog's half, sent signed. A rotation
// asked with the previous credential (the answer to one before was lost)
// rotates from that.
func (r *Registry) Rotate(n *store.ComputeNode, req RotateRequest) (*RotateAnswer, error) {
	used := n.TokenDigest
	if n.PrevTokenDigest != "" {
		// authenticate forgets the previous digest when the current one
		// verifies, so one still here is the one this request used.
		used = n.PrevTokenDigest
	}
	raw, err := base64.StdEncoding.DecodeString(req.Pub)
	if err != nil {
		return nil, fmt.Errorf("%w: not a key", ErrInvalid)
	}
	machinePub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: not a key", ErrInvalid)
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(machinePub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	fogPub := base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
	fresh := RotatedDigest(shared, n.ID, used, req.Pub, fogPub)
	if err := r.Store.RotateComputeCredential(n.ID, used, fresh); err != nil {
		return nil, err
	}
	n.TokenDigest, n.PrevTokenDigest = fresh, used
	return &RotateAnswer{Pub: fogPub}, nil
}

// clockOff says how far a machine's clock was from fog's when its last
// request was refused for it; "" when it isn't.
func (r *Registry) clockOff(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	off, ok := r.skew[id]
	if !ok {
		return ""
	}
	dir := "ahead of"
	if off < 0 {
		off, dir = -off, "behind"
	}
	return fmt.Sprintf("its clock is %s %s fog's, so fog refuses its polls: set its time (NTP) right", off.Round(time.Second), dir)
}

// Heard records a poll from a node, and its inventory when it sent one.
func (r *Registry) Heard(n *store.ComputeNode, inv *Inventory) error {
	if n.ConnectedAt == "" {
		n.ConnectedAt = r.clock().UTC().Format(time.RFC3339)
		if err := r.Store.MarkComputeNodeConnected(n.ID, n.ConnectedAt); err != nil {
			return err
		}
	}
	if inv != nil {
		raw, err := json.Marshal(inv)
		if err != nil {
			return err
		}
		if string(raw) != n.Inventory {
			n.Inventory = string(raw)
			r.heard(n.ID, true)
			return r.Store.SetComputeInventory(n.ID, n.Inventory, r.clock().UTC().Format(time.RFC3339))
		}
	}
	if r.heard(n.ID, false) {
		return r.Store.TouchComputeNode(n.ID, r.clock().UTC().Format(time.RFC3339))
	}
	return nil
}

// heard notes the node alive, and answers whether the store is due a
// last_seen_at (always when force).
func (r *Registry) heard(id string, force bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen, r.touched = map[string]time.Time{}, map[string]time.Time{}
	}
	at := r.clock()
	r.seen[id] = at
	if force || at.Sub(r.touched[id]) >= touchEvery {
		r.touched[id] = at
		return true
	}
	return false
}

// online reports whether a node polled recently.
func (r *Registry) online(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.seen[id]
	return ok && r.clock().Sub(at) < Online
}

// Forget drops what the registry holds in memory for a removed node.
func (r *Registry) Forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.seen, id)
	delete(r.touched, id)
}

// Machine is one paired node as the registry sees it now.
type Machine struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Inventory  Inventory `json:"inventory"`
	Online     bool      `json:"online"`
	Paused     bool      `json:"paused"`
	LastSeenAt string    `json:"lastSeenAt,omitempty"`
	// ConnectedAt is when its service first polled fog; "" while it never
	// has, which Settings › Compute and the Inbox both say.
	ConnectedAt string `json:"connectedAt,omitempty"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   string `json:"createdAt"`
	// Pool is the video pool it belongs to, or "" with Why not.
	Pool string `json:"pool,omitempty"`
	Why  string `json:"why,omitempty"`
	// ClockOff says the machine's clock is too far from fog's for fog to
	// take its polls; "" when it isn't.
	ClockOff string `json:"clockOff,omitempty"`
	// Budget is the operator's override of its slots, nil for its own.
	Budget *Slots `json:"budget,omitempty"`
	// Slots is what it lends now: its budget or its own, nothing while
	// paused or offline, no batch while its owner marked it busy.
	Slots Slots `json:"slots"`
	// Behind says it runs another fog-node than fog's release (Service).
	Behind *Behind `json:"behind,omitempty"`
	// secret is the key fog MACs its inference requests with.
	secret string
}

// machine reads a stored node.
func (r *Registry) machine(n store.ComputeNode) Machine {
	m := Machine{ID: n.ID, Name: n.Name, Paused: n.Paused, LastSeenAt: n.LastSeenAt, ConnectedAt: n.ConnectedAt, CreatedBy: n.CreatedBy,
		CreatedAt: n.CreatedAt, Online: r.online(n.ID), secret: InferenceKey(n.TokenDigest), ClockOff: r.clockOff(n.ID)}
	_ = json.Unmarshal([]byte(n.Inventory), &m.Inventory)
	m.Pool, m.Why = poolFor(m.Inventory.Video)
	if n.Budget != "" {
		var b Slots
		if json.Unmarshal([]byte(n.Budget), &b) == nil {
			m.Budget = &b
		}
	}
	if m.Online && !m.Paused && m.Pool != "" {
		m.Slots = m.Inventory.Video.Slots
		if m.Budget != nil {
			m.Slots = *m.Budget
		}
		if m.Inventory.Busy {
			m.Slots.Batch = 0
		}
	}
	return m
}

// Machines answers every paired node.
func (r *Registry) Machines() ([]Machine, error) {
	nodes, err := r.Store.ListComputeNodes()
	if err != nil {
		return nil, err
	}
	out := make([]Machine, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, r.machine(n))
	}
	return out, nil
}

// Machine answers one node.
func (r *Registry) Machine(id string) (*Machine, error) {
	n, err := r.Store.ComputeNode(id)
	if err != nil {
		return nil, err
	}
	m := r.machine(*n)
	return &m, nil
}

// Pool is a set of machines that run the same command lines, as apps see
// it: one transcoder.
type Pool struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Family   string   `json:"family"`
	Machines []string `json:"machines"`
	Online   int      `json:"online"`
	Capacity Slots    `json:"capacity"`
	// Models are the inference pool's: every model a machine in it has.
	Models []string `json:"models,omitempty"`
	// Hello is how the pool introduces itself to an app.
	Hello Hello `json:"-"`
}

// Pools groups machines into their pools, with the room each lends now.
// A pool with no machine online still shows, with no room: apps keep their
// transcoder and see it offline.
func Pools(machines []Machine) []Pool {
	byID := map[string]*Pool{}
	var order []string
	for _, m := range machines {
		if m.Pool == "" {
			continue
		}
		p := byID[m.Pool]
		if p == nil {
			fam := strings.TrimPrefix(m.Pool, "video/")
			p = &Pool{ID: m.Pool, Name: PoolName(m.Pool), Family: fam}
			byID[m.Pool] = p
			order = append(order, m.Pool)
		}
		p.Machines = append(p.Machines, m.ID)
		if !m.Online {
			continue
		}
		p.Online++
		p.Capacity.Interactive += m.Slots.Interactive
		p.Capacity.Batch += m.Slots.Batch
		v := m.Inventory.Video
		if p.Hello.Encoder == "" {
			p.Hello = Hello{Encoder: p.Family, Hostname: "fog", Platform: m.Pool, FFmpeg: v.FFmpeg, Worker: "fog-node", Filters: v.Filters}
		} else {
			// Only what every machine has: an app's argv runs on any of them.
			p.Hello.Filters = slices.DeleteFunc(slices.Clone(p.Hello.Filters), func(f string) bool { return !slices.Contains(v.Filters, f) })
		}
	}
	out := make([]Pool, 0, len(order))
	slices.Sort(order)
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}
