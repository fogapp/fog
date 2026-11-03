package server

import (
	"net/http"
	"os/exec"
	"time"
)

// Powering the host from the UI: a home server rarely has a keyboard
// attached, and "turn it off and on again" otherwise means pulling the plug.
// After a reboot fogd comes back via systemd (Restart=always +
// WantedBy=multi-user.target), remounting NAS shares on the way up; after a
// shutdown nothing comes back until someone presses the power button.

func rebootHost() error {
	if err := exec.Command("systemctl", "reboot").Run(); err != nil {
		// Non-systemd fallback (and dev machines, where it fails harmlessly).
		return exec.Command("reboot").Run()
	}
	return nil
}

func shutdownHost() error {
	if err := exec.Command("systemctl", "poweroff").Run(); err != nil {
		return exec.Command("shutdown", "-h", "now").Run()
	}
	return nil
}

func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	s.powerOff(w, r, "system.reboot", "reboot", s.rebootFn)
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	s.powerOff(w, r, "system.shutdown", "shutdown", s.shutdownFn)
}

// powerOff answers first and pulls the floor out after: the UI needs the 200
// to land so it can show "rebooting" or "shutting down" instead of a dead
// connection error.
func (s *Server) powerOff(w http.ResponseWriter, r *http.Request, action, what string, fn func() error) {
	s.audit(r, action, "", nil)
	s.log.Info(what+" requested", "user", s.actor(r))
	writeOK(w)
	time.AfterFunc(2*time.Second, func() {
		if err := fn(); err != nil {
			s.log.Error(what+" failed", "err", err)
		}
	})
}
