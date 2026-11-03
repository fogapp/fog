package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
)

// Connect revocation, made durable.
//
// A credential an app mints for a peer through Connect lives in the app,
// out of fog's reach: fog can only ask the app to revoke it
// (DELETE /fog/connect/grants/{instance}), and the app may be down when it
// is asked. So fog keeps two records, in its settings:
//
//   - minted: for each app, the peer instances it has minted a credential
//     for — current peers and former ones alike;
//   - owed: each revocation fog has decided on (an unpair, a narrowed
//     apps.introduce, an app's deletion) and not yet seen answered 2xx.
//
// An owed revocation is tried at once, then every minute until the app
// holding that name answers 2xx — the same app, or one installed later
// under its name with its data. An app's data is kept on deletion only when
// nothing is owed for it (or the operator says to keep it anyway), and a
// new credential is minted for a peer only once anything owed to that peer
// by that app is paid. While an app is being deleted no credential is
// minted for it at all, so none can slip in between revoking and deleting.

const (
	settingConnectMinted = "fed_connect_minted"
	settingConnectOwed   = "fed_connect_owed"
)

// owedRevocation is one revocation fog owes a peer at an app.
type owedRevocation struct {
	App      string `json:"app"`
	Instance string `json:"instance"`
	Reason   string `json:"reason"`
	Since    string `json:"since"`
}

// connectLedger guards the two records and the minting freeze.
type connectLedger struct {
	mu       sync.Mutex
	cond     *sync.Cond
	frozen   map[string]bool // app (lowercase) -> no minting
	inflight map[string]int  // app (lowercase) -> mints under way
}

var connectLedgerBy sync.Map // *Server -> *connectLedger

func (s *Server) connectLedger() *connectLedger {
	if l, ok := connectLedgerBy.Load(s); ok {
		return l.(*connectLedger)
	}
	fresh := &connectLedger{frozen: map[string]bool{}, inflight: map[string]int{}}
	fresh.cond = sync.NewCond(&fresh.mu)
	l, _ := connectLedgerBy.LoadOrStore(s, fresh)
	return l.(*connectLedger)
}

func appKey(name string) string { return strings.ToLower(name) }

