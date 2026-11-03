package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// handleAppExec bridges a websocket to `docker exec -it` through a local pty.
// Binary messages carry terminal bytes both ways; text messages carry control
// JSON ({"type":"resize","cols":N,"rows":N}).
func (s *Server) handleAppExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cmd := r.URL.Query().Get("cmd")
	if cmd == "" {
		cmd = "/bin/sh"
	}
	process := r.URL.Query().Get("process")

	// With no process named, attach to the app's main container — not a
	// sidecar, which is a different program entirely.
	c, err := s.docker.ProcessContainer(r.Context(), name, process)
	if errors.Is(err, dockerx.ErrNoContainer) || (err != nil && !errors.Is(err, dockerx.ErrNoProcess)) {
		notFound(w, "no container for app")
		return
	}
	if err != nil || c.State != "running" {
		conflict(w, "container not running")
		return
	}
	target := c.ID

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// The shell command line is passed to sh -c inside the container so
	// operators can type "bundle exec rails console" naturally.
	ex := exec.Command("docker", "exec", "-it", target, "sh", "-c", cmd)
	tty, err := pty.Start(ex)
	if err != nil {
		conn.Write(r.Context(), websocket.MessageBinary, []byte("failed to start: "+err.Error()+"\r\n"))
		return
	}
	defer func() {
		tty.Close()
		if ex.Process != nil {
			ex.Process.Kill()
		}
		ex.Wait()
	}()
	s.audit(r, "app.exec", name, map[string]any{"cmd": cmd})

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Hour)
	defer cancel()

	// pty -> ws
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				conn.Close(websocket.StatusNormalClosure, "process exited")
				cancel()
				return
			}
		}
	}()

	// ws -> pty (+ control messages)
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var ctl struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 {
				pty.Setsize(tty, &pty.Winsize{Cols: ctl.Cols, Rows: ctl.Rows})
			}
			continue
		}
		if _, err := tty.Write(data); err != nil {
			return
		}
	}
}
