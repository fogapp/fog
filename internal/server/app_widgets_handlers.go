// The fog-widgets/1 relay: GET /app/v1/widgets/{app} and /app/v1/search/{app}
// pass a start page's reads on to an app's /fog/widgets and /fog/search, so
// the start page needs no credential of the app's and no way to reach it. An
// app that may read the home view may read these: they are the rest of the
// same page. fogd keeps no cache and no schedule. It relays the ETag, and the
// reader owns both. See docs/widgets-contract.md.
package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/build"
)

const (
	// relayTimeout is less than the five seconds newtab gives fogd, so a slow
	// app is reported as one rather than as fogd not answering.
	relayTimeout = 4 * time.Second
	// maxRelayed is what newtab reads of an answer.
	maxRelayed = 256 << 10
)

func (s *Server) handleAppWidgets(w http.ResponseWriter, r *http.Request) {
	s.relayToApp(w, r, "/fog/widgets")
}

// handleAppSearch asks an app for q. Search is optional in the contract: an
// app that doesn't search answers 404, and the reader stops asking.
func (s *Server) handleAppSearch(w http.ResponseWriter, r *http.Request) {
	s.relayToApp(w, r, "/fog/search?"+url.Values{"q": {r.URL.Query().Get("q")}}.Encode())
}

// relayToApp answers r with the {app} path value's answer to path. The status
// the reader gets means something to it: 403 is its own grant, 404 is that the
// app doesn't serve this, 304 is that nothing changed. So those three come only
// from where they should, and any other way an app fails is a 502. An app
// refusing fog's token must not read as the reader not being granted.
func (s *Server) relayToApp(w http.ResponseWriter, r *http.Request, path string) {
	reader, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	spec, ok := s.appSpec(w, reader)
	if !ok {
		return
	}
	if !spec.UsesGrant(apps.UsesHome) {
		forbidden(w, `reading another app's widgets needs "home" in this app's uses`)
		return
	}
	// The same answer whether the app is missing or undeclared: either way,
	// this app has no widgets.
	target, err := s.store.AppByName(r.PathValue("app"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such app serves widgets")
		return
	}
	declared, err := s.store.AppContracts(target.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !slices.Contains(declared, build.WidgetsContract) {
		writeErr(w, http.StatusNotFound, "no such app serves widgets")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), relayTimeout)
	defer cancel()
	header := http.Header{"Accept": {"application/json"}}
	if tag := r.Header.Get("If-None-Match"); tag != "" {
		header.Set("If-None-Match", tag)
	}
	resp, err := s.appCaller().Open(ctx, target, path, header)
	if err != nil {
		msg := target.Name + " didn't answer"
		if errors.Is(err, appcall.ErrUnreachable) {
			msg = target.Name + " isn't running"
		}
		writeErr(w, http.StatusBadGateway, msg)
		return
	}
	defer resp.Body.Close()

	if tag := resp.Header.Get("ETag"); tag != "" {
		w.Header().Set("ETag", tag)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		w.WriteHeader(http.StatusNotModified)
		return
	case http.StatusNotFound:
		w.Header().Del("ETag")
		writeErr(w, http.StatusNotFound, target.Name+" doesn't serve this")
		return
	default:
		w.Header().Del("ETag")
		writeErr(w, http.StatusBadGateway, target.Name+" answered "+resp.Status)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayed+1))
	if err != nil || len(body) > maxRelayed {
		w.Header().Del("ETag")
		writeErr(w, http.StatusBadGateway, target.Name+"'s answer was unreadable or too large")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
