// Connect: the one way an app in this household gets a credential from its
// sibling in a paired household (docs/connect-contract.md). The app asks its
// own fogd; fogd mints a short-lived vouch, signed with the instance key,
// and relays it over the signed federation channel to the peer's fogd. That
// fogd runs every policy check in one place — the vouch verifies against the
// key it pinned at pairing, the operator's allow-list covers the app, the
// app declares the contract — and only then asks its local app, via the
// fog-connect/1 contract, to mint a credential. The credential flows back the
// same way. Fog carries it; it never stores it and cannot use it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/lucid"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// ContractConnectV1 is the contract an app must declare (repo fog.yaml:
// connect.contract) before this fogd will connect a peer's app to it.
const ContractConnectV1 = "fog-connect/1"

// AppGrantsPath is the contract endpoint an app serves to mint a credential
// (POST) and, with the peer's instance id appended, to revoke what it minted
// (DELETE).
const AppGrantsPath = "/fog/connect/grants"

// connectRequest crosses the wire between the two fogds. The vouch names
// who is asking, for which app, with which key — signed by the requesting
// fogd independently of the transport, so the grantor's app can keep it as
// evidence. Payload is the requesting app's own opaque handshake material,
// relayed untouched to the granting app.
type connectRequest struct {
	Vouch   string          `json:"vouch"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// connectResponse is what the granting fogd answers and what the requesting
// app receives, with the peer added by the requesting fogd (only it knows
// what the peer is called here). Credential and Payload are the granting
// app's own — fog treats both as opaque.
type connectResponse struct {
	App        string          `json:"app"`
	URL        string          `json:"url,omitempty"`
	Credential json.RawMessage `json:"credential"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Peer       *connectPeer    `json:"peer,omitempty"`
}

type connectPeer struct {
	Instance string `json:"instance"`
	Name     string `json:"name"`
}

// connectNonceTTL is how long Server.connectNonces keeps an answer: a
// network-level replay of the same signed request (same vouch nonce) gets
// the cached answer instead of minting a second credential.
const connectNonceTTL = 10 * time.Minute

// appGrantCallTimeout bounds one fog-connect/1 grant call to a local app.
const appGrantCallTimeout = 10 * time.Second

// resolveApps answers how to reach local apps' /fog/* contract endpoints:
// each one's base URL on the fog network and its FOG_API_TOKEN. The default
// needs a running engine (docker); tests set appGrantEndpointFn, read here
// at call time, to answer a local server instead.
func (s *Server) resolveApps(ctx context.Context, list []store.App) []apps.AppEndpoint {
	out := make([]apps.AppEndpoint, len(list))
	switch {
	case s.appGrantEndpointFn != nil:
		for i := range list {
			if out[i].Err = ctx.Err(); out[i].Err == nil {
				out[i].Base, out[i].Token, out[i].Err = s.appGrantEndpointFn(ctx, &list[i])
			}
		}
	case s.engine == nil:
		for i := range out {
			out[i].Err = fmt.Errorf("app engine unavailable")
		}
	default:
		out = s.engine.AppEndpoints(ctx, list)
	}
	return out
}

// appCaller makes contract calls to local apps, reading a refusal as the
// status the app answered.
func (s *Server) appCaller() *appcall.Caller {
	return appcall.New(s.resolveApps)
}

// callApp is one contract call to a local app; see appcall.Caller.Call.
func (s *Server) callApp(ctx context.Context, app *store.App, method, path string, body any) ([]byte, error) {
	return s.appCaller().Call(ctx, app, method, path, body)
}

// fanOutApps calls fn for every reachable app in list at once, each under
// its own timeout; see appcall.Caller.FanOut.
func (s *Server) fanOutApps(ctx context.Context, list []store.App, timeout time.Duration, fn func(context.Context, *appcall.Target)) {
	s.appCaller().FanOut(ctx, list, timeout, fn)
}

