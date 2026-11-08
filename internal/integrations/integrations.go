// Package integrations connects one installed app to the others it can
// work with (docs/integrations-contract.md). An app's wants name them, and
// its Integrations tab lists one entry per app. Connecting one does all its
// wants of that app ask: each offer is minted by the provider, under
// fog-integration/1, for this app alone, with the offer's env and the
// minted secrets set in the consumer's spec; an app want hands it the
// provider's connection through uses. It is one revision like any other,
// so History shows it and the Config tab lists the values, and the caller
// redeploys it. Choosing the app is the operator's grant; the provider
// trusts fog's call as it trusts a backup.
//
// The Service knows nothing of HTTP: it answers typed errors (ErrNotFound,
// ErrConflict, ErrProvider) whose message is the whole answer to show, and
// the handlers map them to statuses.
package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/store"
)

// Path is the provider's fog-integration/1 surface: POST mints,
// GET {path}/{id} reports, DELETE {path}/{id} revokes.
const Path = "/fog/integrations"

// ReturnPath is where a provider sends a person back to fog after they
// answered a consent offer.
const ReturnPath = "/api/integrations/return"

// consentTTL is how long a connection waits for a person to approve it at
// the provider: Glass's ticket lasts as long.
const consentTTL = 30 * time.Minute

const (
	// callTimeout bounds one fog-integration/1 call.
	callTimeout = 15 * time.Second
	// statusTimeout bounds one status probe: the list renders on a page.
	statusTimeout = 3 * time.Second
)

// The kinds of refusal an Error carries, for errors.Is.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	// ErrProvider: the provider couldn't be reached, refused, or answered
	// something fog can't use.
	ErrProvider = errors.New("provider failed")
)

// Error is a refusal the operator can act on: Msg is the whole answer,
// shown as it is; Kind says which.
type Error struct {
	Kind error
	Msg  string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Kind }

