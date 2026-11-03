package main

import (
	"context"
	"gitea.lan/matthew/fog/internal/appevents"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/build"
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
	"gitea.lan/matthew/fog/internal/issues"
	"gitea.lan/matthew/fog/internal/janitor"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/metrics"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/portfwd"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/selfupdate"
	"gitea.lan/matthew/fog/internal/server"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/signing"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/tunnel"
	"gitea.lan/matthew/fog/internal/updates"
	"gitea.lan/matthew/fog/internal/vault"
	"gitea.lan/matthew/fog/internal/vpn"
	"gitea.lan/matthew/fog/internal/wan"
)

// The wire steps build fogd's components. Each reads only what an earlier
// step set; none starts a goroutine — that is the start steps' job.

// wireApps builds the app engine and what hangs directly off it: the
// proxy, backups, update checks, the janitor and the host sampler.
func (d *daemon) wireApps() {
	cfg, st, vlt, log := d.cfg, d.st, d.vlt, d.log
	d.docker = dockerx.New()
	d.events = appevents.New(st)
	d.keys = &signing.Keyring{Store: st, Vault: d.vlt}
	d.notifier = &notify.Notifier{Store: st, Log: log, Events: d.events}
	// Containers — Caddy and the apps alike — reach fogd through the docker
	// host gateway, on whatever port fogd actually listens.
	var hostAddr string
	if port := d.listenPort(); port != "" {
		hostAddr = "host.docker.internal:" + port
	}
	d.px = &proxy.Proxy{DataDir: cfg.DataDir, Store: st, Vault: vlt, Log: log, Docker: d.docker, FogUpstream: hostAddr}
	d.engine = &apps.Engine{
		Store:   st,
		Vault:   vlt,
		Docker:  d.docker,
		Mat:     newMaterializer(cfg.DataDir, cfg.Dev),
		Builder: &build.Builder{DataDir: cfg.DataDir},
		PG:      &pg.Manager{DataDir: cfg.DataDir, Store: st, Vault: vlt, Log: log, Network: apps.NetworkName, SealedNetwork: apps.SealedNetworkName, Docker: d.docker},
		Log:     log,

		DevPermissiveProbe: cfg.Dev,
		Notify:             d.notifier.Send,
		Signin:             &signin.Registry{Store: st, Vault: vlt},
		FogHostAddr:        hostAddr,
	}
	// A bind of another app's Drive folder renders when that app shares it
	// with this one (apps.Engine.SharesFor).
	d.engine.Mat.SharesFor = d.engine.SharesFor

	d.bk = &backup.Manager{DataDir: cfg.DataDir, Store: st, PG: d.engine.PG, Mat: d.engine.Mat, Log: log, Notify: d.notifier.Send, Engine: d.engine, Vault: vlt}
	// Seal a recovery-kit passphrase an older fog left in the settings table.
	if err := backup.MigrateKitPassphrase(st, vlt); err != nil {
		log.Warn("sealing the stored recovery-kit passphrase", "err", err)
	}
	d.engine.RemoveBackups = d.bk.RemoveAppBackups
	d.engine.RestoreStaged = d.bk.RestoreStaged
	d.engine.StageArtifact = d.bk.StageArtifact
	d.upd = &updates.Checker{Store: st, Engine: d.engine, Backup: d.bk, Log: log, Notify: d.notifier.Send}
	d.updater = &selfupdate.Updater{Store: st, Vault: vlt, Busy: d.engine.DeployingApps}
	d.jan = &janitor.Janitor{Store: st, DataDir: cfg.DataDir, BackupsRoot: d.bk.Root, Log: log}
	d.sampler = metrics.New(cfg.DataDir, 10*time.Second, time.Hour)
	d.sampler.OnSample = d.reportHost
	// Machines that lend fog their hardware: fog-node builds come from
	// FOG_NODE_BIN_DIR on a development fogd, else from this release on
	// fog's own update source, fetched the first time a machine asks.
	d.comp = &compute.Service{
		Registry: &compute.Registry{Store: st, Log: log},
		Binaries: &compute.Binaries{Dir: os.Getenv("FOG_NODE_BIN_DIR"), Cache: filepath.Join(cfg.DataDir, "compute", "fog-node"),
			Version: config.Version, Download: d.updater.DownloadArtifact, Manifest: d.updater.SignedManifest},
		Version: config.Version,
		Log:     log,
	}
	// The relay polls apps a few times a second, so it reuses each app's
	// address for a while rather than asking Docker every call.
	d.relay = compute.NewRelay(d.comp, st, appcall.New(appcall.Cached(d.engine.AppEndpoints, 10*time.Second)).Refusing(compute.Refusal), log)
	// An app declaring fog-inference-router/1 orders the machines for each
	// inference request, within compute.RouterBudget.
	d.comp.Router = compute.NewRouter(st, appcall.New(appcall.Cached(d.engine.AppEndpoints, 10*time.Second)))
}

