package main

import (
	"context"
	"errors"
	"gitea.lan/matthew/fog/internal/appevents"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/bootmark"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/compute"
	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/dnssync"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/feedback"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/firstboot"
	"gitea.lan/matthew/fog/internal/flare"
	"gitea.lan/matthew/fog/internal/issues"
	"gitea.lan/matthew/fog/internal/janitor"
	"gitea.lan/matthew/fog/internal/logring"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/lucid"
	"gitea.lan/matthew/fog/internal/metrics"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/portfwd"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/sdnotify"
	"gitea.lan/matthew/fog/internal/selfupdate"
	"gitea.lan/matthew/fog/internal/server"
	"gitea.lan/matthew/fog/internal/signing"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/tunnel"
	"gitea.lan/matthew/fog/internal/updates"
	"gitea.lan/matthew/fog/internal/vault"
	"gitea.lan/matthew/fog/internal/vpn"
)

// daemon is everything fogd wires together. main builds it in dependency
// order — each wire step reads only fields an earlier step set — then
// starts the background work and waits for a signal.
type daemon struct {
	cfg     config.Config
	log     *slog.Logger
	logRing *logring.Ring
	// ctx is the root context: cancelled first thing at shutdown.
	ctx context.Context
	// tasks are the goroutines running under ctx; shutdown waits for them.
	tasks tasks

	st  *store.Store
	vlt *vault.Vault

	// Apps and their runtime.
	docker   *dockerx.Client
	notifier *notify.Notifier
	events   *appevents.Bus
	keys     *signing.Keyring
	px       *proxy.Proxy
	engine   *apps.Engine
	bk       *backup.Manager
	upd      *updates.Checker
	updater  *selfupdate.Updater
	jan      *janitor.Janitor
	sampler  *metrics.Sampler

	// Networking.
	tun    *tunnel.Manager
	vpnMgr *vpn.Manager
	fwd    *portfwd.Forwarder
	nasMgr *nas.Manager
	dns    *dnssync.Keeper

	// Federation and chat.
	fedSvc      *fed.Service
	flareFwd    *flare.Forwarder
	feedbackFwd *feedback.Forwarder
	chatSvc     *chat.Service

	// Files and offsite.
	fm   *files.Manager
	ffed *filesfed.Service
	off  *offsite.Manager
	// comp is the machines that lend fog their hardware.
	comp *compute.Service
	// relay is fogd as each pool's transcoder to the apps.
	relay *compute.Relay

	iss        *issues.Scanner
	pgUpgrader *pg.Upgrader

	api *server.Server
	srv *http.Server

	// telemetry reports fogd to Lucid (lucid.go); inert until Lucid is
	// installed and has minted fogd a key.
	telemetry *lucid.Client
	// lucidChecked: the refused key's integration was already asked about.
	lucidChecked bool
}

func main() {
	// `fogd pin-bind`: fogd itself, run in the host's mount namespace to pin
	// one bind source (internal/apps/pin.go).
	if len(os.Args) > 1 && os.Args[1] == "pin-bind" {
		os.Exit(apps.PinMain(os.Args[2:]))
	}
	if code := run(); code != 0 {
		os.Exit(code)
	}
}

// run is fogd from boot to exit; it returns the process exit code. Its own
// defers run before main exits, whatever the code.
func run() int {
	// Stdout stays the record (journald keeps it); the ring beside it is the
	// short tail a feedback report attaches, and Lucid, once connected, gets
	// the lines at its log_level.
	logRing := logring.New(1000)
	telemetry := lucid.New(config.Version)
	lucid.SetDefault(telemetry)
	d := &daemon{
		cfg:       config.FromEnv(),
		log:       slog.New(lucid.LogHandler(logRing.Handler(slog.NewTextHandler(os.Stdout, nil)))),
		logRing:   logRing,
		telemetry: telemetry,
	}
	// Outbound calls made through http.DefaultClient inside a request or a
	// job become spans of it. DefaultTransport itself stays the stock
	// *http.Transport: fed and backup clone it by type.
	http.DefaultClient.Transport = lucid.Transport{}
	d.tasks.log = d.log

	restoredFromKit, ok := d.prepareDataDir()
	if !ok {
		return 1
	}
	if !d.openState(restoredFromKit) {
		return 1
	}
	defer d.st.Close()

	// The context every background loop runs under. Made before wiring, so
	// the hooks wired below (a late NAS mount, a Files root move) start
	// their redeploys under it; shutdown cancels it.
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	d.ctx = rootCtx

	// Construction, in dependency order. Nothing here starts a goroutine.
	d.wireApps()
	d.wireNetworking()
	d.wireFederation()
	d.wireFiles()
	defer d.fm.Close()
	d.wireOffsite()
	d.wireIssues()
	d.wireHooks()
	d.wirePGUpgrader()
	d.chatSvc = server.NewChatService(d.cfg, d.st, d.fedSvc, d.log)
	d.wireHTTP()

	listenErr := d.serve()
	d.startRuntime()
	d.startFederation()
	d.startBackups()
	d.startFiles()
	d.startNetworking()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	// A listen failure (port already bound, bad address) is a deliberate exit,
	// not a crash: it takes the same shutdown path as a signal, so the store
	// closes and the boot marker lands — without it the next boot would file a
	// "recovered from an unexpected shutdown" notice for every failed bind.
	exitCode := 0
	select {
	case <-stop:
	case err := <-listenErr:
		d.log.Error("http server", "err", err)
		exitCode = 1
	}
	d.shutdown(cancelRoot)
	return exitCode
}