func refuse(kind error, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// errGone is a provider that no longer knows the credential (404).
var errGone = errors.New("no such integration")

// Refusal reads a provider's no: a 404 to anything but a mint is a
// credential it no longer knows (already revoked), and otherwise the
// provider's own words when it sent {errors: [...]}. The Caller a Service
// is given reads refusals with it.
func Refusal(resp *http.Response, raw []byte) error {
	if resp.StatusCode == http.StatusNotFound && resp.Request.Method != http.MethodPost {
		return errGone
	}
	var refusal struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(raw, &refusal) == nil && len(refusal.Errors) > 0 {
		return errors.New(strings.Join(refusal.Errors, "; "))
	}
	return fmt.Errorf("answered %s", resp.Status)
}

// Caller makes one fog-integration/1 call to a provider; appcall.Caller,
// reading refusals with Refusal, is the real one. A call that never reached
// the provider matches appcall.ErrUnreachable.
type Caller interface {
	Call(ctx context.Context, app *store.App, method, path string, body any) ([]byte, error)
}

// SpecWriter writes a new revision of an app's spec, with its secrets
// (an empty value deletes one): apps.Engine.UpdateSpec.
type SpecWriter interface {
	UpdateSpec(ctx context.Context, name string, spec *apps.AppSpec, secrets map[string]string, comment, actor string) error
}

// Service runs integrations for the handlers.
type Service struct {
	Store *store.Store
	// Specs is nil while the app engine is unavailable.
	Specs SpecWriter
	Apps  Caller
	// PublicURL is an app's routed address under the base domain, or empty.
	PublicURL func(*store.App) string
	// Wire, when set, mounts into an app's spec the folders the apps it
	// uses share, as installing it would.
	Wire func(name string, spec *apps.AppSpec)
	Log  *slog.Logger
}

func (s *Service) audit(actor, action, subject string, detail map[string]any) {
	s.Store.Audit(actor, action, subject, store.AuditDetail(detail))
}

// Status is what the provider says about one credential.
type Status struct {
	Active         bool   `json:"active"`
	LastActivityAt string `json:"lastActivityAt,omitempty"`
}

// Entry states.
const (
	StateAvailable = "available"
	StatePending   = "pending"
	StateConnected = "connected"
)

// Entry is one app on the consumer's tab: an app the consumer can connect
// to, or is connected to. What connecting it involves — which offers are
// minted, under which names, and whether the consumer is handed the app's
// connection — is the consumer's wants of that app, and stays off the tab.
type Entry struct {
	Provider string `json:"provider"`
	// Capabilities say what the consumer gets from the provider, one line
	// each.
	Capabilities []string `json:"capabilities"`
	// State is available, pending (waiting for a person to approve it at
	// the provider) or connected.
	State string `json:"state" ts:"\"available\" | \"pending\" | \"connected\""`
	// Incomplete: connected, but the consumer now wants more of the
	// provider than was connected; connecting again adds the rest.
	Incomplete bool `json:"incomplete,omitempty"`
	// Consent: connecting sends the operator to the provider to approve it.
	Consent bool `json:"consent,omitempty"`
	// Replaces: connecting overwrites a value the operator set by hand.
	Replaces bool `json:"replaces,omitempty"`
	// Installed: the provider is installed here.
	Installed bool `json:"installed"`
	// ConnectedAt and ConnectedBy are the first credential's, when any was
	// minted.
	ConnectedAt string `json:"connectedAt,omitempty"`
	ConnectedBy string `json:"connectedBy,omitempty"`
	// LastActivityAt is the latest any of its credentials was used, as the
	// provider reports it.
	LastActivityAt string `json:"lastActivityAt,omitempty"`
	Problem        string `json:"problem,omitempty"`
}

// List is the consumer's Integrations tab: connected (and pending) first,
// then the rest it can connect, each in the order its wants name them.
type List struct {
	Connected []Entry `json:"connected"`
	Available []Entry `json:"available"`
}

// Running is whether fog can reach the app to ask it anything.
func Running(a *store.App) bool {
	return a.DesiredState == "running" && a.CurrentRev > 0
}

// declared is what app's repo fog.yaml declares about integrations, as
// recorded at its last deploy, or nil.
func (s *Service) declared(appID int64) *apps.Declared {
	raw, err := s.Store.AppIntegrations(appID)
	if err != nil {
		return nil
	}
	return apps.ParseDeclared(raw)
}

// connectionOf is what app offers other apps: its spec's connection with
// the offers its repo declares (apps.EffectiveConnection).
func (s *Service) connectionOf(app *store.App, spec *apps.AppSpec) *apps.Connection {
	return apps.EffectiveConnection(spec, s.declared(app.ID))
}

// wantsOf is what consumer wants of provider, from its spec and its repo
// together, in the order they name it. An app wants nothing of itself.
func (s *Service) wantsOf(consumer *store.App, spec *apps.AppSpec, provider string) []apps.Want {
	if provider == consumer.Name {
		return nil
	}
	var out []apps.Want
	for _, w := range apps.EffectiveWants(spec, s.declared(consumer.ID)) {
		if w.Provider() == provider {
			out = append(out, w)
		}
	}
	return out
}

// usesApp is whether spec is handed provider's connection.
func usesApp(spec *apps.AppSpec, provider string) bool {
	return !apps.SurfaceGrant(provider) && spec.UsesGrant(provider)
}

// plan is what connecting consumer to provider still has to do: the offers
// it wants that nothing has minted yet, and whether it wants the
// provider's connection and isn't handed it yet.
type plan struct {
	offers  []*apps.Offer
	useApp  bool
	missing []string // offers wanted that the provider doesn't make
	noConn  bool     // the app's connection is wanted, and it offers none
}

func (p plan) empty() bool { return len(p.offers) == 0 && !p.useApp }

func planFor(wants []apps.Want, rows []store.Integration, spec *apps.AppSpec, provSpec *apps.AppSpec, conn *apps.Connection, provider string) plan {
	var p plan
	for _, w := range wants {
		if w.App != "" {
			if usesApp(spec, provider) {
				continue
			}
			if provSpec == nil || provSpec.Connection == nil {
				p.noConn = true
				continue
			}
			p.useApp = true
			continue
		}
		id := w.OfferID()
		if slices.ContainsFunc(rows, func(in store.Integration) bool { return in.Offer == id }) {
			continue
		}
		if o := conn.Offer(id); o != nil {
			p.offers = append(p.offers, o)
		} else {
			p.missing = append(p.missing, id)
		}
	}
	return p
}

// capabilities are what the tab lists for provider: the wants' own lines
// where they give any, else those of the offers wanted or connected, else
// their blurbs.
func capabilities(wants []apps.Want, rows []store.Integration, conn *apps.Connection) []string {
	out := []string{}
	add := func(lines ...string) {
		for _, l := range lines {
			if l = strings.TrimSpace(l); l != "" && !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	offers := []string{}
	for _, w := range wants {
		add(w.Capabilities...)
		if id := w.OfferID(); id != "" {
			offers = append(offers, id)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, in := range rows {
		if !slices.Contains(offers, in.Offer) {
			offers = append(offers, in.Offer)
		}
	}
	for _, id := range offers {
		if o := conn.Offer(id); o != nil {
			if len(o.Capabilities) > 0 {
				add(o.Capabilities...)
			} else {
				add(o.Blurb)
			}
		}
	}
	return out
}

// List is consumer's tab: one entry per app it can connect to or is
// connected to, its credentials each asked of their provider in parallel.
func (s *Service) List(ctx context.Context, consumer *store.App) (*List, error) {
	spec, err := apps.LatestSpec(s.Store, consumer.ID)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.ListIntegrations(consumer.ID)
	if err != nil {
		return nil, err
	}
	all, err := apps.WithSpecs(s.Store)
	if err != nil {
		return nil, err
	}
	specs := map[string]*apps.AppSpec{}
	byName := map[string]store.App{}
	// An app held for review is no one's provider until it is confirmed.
	held, _ := s.Store.HeldApps()
	for _, a := range all {
		if held[a.Name] {
			continue
		}
		byName[a.Name] = a.App
		if a.Spec != nil {
			specs[a.Name] = a.Spec
		}
	}
	recorded, err := s.Store.AllAppIntegrations()
	if err != nil {
		return nil, err
	}

	// The providers, in the order the consumer's wants name them, then any
	// it is connected to without wanting (by hand, or before its wants
	// said so).
	var providers []string
	wants := map[string][]apps.Want{}
	note := func(p string) {
		if p != consumer.Name && !slices.Contains(providers, p) {
			providers = append(providers, p)
		}
	}
	for _, w := range apps.EffectiveWants(spec, apps.ParseDeclared(recorded[consumer.ID])) {
		if w.Provider() == consumer.Name {
			continue
		}
		note(w.Provider())
		wants[w.Provider()] = append(wants[w.Provider()], w)
	}
	byProvider := map[string][]store.Integration{}
	var extra []string
	for _, in := range rows {
		byProvider[in.Provider] = append(byProvider[in.Provider], in)
		if !slices.Contains(providers, in.Provider) && !slices.Contains(extra, in.Provider) {
			extra = append(extra, in.Provider)
		}
	}
	for _, u := range spec.Uses {
		if usesApp(spec, u) && u != consumer.Name && !slices.Contains(providers, u) && !slices.Contains(extra, u) {
			extra = append(extra, u)
		}
	}
	sort.Strings(extra)
	for _, p := range extra {
		note(p)
	}

	entries := make([]Entry, len(providers))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, name := range providers {
		prov, installed := byName[name]
		provSpec := specs[name]
		var conn *apps.Connection
		if provSpec != nil {
			conn = apps.EffectiveConnection(provSpec, apps.ParseDeclared(recorded[prov.ID]))
		}
		mine := byProvider[name]
		e := Entry{Provider: name, Installed: installed, State: StateAvailable,
			Capabilities: capabilities(wants[name], mine, conn)}
		if len(mine) > 0 || usesApp(spec, name) {
			e.State = StateConnected
		}
		var expired bool
		for _, in := range mine {
			if in.State == store.IntegrationPending {
				e.State = StatePending
				expired = expired || consentExpired(in)
			}
			if e.ConnectedAt == "" || in.CreatedAt < e.ConnectedAt {
				e.ConnectedAt, e.ConnectedBy = in.CreatedAt, in.CreatedBy
			}
		}
		p := planFor(wants[name], mine, spec, provSpec, conn, name)
		for _, o := range p.offers {
			e.Consent = e.Consent || o.Consent
			if replaces(spec, wants[name], o, conn.ConnectionURL(name)) {
				e.Replaces = true
			}
		}
		e.Incomplete = e.State == StateConnected && !p.empty()
		switch {
		case expired:
			e.Problem = "Nobody approved this in " + name + " in time — cancel it and connect again"
		case e.State == StatePending:
			e.Problem = "Waiting for someone to approve this in " + name
		case !installed:
			if e.State == StateConnected {
				e.Problem = name + " is no longer installed"
			} else {
				e.Problem = "Install " + name + ", then connect it"
			}
		case e.State == StateAvailable && (len(p.missing) > 0 || p.noConn):
			e.Problem = name + " can't connect to " + consumer.Name + " yet — update " + name + ", then connect"
		case !Running(&prov):
			e.Problem = name + " is not running"
		}
		if e.Problem == "" {
			for _, in := range mine {
				if conn.Offer(in.Offer) == nil {
					e.Problem = name + " no longer offers part of this — disconnect it"
				}
			}
		}
		entries[i] = e
		if e.Problem != "" || e.State != StateConnected {
			continue
		}
		for _, in := range mine {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st, err := s.status(ctx, &prov, in.RemoteID)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					entries[i].Problem = err.Error()
				case !st.Active:
					entries[i].Problem = name + " has revoked " + consumer.Name + "'s access — disconnect and connect again"
				case st.LastActivityAt > entries[i].LastActivityAt:
					entries[i].LastActivityAt = st.LastActivityAt
				}
			}()
		}
	}
	wg.Wait()

	out := &List{Connected: []Entry{}, Available: []Entry{}}
	for _, e := range entries {
		if e.State == StateAvailable {
			out.Available = append(out.Available, e)
		} else {
			out.Connected = append(out.Connected, e)
		}
	}
	return out, nil
}

// replaces is whether connecting offer would overwrite what the operator
// set by hand in spec: a different value, or a secret of the same name.
// The same value written again replaces nothing.
func replaces(spec *apps.AppSpec, wants []apps.Want, offer *apps.Offer, url string) bool {
	want := findWant(wants, offer.ID)
	for k, v := range offer.Env {
		if have, set := spec.Env[want.Name(k)]; set && have != strings.ReplaceAll(v, apps.URLTemplate, url) {
			return true
		}
	}
	for _, k := range offer.Secrets {
		if slices.Contains(spec.Secrets, want.Name(k)) {
			return true
		}
	}
	return false
}

func findWant(wants []apps.Want, offer string) *apps.Want {
	for i := range wants {
		if wants[i].OfferID() == offer {
			return &wants[i]
		}
	}
	return nil
}

// ConnectRequest is what the operator chose on the tab.
type ConnectRequest struct {
	Provider string `json:"provider"`
	Deploy   bool   `json:"deploy"`
}

// Connect sets up everything the consumer wants of the provider that isn't
// yet: each offer is minted for it alone, and an app want grants it the
// provider's connection. All but a consent offer land in one revision. A
// consent offer answers the provider's page to send the person to
// (returnBase is fog's own address, for the way back); it waits, pending,
// until they come back (Return).
func (s *Service) Connect(ctx context.Context, consumer *store.App, req ConnectRequest, returnBase, actor string) (string, error) {
	if s.Specs == nil {
		return "", fmt.Errorf("app engine unavailable")
	}
	provider := req.Provider
	if provider == consumer.Name {
		return "", refuse(ErrConflict, "%s can't connect to itself", provider)
	}
	prov, err := s.Store.AppByName(provider)
	if errors.Is(err, store.ErrNotFound) {
		return "", refuse(ErrNotFound, "%s is not installed", provider)
	}
	if err != nil {
		return "", err
	}
	if held, err := s.Store.AppReviewHold(prov.ID); err != nil {
		return "", err
	} else if held {
		return "", refuse(ErrNotFound, "%s is waiting for its import to be reviewed", provider)
	}
	spec, err := apps.LatestSpec(s.Store, consumer.ID)
	if err != nil {
		return "", err
	}
	wants := s.wantsOf(consumer, spec, provider)
	if len(wants) == 0 {
		return "", refuse(ErrNotFound, "%s doesn't connect to %s", consumer.Name, provider)
	}
	provSpec, err := apps.LatestSpec(s.Store, prov.ID)
	if err != nil {
		return "", err
	}
	conn := s.connectionOf(prov, provSpec)
	rows, err := s.Store.ListIntegrations(consumer.ID)
	if err != nil {
		return "", err
	}
	var mine []store.Integration
	for _, in := range rows {
		if in.Provider != provider {
			continue
		}
		if in.State == store.IntegrationPending {
			if !consentExpired(in) {
				return "", refuse(ErrConflict, "%s is waiting for someone to approve it in %s — cancel it to start again", consumer.Name, provider)
			}
			// A request nobody answered in time: forget it and ask again.
			s.revoke(ctx, prov, in.RemoteID)
			if err := s.Store.DeleteIntegration(in.ID); err != nil {
				return "", err
			}
			continue
		}
		mine = append(mine, in)
	}
	p := planFor(wants, mine, spec, provSpec, conn, provider)
	switch {
	case len(p.missing) > 0 || p.noConn:
		return "", refuse(ErrNotFound, "%s can't connect to %s yet — update %s, then connect", provider, consumer.Name, provider)
	case p.empty():
		return "", refuse(ErrConflict, "%s is already connected to %s", consumer.Name, provider)
	case !Running(prov):
		return "", refuse(ErrConflict, "%s is not running — start it, then connect", provider)
	}

	var plain []*apps.Offer
	var consent *apps.Offer
	for _, o := range p.offers {
		if o.Consent && consent == nil {
			consent = o
		} else if !o.Consent {
			plain = append(plain, o)
		}
	}

	// The consent offer is asked first: refused, nothing else is done.
	var pending *store.Integration
	var consentURL string
	if consent != nil {
		pending, consentURL, err = s.ask(ctx, consumer, prov, spec, wants, consent, returnBase, req.Deploy, actor)
		if err != nil {
			return "", err
		}
	}
	undoAsk := func() {
		if pending != nil {
			s.revoke(ctx, prov, pending.RemoteID)
			if err := s.Store.DeleteIntegration(pending.ID); err != nil {
				s.Log.Warn("integration: forget", "app", consumer.Name, "err", err)
			}
		}
	}
	if len(plain) == 0 && !p.useApp {
		return consentURL, nil
	}

	minted := map[string]mint{}
	revokeAll := func() {
		for _, m := range minted {
			s.revoke(ctx, prov, m.id)
		}
	}
	for _, o := range plain {
		m, err := s.mint(ctx, consumer, prov, o, nil)
		if err != nil {
			revokeAll()
			undoAsk()
			return "", err
		}
		minted[o.ID] = m
	}
	names, err := s.apply(ctx, consumer, provider, conn, plain, minted, p.useApp, actor)
	if err != nil {
		revokeAll()
		undoAsk()
		return "", err
	}
	for _, o := range plain {
		in := &store.Integration{AppID: consumer.ID, Provider: provider, Offer: o.ID, RemoteID: minted[o.ID].id,
			Env: names[o.ID].env, Secrets: names[o.ID].secrets, CreatedBy: actor}
		if err := s.Store.CreateIntegration(in); err != nil {
			revokeAll()
			undoAsk()
			return "", err
		}
	}
	s.audit(actor, "app.integration.connect", consumer.Name, map[string]any{"provider": provider, "offers": offerIDs(plain), "uses": p.useApp})
	return consentURL, nil
}

func offerIDs(offers []*apps.Offer) []string {
	out := []string{}
	for _, o := range offers {
		out = append(out, o.ID)
	}
	return out
}

// mint is what a provider answered a mint with.
type mint struct {
	id      string
	secrets map[string]string
	consent string
}

// mint asks the provider for one offer's credential for consumer; extra
// goes in the ask (a consent offer's return_to).
func (s *Service) mint(ctx context.Context, consumer, prov *store.App, offer *apps.Offer, extra map[string]string) (mint, error) {
	ask := map[string]string{"consumer": consumer.Name, "offer": offer.ID}
	for k, v := range extra {
		ask[k] = v
	}
	raw, err := s.call(ctx, prov, http.MethodPost, Path, ask)
	if errors.Is(err, appcall.ErrUnreachable) {
		s.Log.Warn("integration: reach provider", "provider", prov.Name, "err", err)
		return mint{}, refuse(ErrProvider, "Can't reach %s — check that it's running and healthy, then try again", prov.Name)
	}
	if err != nil {
		return mint{}, refuse(ErrProvider, "%s refused: %v", prov.Name, err)
	}
	var minted struct {
		ID      string            `json:"id"`
		Secrets map[string]string `json:"secrets"`
		Consent string            `json:"consent"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil || minted.ID == "" {
		return mint{}, refuse(ErrProvider, "%s answered without an integration id", prov.Name)
	}
	return mint{id: minted.ID, secrets: minted.Secrets, consent: minted.Consent}, nil
}

// ask starts a consent offer: the provider mints a request and names the
// page a person approves it on, and fog records it pending.
func (s *Service) ask(ctx context.Context, consumer, prov *store.App, spec *apps.AppSpec, wants []apps.Want,
	offer *apps.Offer, returnBase string, deploy bool, actor string) (*store.Integration, string, error) {
	state, stateDigest := auth.NewToken()
	m, err := s.mint(ctx, consumer, prov, offer, map[string]string{
		"return_to": returnBase + ReturnPath + "?state=" + url.QueryEscape(state)})
	if err != nil {
		return nil, "", err
	}
	public := s.PublicURL(prov)
	switch {
	case !strings.HasPrefix(m.consent, "/"):
		s.revoke(ctx, prov, m.id)
		return nil, "", refuse(ErrProvider, "%s answered without a page to approve this on", prov.Name)
	case public == "":
		s.revoke(ctx, prov, m.id)
		return nil, "", refuse(ErrConflict, "%s has no address under your domain, so nobody can approve this there — set one under Settings › Network › Domain & HTTPS", prov.Name)
	}
	env, secrets := offerNames(findWant(wants, offer.ID), offer)
	in := &store.Integration{AppID: consumer.ID, Provider: prov.Name, Offer: offer.ID, RemoteID: m.id,
		Env: env, Secrets: secrets, CreatedBy: actor, State: store.IntegrationPending,
		ReturnDigest: stateDigest, Deploy: deploy}
	if err := s.Store.CreateIntegration(in); err != nil {
		s.revoke(ctx, prov, m.id)
		return nil, "", err
	}
	s.audit(actor, "app.integration.ask", consumer.Name, map[string]any{"provider": prov.Name, "offer": offer.ID})
	return in, public + m.consent, nil
}

// names are the env and secret names one offer set in the consumer.
type names struct{ env, secrets []string }

// apply writes what the provider minted for each offer, and the offers'
// env, into the consumer's spec as one revision, under the names it reads;
// useApp also grants it the provider's connection, mounting the folders
// the provider shares.
func (s *Service) apply(ctx context.Context, consumer *store.App, provider string, conn *apps.Connection,
	offers []*apps.Offer, minted map[string]mint, useApp bool, actor string) (map[string]names, error) {
	for _, o := range offers {
		for _, k := range o.Secrets {
			if minted[o.ID].secrets[k] == "" {
				return nil, refuse(ErrProvider, "%s answered without %s", provider, k)
			}
		}
	}
	spec, err := apps.LatestSpec(s.Store, consumer.ID)
	if err != nil {
		return nil, err
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	wants := s.wantsOf(consumer, spec, provider)
	url := conn.ConnectionURL(provider)
	secrets := map[string]string{}
	out := map[string]names{}
	// Every name the provider's offer lands on, checked again here as it is
	// written: an offer recorded before a name was refused must not set
	// fog's own secrets (_fog_api_token is the consumer's FOG_API_TOKEN) or
	// what the consumer's runtime loads.
	for _, o := range offers {
		want := findWant(wants, o.ID)
		for _, k := range append(sortedKeys(o.Env), o.Secrets...) {
			if err := apps.CheckOfferedName(want.Name(k)); err != nil {
				return nil, refuse(ErrProvider, "%s offers %s: %v", provider, want.Name(k), err)
			}
		}
	}
	for _, o := range offers {
		want := findWant(wants, o.ID)
		for _, k := range sortedKeys(o.Env) {
			spec.Env[want.Name(k)] = strings.ReplaceAll(o.Env[k], apps.URLTemplate, url)
		}
		for _, k := range o.Secrets {
			as := want.Name(k)
			secrets[as] = minted[o.ID].secrets[k]
			if !slices.Contains(spec.Secrets, as) {
				spec.Secrets = append(spec.Secrets, as)
			}
		}
		env, sec := offerNames(want, o)
		out[o.ID] = names{env, sec}
	}
	if useApp && !spec.UsesGrant(provider) {
		spec.Uses = append(spec.Uses, provider)
		if s.Wire != nil {
			s.Wire(consumer.Name, spec)
		}
	}
	// Connecting is the operator's approval of the consumer reaching the
	// provider on the network (apps.AppSpec.Reach).
	if !slices.Contains(spec.Reach, provider) {
		spec.Reach = append(spec.Reach, provider)
	}
	if err := s.Specs.UpdateSpec(ctx, consumer.Name, spec, secrets, "integration: connect "+provider, actor); err != nil {
		return nil, err
	}
	return out, nil
}

// offerNames are the env and secret names an offer sets in an app that
// wants it as want says.
func offerNames(want *apps.Want, offer *apps.Offer) ([]string, []string) {
	env := []string{}
	for _, k := range sortedKeys(offer.Env) {
		env = append(env, want.Name(k))
	}
	secrets := []string{}
	for _, k := range offer.Secrets {
		secrets = append(secrets, want.Name(k))
	}
	return env, secrets
}

func consentExpired(in store.Integration) bool {
	if in.State != store.IntegrationPending {
		return false
	}
	at, err := time.Parse(time.RFC3339, in.CreatedAt)
	return err == nil && time.Since(at) > consentTTL
}

// Returned is how a consent round trip ended.
type Returned struct {
	// Consumer is the app whose tab the person lands on; nil when the state
	// named no pending connection (unknown, or already spent).
	Consumer *store.App
	// Outcome is the tab's query: connected=, declined= or error=.
	Outcome string
	// Connected: the credential is set, and Deploy is whether the operator
	// asked for a redeploy when they connected.
	Connected bool
	Deploy    bool
}

// Return finishes a consent offer when the provider sends the person back:
// state names the pending connection, approved is their answer. Approved,
// fog claims the credential once and applies it; otherwise, or when that
// fails, the request is revoked and forgotten.
func (s *Service) Return(ctx context.Context, state string, approved bool, actor string) Returned {
	in, err := s.Store.IntegrationByReturnDigest(auth.DigestToken(state))
	if err != nil {
		return Returned{}
	}
	consumer, err := s.Store.AppByID(in.AppID)
	if err != nil {
		return Returned{}
	}
	prov, provErr := s.Store.AppByName(in.Provider)
	fail := func(outcome string) Returned {
		if provErr == nil && Running(prov) {
			s.revoke(ctx, prov, in.RemoteID)
		}
		if err := s.Store.DeleteIntegration(in.ID); err != nil {
			s.Log.Warn("integration: forget", "app", consumer.Name, "err", err)
		}
		return Returned{Consumer: consumer, Outcome: outcome}
	}
	if !approved {
		s.audit(actor, "app.integration.declined", consumer.Name, map[string]any{"provider": in.Provider, "offer": in.Offer})
		return fail("declined=" + url.QueryEscape(in.Provider))
	}
	failed := func(err error) Returned {
		s.Log.Warn("integration: claim", "app", consumer.Name, "provider", in.Provider, "err", err)
		return fail("error=" + url.QueryEscape(err.Error()))
	}
	if provErr != nil {
		return failed(fmt.Errorf("%s is no longer installed", in.Provider))
	}
	if s.Specs == nil {
		return failed(fmt.Errorf("app engine unavailable"))
	}
	provSpec, err := apps.LatestSpec(s.Store, prov.ID)
	if err != nil {
		return failed(err)
	}
	conn := s.connectionOf(prov, provSpec)
	offer := conn.Offer(in.Offer)
	if offer == nil {
		return failed(fmt.Errorf("%s no longer offers what %s asked for", in.Provider, consumer.Name))
	}
	raw, err := s.call(ctx, prov, http.MethodPost, Path+"/"+in.RemoteID+"/claim", []byte("{}"))
	if errors.Is(err, appcall.ErrUnreachable) {
		return failed(fmt.Errorf("can't reach %s to finish connecting", in.Provider))
	}
	if err != nil {
		return failed(fmt.Errorf("%s wouldn't finish connecting: %v", in.Provider, err))
	}
	var claimed struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &claimed); err != nil {
		return failed(fmt.Errorf("%s answered something unreadable", in.Provider))
	}
	set, err := s.apply(ctx, consumer, in.Provider, conn, []*apps.Offer{offer},
		map[string]mint{offer.ID: {id: in.RemoteID, secrets: claimed.Secrets}}, false, actor)
	if err != nil {
		return failed(err)
	}
	if err := s.Store.MarkIntegrationConnected(in.ID, set[offer.ID].env, set[offer.ID].secrets); err != nil {
		return failed(err)
	}
	s.audit(actor, "app.integration.connect", consumer.Name, map[string]any{"provider": in.Provider, "offers": []string{in.Offer}})
	return Returned{Consumer: consumer, Outcome: "connected=" + url.QueryEscape(in.Provider), Connected: true, Deploy: in.Deploy}
}

// Disconnect undoes everything connecting consumer to provider did: each
// credential is revoked at the provider, what they set comes out of the
// consumer's spec, and so does the provider's connection. A request still
// waiting for approval is cancelled. A provider that cannot revoke stops
// it unless force — the operator saying to go on and revoke it by hand
// there. Answers whether the spec changed, so the consumer needs a
// redeploy.
func (s *Service) Disconnect(ctx context.Context, consumer *store.App, provider string, force bool, actor string) (bool, error) {
	if s.Specs == nil {
		return false, fmt.Errorf("app engine unavailable")
	}
	rows, err := s.Store.ListIntegrations(consumer.ID)
	if err != nil {
		return false, err
	}
	spec, err := apps.LatestSpec(s.Store, consumer.ID)
	if err != nil {
		return false, err
	}
	var connected []store.Integration
	var pending []store.Integration
	for _, in := range rows {
		switch {
		case in.Provider != provider:
		case in.State == store.IntegrationPending:
			pending = append(pending, in)
		default:
			connected = append(connected, in)
		}
	}
	uses := usesApp(spec, provider)
	if len(connected) == 0 && len(pending) == 0 && !uses {
		return false, refuse(ErrNotFound, "%s isn't connected to %s", consumer.Name, provider)
	}

	prov, err := s.Store.AppByName(provider)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Gone with its credentials: nothing to revoke.
	case err != nil:
		return false, err
	case len(connected) == 0:
	case !Running(prov) && !force:
		return false, refuse(ErrConflict, "%s is not running, so it can't revoke %s's access — start it, or disconnect anyway", provider, consumer.Name)
	case Running(prov):
		for _, in := range connected {
			if err := s.revokeAt(ctx, prov, in.RemoteID); err != nil && !force {
				return false, refuse(ErrProvider, "%s couldn't revoke %s's access (%v) — try again, or disconnect anyway", provider, consumer.Name, err)
			}
		}
	}
	// Nothing was set for a request still waiting: cancel it, best-effort.
	for _, in := range pending {
		if prov != nil && Running(prov) {
			s.revoke(ctx, prov, in.RemoteID)
		}
		if err := s.Store.DeleteIntegration(in.ID); err != nil {
			return false, err
		}
	}
	if len(connected) == 0 && !uses {
		return false, nil
	}

	secrets := map[string]string{}
	for _, in := range connected {
		for _, k := range in.Env {
			delete(spec.Env, k)
		}
		spec.Secrets = slices.DeleteFunc(spec.Secrets, func(k string) bool { return slices.Contains(in.Secrets, k) })
		for _, k := range in.Secrets {
			secrets[k] = "" // empty deletes the vault value
		}
	}
	spec.Uses = slices.DeleteFunc(spec.Uses, func(u string) bool { return u == provider })
	spec.Reach = slices.DeleteFunc(spec.Reach, func(u string) bool { return u == provider })
	if err := s.Specs.UpdateSpec(ctx, consumer.Name, spec, secrets, "integration: disconnect "+provider, actor); err != nil {
		return false, err
	}
	for _, in := range connected {
		if err := s.Store.DeleteIntegration(in.ID); err != nil {
			return false, err
		}
	}
	s.audit(actor, "app.integration.disconnect", consumer.Name, map[string]any{"provider": provider, "forced": force})
	return true, nil
}

// RevokeAll runs after an app is deleted: every credential minted for it
// is revoked at its provider, best-effort — the app and its env are gone
// either way.
func (s *Service) RevokeAll(ctx context.Context, list []store.Integration) {
	for _, in := range list {
		if ctx.Err() != nil {
			return
		}
		prov, err := s.Store.AppByName(in.Provider)
		if err == nil && Running(prov) {
			s.revoke(ctx, prov, in.RemoteID)
		}
	}
}

// revoke is revokeAt, best-effort: fifteen seconds at most, and less when
// ctx (the request, or Shutdown) ends first.
func (s *Service) revoke(ctx context.Context, prov *store.App, remoteID string) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := s.revokeAt(ctx, prov, remoteID); err != nil {
		s.Log.Warn("integration: revoke", "provider", prov.Name, "err", err)
	}
}

// revokeAt asks the provider to revoke; one it no longer knows (404) is
// already revoked.
func (s *Service) revokeAt(ctx context.Context, prov *store.App, remoteID string) error {
	_, err := s.call(ctx, prov, http.MethodDelete, Path+"/"+remoteID, nil)
	if errors.Is(err, errGone) {
		return nil
	}
	return err
}

// status asks the provider how one credential is doing.
func (s *Service) status(ctx context.Context, prov *store.App, remoteID string) (*Status, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	raw, err := s.call(ctx, prov, http.MethodGet, Path+"/"+remoteID, nil)
	if errors.Is(err, appcall.ErrUnreachable) {
		s.Log.Warn("integration: reach provider", "provider", prov.Name, "err", err)
		return nil, fmt.Errorf("Can't reach %s to ask how this is going", prov.Name)
	}
	if errors.Is(err, errGone) {
		return &Status{Active: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s didn't say how this is going (%v)", prov.Name, err)
	}
	var st struct {
		Active         bool   `json:"active"`
		LastActivityAt string `json:"last_activity_at"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%s answered something unreadable", prov.Name)
	}
	return &Status{Active: st.Active, LastActivityAt: st.LastActivityAt}, nil
}

// call is one fog-integration/1 call, fifteen seconds at most.
func (s *Service) call(ctx context.Context, prov *store.App, method, path string, body any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return s.Apps.Call(ctx, prov, method, path, body)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
