// Package server wires fogd's HTTP surface: the JSON API under /api and the
// embedded SPA for everything else. Stdlib routing (Go 1.22 patterns) — no
// framework needed at this size.
package server

import (
	"bufio"
	"context"
	"fmt"
	"gitea.lan/matthew/fog/internal/appevents"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/compute"
	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/dnssync"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/feedback"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/flare"
	"gitea.lan/matthew/fog/internal/integrations"
	"gitea.lan/matthew/fog/internal/issues"
	"gitea.lan/matthew/fog/internal/janitor"
	"gitea.lan/matthew/fog/internal/logring"
	"gitea.lan/matthew/fog/internal/lucid"
	"gitea.lan/matthew/fog/internal/mcpserver"
	"gitea.lan/matthew/fog/internal/metrics"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/selfupdate"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/signing"
	"gitea.lan/matthew/fog/internal/spa"
	"gitea.lan/matthew/fog/internal/sshaccess"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/ttlmap"
	"gitea.lan/matthew/fog/internal/tunnel"
	"gitea.lan/matthew/fog/internal/ui"
	"gitea.lan/matthew/fog/internal/updates"
	"gitea.lan/matthew/fog/internal/vault"
	"gitea.lan/matthew/fog/internal/vpn"
)

type Server struct {
	cfg       config.Config
	store     *store.Store
	vault     *vault.Vault
	docker    *dockerx.Client
	installer *dockerx.Installer
	engine    *apps.Engine
	proxy     *proxy.Proxy
	backup    *backup.Manager
	updates   *updates.Checker
	updater   *selfupdate.Updater
	tunnel    *tunnel.Manager
	vpn       *vpn.Manager
	sshAccess *sshaccess.Manager
	dns       *dnssync.Keeper
	dnsOnce   sync.Once
	janitor   *janitor.Janitor
	nas       *nas.Manager
	metrics   *metrics.Sampler
	// diskSizer caches the directory walks behind the Storage page's
	// usage breakdown (disksize.go).
	diskSizer diskSizer
	fed       *fed.Service
	issues    *issues.Scanner
	// compute is the machines that lend fog their hardware.
	compute *compute.Service
	// files is fog's own filesystem root (the Drive page); filesFed is
	// Files across peers.
	files    *files.Manager
	filesFed *filesfed.Service
	// offsite is the locker in the cloud: targets, sync, restore doors.
	offsite *offsite.Manager
	// pgUpgrader moves the shared Postgres to a newer major; nil (tests)
	// makes the Postgres surface report itself as not wired.
	pgUpgrader *pg.Upgrader
	limiter    *auth.RateLimiter
	// signinLimitsState is sign-in's per-account and per-address buckets
	// (signin_handlers_limits.go), built on first use.
	signinLimitsState signinLimiters
	// ceremonies holds in-flight WebAuthn challenges for people (passkeys);
	// deviceNonces the possession challenges for binding a device session.
	ceremonies   *people.Ceremonies
	deviceNonces *people.Nonces
	// chat is the sealed room server (internal/chat).
	chat *chat.Service
	// Flare: event processing and per-app ingest rate limiting.
	flareProc    *flare.Processor
	flareLimiter *auth.RateLimiter
	// Feedback: the forwarder that sends reports to peers, fogd's own recent
	// log tail for their dossiers, and the phone push a peer's report earns.
	feedbackFwd *feedback.Forwarder
	logRing     *logring.Ring
	pushNotify  func(title, msg string, ok bool)
	// /app/v1 per-app rate limiting.
	appAPILimiter *auth.RateLimiter
	events        *appevents.Bus
	// keys signs what fog vouches for: gate assertions, ID tokens.
	keys *signing.Keyring
	// signin registers apps as OIDC clients (internal/signin); logouts
	// delivers back-channel logout tokens from the outbox.
	signin  *signin.Registry
	logouts *signin.Deliverer
	// scim keeps apps that take provisioning in step (internal/signin).
	scim *signin.Provisioner
	// gateHosts maps a gated hostname to its app (gate_handlers.go).
	gateHosts ttlCache[map[string]int64]
	// gateSchemeCache is the scheme the gate signs with (gate_handlers.go).
	gateSchemeCache ttlCache[string]
	// chatLimiter bounds each person's chat API calls; hookLimiter bounds
	// each incoming webhook token.
	chatLimiter *auth.RateLimiter
	hookLimiter *auth.RateLimiter
	// MCP: the tool server mounted at /mcp. Its rate limit, like WebDAV's
	// and share links', is a stranger budget (signin_handlers_limits.go).
	mcpHandler http.Handler
	// Connect: replayed signed requests get cached answers, and tests
	// override how a local app's contract endpoints are reached.
	connectNonces      *ttlmap.Map[string, []byte]
	appGrantEndpointFn func(context.Context, *store.App) (string, string, error)
	// rebootFn and shutdownFn actually power the host; tests override them
	// so a handler test can't restart or halt the machine running the suite.
	rebootFn   func() error
	shutdownFn func() error
	log        *slog.Logger
	startedAt  time.Time

	// Background work handlers start (goSafe): its parent context, and what
	// Shutdown waits on. bgClosed refuses new tasks once shutdown began.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
	bgMu     sync.Mutex
	bgClosed bool

	// Notices to apps: federation directory changes (peers_notify.go) and
	// Files changes (files_notify.go).
	dirNotify   dirNotifier
	filesNotify filesNotifier

	// Cached answers for the polled parts of /api/system (status_cache.go).
	dockerCache   dockerStatusCache
	warningsCache ttlCache[[]string]
	// The home view's app statuses, polled by every open start page
	// (app_home_handlers.go).
	homeStatuses ttlCache[homeStatusesResult]
}

type Deps struct {
	Vault   *vault.Vault
	Docker  *dockerx.Client
	Engine  *apps.Engine
	Proxy   *proxy.Proxy
	Backup  *backup.Manager
	Updates *updates.Checker
	Updater *selfupdate.Updater
	Tunnel  *tunnel.Manager
	VPN     *vpn.Manager
	// DNS keeps *.<domain> pointed where the operator chose; nil (tests)
	// gets a keeper over the same store and vault with no loop running.
	DNS     *dnssync.Keeper
	Janitor *janitor.Janitor
	Nas     *nas.Manager
	Metrics *metrics.Sampler
	// Flare is the fog-to-fog forwarder; nil (tests, forwarding off) means
	// locally ingested events simply aren't relayed.
	Flare *flare.Forwarder
	// Feedback sends reports to peers.
	Feedback *feedback.Forwarder
	// LogRing is fogd's recent log tail, attached to feedback dossiers; nil
	// means reports say the logs were not captured.
	LogRing *logring.Ring
	// Fed is the federation plane (main passes the one whose heartbeat loop
	// is running).
	Fed *fed.Service
	// Issues is the configuration scanner; nil (tests) serves the stored
	// issues read-only and refuses manual scans.
	Issues *issues.Scanner
	// PGUpgrader is the managed Postgres major upgrade; nil (tests) makes
	// the Postgres settings surface report itself as not wired.
	PGUpgrader *pg.Upgrader
	// Files is the files root manager; FilesFed is Files across peers.
	Files    *files.Manager
	FilesFed *filesfed.Service
	// Chat is the sealed room server (NewChatService). main builds it
	// itself so it can start the fog bot alongside.
	Chat *chat.Service
	// Offsite is the locker manager (main passes the one whose sync loop
	// is running).
	Offsite *offsite.Manager
	// Events is the household event stream: the one fogd's own
	// notifications publish into, so long-polling readers wake for those
	// too.
	Events *appevents.Bus
	// Keys signs gate assertions and ID tokens (internal/signing).
	Keys *signing.Keyring
	// Compute is the machines that lend fog their hardware; nil (tests)
	// makes /compute/v1 answer that it isn't available.
	Compute *compute.Service
}