// handleAppConnect is the requesting side: a local app asks for a
// credential from one of a peer's apps. The target app defaults to the
// caller's own name — a hook connects to a hook.
func (s *Server) handleAppConnect(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	// Only an app that declares fog-connect/1 asks, as only one that does
	// is asked: a token alone, held by an app that happens to share a
	// connectable app's name, is not a request to connect.
	if contracts, _ := s.store.AppContracts(app.ID); !containsFold(contracts, ContractConnectV1) {
		writeErr(w, http.StatusForbidden, app.Name+" does not declare "+ContractConnectV1)
		return
	}
	var req struct {
		Peer      string          `json:"peer"`
		App       string          `json:"app"`
		PublicKey string          `json:"publicKey"` // the requesting app's fogfed key, bound into the vouch
		Payload   json.RawMessage `json:"payload"`   // the app's own handshake material, relayed opaque
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Peer == "" {
		badRequest(w, "peer is required")
		return
	}
	target := req.App
	if target == "" {
		target = app.Name
	}
	// Apps only see peers the operator exposed; connecting holds the same
	// line, and an unexposed peer reads as unknown rather than as refused.
	peer := s.findVisiblePeer(req.Peer)
	if peer == nil {
		s.connectOutcome(r.Context(), "app:"+app.Name, "connect.request", req.Peer, connectAttrs{app: target, from: app.Name}, "no such peer household")
		badRequest(w, "no such peer household")
		return
	}
	// Connect joins an app to its sibling: a hook to a hook. Every
	// connectable app declares the same contract, so a requester that picked
	// its target by contract alone can land on the wrong app — a Hub asking a
	// Feed — and the far app would mint a credential of a kind the requester
	// cannot use. Refused here, before anything crosses to the peer.
	if !strings.EqualFold(target, app.Name) {
		reason := fmt.Sprintf("%s can only connect to %s on a peer, not to %s", app.Name, app.Name, target)
		s.connectOutcome(r.Context(), "app:"+app.Name, "connect.request", peer.Name, connectAttrs{app: target, from: app.Name}, reason)
		badRequest(w, reason)
		return
	}
	id, err := s.fed.Identity()
	if err != nil {
		writeError(w, err)
		return
	}
	now := time.Now().UTC()
	v := fed.Vouch{
		From:      id.InstanceID,
		To:        peer.InstanceID,
		App:       target,
		FromApp:   s.appPublicURL(app),
		FromKey:   req.PublicKey,
		Nonce:     randx.Token(),
		IssuedAt:  now.Format(time.RFC3339),
		ExpiresAt: now.Add(fed.VouchTTL).Format(time.RFC3339),
	}
	body, _ := json.Marshal(connectRequest{Vouch: fed.EncodeVouch(v, fed.SignVouch(id, v)), Payload: req.Payload})

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	respBody, err := s.fed.SignedPostVerified(ctx, peer, "/fed/v1/connect", "application/json", body)
	attrs := connectAttrs{app: target, from: app.Name}
	if err != nil {
		s.connectOutcome(r.Context(), "app:"+app.Name, "connect.request", peer.Name, attrs, err.Error())
		var remote *fed.RemoteError
		if errors.As(err, &remote) && remote.Status == http.StatusNotFound {
			writeErr(w, http.StatusBadGateway, peer.Name+"'s fog predates Connect; it needs to update before its apps can be connected to")
			return
		}
		writeErr(w, http.StatusBadGateway, "connection refused or peer unreachable: "+err.Error())
		return
	}
	var resp connectResponse
	if err := json.Unmarshal(respBody, &resp); err != nil || len(resp.Credential) == 0 {
		s.connectOutcome(r.Context(), "app:"+app.Name, "connect.request", peer.Name, attrs, "no credential in the answer")
		writeErr(w, http.StatusBadGateway, peer.Name+" answered without a credential")
		return
	}
	resp.Peer = &connectPeer{Instance: peer.InstanceID, Name: peer.Name}
	attrs.credentialKeys = credentialKeys(resp.Credential)
	s.connectOutcome(r.Context(), "app:"+app.Name, "connect.request", peer.Name, attrs, "")
	writeJSON(w, http.StatusOK, resp)
}

// connectAttrs are what one connect outcome is recorded with: the target
// app, who asked (the requesting app's name on the requesting side, its
// public URL as the vouch carries it on the granting side), and, once a
// credential exists, the names of its fields. The names are what tell a
// Hub's api_key from a Feed's token; the values never leave the answer.
type connectAttrs struct {
	app, from      string
	credentialKeys string
}

// connectOutcome records one side of one connect wherever an operator might
// look for it: the audit log, the request's Lucid trace, and a log line that
// ships to Lucid on the same trace. reason is empty for a success; a refusal
// is logged at warn and marks the trace errored, so a failed connect shows
// up in Lucid's errors rather than only in this fogd's audit log.
func (s *Server) connectOutcome(ctx context.Context, actor, action, peer string, a connectAttrs, reason string) {
	detail := map[string]any{"app": a.app, "from": a.from, "ok": reason == ""}
	args := []any{"peer", peer, "app", a.app, "from", a.from}
	if a.credentialKeys != "" {
		detail["credentialKeys"] = a.credentialKeys
		args = append(args, "credential_keys", a.credentialKeys)
	}
	if reason != "" {
		detail["reason"] = reason
		args = append(args, "reason", reason)
	}
	s.auditAs(actor, action, peer, detail)

	txn := lucid.FromContext(ctx)
	txn.Attr("connect.action", action)
	txn.Attr("connect.peer", peer)
	txn.Attr("connect.app", a.app)
	txn.Attr("connect.from", a.from)
	txn.Attr("connect.ok", reason == "")
	if a.credentialKeys != "" {
		txn.Attr("connect.credential_keys", a.credentialKeys)
	}
	if reason == "" {
		s.log.InfoContext(ctx, action, args...)
		return
	}
	txn.Attr("connect.reason", reason)
	txn.Fail(action + " refused")
	s.log.WarnContext(ctx, action+" refused", args...)
}

// credentialKeys names the top-level fields of an app's credential, sorted
// and comma-joined, or "" when it is not a JSON object.
func credentialKeys(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	keys := slices.Sorted(maps.Keys(fields))
	return strings.Join(keys, ",")
}

// handleFedConnect is the granting side: a peer's fogd relays one of its
// apps' vouches. authorizeConnection decides; the local app mints.
func (s *Server) handleFedConnect(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedAuthed(w, r, 1<<20)
	if !ok {
		return
	}
	var req connectRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Vouch == "" {
		badRequest(w, "invalid connect request")
		return
	}
	v, sig, err := fed.DecodeVouch(req.Vouch)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	attrs := connectAttrs{app: v.App, from: v.FromApp}
	app, status, reason := s.authorizeConnection(peer, v, sig)
	if app == nil {
		s.connectOutcome(r.Context(), "federation", "connect.serve", peer.Name, attrs, reason)
		writeErr(w, status, reason)
		return
	}
	// Idempotency: a replayed request gets the same answer, not a second
	// credential.
	nonceKey := peer.InstanceID + ":" + v.Nonce
	if cached, ok := s.connectNonces.Get(nonceKey); ok {
		s.writeFedSignedBytes(w, r, cached)
		return
	}

	// Not while the app is being removed, and not before anything still
	// owed to this peer at this app is paid; the credential is recorded.
	done, merr := s.beginMint(r.Context(), app.Name, peer.InstanceID)
	if merr != nil {
		s.connectOutcome(r.Context(), "federation", "connect.serve", peer.Name, attrs, merr.Error())
		writeErr(w, http.StatusServiceUnavailable, merr.Error())
		return
	}
	credential, payload, err := s.mintAppGrant(r.Context(), app, map[string]any{
		"peer":    map[string]string{"instance": peer.InstanceID, "name": peer.Name, "publicKey": v.FromKey},
		"appUrl":  v.FromApp,
		"vouch":   req.Vouch,
		"payload": req.Payload,
	})
	// Recorded even on an error: an app that minted and then failed to
	// answer may hold a credential all the same.
	done(err == nil || !errors.Is(err, appcall.ErrUnreachable))
	if errors.Is(err, appcall.ErrUnreachable) {
		s.connectOutcome(r.Context(), "federation", "connect.serve", peer.Name, attrs, v.App+" is not reachable right now")
		writeErr(w, http.StatusBadGateway, v.App+" is not reachable right now")
		return
	}
	if err != nil {
		s.connectOutcome(r.Context(), "federation", "connect.serve", peer.Name, attrs, err.Error())
		writeErr(w, http.StatusBadGateway, v.App+" refused to mint a grant: "+err.Error())
		return
	}
	attrs.credentialKeys = credentialKeys(credential)
	s.connectOutcome(r.Context(), "federation", "connect.serve", peer.Name, attrs, "")

	out, _ := json.Marshal(connectResponse{App: v.App, URL: s.appPublicURL(app), Credential: credential, Payload: payload})
	s.connectNonces.Put(nonceKey, out)
	// Signed like every other fed answer: the credential the requester ends
	// up holding carries proof it came from this peer's key, not merely from
	// whatever answered at the dialed address.
	s.writeFedSignedBytes(w, r, out)
}

// authorizeConnection is the one place a connection is allowed or refused.
// The vouch must be the signing peer's own, minted for this instance,
// signed by the key pinned for that peer at pairing, unexpired and carrying
// a nonce; the operator must have granted the peer apps.introduce and its
// allow-list must cover the app; the app must exist and declare
// fog-connect/1. A refusal is a nil app with the status and reason to
// answer.
func (s *Server) authorizeConnection(peer *store.FedPeer, v fed.Vouch, sig string) (*store.App, int, string) {
	if v.From != peer.InstanceID {
		return nil, http.StatusForbidden, "vouch was minted by another instance"
	}
	id, err := s.fed.Identity()
	if err != nil || v.To != id.InstanceID {
		return nil, http.StatusForbidden, "vouch is for another instance"
	}
	pub, err := fed.ParsePublicKey(peer.PublicKey)
	if err != nil {
		return nil, http.StatusForbidden, "peer key unusable"
	}
	if err := fed.VerifyVouch(pub, v, sig, time.Now()); err != nil {
		return nil, http.StatusForbidden, err.Error()
	}
	if v.Nonce == "" {
		return nil, http.StatusBadRequest, "vouch carries no nonce"
	}
	return s.connectTarget(peer, v.App)
}

// connectTarget is the operator's half of authorizeConnection: whether peer
// may connect to the local app named target, under the apps.introduce grant
// and its allow-list, and whether that app accepts connections at all.
func (s *Server) connectTarget(peer *store.FedPeer, target string) (*store.App, int, string) {
	raw, granted, err := s.store.FedCapabilityConfig(peer.ID, store.CapAppsIntroduce, store.DirGrant)
	if err != nil {
		return nil, http.StatusInternalServerError, "couldn't read this peer's grants"
	}
	if !granted {
		return nil, http.StatusForbidden, "this peer may not connect to apps here"
	}
	// The allow-list exactly as configured: {} is every contracted app,
	// {"apps": [...]} those named and no others, and a config that can't be
	// read allows nothing.
	scope, err := fed.ParseAppScope(raw)
	if err != nil || !scope.Covers(target) {
		return nil, http.StatusForbidden, "this peer may not connect to " + target
	}
	app, err := s.store.AppByName(target)
	if err != nil {
		return nil, http.StatusBadRequest, "no app named " + target + " here"
	}
	contracts, _ := s.store.AppContracts(app.ID)
	if !containsFold(contracts, ContractConnectV1) {
		return nil, http.StatusBadRequest, target + " does not accept connections (no " + ContractConnectV1 + " contract)"
	}
	return app, 0, ""
}

// mintAppGrant asks a local app to mint a credential for a peer's app. The
// credential and any handshake payload come back raw — they are the app's
// to shape.
func (s *Server) mintAppGrant(ctx context.Context, app *store.App, body any) (json.RawMessage, json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, appGrantCallTimeout)
	defer cancel()
	raw, err := s.callApp(ctx, app, "POST", AppGrantsPath, body)
	if err != nil {
		return nil, nil, err
	}
	var parsed struct {
		Credential json.RawMessage `json:"credential"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Credential) == 0 {
		return nil, nil, fmt.Errorf("app's grant response carried no credential")
	}
	return parsed.Credential, parsed.Payload, nil
}

// revokeAppGrants tells every contracted local app to revoke what it minted
// for a departing peer. Best-effort — the peer's fed access is already gone;
// this cleans up the app-level credentials fog brokered. The apps are asked
// at once, ten seconds each, all under ctx, so neither a hung app nor a
// shutdown holds the sweep for long.
func (s *Server) revokeAppGrants(ctx context.Context, peer *store.FedPeer) {
	s.revokeAppGrantsOutside(ctx, peer, fed.AppScope{})
}

// revokeAppGrantsOutside is revokeAppGrants for the contracted apps that
// scope — a peer's apps.introduce allow-list as it now stands — no longer
// covers. The zero scope covers nothing, so every app is asked.
func (s *Server) revokeAppGrantsOutside(ctx context.Context, peer *store.FedPeer, scope fed.AppScope) {
	if peer.InstanceID == "" {
		return
	}
	// Every contracted app the scope no longer covers, and every app that
	// ever minted for this peer: each owes a revocation, kept until paid
	// (connect_ledger.go).
	var names []string
	if all, err := s.store.AppsWithContract(ContractConnectV1); err == nil {
		for _, a := range all {
			if !scope.Covers(a.Name) {
				names = append(names, a.Name)
			}
		}
	}
	l := s.connectLedger()
	l.mu.Lock()
	for app, insts := range s.loadMinted() {
		if slices.Contains(insts, peer.InstanceID) && !scope.Covers(app) && !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, app) }) {
			names = append(names, app)
		}
	}
	l.mu.Unlock()
	s.owe(ctx, names, peer.InstanceID, "access withdrawn")
}

// forgetAppForPeers is the federation side of an app's deletion: every
// grant that names the app — apps.directory and apps.introduce, which name
// this household's apps — has the name taken out, so a new app that takes
// the name is covered by no grant made for the old one. A grant for every
// app stays one; it never named the app.
func (s *Server) forgetAppForPeers(name string) {
	all, err := s.store.AllFedCapabilities()
	if err != nil {
		s.log.Warn("federation: reading grants to forget a deleted app", "app", name, "err", err)
		return
	}
	for peerID, caps := range all {
		for _, c := range caps {
			if c.Direction != store.DirGrant || (c.Capability != store.CapAppsDirectory && c.Capability != store.CapAppsIntroduce) {
				continue
			}
			scope, err := fed.ParseAppScope(string(c.Config))
			if err != nil {
				continue // covers nothing already
			}
			if cfg, changed := scope.Without(name); changed {
				if err := s.store.SetFedCapability(peerID, c.Capability, store.DirGrant, cfg); err != nil {
					s.log.Warn("federation: forgetting a deleted app", "app", name, "peer", peerID, "err", err)
					continue
				}
				s.auditAs("federation", "federation.capability.app_forgotten", name, map[string]any{"peer": peerID, "capability": c.Capability})
			}
		}
	}
}

// findVisiblePeer resolves a peer by name or instance id, but only among
// those the operator has exposed to apps (use:apps.directory).
func (s *Server) findVisiblePeer(ref string) *store.FedPeer {
	for _, p := range mustList(s.store.ListFedPeers()) {
		if p.Name != ref && p.InstanceID != ref {
			continue
		}
		peer := p
		if !s.peerVisibleToApps(&peer) {
			return nil
		}
		return &peer
	}
	return nil
}

// appPublicURL is the app's routed address under the base domain, or empty
// when it has none — never a fabricated URL.
func (s *Server) appPublicURL(app *store.App) string {
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		return ""
	}
	spec, err := apps.LatestSpec(s.store, app.ID)
	if err != nil || len(spec.Routes) == 0 {
		return ""
	}
	host := apps.RouteHost(app.Name, spec.Routes[0], domain)
	if host == "" {
		return ""
	}
	return "https://" + host
}

// containsFold reports whether list holds v, ignoring case: app names and
// contract ids are compared the way people type them.
func containsFold(list []string, v string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.EqualFold(s, v) })
}

func mustList(peers []store.FedPeer, err error) []store.FedPeer {
	if err != nil {
		return nil
	}
	return peers
}
