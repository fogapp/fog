// The household event stream and app-to-app connections on the app-facing
// surface (docs/events-contract.md). Any app may publish; reading the
// stream, and being handed another app's connection, takes a uses grant in
// the reader's spec — the operator's say-so, visible on its app page.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"gitea.lan/matthew/fog/internal/appevents"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// maxEventWait caps a long poll: under the 120s idle timeout fogd's server
// keeps, with room for the client's own.
const maxEventWait = 60 * time.Second

// handleAppEventsPublish takes one CloudEvent (structured JSON) or a batch
// (a JSON array), and appends it to the stream as the calling app.
func (s *Server) handleAppEventsPublish(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, appevents.MaxBatch*appevents.MaxDataSize+64<<10))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	var evs []appevents.Event
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		err = json.Unmarshal(trimmed, &evs)
	} else {
		var one appevents.Event
		err = json.Unmarshal(trimmed, &one)
		evs = []appevents.Event{one}
	}
	if err != nil {
		badRequest(w, "body must be a CloudEvent or an array of them")
		return
	}
	n, err := s.events.Publish(app.Name, evs)
	if errors.Is(err, appevents.ErrInvalid) {
		badRequest(w, err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": len(evs), "new": n})
}

// handleAppEventsRead pages the stream forward: ?after=<cursor> (omit it to
// start from now), ?limit, and ?wait=<seconds> to long-poll for the next
// event when there is none yet.
func (s *Server) handleAppEventsRead(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	spec, ok := s.appSpec(w, app)
	if !ok {
		return
	}
	if !spec.UsesGrant(apps.UsesEvents) {
		forbidden(w, `reading events needs "events" in this app's uses`)
		return
	}
	q := r.URL.Query()
	after := int64(-1)
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			badRequest(w, "after must be a cursor from a previous read")
			return
		}
		after = n
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	waitSec, _ := strconv.Atoi(q.Get("wait"))
	wait := min(time.Duration(max(waitSec, 0))*time.Second, maxEventWait)
	page, err := s.events.Read(r.Context(), after, limit, wait)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// appConnection is one app the caller may use, as the caller sees it.
// Problem says, in words an operator can act on, why it can't be used yet.
type appConnection struct {
	App        string `json:"app"`
	URL        string `json:"url,omitempty"`
	Credential string `json:"credential,omitempty"`
	Problem    string `json:"problem,omitempty"`
	// Paths translates the provider's container paths into the caller's,
	// for every folder both mount; Unseen names the provider's shared
	// volumes the caller has no view of (docs/files-contract.md, "Wiring").
	Paths  []apps.PathRow `json:"paths"`
	Unseen []string       `json:"unseen"`
}

// handleAppConnections hands the caller every app named in its uses list:
// where to reach it on the fog network and the credential it declared. An
// app named but not installed, or installed without a connection, is listed
// with the problem rather than left out, so a consumer can say why.
func (s *Server) handleAppConnections(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	spec, ok := s.appSpec(w, app)
	if !ok {
		return
	}
	out := []appConnection{}
	for _, name := range spec.Uses {
		if apps.SurfaceGrant(name) {
			continue
		}
		c, err := s.connectionFor(name, spec)
		if err != nil {
			writeError(w, err)
			return
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (s *Server) connectionFor(name string, consumer *apps.AppSpec) (appConnection, error) {
	c := appConnection{App: name, Paths: []apps.PathRow{}, Unseen: []string{}}
	target, err := s.store.AppByName(name)
	if errors.Is(err, store.ErrNotFound) {
		c.Problem = "not installed"
		return c, nil
	}
	if err != nil {
		return c, err
	}
	_, spec, err := apps.SpecAt(s.store, target.ID, 0)
	if err != nil {
		return c, err
	}
	s.connectionPaths(&c, consumer, spec)
	conn := spec.Connection
	if conn == nil {
		c.Problem = "offers no connection"
		return c, nil
	}
	c.URL = "http://" + name + ":" + strconv.Itoa(conn.Port) + conn.Path
	if conn.Secret != "" {
		v, err := s.secrets().Get(target.ID, conn.Secret)
		switch {
		case errors.Is(err, store.ErrNotFound) || (err == nil && v == ""):
			c.Problem = "set the " + conn.Secret + " secret on " + name + " (Config → Secrets)"
		case err != nil:
			return c, err
		default:
			c.Credential = v
		}
	}
	return c, nil
}

// appSpec loads the calling app's current spec, where its grants live.
func (s *Server) appSpec(w http.ResponseWriter, app *store.App) (*apps.AppSpec, bool) {
	_, spec, err := apps.SpecAt(s.store, app.ID, 0)
	if err != nil {
		writeError(w, err)
		return nil, false
	}
	return spec, true
}