// Hooks are the server's ends of the callbacks other components make. New
// installs none of them; main does (cmd/fogd/wire.go), so every link
// between fogd's components is made in one place.
type Hooks struct {
	// FeedbackFor and OnFeedback are fed.Service's: what became of the
	// reports a peer relayed here (feedback_handlers.go).
	FeedbackFor func(peer *store.FedPeer) []store.FeedbackStatus
	OnFeedback  func(peerID int64, statuses []store.FeedbackStatus)
	// FilesChanged is filesfed.Service's AppNotify: a folder's change
	// notice to the apps that mount it (files_notify.go).
	FilesChanged func(rel, reason string)
	// SpecChanged is apps.Engine's OnSpecChanged: apps that follow or use
	// another app's folders hear when its spec changes (wiring.go). It runs
	// off the request, since it may revise other apps.
	SpecChanged func(ctx context.Context, name string)
	// DisplayName is chat.Notifier's NameFor: how a push names a sender.
	DisplayName func(addr string) string
}

// Hooks returns the server's hooks, for main to install.
func (s *Server) Hooks() Hooks {
	return Hooks{
		FeedbackFor:  s.feedbackStatusesFor,
		OnFeedback:   s.applyPeerFeedbackStatuses,
		FilesChanged: s.notifyAppsFilesChanged,
		SpecChanged: func(_ context.Context, name string) {
			s.goSafe("wiring", func() { s.onSpecChanged(s.bgContext(), name) })
		},
		DisplayName: func(addr string) string {
			name, _ := s.displayFor(addr)
			return name
		},
	}
}

// New builds the server over deps as given: it builds no component of its
// own that main also builds, and changes none it was handed — main wires
// them together, installing Hooks for the server's side.
func New(cfg config.Config, st *store.Store, deps Deps, log *slog.Logger) *Server {
	var flareForward func(string, []byte)
	if deps.Flare != nil {
		flareForward = deps.Flare.Enqueue
	}
	srv := &Server{
		keys:          deps.Keys,
		signin:        &signin.Registry{Store: st, Vault: deps.Vault},
		events:        deps.Events,
		cfg:           cfg,
		store:         st,
		vault:         deps.Vault,
		docker:        deps.Docker,
		installer:     dockerx.NewInstaller(),
		engine:        deps.Engine,
		proxy:         deps.Proxy,
		backup:        deps.Backup,
		updates:       deps.Updates,
		updater:       deps.Updater,
		tunnel:        deps.Tunnel,
		vpn:           deps.VPN,
		sshAccess:     sshaccess.New(cfg.DataDir),
		dns:           deps.DNS,
		janitor:       deps.Janitor,
		nas:           deps.Nas,
		metrics:       deps.Metrics,
		fed:           deps.Fed,
		issues:        deps.Issues,
		files:         deps.Files,
		filesFed:      deps.FilesFed,
		offsite:       deps.Offsite,
		pgUpgrader:    deps.PGUpgrader,
		compute:       deps.Compute,
		limiter:       auth.NewRateLimiter(10, 15*time.Minute),
		ceremonies:    people.NewCeremonies(),
		deviceNonces:  people.NewNonces(),
		connectNonces: ttlmap.New[string, []byte](connectNonceTTL, 0),
		flareProc:     &flare.Processor{Store: st, Log: log, Forward: flareForward},
		feedbackFwd:   deps.Feedback,
		logRing:       deps.LogRing,
		// 120 envelopes per app-minute: far above anything a healthy fleet
		// sends, low enough that a crash loop can't monopolize the store.
		flareLimiter:  auth.NewRateLimiter(120, time.Minute),
		appAPILimiter: auth.NewRateLimiter(120, time.Minute),
		chatLimiter:   auth.NewRateLimiter(600, time.Minute),
		hookLimiter:   auth.NewRateLimiter(60, time.Minute),
		rebootFn:      rebootHost,
		shutdownFn:    shutdownHost,
		log:           log,
		startedAt:     time.Now(),
	}
	srv.bgCtx, srv.bgCancel = context.WithCancel(context.Background())
	srv.diskSizer.docker = srv.dockerDiskUsage
	srv.flareProc.Notify = srv.notify
	srv.pushNotify = srv.notify
	srv.chat = deps.Chat
	srv.mcpHandler = mcpserver.New(mcpserver.Deps{
		Store:   st,
		Engine:  deps.Engine,
		Docker:  deps.Docker,
		Issues:  deps.Issues,
		Updates: deps.Updates,
		Metrics: deps.Metrics,
		Files:   deps.Files,
		Chat:    srv.chat,
		Log:     log,
	})
	srv.scim = &signin.Provisioner{Store: st, Vault: deps.Vault, Log: log,
		Instance: srv.fed.RequireInstanceID,
	}
	srv.logouts = &signin.Deliverer{
		Store: st, Keys: srv.keys, Log: log,
		Issuer:   srv.issuer,
		Instance: srv.fed.RequireInstanceID,
		Notice:   st.CreateNotice,
		Visit:    srv.sendVisitLogout,
	}
	return srv
}

// keeper is the DNS keeper, built lazily over the server's own store and
// vault when main didn't pass one (tests).
func (s *Server) keeper() *dnssync.Keeper {
	s.dnsOnce.Do(func() {
		if s.dns == nil {
			s.dns = &dnssync.Keeper{Store: s.store, Vault: s.vault, Log: s.log}
		}
	})
	return s.dns
}

// notify tells the operator: through the engine's notifier when one is
// wired (tests construct servers without an engine; ingest still works,
// the phone just stays quiet), and into #fog when the bot is up.
func (s *Server) notify(title, msg string, ok bool) {
	if s.engine != nil && s.engine.Notify != nil {
		s.engine.Notify(title, msg, ok)
	}
	s.notifyFogRoom(title, msg)
}

// notifyFogRoom mirrors an alert into #fog, when the bot is up.
func (s *Server) notifyFogRoom(title, msg string) {
	bot := s.chat.Bot()
	if bot == nil {
		return
	}
	if room, err := bot.FogRoom(); err == nil {
		bot.SendAs(room.ID, "**"+title+"**\n"+msg, "flare")
	}
}