// prepareDataDir readies the data directory before anything opens it:
// HOME and DOCKER_CONFIG point inside it, and a staged recovery-kit restore
// moves into place. ok is false when fogd can't continue.
func (d *daemon) prepareDataDir() (restoredFromKit, ok bool) {
	cfg, log := d.cfg, d.log
	// The state tree holds the master key and every secret: owner-only.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Error("creating data dir", "dir", cfg.DataDir, "err", err)
		return false, false
	}
	// The systemd sandbox makes /root read-only. Point HOME at the data dir
	// so ANY child tool (git, ssh, docker, future additions) that writes
	// under $HOME lands somewhere writable — this kills the whole class of
	// read-only-filesystem failures, including on installs with older units.
	os.Setenv("HOME", cfg.DataDir)
	dockerCfg := filepath.Join(cfg.DataDir, "docker-config")
	os.MkdirAll(dockerCfg, 0o700)
	os.Setenv("DOCKER_CONFIG", dockerCfg)
	// A staged recovery-kit restore (POST /api/setup/restore-kit) lands here:
	// the previous process wrote the decrypted kit and exited, and the files
	// must move into place before the store opens them. The originals are
	// kept beside as .pre-restore, so even a botched kit can't destroy state.
	if pending := filepath.Join(cfg.DataDir, "restore-pending"); fileExists(filepath.Join(pending, "fog.db")) {
		log.Info("recovery kit staged — restoring fog state before boot")
		for _, name := range []string{"fog.db", "master.key"} {
			src := filepath.Join(pending, name)
			dst := filepath.Join(cfg.DataDir, name)
			if fileExists(dst) {
				os.Rename(dst, dst+".pre-restore")
			}
			if err := os.Rename(src, dst); err != nil {
				log.Error("recovery restore", "file", name, "err", err)
				return false, false
			}
		}
		// WAL/SHM from the replaced database would poison the restored one.
		os.Remove(cfg.DBPath() + "-wal")
		os.Remove(cfg.DBPath() + "-shm")
		os.RemoveAll(pending)
		restoredFromKit = true
	}
	return restoredFromKit, true
}

// openState opens the store and the vault and settles what the last run
// left behind: interrupted deploys, a restored database, a first-boot
// preseed, an unclean stop. ok is false when fogd can't continue; the store
// is closed in that case.
func (d *daemon) openState(restoredFromKit bool) (ok bool) {
	cfg, log := d.cfg, d.log
	// Was the last stop graceful? Consume the marker before anything writes.
	dirtyBoot := bootmark.BeginBoot(cfg.DataDir, cfg.DBPath())

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		log.Error("opening store", "path", cfg.DBPath(), "err", err)
		return false
	}
	d.st = st

	// Deploys the previous fogd started but never finished would read as
	// in-flight forever — FinishDeploy only runs in the goroutine that started
	// the deploy, and that goroutine died with the process.
	if n, err := st.FailInterruptedDeploys(); err != nil {
		log.Error("closing interrupted deploys", "err", err)
	} else if n > 0 {
		log.Info("marked interrupted deploys as failed", "count", n)
	}

	// A restored database was written on another machine — possibly another
	// network, another house. Everything address-shaped in it self-heals or
	// raises issues (mount retries, DNS reconcile, unreachable-source
	// notices), but the operator deserves the checklist up front instead of
	// discovering it one alarm at a time.
	if restoredFromKit {
		st.CreateNotice("restored-verify-addresses", "",
			"Restored from a recovery kit — verify addresses",
			"This configuration was saved on another machine, possibly on another network. Worth a minute each: "+
				"NAS shares (Settings → Storage → Disks & shares — edit any whose server moved), the backup directory, "+
				"the update source (Settings → Server → Updates), and the Domain wizard's DNS step if this server's address changed. "+
				"Fog retries mounts and re-points DNS automatically; only addresses that actually changed need an edit.")
	}

	// A USB install stages the admin account and server identity; apply it
	// before the HTTP surface comes up so the first request already sees a
	// configured server. A rejected file is set aside, never fatal.
	if err := firstboot.Consume(cfg.DataDir, st, log); err != nil {
		log.Error("firstboot preseed", "err", err)
	}

	if dirtyBoot {
		// Power loss or a hard restart. Verify state and say so honestly —
		// a verified fact if the check passed, a warning if it didn't.
		if verdict, err := st.QuickCheck(); err != nil || verdict != "ok" {
			log.Error("dirty boot: database integrity check failed", "verdict", verdict, "err", err)
			st.CreateNotice("dirty-boot", "", "Recovered from an unexpected shutdown — check needed",
				"The server lost power or was hard-restarted, and Fog's database did not pass its integrity check. "+
					"Apps are running from their own state, but review recent changes and make sure a recent recovery kit exists (Settings → Storage → Recovery kit).")
		} else {
			log.Info("dirty boot: state verified after unclean shutdown")
			st.CreateNotice("dirty-boot", "", "Recovered from an unexpected shutdown",
				"The server lost power or was hard-restarted. Fog checked its database, everything passed, and all apps were brought back normally. Nothing needs doing — dismiss this notice.")
		}
	}

	vlt, err := vault.Open(cfg.DataDir)
	if err != nil {
		log.Error("opening vault", "err", err)
		st.Close()
		return false
	}
	d.vlt = vlt
	return true
}