// loadMinted reads the minted record: app -> instances.
func (s *Server) loadMinted() map[string][]string {
	out := map[string][]string{}
	if raw, err := s.store.GetSetting(settingConnectMinted); err == nil && raw != "" {
		json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (s *Server) saveMinted(m map[string][]string) {
	raw, _ := json.Marshal(m)
	if err := s.store.SetSetting(settingConnectMinted, string(raw)); err != nil {
		s.log.Warn("connect: recording minted credentials", "err", err)
	}
}

func (s *Server) loadOwed() []owedRevocation {
	var out []owedRevocation
	if raw, err := s.store.GetSetting(settingConnectOwed); err == nil && raw != "" {
		json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (s *Server) saveOwed(o []owedRevocation) {
	raw, _ := json.Marshal(o)
	if err := s.store.SetSetting(settingConnectOwed, string(raw)); err != nil {
		s.log.Warn("connect: recording owed revocations", "err", err)
	}
}

// recordMinted notes that app minted a credential for instance. The caller
// holds the ledger's lock.
func (s *Server) recordMintedLocked(app, instance string) {
	m := s.loadMinted()
	k := appKey(app)
	if !slices.Contains(m[k], instance) {
		m[k] = append(m[k], instance)
		s.saveMinted(m)
	}
}

// oweLocked adds a revocation to the outbox, once. The caller holds the
// ledger's lock.
func (s *Server) oweLocked(app, instance, reason string) {
	o := s.loadOwed()
	for _, r := range o {
		if appKey(r.App) == appKey(app) && r.Instance == instance {
			return
		}
	}
	o = append(o, owedRevocation{App: app, Instance: instance, Reason: reason, Since: time.Now().UTC().Format(time.RFC3339)})
	s.saveOwed(o)
}

// owe records revocations for instance at each of apps, and tries them.
func (s *Server) owe(ctx context.Context, apps []string, instance, reason string) {
	if instance == "" || len(apps) == 0 {
		return
	}
	l := s.connectLedger()
	l.mu.Lock()
	for _, a := range apps {
		s.oweLocked(a, instance, reason)
	}
	l.mu.Unlock()
	s.payOwed(ctx, func(r owedRevocation) bool { return r.Instance == instance })
}

// payOwed tries the owed revocations which picks out, against the apps
// that now hold their names, and drops each one answered 2xx. It reports
// what is still owed of those it tried.
func (s *Server) payOwed(ctx context.Context, which func(owedRevocation) bool) []owedRevocation {
	l := s.connectLedger()
	l.mu.Lock()
	var todo []owedRevocation
	for _, r := range s.loadOwed() {
		if which(r) {
			todo = append(todo, r)
		}
	}
	l.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}
	byApp := map[string][]owedRevocation{}
	for _, r := range todo {
		byApp[appKey(r.App)] = append(byApp[appKey(r.App)], r)
	}
	var list []store.App
	for k := range byApp {
		if a, err := s.store.AppByName(k); err == nil {
			list = append(list, *a)
		}
	}
	var mu sync.Mutex
	paid := map[owedRevocation]bool{}
	s.fanOutApps(ctx, list, appGrantCallTimeout, func(ctx context.Context, t *appcall.Target) {
		for _, r := range byApp[appKey(t.App.Name)] {
			if _, err := t.Call(ctx, "DELETE", AppGrantsPath+"/"+r.Instance, nil); err != nil {
				s.log.Warn("connect: owed revocation not yet paid", "app", t.App.Name, "instance", r.Instance, "err", err)
				continue
			}
			mu.Lock()
			paid[r] = true
			mu.Unlock()
		}
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	var left, still []owedRevocation
	for _, r := range s.loadOwed() {
		if paid[r] {
			continue
		}
		left = append(left, r)
		if which(r) {
			still = append(still, r)
		}
	}
	if len(paid) > 0 {
		s.saveOwed(left)
	}
	return still
}

// forgiveOwed drops what is owed at app: its data, and the credentials in
// it, are gone.
func (s *Server) forgiveOwed(app string) {
	l := s.connectLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	var left []owedRevocation
	for _, r := range s.loadOwed() {
		if appKey(r.App) != appKey(app) {
			left = append(left, r)
		}
	}
	s.saveOwed(left)
	m := s.loadMinted()
	delete(m, appKey(app))
	s.saveMinted(m)
}

// ConnectRevocationLoop pays owed revocations every minute until each is
// answered.
func (s *Server) ConnectRevocationLoop(ctx context.Context) {
	loop.Every(ctx, time.Minute, time.Minute, func(ctx context.Context) {
		s.payOwed(ctx, func(owedRevocation) bool { return true })
	})
}

// errMintFrozen: the app is being deleted; nothing is minted for it.
var errMintFrozen = errors.New("this app is being removed and takes no new connections")

// beginMint admits one mint for app to instance: not while the app is
// being deleted, and only once anything owed to instance at app is paid.
// The returned function records the credential (when minted) and ends the
// mint.
func (s *Server) beginMint(ctx context.Context, app, instance string) (func(minted bool), error) {
	if owed := s.payOwed(ctx, func(r owedRevocation) bool { return appKey(r.App) == appKey(app) && r.Instance == instance }); len(owed) > 0 {
		return nil, fmt.Errorf("an earlier credential for this household at %s is still being revoked; try again later", app)
	}
	l := s.connectLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.frozen[appKey(app)] {
		return nil, errMintFrozen
	}
	l.inflight[appKey(app)]++
	return func(minted bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if minted {
			if l.frozen[appKey(app)] {
				// Can't happen — the freeze waits for mints under way — but
				// if it did, the credential is owed back at once.
				s.oweLocked(app, instance, "minted during removal")
			}
			s.recordMintedLocked(app, instance)
		}
		if l.inflight[appKey(app)]--; l.inflight[appKey(app)] <= 0 {
			delete(l.inflight, appKey(app))
		}
		l.cond.Broadcast()
	}, nil
}

// freezeMinting stops new mints for app and waits for those under way to
// finish; the returned function lifts the freeze.
func (s *Server) freezeMinting(app string) func() {
	l := s.connectLedger()
	l.mu.Lock()
	l.frozen[appKey(app)] = true
	for l.inflight[appKey(app)] > 0 {
		l.cond.Wait()
	}
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.frozen, appKey(app))
		l.mu.Unlock()
	}
}

// revokeForRemoval owes, for an app about to be deleted, a revocation to
// every peer — paired now, or ever minted for — and pays what it can. It
// reports what is still owed at that app, for any peer.
func (s *Server) revokeForRemoval(ctx context.Context, app *store.App) []owedRevocation {
	contracts, _ := s.store.AppContracts(app.ID)
	l := s.connectLedger()
	l.mu.Lock()
	instances := slices.Clone(s.loadMinted()[appKey(app.Name)])
	l.mu.Unlock()
	if containsFold(contracts, ContractConnectV1) {
		for _, p := range mustList(s.store.ListFedPeers()) {
			if p.InstanceID != "" && !slices.Contains(instances, p.InstanceID) {
				instances = append(instances, p.InstanceID)
			}
		}
	}
	l.mu.Lock()
	for _, inst := range instances {
		s.oweLocked(app.Name, inst, "app removed")
	}
	l.mu.Unlock()
	return s.payOwed(ctx, func(r owedRevocation) bool { return appKey(r.App) == appKey(app.Name) })
}