// NewChatService builds the chat service over the shared store and the
// federation identity. main calls it before New so the fog bot can be
// started with the same instance.
func NewChatService(cfg config.Config, st *store.Store, fedSvc *fed.Service, log *slog.Logger) *chat.Service {
	svc := &chat.Service{
		Store: st, Log: log, DataDir: cfg.DataDir, Bus: chat.NewBus(),
		Instance:  fedSvc.RequireInstanceID,
		Household: func() string { return householdNameOf(st) },
	}
	svc.Fed = chat.NewFederation(svc, fedSvc)
	svc.OnAccepted = svc.Fed.OnAccepted
	return svc
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// admin routes are the operator's; person routes any signed-in person's;
	// chat routes a person's too, charged to their chat budget.
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h)) }
	person := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requirePerson(h)) }
	chat := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requirePerson(s.chatLimited(h))) }

	// Unauthenticated: setup + login. Setup endpoints self-guard on whether
	// an admin already exists.
	mux.HandleFunc("GET /api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	// Full-server DR: restore an encrypted recovery kit before setup.
	mux.HandleFunc("POST /api/setup/restore-kit", s.handleRestoreKit)
	mux.HandleFunc("POST /api/setup/restore-offsite", s.handleRestoreOffsite)
	mux.HandleFunc("GET /api/setup/claim", s.handleSetupClaim)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	// Git-push webhooks: token-in-path auth, no session.
	mux.HandleFunc("POST /hooks/{name}/{token}", s.handleWebhook)
	// Chat webhooks: token-in-path, posted by the fog bot into one room.
	mux.HandleFunc("POST /hooks/chat/{token}", s.handleChatHook)
	// Flare error ingest: DSN-key auth, no session. Registered with and
	// without the trailing slash — the Sentry spec says /envelope/ but not
	// every SDK agrees with it.
	mux.HandleFunc("POST /ingest/api/{app}/envelope", s.handleFlareIngest)
	mux.HandleFunc("POST /ingest/api/{app}/envelope/{$}", s.handleFlareIngest)
	// Federation machine plane: signed-request auth, no session. Pair is the
	// bootstrap (invite-secret + self-signed proof of key possession).
	// Compute nodes: each machine's own surface, authenticated by its
	// credential (the join by its one-time code; the installer and the
	// builds it fetches are public).
	mux.HandleFunc("POST /compute/v1/join", s.handleComputeJoin)
	mux.HandleFunc("POST /compute/v1/poll", s.handleComputePoll)
	mux.HandleFunc("GET /compute/v1/install.sh", s.handleComputeInstall)
	mux.HandleFunc("GET /compute/v1/fog-node/{platform}", s.handleComputeBinary)
	// An app's requests to the inference pool, by its FOG_API_TOKEN.
	mux.HandleFunc(compute.InferencePath, s.handleInference)
	// A machine's run reaching the app it works for: its input, its
	// uploads, under the app's /fog/compute/ alone, by the run's token.
	mux.Handle(compute.DataPath, s.compute.DataHandler(func(ctx context.Context, app *store.App) (string, error) {
		ep := s.resolveApps(ctx, []store.App{*app})[0]
		return ep.Base, ep.Err
	}))

	mux.HandleFunc("POST /fed/v1/pair", s.handleFedPair)
	mux.HandleFunc("POST /fed/v1/heartbeat", s.handleFedHeartbeat)
	mux.HandleFunc("POST /fed/v1/flare/envelope", s.handleFedFlareEnvelope)
	mux.HandleFunc("POST /fed/v1/feedback", s.handleFedFeedback)
	mux.HandleFunc("POST /fed/v1/connect", s.handleFedConnect)
	// Visitors (visit_handlers.go): fog-to-fog sign-in over the pairing.
	mux.HandleFunc("POST /fed/v1/signin/par", s.handleFedVisitPAR)
	mux.HandleFunc("POST /fed/v1/signin/token", s.handleFedVisitToken)
	mux.HandleFunc("GET /fed/v1/signin/keys", s.handleFedVisitKeys)
	mux.HandleFunc("POST /fed/v1/signin/logout", s.handleFedVisitLogout)
	// Files across peers (files_fed_handlers.go): catalog, manifest, blobs,
	// thumbnails, nudges — all signed, all behind the files.share grant.
	mux.HandleFunc("GET /fed/v1/files/catalog", s.handleFedFilesCatalog)
	mux.HandleFunc("GET /fed/v1/files/shares/{id}/manifest", s.handleFedFilesManifest)
	mux.HandleFunc("GET /fed/v1/files/shares/{id}/blob/{path...}", s.handleFedFilesBlob)
	mux.HandleFunc("GET /fed/v1/files/shares/{id}/thumb/{path...}", s.handleFedFilesThumb)
	mux.HandleFunc("GET /fed/v1/files/shares/{id}/relay/{path...}", s.handleFedFilesRelay)
	mux.HandleFunc("POST /fed/v1/files/events", s.handleFedFilesEvents)
	mux.HandleFunc("POST /fed/v1/files/shares/{id}/contributions", s.handleFedContributionBegin)
	mux.HandleFunc("PATCH /fed/v1/files/shares/{id}/contributions/{uid}", s.handleFedContributionChunk)
	mux.HandleFunc("POST /fed/v1/files/shares/{id}/contributions/{uid}/complete", s.handleFedContributionComplete)
	mux.HandleFunc("DELETE /fed/v1/files/shares/{id}/contributions/{uid}", s.handleFedContributionAbort)
	mux.HandleFunc("DELETE /fed/v1/files/shares/{id}/contributions", s.handleFedContributionWithdraw)
	mux.HandleFunc("GET /fed/v1/files/contributions/{origin}/{share}/blob/{path...}", s.handleFedContributionBlob)
	// Chat across households (chat_fed_handlers.go): key material, the hub's
	// room plane for followers, and what the hub pushes back.
	mux.HandleFunc("GET /fed/v1/chat/people/{handle}/keys", s.handleFedChatPersonKeys)
	mux.HandleFunc("POST /fed/v1/chat/rooms/{id}/submit", s.handleFedChatSubmit)
	mux.HandleFunc("GET /fed/v1/chat/rooms/{id}", s.handleFedChatRoom)
	mux.HandleFunc("GET /fed/v1/chat/rooms/{id}/events", s.handleFedChatEvents)
	mux.HandleFunc("GET /fed/v1/chat/rooms/{id}/groupinfo", s.handleFedChatGroupInfo)
	mux.HandleFunc("POST /fed/v1/chat/rooms/{id}/groupinfo", s.handleFedChatPutGroupInfo)
	mux.HandleFunc("GET /fed/v1/chat/rooms/{id}/blob/{sha}", s.handleFedChatBlob)
	mux.HandleFunc("POST /fed/v1/chat/rooms/{id}/blobs", s.handleFedChatPutBlob)
	mux.HandleFunc("POST /fed/v1/chat/rooms/{id}/adopt", s.handleFedChatAdopt)
	mux.HandleFunc("POST /fed/v1/chat/rooms/{id}/update", s.handleFedChatUpdate)
	mux.HandleFunc("GET /fed/v1/chat/rooms/{id}/blobs", s.handleFedChatBlobList)
	mux.HandleFunc("POST /fed/v1/chat/notify", s.handleFedChatNotify)
	mux.HandleFunc("POST /fed/v1/chat/departures", s.handleFedChatDepartures)
	mux.HandleFunc("POST /fed/v1/chat/ephemeral", s.handleFedChatEphemeral)
	// App-facing surface: bearer FOG_API_TOKEN, no session. How managed apps
	// see the paired households and connect to their apps (connect_handlers.go).
	mux.HandleFunc("GET /app/v1/connect/peers", s.handleAppConnectPeers)
	mux.HandleFunc("GET /app/v1/connect/reachability", s.handleAppConnectReachability)
	mux.HandleFunc("POST /app/v1/connect/grants", s.handleAppConnect)
	mux.HandleFunc("GET /app/v1/files/mounts", s.handleAppFilesMounts)
	mux.HandleFunc("GET /app/v1/chat/rooms", s.handleAppChatRooms)
	mux.HandleFunc("POST /app/v1/chat/rooms/{id}/messages", s.handleAppChatPost)
	// The household event stream and app-to-app connections
	// (app_events_handlers.go, docs/events-contract.md).
	mux.HandleFunc("POST /app/v1/events", s.handleAppEventsPublish)
	mux.HandleFunc("GET /app/v1/events", s.handleAppEventsRead)
	mux.HandleFunc("GET /app/v1/connections", s.handleAppConnections)
	// The home view: what a start page shows of this server, for an app
	// granted "home" in its uses (app_home_handlers.go, docs/home-view.md).
	mux.HandleFunc("GET /app/v1/home", s.handleAppHome)
	// What each app shows on that start page, and finds for its search box
	// (app_widgets_handlers.go, docs/widgets-contract.md).
	mux.HandleFunc("GET /app/v1/widgets/{app}", s.handleAppWidgets)
	mux.HandleFunc("GET /app/v1/search/{app}", s.handleAppSearch)
	// On a person's behalf: the app's token plus a Fog-Assertion for the
	// person at its door (presence.go).
	mux.HandleFunc("GET /app/v1/files/search", s.handleAppFilesSearch)
	// The people directory apps that sign in with fog may read, and where a
	// person lives in an app (people_directory_handlers.go).
	mux.HandleFunc("GET /app/v1/people", s.handleAppPeople)
	mux.HandleFunc("GET /app/v1/people/{address...}", s.handleAppPerson)
	mux.HandleFunc("PUT /app/v1/person-pages", s.handleAppPersonPage)
	// The gate in front of auth: true routes (gate_handlers.go): Caddy's
	// forward_auth backend, the decision on fog.<domain>, the callback on the
	// gated host, and the keys its assertions verify against.
	mux.HandleFunc("GET /api/authgate", s.handleAuthGate)
	mux.HandleFunc("GET /gate/authorize", s.handleGateAuthorize)
	mux.HandleFunc("GET "+gatePrefix+"callback", s.handleGateCallback)
	mux.HandleFunc("GET /oauth2/jwks", s.handleJWKS)
	admin("GET /api/apps/{name}/signin", s.handleGetSigninPolicy)
	admin("PUT /api/apps/{name}/signin", s.handlePutSigninPolicy)
	admin("POST /api/apps/{name}/signin/rotate", s.handleRotateSigninSecret)
	admin("POST /api/apps/{name}/signin/signout", s.handleSignEveryoneOut)
	admin("GET /api/apps/{name}/signin/visitors", s.handleGetVisitorGrants)
	admin("PUT /api/apps/{name}/signin/visitors", s.handlePutVisitorGrants)
	// fog's signing keys, and the emergency rotation (signin_handlers_keys.go).
	admin("GET /api/signin/keys", s.handleListSigningKeys)
	admin("POST /api/signin/keys/retire", s.handleRetireSigningKeys)
	// Visitors from paired households (visit_handlers.go): the host's half
	// of fog-to-fog sign-in, and the home's front channel.
	mux.HandleFunc("GET /api/visit/peers", s.handleVisitPeers)
	mux.HandleFunc("GET /visit/start", s.handleVisitStart)
	mux.HandleFunc("GET /visit/callback", s.handleVisitCallback)
	mux.HandleFunc("GET /visit/authorize", s.handleVisitAuthorize)
	mux.HandleFunc("POST /visit/authorize", s.handleVisitAuthorize)
	// fog as the household's OpenID provider (oidc_handlers.go,
	// docs/signin-contract.md). Public by nature: each endpoint
	// authenticates what it needs — the browser's session, the client's
	// secret, an access token.
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOIDCDiscovery)
	mux.HandleFunc("POST /oauth2/par", s.handlePAR)
	mux.HandleFunc("GET /oauth2/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /oauth2/token", s.handleToken)
	mux.HandleFunc("POST /oauth2/device_authorization", s.handleDeviceAuthorization)
	person("POST /api/device/lookup", s.handleDeviceLookup)
	person("POST /api/device/decide", s.handleDeviceDecide)
	mux.HandleFunc("GET /oauth2/userinfo", s.handleUserInfo)
	mux.HandleFunc("POST /oauth2/userinfo", s.handleUserInfo)
	mux.HandleFunc("POST /oauth2/revoke", s.handleRevoke)
	mux.HandleFunc("GET /oauth2/end_session", s.handleEndSession)
	mux.HandleFunc("POST /oauth2/end_session", s.handleEndSession)
	mux.HandleFunc("POST /oauth2/ask", s.handleSigninAsk)
	mux.HandleFunc("GET /app/v1/signin", s.handleAppSigninProof)
	mux.HandleFunc("GET /app/v1/gate", s.handleAppGate)
	// Confirming it's you: a fresh proof on the session already held.
	person("GET /api/people/lookup", s.handlePersonLookup)
	person("POST /api/people/reauth/password", s.handleReauthPassword)
	person("POST /api/people/reauth/passkey/begin", s.handleReauthPasskeyBegin)
	person("POST /api/people/reauth/passkey/finish", s.handleReauthPasskeyFinish)
	// People joining and signing in (people_handlers.go): public, rate
	// limited, separate from the operator's login.
	mux.HandleFunc("GET /api/join/{secret}", s.handleJoinPreview)
	mux.HandleFunc("POST /api/join/{secret}", s.handleJoin)
	// The member client and the chat SDK know this name; both logins are
	// one handler now, so the older path keeps working unchanged.
	mux.HandleFunc("POST /api/people/login", s.handleLogin)
	mux.HandleFunc("POST /api/people/login/passkey/begin", s.handlePasskeyLoginBegin)
	mux.HandleFunc("POST /api/people/login/passkey/finish", s.handlePasskeyLoginFinish)
	mux.HandleFunc("POST /api/people/logout", s.handleLogout)
	// A signed-in person (member or operator) managing their own self.
	person("GET /api/people/me", s.handlePersonMe)
	person("POST /api/people/me/password", s.handleSetOwnPassword)
	person("POST /api/people/me/passkeys/begin", s.handlePasskeyRegisterBegin)
	person("POST /api/people/me/passkeys/finish", s.handlePasskeyRegisterFinish)
	person("DELETE /api/people/me/credentials/{id}", s.handleDeleteOwnCredential)
	// The person's cryptographic self (chatme_handlers.go): identity key,
	// devices, key packages, backup, and the household directory.
	chat("GET /api/chat/me", s.handleChatMe)
	chat("PUT /api/chat/me/identity", s.handleSetIdentity)
	chat("POST /api/chat/me/devices", s.handleRegisterDevice)
	chat("POST /api/chat/me/devices/{id}/attest", s.handleAttestDevice)
	chat("PATCH /api/chat/me/devices/{id}", s.handleRenameDevice)
	chat("DELETE /api/chat/me/devices/{id}", s.handleRevokeDevice)
	chat("POST /api/chat/me/devices/{id}/keypackages", s.handlePublishKeyPackages)
	chat("GET /api/chat/me/devices/{id}/challenge", s.handleDeviceChallenge)
	chat("POST /api/chat/me/devices/{id}/session", s.handleDeviceSession)
	chat("PUT /api/chat/me/backup", s.handlePutBackup)
	chat("GET /api/chat/me/backup", s.handleGetBackup)
	chat("DELETE /api/chat/me/backup", s.handleDeletePersonBackup)
	chat("GET /api/chat/people", s.handleChatPeople)
	chat("GET /api/chat/people/{handle}/keys", s.handleChatPersonKeys)
	chat("GET /api/chat/keys", s.handleChatKeysByAddr)
	// Rooms and the ciphertext log (chat_handlers.go, chat_ws.go).
	chat("GET /api/chat/rooms", s.handleChatRooms)
	chat("POST /api/chat/rooms", s.handleChatCreateRoom)
	chat("GET /api/chat/rooms/{id}", s.handleChatRoom)
	chat("PATCH /api/chat/rooms/{id}", s.handleChatUpdateRoom)
	chat("GET /api/chat/rooms/{id}/events", s.handleChatEvents)
	chat("POST /api/chat/rooms/{id}/events", s.handleChatSubmit)
	chat("GET /api/chat/rooms/{id}/groupinfo", s.handleChatGroupInfo)
	chat("PUT /api/chat/rooms/{id}/groupinfo", s.handleChatPutGroupInfo)
	chat("POST /api/chat/rooms/{id}/read", s.handleChatRead)
	chat("POST /api/chat/rooms/{id}/typing", s.handleChatTyping)
	chat("GET /api/chat/invites", s.handleChatInvites)
	chat("POST /api/chat/invites/{id}/accept", s.handleChatAcceptInvite)
	chat("POST /api/chat/blobs", s.handleChatPutBlob)
	chat("GET /api/chat/blobs/{sha}", s.handleChatGetBlob)
	chat("GET /api/chat/stream", s.handleChatStream)
	// Preferences, push, previews, webhooks (chat_parity_handlers.go).
	chat("GET /api/chat/me/prefs", s.handleChatPrefs)
	chat("GET /api/chat/me/sessions", s.handleChatSessions)
	chat("DELETE /api/chat/me/sessions/{id}", s.handleChatRevokeSession)
	chat("PUT /api/chat/me/prefs", s.handleChatPutPrefs)
	chat("GET /api/chat/push/vapid", s.handleChatVAPID)
	chat("POST /api/chat/me/devices/{id}/push", s.handleChatPushSubscribe)
	chat("DELETE /api/chat/me/devices/{id}/push", s.handleChatPushUnsubscribe)
	chat("GET /api/chat/preview", s.handleChatPreview)
	chat("POST /api/chat/rooms/{id}/migrate", s.handleChatMigrate)
	chat("GET /api/chat/rooms/{id}/webhooks", s.handleChatWebhooks)
	chat("POST /api/chat/rooms/{id}/webhooks", s.handleChatCreateWebhook)
	chat("DELETE /api/chat/rooms/{id}/webhooks/{hook}", s.handleChatDeleteWebhook)
	// MCP tool server: bearer MCP token, no session. How AI clients manage
	// the box (see internal/mcpserver).
	mux.Handle("/mcp", s.mcpAuthed(s.mcpHandler))
	// Files for desktops: WebDAV behind an app password (files_access_handlers.go).
	dav := s.davAuthed(davHeaders(s.files.DAVHandler("/dav")))
	mux.Handle("/dav", dav)
	mux.Handle("/dav/", dav)
	// Files for anyone with the link: share pages, no session.
	mux.HandleFunc("GET /s/{token}", s.handleShare)
	mux.HandleFunc("GET /s/{token}/{path...}", s.handleShare)

	// Authenticated API.
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	person("GET /api/auth/me", s.handleMe)
	admin("GET /api/system", s.handleSystem)
	admin("GET /api/system/metrics", s.handleSystemMetrics)
	admin("GET /api/settings", s.handleGetSettings)
	admin("PUT /api/settings", s.handlePutSettings)
	admin("GET /api/settings/mcp-tokens", s.handleListMCPTokens)
	admin("POST /api/settings/mcp-tokens", s.handleCreateMCPToken)
	admin("DELETE /api/settings/mcp-tokens/{id}", s.handleDeleteMCPToken)
	admin("GET /api/audit", s.handleAudit)
	// People administration (people_handlers.go): the operator only.
	admin("GET /api/people", s.handleListPeople)
	admin("POST /api/people/invites", s.handleCreatePersonInvite)
	admin("DELETE /api/people/invites/{id}", s.handleDeletePersonInvite)
	admin("PATCH /api/people/{id}", s.handleUpdatePerson)
	admin("DELETE /api/people/{id}", s.handleDeletePerson)
	admin("POST /api/people/{id}/reset-identity", s.handleResetPersonIdentity)
	admin("GET /api/people/{id}/sessions", s.handleAdminPersonSessions)
	admin("POST /api/people/{id}/signout", s.handleAdminPersonSignout)
	admin("POST /api/people/{id}/password", s.handleSetPersonPassword)
	admin("POST /api/system/docker/install", s.handleDockerInstallStart)
	admin("GET /api/system/docker/install", s.handleDockerInstallStatus)

	// Settings › Compute.
	admin("GET /api/compute", s.handleComputeOverview)
	admin("GET /api/compute/activity", s.handleComputeActivity)
	admin("POST /api/compute/codes", s.handleComputeCode)
	admin("PATCH /api/compute/nodes/{id}", s.handleComputeNodeUpdate)
	admin("DELETE /api/compute/nodes/{id}", s.handleComputeNodeRemove)
	admin("POST /api/compute/nodes/{id}/update", s.handleComputeNodeRetryUpdate)
	admin("PUT /api/compute/grants", s.handleComputeGrant)

	// App store catalog.
	admin("GET /api/catalog", s.handleCatalog)
	admin("GET /api/capacity", s.handleCapacity)
	admin("GET /api/system/dependencies", s.handleDependencies)

	// Apps.
	admin("GET /api/apps", s.handleListApps)
	admin("POST /api/apps", s.handleCreateApp)
	admin("GET /api/apps/{name}", s.handleGetApp)
	admin("PUT /api/apps/{name}", s.handleUpdateApp)
	admin("POST /api/apps/{name}/actions", s.handleAppAction)
	admin("POST /api/apps/{name}/wire", s.handleAppWire)
	admin("GET /api/apps/{name}/integrations", s.handleListIntegrations)
	admin("POST /api/apps/{name}/integrations", s.handleConnectIntegration)
	admin("DELETE /api/apps/{name}/integrations/{provider}", s.handleDisconnectIntegration)
	admin("GET "+integrations.ReturnPath, s.handleIntegrationReturn)
	admin("DELETE /api/apps/{name}", s.handleDeleteApp)
	admin("GET /api/apps/{name}/logs", s.handleAppLogs)
	admin("GET /api/apps/{name}/container", s.handleAppContainer)
	admin("POST /api/apps/{name}/volumes/{volume}/move", s.handleAppVolumeMove)
	admin("GET /api/apps/{name}/docs", s.handleAppDocs)
	admin("GET /api/apps/{name}/docs/{file}", s.handleAppDoc)
	admin("POST /api/apps/{name}/test-connection", s.handleTestConnection)
	admin("POST /api/apps/{name}/health-check", s.handleHealthCheck)
	admin("POST /api/apps/{name}/regenerate-deploy-key", s.handleRegenerateDeployKey)
	admin("GET /api/apps/{name}/deploy-status", s.handleDeployStatus)
	admin("GET /api/deploys", s.handleLiveDeploys)
	admin("POST /api/apps/{name}/deploy/cancel", s.handleCancelDeploy)
	admin("GET /api/apps/{name}/exec", s.handleAppExec)
	admin("GET /api/apps/{name}/export", s.handleExportApp)
	admin("POST /api/apps/import-compose", s.handleImportCompose)
	admin("POST /api/apps/import", s.handleImportManifest)
	admin("GET /api/apps/{name}/review", s.handleAppReview)
	admin("POST /api/apps/{name}/review/confirm", s.handleConfirmReview)
	admin("GET /api/system/export", s.handleExportAll)

	// Backups + updates + recovery + self-update.
	admin("POST /api/apps/{name}/backups", s.handleCreateBackup)
	admin("GET /api/apps/{name}/backups", s.handleListBackups)
	admin("POST /api/backups/{id}/restore", s.handleRestoreBackup)
	admin("GET /api/backups/{id}/download", s.handleDownloadBackup)
	admin("DELETE /api/backups/{id}", s.handleDeleteBackup)
	admin("POST /api/system/backup-uploads", s.handleBackupUpload)
	admin("POST /api/system/backup-artifacts/inspect", s.handleInspectArtifact)
	admin("POST /api/apps/{name}/restore", s.handleRestoreArtifact)
	admin("POST /api/apps/{name}/restore/cancel", s.handleCancelRestore)
	admin("POST /api/updates/check", s.handleUpdatesCheck)
	admin("GET /api/updates/status", s.handleUpdatesStatus)
	admin("GET /api/backups/status", s.handleBackupsStatus)
	admin("POST /api/notify/test", s.handleNotifyTest)
	admin("POST /api/apps/{name}/update", s.handleApplyUpdate)
	admin("GET /api/apps/{name}/versions", s.handleAppVersions)
	admin("POST /api/apps/{name}/rollback", s.handleAppRollback)
	admin("POST /api/apps/{name}/update-policy", s.handleAppUpdatePolicy)
	admin("GET /api/system/recovery-kit", s.handleRecoveryKit)
	// Offsite: the locker in the cloud (offsite_handlers.go).
	admin("GET /api/offsite/presets", s.handleOffsitePresets)
	admin("GET /api/offsite/targets", s.handleOffsiteTargets)
	admin("POST /api/offsite/targets", s.handleOffsiteCreate)
	admin("POST /api/offsite/b2/setup", s.handleOffsiteB2Setup)
	admin("PUT /api/offsite/targets/{name}", s.handleOffsiteUpdate)
	admin("POST /api/offsite/targets/{name}/probe", s.handleOffsiteProbe)
	admin("POST /api/offsite/targets/{name}/sync", s.handleOffsiteSync)
	admin("POST /api/offsite/targets/{name}/copy-to/{dest}", s.handleOffsiteCopy)
	admin("POST /api/offsite/targets/{name}/retire", s.handleOffsiteRetire)
	admin("POST /api/offsite/targets/{name}/activate", s.handleOffsiteActivate)
	admin("GET /api/offsite/jobs/{id}", s.handleOffsiteJob)
	admin("GET /api/apps/{name}/offsite", s.handleAppOffsite)
	admin("POST /api/offsite/fetch", s.handleOffsiteFetch)
	admin("GET /api/offsite/files", s.handleOffsiteFiles)
	admin("POST /api/offsite/files/restore", s.handleOffsiteFilesRestore)
	admin("POST /api/offsite/recover", s.handleOffsiteRecover)
	admin("DELETE /api/offsite/targets/{name}", s.handleOffsiteDelete)
	admin("GET /api/system/disks", s.handleSystemDisks)
	admin("GET /api/system/orphans", s.handleListOrphans)
	admin("POST /api/system/orphans/reclaim", s.handleReclaimOrphan)

	// Host filesystem browser: the path picker behind host-path fields.
	admin("GET /api/system/fs", s.handleFsList)
	admin("POST /api/system/fs/mkdir", s.handleFsMkdir)

	// Files: fog's own root (see files_handlers.go).
	admin("GET /api/files/status", s.handleFilesStatus)
	admin("POST /api/files/reindex", s.handleFilesReindex)
	admin("POST /api/files/root/move", s.handleFilesRootMove)
	admin("POST /api/files/placement", s.handleFilesPlacement)
	admin("GET /api/files/ls", s.handleFilesList)
	admin("GET /api/files/stat", s.handleFilesStat)
	admin("GET /api/files/pins", s.handleFilePinsList)
	admin("POST /api/files/pins", s.handleFilePinAdd)
	admin("DELETE /api/files/pins", s.handleFilePinRemove)
	admin("GET /api/files/search", s.handleFilesSearch)
	admin("GET /api/files/raw/{path...}", s.handleFilesRaw)
	admin("GET /api/files/zip", s.handleFilesZip)
	admin("GET /api/files/thumb/{path...}", s.handleFilesThumb)
	admin("POST /api/files/mkdir", s.handleFilesMkdir)
	admin("POST /api/files/rename", s.handleFilesRename)
	admin("POST /api/files/move", s.handleFilesMove)
	admin("POST /api/files/copy", s.handleFilesCopy)
	admin("POST /api/files/trash", s.handleFilesTrash)
	admin("GET /api/files/trash", s.handleFilesTrashList)
	admin("POST /api/files/trash/restore", s.handleFilesTrashRestore)
	admin("DELETE /api/files/trash/{id}", s.handleFilesTrashPurge)
	admin("DELETE /api/files/trash", s.handleFilesTrashEmpty)
	admin("POST /api/files/uploads", s.handleFilesUploadBegin)
	admin("GET /api/files/uploads/{id}", s.handleFilesUploadStatus)
	admin("PATCH /api/files/uploads/{id}", s.handleFilesUploadChunk)
	admin("POST /api/files/uploads/{id}/complete", s.handleFilesUploadComplete)
	admin("DELETE /api/files/uploads/{id}", s.handleFilesUploadAbort)
	admin("GET /api/files/snapshots", s.handleFilesSnapshots)
	admin("POST /api/files/snapshots", s.handleFilesSnapshotRun)
	admin("GET /api/files/snapshots/{id}/ls", s.handleFilesSnapshotList)
	admin("GET /api/files/snapshots/{id}/raw/{path...}", s.handleFilesSnapshotRaw)
	admin("POST /api/files/snapshots/{id}/restore", s.handleFilesSnapshotRestore)
	admin("DELETE /api/files/snapshots/{id}", s.handleFilesSnapshotDelete)
	admin("GET /api/files/excludes", s.handleFilesExcludes)
	admin("POST /api/files/excludes", s.handleFilesExcludeSet)
	admin("GET /api/files/access-tokens", s.handleListFilesAccessTokens)
	admin("POST /api/files/access-tokens", s.handleCreateFilesAccessToken)
	admin("DELETE /api/files/access-tokens/{id}", s.handleDeleteFilesAccessToken)
	admin("GET /api/files/shares", s.handleListFileShares)
	admin("POST /api/files/shares", s.handleCreateFileShare)
	admin("DELETE /api/files/shares/{id}", s.handleDeleteFileShare)
	admin("GET /api/files/peer-shares", s.handleListPeerShares)
	admin("POST /api/files/peer-shares", s.handleCreatePeerShare)
	admin("PUT /api/files/peer-shares/{id}", s.handleUpdatePeerShare)
	admin("DELETE /api/files/peer-shares/{id}", s.handleDeletePeerShare)
	admin("POST /api/files/keep", s.handleFilesKeep)
	admin("POST /api/files/hydrate", s.handleFilesHydrate)
	admin("POST /api/files/contributions/reject", s.handleFilesReject)
	admin("GET /api/federation/peers/{id}/files", s.handlePeerFilesCatalog)
	admin("GET /api/files/subscriptions", s.handleListFileSubscriptions)
	admin("POST /api/files/subscriptions", s.handleSubscribeFiles)
	admin("PUT /api/files/subscriptions/{id}", s.handleUpdateFileSubscription)
	admin("POST /api/files/subscriptions/{id}/{action}", s.handleFileSubscriptionAction)
	admin("DELETE /api/files/subscriptions/{id}", s.handleUnsubscribeFiles)
	admin("GET /api/files/jobs", s.handleFilesJobs)
	admin("GET /api/files/jobs/{id}", s.handleFilesJob)
	admin("POST /api/files/jobs/{id}/cancel", s.handleFilesJobCancel)

	admin("POST /api/system/reboot", s.handleReboot)
	admin("POST /api/system/shutdown", s.handleShutdown)

	// The shared Postgres: which major runs, and the move to a newer one.
	admin("GET /api/system/postgres", s.handlePostgresStatus)
	admin("POST /api/system/postgres/upgrade", s.handlePostgresUpgrade)
	admin("POST /api/system/postgres/upgrade/cancel", s.handlePostgresUpgradeCancel)

	// Flare triage: the fleet inbox, per-app groups, and lifecycle actions.
	admin("GET /api/errors", s.handleFleetErrors)
	admin("GET /api/apps/{name}/errors", s.handleListAppErrors)
	admin("GET /api/apps/{name}/errors/{id}", s.handleGetErrorGroup)
	admin("GET /api/apps/{name}/errors/{id}/events/{eid}", s.handleGetErrorEvent)
	admin("POST /api/apps/{name}/errors/{id}/resolve", s.handleResolveErrorGroup)
	admin("POST /api/apps/{name}/errors/{id}/ignore", s.handleIgnoreErrorGroup)
	admin("POST /api/apps/{name}/errors/{id}/unresolve", s.handleUnresolveErrorGroup)
	// Global (unscoped) variants: what the fleet inbox uses, and the only
	// way to reach peer-relayed groups, whose apps have no local namespace.
	admin("GET /api/errors/{id}", s.handleGetErrorGroupGlobal)
	admin("GET /api/errors/{id}/events/{eid}", s.handleGetErrorEventGlobal)
	admin("POST /api/errors/{id}/resolve", s.handleResolveErrorGroupGlobal)
	admin("POST /api/errors/{id}/ignore", s.handleIgnoreErrorGroupGlobal)
	admin("POST /api/errors/{id}/unresolve", s.handleUnresolveErrorGroupGlobal)
	// Federation control plane: identity, invites, joins, peer capabilities.
	admin("GET /api/federation", s.handleGetFederation)
	admin("GET /api/federation/ledger", s.handleFederationLedger)
	admin("PUT /api/federation/identity", s.handlePutFederationIdentity)
	admin("POST /api/federation/identity/renew", s.handleRenewFedIdentity)
	admin("POST /api/federation/invites", s.handleCreateFedInvite)
	admin("DELETE /api/federation/invites/{id}", s.handleDeleteFedInvite)
	admin("POST /api/federation/join/preview", s.handleFedJoinPreview)
	admin("POST /api/federation/join", s.handleFedJoin)
	admin("PUT /api/federation/peers/{id}", s.handleUpdateFedPeer)
	admin("GET /api/federation/peers/{id}/mirror", s.handleFedPeerMirror)
	admin("POST /api/federation/peers/{id}/recheck", s.handleRecheckFedPeer)
	admin("DELETE /api/federation/peers/{id}", s.handleUnpairFedPeer)

	// Notices: the persistent operator inbox.
	admin("GET /api/notices", s.handleListNotices)
	admin("GET /api/inbox/summary", s.handleInboxSummary)
	// Feedback from the header's bug button (feedback_handlers.go).
	admin("POST /api/feedback", s.handleCreateFeedback)
	admin("GET /api/feedback", s.handleListFeedback)
	admin("GET /api/feedback/{id}", s.handleGetFeedback)
	admin("POST /api/feedback/{id}/status", s.handleSetFeedbackStatus)
	admin("DELETE /api/feedback/{id}", s.handleDeleteFeedback)
	admin("POST /api/notices/{id}/dismiss", s.handleDismissNotice)

	// Issues: the configuration scanner's findings.
	admin("GET /api/issues", s.handleListIssues)
	admin("GET /api/issues/summary", s.handleIssuesSummary)
	admin("POST /api/issues/{id}/ack", s.handleAckIssue)
	admin("POST /api/issues/{id}/unack", s.handleUnackIssue)
	admin("POST /api/issues/scan", s.handleIssuesScan)
	admin("GET /api/system/self-update", s.handleSelfUpdateCheck)
	admin("POST /api/system/self-update", s.handleSelfUpdateApply)
	admin("POST /api/system/self-update/ignore", s.handleIgnoreUpdate)

	// Domain & HTTPS wizard.
	admin("GET /api/domain/status", s.handleDomainStatus)
	admin("POST /api/domain/check", s.handleDomainCheck)
	admin("POST /api/domain/dns", s.handleDomainDNS)
	admin("POST /api/domain/apply", s.handleDomainApply)
	admin("POST /api/domain/verify", s.handleDomainVerify)
	// Where DNS points (LAN or the public address), the router's port
	// mapping, and the proof-from-outside beacon.
	admin("GET /api/domain/reach", s.handleReachStatus)
	admin("PUT /api/domain/reach", s.handleReachUpdate)
	admin("POST /api/domain/refresh", s.handleDomainRefresh)
	// The beacon is opened from a phone on cellular data, before any login
	// — its only secret is the token in the path.
	mux.HandleFunc("GET /api/reach/beacon/{token}", s.handleReachBeacon)

	// Cloudflare Tunnel.
	admin("GET /api/tunnel", s.handleTunnelStatus)
	admin("POST /api/tunnel/start", s.handleTunnelStart)
	admin("POST /api/tunnel/stop", s.handleTunnelStop)
	admin("GET /api/vpn", s.handleVPNStatus)
	admin("POST /api/vpn/start", s.handleVPNStart)
	admin("POST /api/vpn/stop", s.handleVPNStop)
	admin("PUT /api/vpn/routes", s.handleVPNRoutes)
	admin("GET /api/system/ssh", s.handleSSHAccess)
	admin("POST /api/system/ssh/keys", s.handleAddSSHKey)
	admin("DELETE /api/system/ssh/keys", s.handleRemoveSSHKey)

	// Proxy + system secrets.
	// NAS shares mounted on the host.
	admin("GET /api/nas", s.handleNasList)
	admin("POST /api/nas", s.handleNasCreate)
	admin("PUT /api/nas/{name}", s.handleNasUpdate)
	admin("POST /api/nas/{name}/mount", s.handleNasMount)
	admin("POST /api/nas/{name}/unmount", s.handleNasUnmount)
	admin("DELETE /api/nas/{name}", s.handleNasDelete)

	admin("POST /api/proxy/apply", s.handleProxyApply)
	admin("GET /api/system/secrets", s.handleGetSystemSecrets)
	admin("PUT /api/system/secrets", s.handlePutSystemSecret)

	// Everything else: embedded SPA with client-side routing fallback.
	mux.Handle("/", ui.Handler())

	// Lucid's transaction goes directly around the mux, which is what names
	// it: the mux writes the matched pattern on the request it is handed.
	// Refusals on /fed/v1 are signed on the way out (fed_handlers.go).
	// Security headers (CSP, framing, nosniff) wrap everything, outermost, so
	// even protect's own refusals carry them (internal/spa/headers.go).
	return spa.SecurityHeaders(s.protectRoutes(mux, s.signFedRefusals(lucid.Middleware(mux))))
}

