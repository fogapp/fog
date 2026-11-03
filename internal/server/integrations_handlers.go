package server

// Integrations (docs/integrations-contract.md): the HTTP face of
// internal/integrations. Each handler decodes, calls the service, and maps
// its refusals to statuses; the service holds the logic.

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/integrations"
	"gitea.lan/matthew/fog/internal/store"
)

// integrations is the service over this server's store, engine and app
// caller. Built per call: tests swap the engine and the endpoint seam after
// the server is made.
func (s *Server) integrations() *integrations.Service {
	svc := &integrations.Service{
		Store:     s.store,
		Apps:      s.appCaller().Refusing(integrations.Refusal),
		PublicURL: s.appPublicURL,
		Wire:      func(name string, spec *apps.AppSpec) { s.wireSpec(name, spec) },
		Log:       s.log,
	}
	// Left nil, not a nil *apps.Engine, so the service sees no engine.
	if s.engine != nil {
		svc.Specs = s.engine
	}
	return svc
}

// writeIntegrationError answers a service refusal with its own words and
// the status its kind means.
func writeIntegrationError(w http.ResponseWriter, err error) {
	var ie *integrations.Error
	if !errors.As(err, &ie) {
		writeError(w, err)
		return
	}
	status := http.StatusBadGateway
	switch {
	case errors.Is(ie, integrations.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(ie, integrations.ErrConflict):
		status = http.StatusConflict
	}
	writeErr(w, status, ie.Msg)
}

func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	consumer, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	out, err := s.integrations().List(r.Context(), consumer)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleConnectIntegration(w http.ResponseWriter, r *http.Request) {
	consumer, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	var req integrations.ConnectRequest
	if !readJSON(w, r, &req) {
		return
	}
	actor := s.actor(r)
	consent, err := s.integrations().Connect(r.Context(), consumer, req, returnBase(r), actor)
	if err != nil {
		writeIntegrationError(w, err)
		return
	}
	if consent != "" {
		writeJSON(w, http.StatusAccepted, map[string]any{"consentUrl": consent})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"deploying": s.deployAfterIntegration(consumer, req.Deploy, actor)})
}

// returnBase is fog's own address as the browser reaches it: the request's
// Origin (protect has already checked it is this host), else its Host.
func returnBase(r *http.Request) string {
	if o := r.Header.Get("Origin"); strings.HasPrefix(o, "https://") || strings.HasPrefix(o, "http://") {
		return o
	}
	return "https://" + r.Host
}

func (s *Server) handleDisconnectIntegration(w http.ResponseWriter, r *http.Request) {
	consumer, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	actor := s.actor(r)
	changed, err := s.integrations().Disconnect(r.Context(), consumer, r.PathValue("provider"), q.Get("force") == "1", actor)
	if err != nil {
		writeIntegrationError(w, err)
		return
	}
	// A cancelled request set nothing, so there is nothing to redeploy.
	deploy := q.Get("deploy") == "1" && changed
	writeJSON(w, http.StatusOK, map[string]any{"deploying": s.deployAfterIntegration(consumer, deploy, actor)})
}

// deployAfterIntegration redeploys a running consumer when asked: its env
// is read only at deploy. A stopped app picks it up when it next starts.
func (s *Server) deployAfterIntegration(consumer *store.App, deploy bool, actor string) bool {
	if !deploy || !integrations.Running(consumer) {
		return false
	}
	s.deployAsync("deploy after integration", consumer.Name, 0, actor)
	return true
}

// handleIntegrationReturn is where the provider sends the person back
// after they answered a consent offer. Either way they land on the app's
// Integrations tab, which says how it went.
func (s *Server) handleIntegrationReturn(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ret := s.integrations().Return(r.Context(), q.Get("state"), q.Get("result") == "approved", s.actor(r))
	if ret.Consumer == nil {
		http.Redirect(w, r, "/apps", http.StatusSeeOther)
		return
	}
	next := "/apps/" + url.PathEscape(ret.Consumer.Name) + "/integrations?" + ret.Outcome
	if ret.Connected && s.deployAfterIntegration(ret.Consumer, ret.Deploy, s.actor(r)) {
		next += "&deploying=1"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
