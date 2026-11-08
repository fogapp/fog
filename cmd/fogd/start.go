package main

import (
	"context"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/netwatch"
	"gitea.lan/matthew/fog/internal/retention"
	"gitea.lan/matthew/fog/internal/tunnel"
)

// The start steps launch fogd's background work under the root context,
// once everything is wired and the HTTP server is listening. Every
// goroutine goes through d.spawn, so shutdown can wait for it.

// startRuntime starts the app runtime's loops: boot reconcile, health,
// stats, cleanup, retention, update checks and the issue scanner.
func (d *daemon) startRuntime() {
	// Boot-time reconcile: regenerate the proxy config and port forwards
	// from the store, so drift from a crash or manual intervention heals on
	// restart instead of persisting until the next app change.
	d.spawn("boot reconcile", d.engine.OnChange)
	// Every app that should be running is brought up afresh under the
	// rules as they stand — after an upgrade, the new protections reach
	// running containers, not only the next deploy — and its pins, which a
	// reboot's fresh tmpfs lost, are put back.
	d.spawn("app rules", d.engine.ReapplyRules)
	// Apps left stopped to be tried again — waiting for storage, or a
	// start that kept failing — on a loop of their own (rerender.go).
	d.spawn("app retries", d.engine.RetryLoop)
	// Health monitoring for apps with health specs.
	d.spawn("app health", func(ctx context.Context) { d.engine.MonitorLoop(ctx, apps.HealthInterval) })
	// Per-app networks: each app on its own, wired apps sharing one between
	// them (internal/apps/netwire.go) — at boot, and again every minute.
	d.spawn("app networks", func(ctx context.Context) { d.engine.NetworkLoop(ctx, apps.NetworkInterval) })
	// Container CPU/memory sampling, collected in the background and in
	// parallel; the apps API then serves it from memory instead of asking
	// Docker per container, per request, on a five-second dashboard poll.
	d.spawn("container stats", func(ctx context.Context) { d.engine.StatsLoop(ctx, 10*time.Second) })
	// Host utilization ring for the dashboard's metrics ribbon.
	d.spawn("host metrics", d.sampler.Loop)
	// Daily docker-side garbage sweep (dangling images, build cache cap).
	d.spawn("janitor", func(ctx context.Context) { d.jan.Loop(ctx, 24*time.Hour) })
	// Database retention: every table's pruning rule, in one list.
	jobs := append(retention.Store(d.st), retention.Chat(d.chatSvc)...)
	d.spawn("retention", func(ctx context.Context) { retention.Run(ctx, d.log, jobs) })
	// Registry digest polling with notify/auto policies.
	d.spawn("update checks", func(ctx context.Context) { d.upd.Loop(ctx, 6*time.Hour) })
	// The app store's catalogs: fetched now and every few hours, so the
	// store is the catalogs' latest without a fog release.
	d.spawn("catalog sync", d.api.CatalogLoop)
	// Fog's own release polling — feeds the header update notification.
	d.spawn("fog release checks", func(ctx context.Context) { d.updater.Loop(ctx, 30*time.Minute, d.notifier.Send) })
	// Signing keys: publish the next key a week before it signs, retire
	// the last a day after it stops (internal/signing).
	d.spawn("signing keys", func(ctx context.Context) {
		loop.Every(ctx, time.Minute, time.Hour, func(context.Context) {
			if err := d.keys.Reconcile(); err != nil {
				d.log.Error("signing keys", "err", err)
			}
		})
	})
	// Back-channel logouts the outbox owes apps (docs/signin-contract.md);
	// sign-outs also kick a delivery straight away.
	d.spawn("logout delivery", func(ctx context.Context) { loop.Every(ctx, 15*time.Second, 15*time.Second, d.api.DeliverLogouts) })
	// SCIM provisioning: who each app should know about; changes to
	// people and policy also kick a reconcile.
	d.spawn("provisioning", func(ctx context.Context) { loop.Every(ctx, time.Minute, 5*time.Minute, d.api.Provision) })
	// Configuration scanner: spec-vs-reality discrepancies filed as issues
	// (the Inbox's Issues tab).
	d.spawn("issue scanner", func(ctx context.Context) { d.iss.Loop(ctx, time.Hour) })
	// fogd's own telemetry to Glass (glass.go): the sender, and a minute's
	// check that it points at Glass as it runs now.
	d.spawn("glass telemetry", d.telemetry.Run)
	d.spawn("glass connection", func(ctx context.Context) { loop.Every(ctx, 5*time.Second, time.Minute, d.keepGlass) })
	// Compute: fogd as each pool's transcoder to the apps that hand work to
	// machines (docs/compute-contract.md).
	d.spawn("compute relay", d.relay.Run)
	// A machine paired but never heard from reaches the Inbox within
	// minutes, not at the next hourly sweep.
	d.spawn("compute issues", func(ctx context.Context) { loop.Every(ctx, time.Minute, time.Minute, d.iss.SweepCompute) })
}