// protect applies cross-cutting middleware: panic recovery, request logging,
// and a same-origin check on mutating requests (session cookies are
// SameSite=Lax; this closes the gap for any cross-site POST that still carries
// them).
func (s *Server) protect(next http.Handler) http.Handler { return s.protectRoutes(nil, next) }

// protectRoutes is protect, logging each request by the route of routes it
// matched (loggedRoute).
func (s *Server) protectRoutes(routes *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What a proxy says about a request — its scheme, its host, its
		// client — is believed only from Caddy (signin_handlers_origin.go).
		s.dropUnprovenForwarding(r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && strings.HasPrefix(r.URL.Path, "/api/") {
			if origin := r.Header.Get("Origin"); origin != "" {
				if !sameHost(origin, r.Host) {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
		}
		// What a browser says about where it is (signin_handlers_origin.go).
		if !s.browserRequestAllowed(w, r) {
			return
		}
		s.retireOldSessionCookie(w, r)
		s.noteTunnelPath(r) // signin_handlers_tunnel.go
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			// net/http recovers handler panics itself, but it does so silently
			// from the operator's point of view: the connection drops with no
			// response body and nothing structured in the log. Recover here so
			// the client gets an error it can render and the stack lands
			// somewhere it will actually be read.
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				s.log.Error("panic serving request",
					"method", r.Method, "route", loggedRoute(routes, r),
					"panic", v, "stack", string(debug.Stack()))
				if !rec.wrote {
					writeErr(rec, http.StatusInternalServerError, "something went wrong handling this request; check fogd's logs")
				}
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				// A successful read is Debug: the dashboard's polls and the
				// forward-auth check behind every gated asset would otherwise
				// fill the log ring feedback reports draw on, pushing out the
				// mutations and failures worth reading.
				level := slog.LevelInfo
				if (r.Method == http.MethodGet || r.Method == http.MethodHead) && rec.status < 400 {
					level = slog.LevelDebug
				}
				attrs := []any{"method", r.Method, "route", loggedRoute(routes, r),
					"status", rec.status, "dur", time.Since(start).Round(time.Millisecond).String()}
				if rec.status >= 500 {
					level = slog.LevelError
					if rec.errMsg != "" {
						attrs = append(attrs, "err", rec.errMsg)
					}
				}
				s.log.Log(r.Context(), level, "http", attrs...)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// loggedRoute is how a request's path reaches the log: the route pattern it
// matched ("/api/join/{secret}"), never the path itself, whose segments may
// be secrets — an invite, a share link, a webhook token — and whose log
// lines reach feedback reports sent to peers. A path matching no route is
// cut to its first two segments.
func loggedRoute(routes *http.ServeMux, r *http.Request) string {
	if routes != nil {
		if _, pattern := routes.Handler(r); pattern != "" {
			if _, path, ok := strings.Cut(pattern, " "); ok {
				return path
			}
			return pattern
		}
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) > 2 {
		return "/" + parts[0] + "/" + parts[1] + "/… (no route)"
	}
	return "/" + strings.Join(parts, "/") + " (no route)"
}

// goSafe runs background work in a goroutine that cannot take fogd down with
// it.
//
// The recovery in protect only covers the goroutine serving a request, and
// handlers deliberately spawn work that outlives the response: deploys, proxy
// applies, update checks. A panic in any of those is unrecoverable and kills
// the daemon — taking every running app's supervision with it — for what may
// be a trivial bug on a rarely-taken path.
//
// Every task is tracked, so Shutdown can cancel the context background work
// runs under (bgContext) and wait for it before the store closes beneath it.
// A task started after Shutdown is dropped: fogd is on its way out.
func (s *Server) goSafe(task string, fn func()) {
	s.bgMu.Lock()
	if s.bgClosed {
		s.bgMu.Unlock()
		s.log.Warn("background task after shutdown dropped", "task", task)
		return
	}
	s.bgWG.Add(1)
	s.bgMu.Unlock()
	go func() {
		defer s.bgWG.Done()
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic in background task",
					"task", task, "panic", v, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// bgContext is the parent of every background task's context: cancelled by
// Shutdown, so a deploy or update in flight stops at its next checkpoint
// instead of being killed mid-write when the process exits.
func (s *Server) bgContext() context.Context {
	if s.bgCtx == nil { // a Server built without New, in tests
		return context.Background()
	}
	return s.bgCtx
}

// deployCtx: deploys are detached from the request; generous ceiling for
// slow image pulls.
func (s *Server) deployCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.bgContext(), 30*time.Minute)
}

// deployAsync deploys name at rev (0 = latest) in the background, logging a
// failure under task; the caller has already answered the request.
func (s *Server) deployAsync(task, name string, rev int64, actor string) {
	s.goSafe(task, func() {
		ctx, cancel := s.deployCtx()
		defer cancel()
		if err := s.engine.Deploy(ctx, name, rev, actor); err != nil {
			s.log.Error(task, "app", name, "rev", rev, "err", err)
		}
	})
}

// Shutdown cancels background work and waits for it until ctx ends. Call
// it after the HTTP server has stopped taking requests, so nothing new
// starts, and before the store closes.
func (s *Server) Shutdown(ctx context.Context) error {
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgMu.Unlock()
	if s.bgCancel != nil {
		s.bgCancel()
	}
	done := make(chan struct{})
	go func() { s.bgWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// statusRecorder captures the response status for logging and remembers
// whether anything was written — a panic after a partial write must not have a
// second set of headers stapled onto it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
	// errMsg is the message of a 5xx this request answered with, so the
	// request's log line carries the failure and not just its status.
	errMsg string
}

// recordServerError notes a 5xx's message on the request's statusRecorder,
// wherever it sits in w's wrapper chain. The helpers that send these hold no
// logger; protect logs the line.
func recordServerError(w http.ResponseWriter, msg string) {
	for {
		if rec, ok := w.(*statusRecorder); ok {
			rec.errMsg = msg
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush and Hijack are forwarded explicitly, not just via Unwrap. Two callers
// type-assert on the ResponseWriter directly — the log stream checks for
// http.Flusher, and the websocket upgrade for the terminal needs
// http.Hijacker — and a wrapper that failed those assertions would silently
// buffer log output and break `docker exec` sessions outright.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("connection does not support hijacking")
	}
	r.wrote = true // the connection is no longer ours to write to
	return hj.Hijack()
}

func sameHost(origin, host string) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return strings.EqualFold(o, host)
}

type ctxKey int

// requireAuth gates the administrative surface. Everyone signs in through
// the same door now, so the gate is the role on the person's row, not which
// cookie they carry: a member with a perfectly good session is refused here
// exactly as a stranger is.
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pc, ok := s.resolvePerson(r)
		if !ok || !pc.Operator {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPerson, pc)))
	})
}