// wireNetworking builds what connects this box to the outside: the tunnel,
// away access, per-app ports, NAS shares and public DNS.
func (d *daemon) wireNetworking() {
	cfg, st, vlt, log := d.cfg, d.st, d.vlt, d.log
	d.tun = &tunnel.Manager{Store: st, Vault: vlt, Log: log, BinPath: os.Getenv("FOG_CLOUDFLARED_PATH")}
	// Away access: tailscaled, supervised the same way, advertising one
	// route — this box's own LAN address — so the domain keeps resolving
	// to the same place from the road as it does at home.
	d.vpnMgr = &vpn.Manager{Store: st, Vault: vlt, Log: log, DataDir: cfg.DataDir, BinDir: os.Getenv("FOG_TAILSCALE_BIN_DIR"), LANIP: wan.LANIP}
	d.fwd = &portfwd.Forwarder{Store: st, Docker: d.docker, Log: log}
	d.nasMgr = &nas.Manager{Store: st, Vault: vlt, DataDir: cfg.DataDir, Log: log}
	// Before an app starts: the NAS shares behind its volumes, then Apps or
	// Content where they live on another disk — which may itself be on a
	// share, so the share's mount comes first.
	d.engine.EnsureMounts = func(ctx context.Context, paths []string) error {
		for _, folder := range []string{files.AppsDir, files.ContentDir} {
			if loc := d.fm.Location(folder); loc != "" {
				paths = append(paths, loc)
			}
		}
		if err := d.nasMgr.EnsureForPaths(ctx, paths); err != nil {
			return err
		}
		return d.fm.EnsurePlacementsFor(ctx, paths)
	}
	// Public wildcard DNS follows the address the operator chose — this
	// server's LAN address, or the house's public one. Without this, moving
	// the machine to a new network (or the ISP handing out a new address)
	// leaves *.<domain> pointing at the old place until someone re-runs the
	// domain wizard. Built here so the handlers share the one keeper whose
	// loop runs below.
	d.dns = &dnssync.Keeper{Store: st, Vault: vlt, Log: log, Notify: d.notifier.Send}
}

// wireFederation builds the peer service and the forwarders that ride it.
func (d *daemon) wireFederation() {
	st, vlt, log := d.st, d.vlt, d.log
	d.fedSvc = &fed.Service{Store: st, Vault: vlt, Log: log, Version: config.Version, DirectoryFor: d.engine.DirectoryFor}
	// Domainless instances advertise their live LAN address in heartbeats,
	// derived fresh on every dial — so a peer paired by IP hears the new
	// address after this box moves networks, instead of holding the old one
	// forever.
	if port := d.listenPort(); port != "" {
		d.fedSvc.SelfURLFallback = func() string {
			ip := wan.LANIP()
			if ip == "" {
				return ""
			}
			return "http://" + ip + ":" + port
		}
	}
	d.flareFwd = &flare.Forwarder{Store: st, Fed: d.fedSvc, Log: log}
	d.feedbackFwd = &feedback.Forwarder{Store: st, Fed: d.fedSvc, Log: log}
}

