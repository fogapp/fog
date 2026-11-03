package compute

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// Service is compute as fogd runs it: the registry of machines, the builds
// it hands them, and each machine's mailbox of work.
type Service struct {
	Registry *Registry
	Binaries *Binaries
	// Version is this fogd's; a node on another release is offered the
	// matching build.
	Version string
	Log     *slog.Logger

	mu    sync.Mutex
	boxes map[string]*mailbox
	// offers is each node behind this release: since when fog has offered
	// it this one, and what came of it.
	offers map[string]*offer
	// relay takes in what machines report about their runs (NewRelay).
	relay *Relay
	// answering is how many inference requests each machine is answering.
	answering map[string]int
	// removed is each machine removed lately, so a poll it holds open (or
	// one that authenticated just before) answers Removed.
	removed map[string]time.Time
	// Signer is fog's instance identity, which pairing hands a machine to
	// pin and which signs every poll answer (fed.Identity); nil asks the
	// server's.
	Signer Signer
	// Router orders the machines for an inference request, when an app
	// declares fog-inference-router/1; nil, or an error or no answer in
	// RouterBudget, leaves fogd's own order.
	Router func(ctx context.Context, ask RouteAsk) ([]string, error)
}

// Signer is what signs for fog: its instance identity.
type Signer interface {
	PublicKeyB64() string
	SignTranscript(label, nonce string, doc []byte) string
}

// mailbox is what waits for one node's next poll.
type mailbox struct {
	start []NodeRun
	stop  []string
	// wake is closed when something arrives, ending a held poll.
	wake chan struct{}
}

func (s *Service) box(id string) *mailbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.boxes == nil {
		s.boxes = map[string]*mailbox{}
	}
	b := s.boxes[id]
	if b == nil {
		b = &mailbox{wake: make(chan struct{})}
		s.boxes[id] = b
	}
	return b
}