// startFederation starts peer traffic: forwarders, heartbeats and chat.
func (d *daemon) startFederation() {
	// Flare forwarding: drain the outbox toward the upstream instance (no-op
	// until forwarding is configured on the Federation page).
	d.spawn("flare forwarding", func(ctx context.Context) { d.flareFwd.Loop(ctx, 30*time.Second) })
	// Feedback reports toward the peers this household sends them to; a
	// report tries once when written, and this picks up whatever didn't land.
	d.spawn("feedback forwarding", func(ctx context.Context) { d.feedbackFwd.Loop(ctx, time.Minute) })
	// Federation heartbeats: liveness + capability/directory exchange with
	// every paired peer (no-op until a peer exists).
	d.spawn("federation heartbeats", func(ctx context.Context) { d.fedSvc.Loop(ctx, fed.HeartbeatInterval) })
	// Connect revocations owed to peers (an unpair, a narrowed grant, an
	// app removed), retried until the app answers.
	d.spawn("connect revocations", d.api.ConnectRevocationLoop)
	// The fog bot needs the instance identity, which the federation service
	// mints lazily; start it once that exists so #fog has a home address.
	d.spawn("chat bot", func(ctx context.Context) {
		bot, err := chat.NewBot(d.chatSvc, d.vlt)
		if err != nil {
			d.log.Error("chat bot", "err", err)
			return
		}
		d.chatSvc.SetBot(bot)
		bot.Run(ctx)
	})
	d.spawn("chat federation", d.chatSvc.Fed.Loop)
}

// startBackups starts nightly backups, offsite sync and verification.
func (d *daemon) startBackups() {
	// Nightly app backups.
	d.spawn("backup schedule", d.bk.ScheduleLoop)
	// Offsite: every backup product that exists locally and has no copy
	// in a target gets one; then the offsite dead-man's switch.
	d.spawn("offsite sync", d.off.SyncLoop)
	d.spawn("offsite verify", d.off.VerifyLoop)
	// Backup verification (rolling re-hash + test-restore into a scratch
	// database) and the staleness dead-man's switch.
	d.spawn("backup verify", d.bk.VerifyLoop)
}

// startFiles starts Drive's index, snapshots and verification, and Files
// across peers.
func (d *daemon) startFiles() {
	// Drive's Content folder and its starter libraries, made once per root:
	// somewhere obvious to put a film before any app that plays films is
	// installed. Before the walk, so the first index already has them.
	// Apps and Content placed on another disk bind over the root first, so
	// the seed and the walk see the real folders. Any app Docker already
	// started against the bare folder underneath restarts onto the bind.
	for _, mp := range d.fm.EnsurePlacements(d.ctx) {
		d.track("placement restart", func(ctx context.Context) { d.engine.RestartAppsUnder(ctx, mp, "") })
	}
	d.fm.EnsureContent()
	// Files: a full index walk after boot and daily, the hasher between;
	// the nightly snapshot on the backup hour; the weekly sample verify.
	d.spawn("files index", d.fm.Loop)
	d.spawn("files snapshots", d.fm.SnapshotLoop)
	d.spawn("files verify", d.fm.VerifyLoop)
	d.spawn("files federation", d.ffed.Loop)
	d.spawn("files federation sync", d.ffed.SyncLoop)
}

// startNetworking starts per-app ports, NAS mounts, DNS, the network
// watcher, and resumes the tunnel and away access.
func (d *daemon) startNetworking() {
	log := d.log
	// Stable per-app ports, proxied in-process (swap-safe).
	d.spawn("port forwards", func(ctx context.Context) { d.fwd.Loop(ctx, 15*time.Second) })
	// NAS shares: a reconcile loop re-mounts enabled shares (mounts don't
	// survive reboot; fog owns them, not fstab) and keeps retrying with
	// backoff — a NAS that answers minutes after fogd, the normal order when
	// a whole house powers on together, mounts itself. Async so a dead NAS
	// can't wedge startup. A late mount restarts the apps on it (wireHooks).
	d.spawn("nas mounts", d.nasMgr.MountLoop)
	d.spawn("dns keeper", d.dns.Loop)
	// A new DHCP lease or a cable moved to another router reaches the
	// keeper in seconds (kernel netlink events, debounced) instead of on
	// its next five-minute tick.
	d.spawn("network watcher", func(ctx context.Context) {
		(&netwatch.Watcher{}).Watch(ctx, func(fp netwatch.Fingerprint) {
			log.Info("network changed", "ip", fp.LocalIP, "gateway", fp.Gateway)
			d.dns.Kick()
			// The advertised route is keyed on the LAN address it was made
			// for, exactly like the port mapping: a new lease means
			// re-advertising, or away access still points at an address
			// nothing answers on. Refresh re-joins aside, so the watcher
			// is free for the next change.
			d.vpnMgr.Refresh()
		})
	})
	// Cloudflare Tunnel: resume automatically when a token is configured.
	if _, err := d.st.GetSecret(0, tunnel.SecretToken); err == nil {
		if err := d.tun.Start(); err != nil {
			log.Warn("tunnel autostart", "err", err)
		}
	}
	// Away access: resume when the operator left it on. Unlike the tunnel,
	// a saved key isn't the signal — a key stays saved after a deliberate
	// stop, and a stop should survive a reboot.
	if d.vpnMgr.Enabled() {
		if err := d.vpnMgr.Start(false); err != nil {
			log.Warn("vpn autostart", "err", err)
		}
	}
}