// hostOnlySessions says whether this request is one where fog's session is
// __Host-fog_session and nothing else. Under the household's domain that is
// the setup's scheme, the one rule the gate signs by (gateScheme): https
// when Caddy serves TLS, and then always — no request, whatever it says of
// its transport, talks it down to the plain cookie. There fog never reads a
// plain fog_session: a cookie fog did not set as __Host- may have been set
// by a sibling host for the whole domain (a session fixed in advance), and
// the prefix is the browser's word that it was not. Off the domain it is
// the request's own transport.
//
// Elsewhere — an http setup, a plain-http bare address — no cookie can
// carry the prefix (browsers won't store a Secure cookie over http), so the
// session is a host-only fog_session, and what a sibling could plant beside
// it is never used: every same-named cookie is a candidate, and only one
// that is a live session signed in from this very address counts
// (resolvePerson).
func (s *Server) hostOnlySessions(r *http.Request) bool {
	if s.sessionCookieDomain(r) != "" {
		return s.gateScheme() == "https"
	}
	return forwardedSecure(r)
}

// sessionCookieNames is the cookie fog reads a session from on r.
func (s *Server) sessionCookieNames(r *http.Request) []string {
	if s.hostOnlySessions(r) {
		return []string{auth.SecureCookieName}
	}
	return []string{auth.CookieName}
}