// wireFiles builds Fog's own files root (the Drive page, its index, and its
// jobs) and Files across peers.
func (d *daemon) wireFiles() {
	st, log := d.st, d.log
	fm := files.New(d.cfg.DataDir, st, log)
	d.fm = fm
	fm.InstallTools = true
	// A volume that names a Files folder resolves against the live root at
	// deploy time, and the folder is made through Files so it is owned,
	// indexed, and badged like everything else there.
	d.engine.Mat.FilesRoot = fm.Root
	d.engine.Mat.EnsureFilesDir = fm.EnsureDir
	// A host path on a NAS share the operator set up is fog's own storage,
	// and binds with no acknowledgement (internal/apps/hostaccess.go).
	d.engine.Mat.ManagedRoots = func() []string {
		shares, _ := d.st.ListNasShares()
		out := make([]string, 0, len(shares))
		for _, sh := range shares {
			out = append(out, sh.MountPoint)
		}
		return out
	}
	// Deleting an app with its data takes its own folder in Drive too, to
	// Drive's trash rather than straight off the disk.
	d.engine.TrashDriveFolder = fm.TrashAppFolder
	fm.BackupRoot = d.bk.Root
	// Moving the backup directory waits for, and then holds off, every
	// backup, Drive snapshot, restore and verify.
	fm.HoldBackupDir = d.bk.HoldDir
	fm.LockBackupDir = d.bk.LockDir
	fm.Notify = d.notifier.Send
	// A folder an app mounts as a volume must not be moved or trashed
	// from under it.
	fm.MountedPaths = fm.AppMounts
	// Moving Apps or Content to another disk stops the apps that use the
	// folder for the length of the job.
	fm.StopAppsUnder = func(ctx context.Context, path string) ([]string, error) {
		return d.engine.StopAppsUnder(ctx, path, "files-placement")
	}
	fm.StartApps = func(ctx context.Context, names []string) error {
		return d.engine.StartApps(ctx, names, "files-placement")
	}
	// Files across peers: shares, subscriptions, nudges.
	d.ffed = filesfed.New(fm, d.fedSvc, st, log)
	d.ffed.Notify = d.notifier.Send
	// It rules on mounting a peer's copy at deploy time.
	d.engine.Mat.MountPolicy = d.ffed.MountPolicy
}

// wireOffsite builds the locker in the cloud. The recovery-kit passphrase is
// its one secret; the server name names new lockers.
func (d *daemon) wireOffsite() {
	st, bk, engine, fm := d.st, d.bk, d.engine, d.fm
	d.off = &offsite.Manager{DataDir: d.cfg.DataDir, Store: st, Vault: d.vlt, Log: d.log, Notify: d.notifier.Send,
		Passphrase:    func() string { return backup.KitPassphrase(st, d.vlt) },
		ServerName:    func() string { v, _ := st.GetSetting(store.SettingServerName); return v },
		BackupRoot:    bk.Root,
		PathAvailable: bk.PathAvailable,
		HoldBackupDir: bk.HoldDir,
		Jobs:          jobs.New(),
		RestoreApp:    bk.RestoreApp,
		TestRestore:   bk.TestRestore,
		Deployed: func(name string) bool {
			_, err := os.Stat(filepath.Join(engine.Mat.AppDir(name), "compose.yaml"))
			return err == nil
		},
		DeployRestored: engine.DeployRecovered,
		Deploy:         func(ctx context.Context, name string) error { return engine.Deploy(ctx, name, 0, "offsite") },
		FilesRoot:      fm.Root,
		FilesJobs:      fm.Jobs,
		FilesRestored:  func() { d.spawn("files walk after restore", func(ctx context.Context) { fm.Walk(ctx) }) }}
}

// wireIssues builds the configuration scanner: spec-vs-reality
// discrepancies filed as issues.
func (d *daemon) wireIssues() {
	st, docker := d.st, d.docker
	d.iss = &issues.Scanner{
		Store: st, Log: d.log, Notify: d.notifier.Send,
		Docker: docker, DockerAvailable: docker.Available, PG: d.engine.PG, Nas: d.nasMgr, DataDir: d.cfg.DataDir,
		Deploying:       d.engine.Deploying,
		ExecInApp:       d.engine.ExecInApp,
		AppContainers:   d.engine.AppContainers,
		FetchDescriptor: d.bk.Descriptor,
		BackupDir:       d.bk.Root,
		FilesRoot:       d.fm.Root,
		FilesOwner:      d.fm.Owner,
		SecretValue:     vault.Secrets{Store: st, Vault: d.vlt}.Get,
		ComputeStuck:    d.comp.Stuck,
		FogdPort:        d.engine.FogdPort,
		RulesRefused:    d.engine.RulesRefused,
		SecretsMissing:  d.engine.SecretsMissing,
		Waiting:         d.engine.Waiting,
	}
}

