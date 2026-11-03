// fog-setup is the bootable installer's agent: it serves the setup wizard on
// the machine's own display (kiosk browser) and to any LAN browser at
// http://fog.local, inventories the hardware, and drives the install.
//
// Env:
//
//	FOG_SETUP_LISTEN  listen address (default ":80"; ":8090" in mock mode)
//	FOG_SETUP_MOCK=1  fake hardware + simulated install, for wizard dev
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/sdnotify"
	"gitea.lan/matthew/fog/internal/setup"
	"gitea.lan/matthew/fog/internal/setupui"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := os.Getenv("FOG_SETUP_MOCK") == "1"

	agent := &setup.Agent{Log: log, Version: config.Version}
	// Booted from the on-disk recovery partition? Same agent, repair menu.
	if raw, err := os.ReadFile("/proc/cmdline"); err == nil && strings.Contains(string(raw), "fog.repair=1") {
		agent.Mode = "repair"
	}
	if os.Getenv("FOG_SETUP_REPAIR") == "1" {
		agent.Mode = "repair"
	}
	if mock {
		agent.Inventory = setup.MockInventory
		agent.Exec = setup.MockExecutor{}
		agent.Reboot = func() error {
			log.Info("mock reboot — the pretend machine restarts")
			return nil
		}
		agent.Display = setup.NewMockKiosk(1.25)
	} else {
		agent.Inventory = setup.LiveInventory
		agent.Exec = &setup.RealExecutor{
			// live-boot mounts the stick's payload partition here.
			PayloadDir: "/run/live/medium/fog",
			MediumDir:  "/run/live/medium",
			Version:    config.Version,
		}
		agent.Reboot = func() error { return exec.Command("systemctl", "reboot").Run() }
		// fog-kiosk's state files; the rescale unit restarts the kiosk only
		// when the wanted scale differs from the one it runs at.
		agent.Display = setup.KioskDir{
			Dir: "/run/fog-kiosk",
			Rescale: func() error {
				return exec.Command("systemctl", "--no-block", "start", "fog-kiosk-rescale.service").Run()
			},
			// fog-kiosk off stops the kiosk and tells the shell it uncovers
			// what happened; on starts it again.
			Kiosk: func(running bool) error {
				sub := "off"
				if running {
					sub = "on"
				}
				return exec.Command("/usr/local/bin/fog-kiosk", sub).Run()
			},
		}
		// The unit runs Restart=always: a panic or OOM kill mid-install comes
		// back as a blank agent while the disk sits half-written and mounted.
		// Sweep the leftovers (a retry's mkfs refuses mounted devices), and
		// when the in-flight marker says an install died with the last
		// process, start errored — the welcome screen would be a lie.
		if setup.RecoverInterruptedInstall(context.Background(), "", "", log) {
			agent.MarkInterrupted("The installer restarted in the middle of an install — the machine may have run out of memory or hit a bug, and the disk was left partly written. Nothing was harmed beyond the disk being installed to. Choose your disk again and retry the install; if this repeats, download diagnostics and reboot the machine.")
		}
	}

	addr := os.Getenv("FOG_SETUP_LISTEN")
	if addr == "" {
		addr = ":80"
		if mock {
			addr = ":8090"
		}
	}

	srv := &http.Server{
		Handler:           agent.Handler(setupui.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Bind before declaring readiness: the kiosk unit orders itself after
	// this service (Type=notify), and a chromium that starts one beat early
	// renders connection-refused forever — --kiosk pages never reload.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("listen", "addr", addr, "err", err)
		os.Exit(1)
	}
	go func() {
		log.Info("fog-setup listening", "addr", addr, "mock", mock, "version", config.Version)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()
	sdnotify.Notify("READY=1")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