// deliver puts work in a node's mailbox and wakes its held poll.
func (s *Service) deliver(id string, start []NodeRun, stop []string) {
	b := s.box(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	b.start = append(b.start, start...)
	b.stop = append(b.stop, stop...)
	select {
	case <-b.wake:
	default:
		close(b.wake)
	}
}

// take empties a node's mailbox, and hands back the channel that says when
// it next has something.
func (s *Service) take(id string) (start []NodeRun, stop []string, wake <-chan struct{}) {
	b := s.box(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	start, stop = b.start, b.stop
	b.start, b.stop = nil, nil
	select {
	case <-b.wake:
		b.wake = make(chan struct{})
	default:
	}
	return start, stop, b.wake
}

// Poll answers one node's poll: its report recorded, then whatever work it
// has, waiting up to HoldPoll for some when the node allows. from is the
// address the poll came from: fog reaches the machine's inference listener
// there, never at an address the machine names.
func (s *Service) Poll(ctx context.Context, n *store.ComputeNode, report NodeReport, from string) (*NodeAnswer, error) {
	if s.isRemoved(n.ID) {
		return &NodeAnswer{Removed: true}, nil
	}
	if inv := report.Inventory; inv != nil && inv.Inference != nil {
		lent := *inv.Inference
		lent.URL = reachAt(from, lent.URL)
		if lent.URL == "" {
			inv.Inference = nil
		} else {
			inv.Inference = &lent
		}
	}
	if err := s.Registry.Heard(n, report.Inventory); err != nil {
		return nil, err
	}
	if report.Rotate != nil {
		// Answered at once, never held: the machine waits on it to sign
		// anything more.
		rotated, err := s.Registry.Rotate(n, *report.Rotate)
		if err != nil {
			return nil, err
		}
		s.received(n, report)
		return &NodeAnswer{PollMS: 250, Rotated: rotated}, nil
	}
	s.received(n, report)
	ans := &NodeAnswer{PollMS: 250, Update: s.update(ctx, n, report.Inventory != nil)}
	start, stop, wake := s.take(n.ID)
	if len(start) == 0 && len(stop) == 0 && report.Wait && ans.Update == nil {
		timer := time.NewTimer(HoldPoll)
		defer timer.Stop()
		select {
		case <-wake:
			start, stop, _ = s.take(n.ID)
		case <-timer.C:
		case <-ctx.Done():
			return ans, nil
		}
		if s.isRemoved(n.ID) {
			return &NodeAnswer{Removed: true}, nil
		}
		// Heard again at the end of the hold, so a node that's only been
		// waiting on fog still counts as online; and Update now may be what
		// ended it.
		s.Registry.heard(n.ID, false)
		ans.Update = s.update(ctx, n, false)
	}
	ans.Start, ans.Stop = start, stop
	return ans, nil
}

// received takes in what a node said about its runs.
func (s *Service) received(n *store.ComputeNode, report NodeReport) {
	if s.relay != nil {
		s.relay.fromNode(n.ID, report)
	}
}

// UpdateGrace is how long a machine may stay behind fog's release, once
// offered it, before it counts as stuck: an update takes seconds.
var UpdateGrace = 10 * time.Minute

// reoffer is how often an offer is made again to a node that hasn't taken
// it, in case the answer that carried it was lost. A node doesn't try
// again a version it failed (until Update now), so this costs nothing.
const reoffer = 10 * time.Minute

// offer is fog's release as offered to one node behind it.
type offer struct {
	version string
	since   time.Time
	sent    time.Time
	// err is why fog couldn't offer it: no build for the node.
	err   string
	retry bool
}

// Behind is a machine on another fog-node than this fog's release.
type Behind struct {
	// Want is fog's release, the one it's offered.
	Want string `json:"want"`
	// Since is when fog first offered it; "" until the machine next polls.
	Since string `json:"since,omitempty"`
	// Error is why it hasn't updated, as the machine or fog said.
	Error string `json:"error,omitempty"`
	// Stuck: it failed, or has been offered it longer than UpdateGrace.
	// The Inbox says so.
	Stuck bool `json:"stuck"`
}

// update offers a node the fog-node of this fogd's release when it runs
// another: once, again when the node starts afresh (fresh: it sent its
// inventory), when the operator asks, and now and then.
func (s *Service) update(ctx context.Context, n *store.ComputeNode, fresh bool) *Update {
	if s.Binaries == nil || s.Version == "" || s.Version == "dev" {
		return nil
	}
	m := s.Registry.machine(*n)
	if m.Inventory.Version == s.Version || m.Inventory.OS == "" {
		s.mu.Lock()
		delete(s.offers, n.ID)
		s.mu.Unlock()
		return nil
	}
	now := s.Registry.clock()
	s.mu.Lock()
	o := s.offerLocked(n.ID, now)
	due := o.sent.IsZero() || o.retry || fresh || now.Sub(o.sent) > reoffer
	retry := o.retry
	s.mu.Unlock()
	if !due {
		return nil
	}
	platform := m.Inventory.OS + "-" + m.Inventory.Arch
	b, err := s.Binaries.Get(ctx, platform)
	var manifest []byte
	var sig string
	if err == nil {
		manifest, sig, err = s.Binaries.signed(ctx, platform, b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		o.err = fmt.Sprintf("fog couldn't offer fog-node %s for %s: %v", s.Version, platform, err)
		return nil
	}
	o.err, o.sent, o.retry = "", now, false
	return &Update{Version: b.Version, URL: "/compute/v1/fog-node/" + platform, SHA256: b.SHA256, Retry: retry,
		Manifest: manifest, ManifestSig: sig}
}

// offerLocked is the node's offer of this release, begun now if new.
func (s *Service) offerLocked(id string, now time.Time) *offer {
	if s.offers == nil {
		s.offers = map[string]*offer{}
	}
	o := s.offers[id]
	if o == nil || o.version != s.Version {
		o = &offer{version: s.Version, since: now}
		s.offers[id] = o
	}
	return o
}

// RetryUpdate has a machine try fog's release again at its next poll,
// waking the one it holds: Update now, in Settings › Compute.
func (s *Service) RetryUpdate(id string) {
	s.mu.Lock()
	o := s.offerLocked(id, s.Registry.clock())
	o.retry, o.err = true, ""
	s.mu.Unlock()
	s.deliver(id, nil, nil)
}

// Machines is every paired machine, each behind this release saying so.
func (s *Service) Machines() ([]Machine, error) {
	ms, err := s.Registry.Machines()
	if err != nil {
		return nil, err
	}
	for i := range ms {
		s.behind(&ms[i])
	}
	return ms, nil
}

// Machine is one machine, as Machines.
func (s *Service) Machine(id string) (*Machine, error) {
	m, err := s.Registry.Machine(id)
	if err != nil {
		return nil, err
	}
	s.behind(m)
	return m, nil
}

// Stuck is each machine online but stuck behind this release.
func (s *Service) Stuck() []Machine {
	ms, err := s.Machines()
	if err != nil {
		return nil
	}
	out := []Machine{}
	for _, m := range ms {
		if m.Online && m.Behind != nil && m.Behind.Stuck {
			out = append(out, m)
		}
	}
	return out
}

func (s *Service) behind(m *Machine) {
	if s.Version == "" || s.Version == "dev" || m.Inventory.Version == "" || m.Inventory.Version == s.Version {
		return
	}
	b := &Behind{Want: s.Version}
	if f := m.Inventory.UpdateFailed; f != nil && f.Version == s.Version {
		b.Error, b.Stuck = f.Error, true
	}
	s.mu.Lock()
	if o := s.offers[m.ID]; o != nil && o.version == s.Version {
		b.Since = o.since.UTC().Format(time.RFC3339)
		if b.Error == "" {
			b.Error = o.err
		}
		if o.err != "" || s.Registry.clock().Sub(o.since) > UpdateGrace {
			b.Stuck = true
		}
	}
	s.mu.Unlock()
	m.Behind = b
}

// removedFor is how long a removed machine's id is remembered, to answer a
// poll it held open: far longer than any hold.
const removedFor = time.Hour

// Remove forgets a machine: its credential stops working, the work it held
// is reported lost to the apps that gave it (their data routes closing with
// it), and a poll it holds open answers at once that it was removed, so it
// stops its runs and forgets the pairing now rather than at its next poll.
func (s *Service) Remove(id string) error {
	if err := s.Registry.Store.DeleteComputeNode(id); err != nil {
		return err
	}
	s.Registry.Forget(id)
	if s.relay != nil {
		s.relay.loseNode(id, "the machine running it was removed")
	}
	now := s.Registry.clock()
	s.mu.Lock()
	if s.removed == nil {
		s.removed = map[string]time.Time{}
	}
	for old, at := range s.removed {
		if now.Sub(at) > removedFor {
			delete(s.removed, old)
		}
	}
	s.removed[id] = now
	if b := s.boxes[id]; b != nil {
		select {
		case <-b.wake:
		default:
			close(b.wake)
		}
	}
	delete(s.boxes, id)
	delete(s.offers, id)
	s.mu.Unlock()
	return nil
}

func (s *Service) isRemoved(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.removed[id]
	return ok
}

// reachAt is where fog dials a machine's inference listener: the address its
// poll came from, on the port it names. A machine can't point fog anywhere
// else (fogd itself, an app, the internet). "" when the port isn't one; the
// URL as it is when the poll's address isn't known (tests).
func reachAt(from, lent string) string {
	u, err := url.Parse(lent)
	if err != nil || u.Scheme != "http" {
		return ""
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return ""
	}
	if from == "" {
		return lent
	}
	ip := net.ParseIP(from)
	if ip == nil {
		return ""
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port))
}

// Usage is each app's use of each pool: every app that declares video work
// against each video pool, and every app that declares inference against
// the inference pool when there is one.
func (s *Service) Usage(pools []Pool) ([]Usage, error) {
	out := []Usage{}
	if s.relay != nil {
		video, err := s.relay.Usage(pools)
		if err != nil {
			return nil, err
		}
		out = append(out, video...)
	}
	if !slices.ContainsFunc(pools, func(p Pool) bool { return p.ID == InferencePool }) {
		return out, nil
	}
	apps, err := s.Registry.Store.ListApps()
	if err != nil {
		return nil, err
	}
	declared, err := s.Registry.Store.AllAppCompute()
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		if d := ParseDeclared(declared[a.ID]); d != nil && d.Inference != nil {
			off, _ := s.Registry.Store.ComputeGrantOff(a.ID, InferencePool)
			out = append(out, Usage{App: a.Name, Pool: InferencePool, On: !off})
		}
	}
	return out, nil
}