// setSessionCookie issues fog's session cookie. It is host-only in every
// case: __Host-fog_session wherever hostOnlySessions holds, which the
// browser returns to fog's own host and nowhere else; on a plain-http bare
// address a plain host-only fog_session. Gated apps get cookies of their
// own from the gate (gate_handlers.go) — none of them ever holds this one.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	c := &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionTTL.Seconds()),
	}
	if s.hostOnlySessions(r) {
		c.Name, c.Secure = auth.SecureCookieName, true
	}
	http.SetCookie(w, c)
	s.bindBrowser(w, r, token) // signin_handlers_cookies.go
}

// retireOldSessionCookie expires a fog_session presented where fog only
// reads __Host-fog_session (hostOnlySessions): the domain-wide cookie fog
// issued before __Host-, or one a sibling host planted. It is never adopted
// — a browser that held only the old cookie signs in again, once — and
// clearing it keeps it from coming back on every request.
//
// On an http setup, where fog_session is fog's own cookie, a second one
// beside it can only be a domain-wide copy a sibling planted: that one is
// expired (a Set-Cookie with the Domain attribute matches it, not fog's
// host-only cookie).
func (s *Server) retireOldSessionCookie(w http.ResponseWriter, r *http.Request) {
	s.expirePlantedCookies(w, r) // signin_handlers_cookies.go
	// Cookies first: this runs on every request, and only a browser still
	// holding the old cookie needs the settings read below.
	if old, err := r.Cookie(auth.CookieName); err != nil || old.Value == "" {
		return
	}
	if !s.hostOnlySessions(r) {
		if domain := s.sessionCookieDomain(r); domain != "" && len(cookieValues(r, auth.CookieName)) > 1 {
			http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", Domain: domain, HttpOnly: true, MaxAge: -1})
		}
		return
	}
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	if domain := s.sessionCookieDomain(r); domain != "" {
		http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", Domain: domain, HttpOnly: true, Secure: true, MaxAge: -1})
	}
}