// shutdown stops fogd. The order matters:
//   - the VPN withdraws its route before cancelRoot, while tailscaled is
//     still up to hear it;
//   - the HTTP server stops taking requests before the server's background
//     tasks are waited for, so no request starts one mid-wait;
//   - those tasks (deploys, notices) and fogd's own loops and hooks finish
//     before the store closes, all within one ten-second budget;
//   - the clean-stop marker lands last, once the store is closed.
func (d *daemon) shutdown(cancelRoot context.CancelFunc) {
	sdnotify.Notify("STOPPING=1")
	d.vpnMgr.Shutdown()
	cancelRoot()
	d.tun.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d.srv.Shutdown(ctx)
	// Requests have stopped; now the deploys and notices they started. They
	// stop at their next checkpoint, and must before the store closes.
	if err := d.api.Shutdown(ctx); err != nil {
		d.log.Warn("background tasks still running at exit", "err", err)
	}
	// The loops the start steps launched and the work the hooks set off saw
	// cancelRoot above; each finishes the pass it is in.
	if left := d.tasks.wait(ctx); len(left) > 0 {
		d.log.Warn("daemon tasks still running at exit", "tasks", left)
	}
	d.st.Close()
	bootmark.EndClean(d.cfg.DataDir)
	d.log.Info("fogd stopped")
}

// serve starts the HTTP listener and tells systemd fogd is ready. A listen
// failure arrives on the returned channel.
func (d *daemon) serve() <-chan error {
	listenErr := make(chan error, 1)
	// Under fogd.socket, systemd holds the port and hands it over, so it is
	// never free between one fogd and the next (a self-update's restart);
	// without it, fogd binds FOG_LISTEN itself.
	ln, err := sdnotify.Listener()
	if err != nil {
		d.log.Warn("socket activation", "err", err)
	}
	d.spawn("http server", func(context.Context) {
		var err error
		if ln != nil {
			d.log.Info("fogd listening", "addr", ln.Addr().String(), "from", "fogd.socket", "version", config.Version, "data", d.cfg.DataDir)
			err = d.srv.Serve(ln)
		} else {
			d.log.Info("fogd listening", "addr", d.cfg.ListenAddr, "version", config.Version, "data", d.cfg.DataDir)
			err = d.srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	})
	sdnotify.Notify("READY=1")
	// Under WatchdogSec, heartbeat only while the database answers: a wedged
	// store means a wedged fogd, and systemd's restart is the recovery.
	if wd, ok := sdnotify.WatchdogInterval(); ok {
		d.spawn("watchdog", func(ctx context.Context) {
			loop.Every(ctx, wd/2, wd/2, func(context.Context) {
				if _, err := d.st.GetSetting(store.SettingSetupDone); err == nil {
					sdnotify.Notify("WATCHDOG=1")
				}
			})
		})
	}
	return listenErr
}

// spawn runs fn on its own goroutine under the root context, counted so
// shutdown waits for it before the store closes (see tasks).
func (d *daemon) spawn(name string, fn func(context.Context)) {
	d.tasks.spawn(d.ctx, name, fn)
}

// track runs fn here, under the root context, counted the same way: for a
// hook that a component already calls on a goroutine of its own.
func (d *daemon) track(name string, fn func(context.Context)) {
	d.tasks.run(d.ctx, name, fn)
}

// listenPort is the port fogd listens on, or "" when the address has none.
func (d *daemon) listenPort() string {
	if _, port, err := net.SplitHostPort(d.cfg.ListenAddr); err == nil {
		return port
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