// wireHooks connects components that react to each other: an app change
// re-renders the proxy and ports, a Files root move redeploys the apps that
// mount it, and a NAS share mounting late restarts the apps that need it.
func (d *daemon) wireHooks() {
	engine, fm, log := d.engine, d.fm, d.log
	engine.OnChange = func(ctx context.Context) {
		// First, any app holding a port of fog's is stopped: at boot Docker
		// started the apps before fogd, and one may have taken Caddy's.
		engine.ReclaimFogPorts(ctx)
		if err := d.px.Apply(ctx); err != nil {
			log.Error("proxy apply", "err", err)
		}
		// The apply may have recreated the proxy, which then sits on none of
		// the apps' own networks until it is put back.
		if err := engine.ReconcileNetworks(ctx, nil); err != nil {
			log.Warn("app networks", "err", err)
		}
		d.fwd.Reconcile(ctx)
	}
	// After the root moves, every running app that mounts a Files folder
	// is redeployed so its compose file binds the folder where it is now.
	// Aside from the move's job, which goes straight on to re-index.
	fm.OnRootMoved = func() { d.spawn("files root redeploy", engine.RedeployFilesApps) }
	// A NAS share that mounts after Docker already started the apps on it
	// restarts them, so they bind the share rather than the bare directory.
	// The NAS manager already calls this on a goroutine of its own.
	d.nasMgr.OnLateMount = func(sh store.NasShare) {
		d.track("late mount restart", func(ctx context.Context) {
			engine.RestartAppsUnder(ctx, sh.MountPoint, sh.Name)
			// Apps or Content placed on that share bind now, and the apps
			// in them restart the same way.
			for _, mp := range fm.EnsurePlacements(ctx) {
				engine.RestartAppsUnder(ctx, mp, sh.Name)
			}
		})
	}
}

// wirePGUpgrader builds the Postgres major upgrade. Its hooks are the only
// place fog stops every database-backed app at once: the cluster must have
// no writers between the inventory and the dump, and the apps must come
// back whether the move succeeded or rolled back.
func (d *daemon) wirePGUpgrader() {
	engine := d.engine
	d.pgUpgrader = &pg.Upgrader{PG: engine.PG, Hooks: pg.UpgradeHooks{
		Busy:         engine.DeployingAny,
		StopApps:     engine.StopPostgresApps,
		StartApps:    engine.StartPostgresApps,
		AfterUpgrade: func() { d.iss.Sweep(d.ctx) },
	}}
}

// wireHTTP builds the API and the HTTP server in front of it.
func (d *daemon) wireHTTP() {
	d.api = server.New(d.cfg, d.st, server.Deps{
		Vault: d.vlt, Docker: d.docker, Engine: d.engine, Proxy: d.px,
		Backup: d.bk, Updates: d.upd, Updater: d.updater, Tunnel: d.tun, VPN: d.vpnMgr,
		Janitor: d.jan, Nas: d.nasMgr, Metrics: d.sampler, Flare: d.flareFwd, Fed: d.fedSvc,
		Feedback: d.feedbackFwd, LogRing: d.logRing,
		Issues: d.iss, PGUpgrader: d.pgUpgrader, Files: d.fm, FilesFed: d.ffed, Offsite: d.off,
		Chat: d.chatSvc, DNS: d.dns, Events: d.events, Keys: d.keys, Compute: d.comp,
	}, d.log)
	// The server's ends of the callbacks the components make into it.
	hooks := d.api.Hooks()
	// Peers learn our people the way they learn our apps, in the heartbeat,
	// and what became of the reports they relayed here.
	d.fedSvc.PeopleFor = d.chatSvc.Fed.PeopleFor
	d.fedSvc.FeedbackFor = hooks.FeedbackFor
	d.fedSvc.OnFeedback = hooks.OnFeedback
	d.ffed.AppNotify = hooks.FilesChanged
	d.engine.OnSpecChanged = hooks.SpecChanged
	d.chatSvc.Notify = &chat.Notifier{Svc: d.chatSvc, Vault: d.vlt, NameFor: hooks.DisplayName, Ntfy: d.notifier.SendTo}
	d.srv = &http.Server{
		Addr:              d.cfg.ListenAddr,
		Handler:           d.api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout reclaims keep-alive connections a client has stopped
		// using; without it they sat open indefinitely.
		//
		// Neither ReadTimeout nor WriteTimeout is set, and that is deliberate.
		// Both put a deadline on the whole request or response, and this server
		// streams container logs and hijacks connections for interactive
		// terminal sessions that are meant to stay open as long as the operator
		// wants. A hijacked connection inherits the read deadline, so a
		// ReadTimeout here would kill `docker exec` sessions mid-use. Request
		// bodies are already bounded by MaxBytesReader.
		IdleTimeout: 120 * time.Second,
	}
}