// cookieValues is every non-empty value a request carries for name: a
// browser sends a host-only cookie and a domain-wide one of the same name
// side by side, in an order the server can't rely on.
func cookieValues(r *http.Request, name string) []string {
	var out []string
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			out = append(out, c.Value)
		}
	}
	return out
}

// sessionCookieDomain is the Domain a session cookie carries on this
// request: the base domain when the request came in under it, else none.
func (s *Server) sessionCookieDomain(r *http.Request) string {
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		return ""
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == domain || strings.HasSuffix(host, "."+domain) {
		return domain
	}
	return ""
}

// clearSessionCookie expires the session cookie under every name and scope
// fog issues. A browser only drops a cookie whose Domain matches, so under
// the base domain the domain-wide cookie is expired too, alongside any
// host-only one left from direct access.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	domain := s.sessionCookieDomain(r)
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	if domain != "" {
		http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", Domain: domain, HttpOnly: true, Secure: true, MaxAge: -1})
	}
	if s.hostOnlySessions(r) {
		http.SetCookie(w, &http.Cookie{Name: auth.SecureCookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, MaxAge: -1})
	}
	s.clearBrowserCookie(w, r)
}

// secrets reads and writes vault-encrypted secrets in plaintext.
func (s *Server) secrets() vault.Secrets {
	return vault.Secrets{Store: s.store, Vault: s.vault}
}
