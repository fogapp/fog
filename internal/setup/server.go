package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/spa"
)

// Handler is the wizard's API. It is deliberately tiny and unauthenticated:
// it exists only inside the booted live installer, on a machine whose disk
// the person at the keyboard (or on the LAN, which for a home server is the
// same trust domain as the keyboard) is about to erase.
func (a *Agent) Handler(ui http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/setup/inventory", func(w http.ResponseWriter, r *http.Request) {
		inv, err := a.Inventory()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, inv)
	})

	mux.HandleFunc("GET /api/setup/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.State())
	})

	mux.HandleFunc("POST /api/setup/install", func(w http.ResponseWriter, r *http.Request) {
		var cfg InstallConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&cfg); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := a.Start(cfg); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/setup/kit", func(w http.ResponseWriter, r *http.Request) {
		// The live installer buffers this upload on a tmpfs-rooted system;
		// maxKitFile keeps a hostile or mistaken upload (a disk image, say)
		// from eating the RAM the install itself needs.
		r.Body = http.MaxBytesReader(w, r.Body, maxKitFile)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeErr(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("upload is larger than the %dMB limit — recovery kits are a few MB; is this the right file?", maxKitFile>>20))
				return
			}
			writeErr(w, http.StatusBadRequest, "expected a multipart upload with `kit` and `passphrase`")
			return
		}
		file, _, err := r.FormFile("kit")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "missing `kit` file")
			return
		}
		defer file.Close()
		sealed, err := io.ReadAll(file)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		info, err := a.LoadKit(sealed, r.FormValue("passphrase"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kit": info})
	})

	mux.HandleFunc("POST /api/setup/claim-token", func(w http.ResponseWriter, r *http.Request) {
		token, ok := a.TakeClaimToken()
		if !ok {
			writeErr(w, http.StatusNotFound, "no claim token available")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"claimToken": token})
	})

	mux.HandleFunc("POST /api/setup/retry", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Retry(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/setup/repair", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := a.StartRepair(req.Action); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/setup/repair/reset", func(w http.ResponseWriter, r *http.Request) {
		if err := a.resetAfterRepair(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// A plain-text diagnostics bundle, downloadable from any browser on the
	// LAN — the "save something a human can send for help" recourse.
	mux.HandleFunc("GET /api/setup/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="fog-diagnostics.txt"`)
		fmt.Fprintf(w, "fog installer diagnostics — version %s, mode %s\n\n", a.Version, a.modeLocked())
		for _, c := range [][]string{
			{"lsblk", "-f"},
			{"blkid"},
			{"ip", "addr"},
			{"dmesg", "--level=err,warn"},
		} {
			out, _ := runCmd(r.Context(), c[0], c[1:]...)
			fmt.Fprintf(w, "==== %s ====\n%s\n", strings.Join(c, " "), out)
		}
		if re, ok := a.Exec.(*RealExecutor); ok && re.Transcript != nil {
			fmt.Fprintf(w, "==== install transcript ====\n%s\n", re.Transcript.String())
		}
	})

	mux.HandleFunc("POST /api/setup/reboot", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		phase := a.phaseLocked()
		a.mu.Unlock()
		// Allowed whenever nothing is writing to the disk: after a finished
		// install, from an idle repair menu, and from the "already
		// installed" screen — where restarting (stick removed) is the whole
		// instruction.
		if phase == "installing" {
			writeErr(w, http.StatusBadRequest, "reboot is only offered once nothing is running")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rebooting": true})
		if a.Reboot != nil {
			go func() {
				time.Sleep(1 * time.Second) // let the response flush
				if err := a.Reboot(); err != nil && a.Log != nil {
					a.Log.Error("reboot", "err", err)
				}
			}()
		}
	})

	// The machine's own display: how big the kiosk draws the wizard, and the
	// screen-size buttons that overrule a wrong guess.
	mux.HandleFunc("GET /api/setup/display", func(w http.ResponseWriter, r *http.Request) {
		if a.Display == nil {
			writeErr(w, http.StatusNotFound, ErrNoKiosk.Error())
			return
		}
		st, err := a.Display.Scale()
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrNoKiosk) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("POST /api/setup/display", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scale *float64 `json:"scale"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Scale == nil {
			writeErr(w, http.StatusBadRequest, "expected {\"scale\": n}, with 0 for automatic")
			return
		}
		if !validKioskScale(*req.Scale) {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("scale must be between %g and %g, or 0 for automatic", MinKioskScale, MaxKioskScale))
			return
		}
		if a.Display == nil {
			writeErr(w, http.StatusNotFound, ErrNoKiosk.Error())
			return
		}
		restarting, err := a.Display.SetScale(*req.Scale)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrNoKiosk) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": restarting})
	})

	// Hand the machine's display to the command line (on) and back (off).
	// The recovery menu's safe-mode exit: the kiosk stops, the root shell on
	// the console shows, and nothing on the disk is mounted or changed.
	mux.HandleFunc("POST /api/setup/console", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On *bool `json:"on"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.On == nil {
			writeErr(w, http.StatusBadRequest, "expected {\"on\": true|false}")
			return
		}
		if err := a.Console(*req.On); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrNoKiosk) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "console": *req.On})
	})

	// Server-sent events: the full story so far, then live progress. Both
	// wizard surfaces watch this stream, which is what keeps the TV and the
	// phone telling the same truth at the same moment.
	mux.HandleFunc("GET /api/setup/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch, cancel := a.Subscribe()
		defer cancel()
		// A comment straight away commits the headers so EventSource fires
		// its open event even while the log is still empty.
		fmt.Fprint(w, ": connected\n\n")
		fl.Flush()
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-heartbeat.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			case p, ok := <-ch:
				if !ok {
					// The engine closed this subscriber because it fell too
					// far behind to ever catch up in-band. End the response:
					// EventSource reconnects and the replay delivers the
					// full, gapless story.
					return
				}
				raw, _ := json.Marshal(p)
				fmt.Fprintf(w, "data: %s\n\n", raw)
				fl.Flush()
			}
		}
	})

	if ui != nil {
		mux.Handle("/", ui)
	}
	// The wizard's pages get fog's CSP and framing headers like the
	// dashboard's (internal/spa/headers.go).
	return spa.SecurityHeaders(sameOriginOnly(mux))
}

// sameOriginOnly refuses cross-site browser requests. The API is
// deliberately unauthenticated on the LAN, but that trust must not extend
// to arbitrary web pages: without this, any site open in a LAN browser
// could fire a no-preflight POST at fog.local and drive a disk erase.
// Browsers always send Origin on cross-site POSTs; requests without one
// (curl, same-origin GETs) pass untouched. Hostname-only comparison, so
// dev proxies on other ports keep working.
//
// A browser's request must also name this box as its Host
// (auth.HostAllowed): a page whose DNS name was rebound to the box's address
// is "same-origin" with itself, and would otherwise pass.
func sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !strings.EqualFold(u.Hostname(), hostnameOnly(r.Host)) {
				writeErr(w, http.StatusForbidden, "cross-origin requests are not allowed")
				return
			}
		}
		browser := r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Sec-Fetch-Mode") != ""
		if browser && !auth.HostAllowed(r.Host) {
			writeErr(w, http.StatusMisdirectedRequest, "this request named a host that isn't this machine")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hostnameOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// writeErr answers with the {"error": msg} body the wizard displays.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
