// The home view: GET /app/v1/home, what a start page (newtab) shows of this
// server — every app with its state, the Inbox summary and the host's
// vitals — in one read. Default-deny: every app holds a FOG_API_TOKEN, so
// only one granted "home" in its uses may read it. It is built from the
// same calls the dashboard and the Inbox badge make, so the three cannot
// disagree. See docs/home-view.md.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/catalog"
	"gitea.lan/matthew/fog/internal/store"
)

type homeView struct {
	// Self is the reading app's name, so it can leave itself out.
	Self   string     `json:"self"`
	Server homeServer `json:"server"`
	Apps   []homeApp  `json:"apps"`
	Inbox  homeInbox  `json:"inbox"`
	// Metrics is nil until the sampler has a reading.
	Metrics *homeMetrics `json:"metrics"`
}

type homeServer struct {
	// Name is the operator's name for the server; empty when unset.
	Name string `json:"name"`
	// URL is fog's own address, https://fog.<domain>; empty without a
	// domain.
	URL string `json:"url"`
}

type homeApp struct {
	Name string `json:"name"`
	// Title and Icon come from the app's store listing; an app installed
	// otherwise has its name for a title and no icon. Icon is a path fogd
	// serves without auth.
	Title           string   `json:"title"`
	Icon            string   `json:"icon,omitempty"`
	URL             string   `json:"url,omitempty"`
	State           string   `json:"state"`
	DesiredState    string   `json:"desiredState"`
	Healthy         *bool    `json:"healthy,omitempty"`
	Deploying       bool     `json:"deploying"`
	UpdateAvailable bool     `json:"updateAvailable"`
	OpenErrors      *int64   `json:"openErrors,omitempty"`
	Contracts       []string `json:"contracts,omitempty"`
}

type homeInbox struct {
	Errors    int64           `json:"errors"`
	Issues    issueSeverities `json:"issues"`
	Notices   int64           `json:"notices"`
	FogUpdate fogUpdateView   `json:"fogUpdate"`
}

type homeMetrics struct {
	CPUPct    float64 `json:"cpuPct"`
	MemUsed   uint64  `json:"memUsed"`
	MemTotal  uint64  `json:"memTotal"`
	DiskUsed  uint64  `json:"diskUsed"`
	DiskTotal uint64  `json:"diskTotal"`
}

func (s *Server) handleAppHome(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	spec, ok := s.appSpec(w, app)
	if !ok {
		return
	}
	if !spec.UsesGrant(apps.UsesHome) {
		forbidden(w, `reading the home view needs "home" in this app's uses`)
		return
	}
	view, err := s.homeView(r, app)
	if err != nil {
		writeError(w, err)
		return
	}
	body, err := json.Marshal(view)
	if err != nil {
		writeError(w, err)
		return
	}
	// A start page polls every few seconds while someone looks at it; most
	// polls find nothing changed.
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// homeStatusesTTL bounds how stale the home view's app states may be. Every
// open start page polls every few seconds, and each poll would otherwise
// cost a Docker round-trip and the status queries; two seconds is below
// what a person looking at a start page notices. The operator's own
// /api/apps is not cached: the dashboard refetches right after a start,
// stop or deploy and must see it, and the engine has no hook for those
// (OnChange fires on routing changes only) to invalidate on.
const homeStatusesTTL = 2 * time.Second

type homeStatusesResult struct {
	list []apps.AppStatus
	err  error
}

func (s *Server) homeView(r *http.Request, self *store.App) (*homeView, error) {
	// Detached from the request: the answer is shared for the TTL, and a
	// caller hanging up mid-read must not leave every app "missing" for the
	// next one.
	ctx := context.WithoutCancel(r.Context())
	res := s.homeStatuses.get(homeStatusesTTL, func() homeStatusesResult {
		list, err := s.engine.Statuses(ctx, false)
		return homeStatusesResult{list, err}
	})
	statuses, err := res.list, res.err
	if err != nil {
		return nil, err
	}
	// One query for every app's contracts. A row with corrupt contracts is
	// left out, so that app shows none rather than failing the whole view.
	contracts, err := s.store.AllAppContracts()
	if err != nil {
		return nil, err
	}
	listed := map[string]catalog.Entry{}
	for _, e := range s.catalogEntries() {
		listed[e.ID] = e
	}

	v := &homeView{Self: self.Name, Apps: make([]homeApp, 0, len(statuses))}
	v.Server.Name, _ = s.store.GetSetting(store.SettingServerName)
	v.Server.URL = s.fogURL()
	for _, st := range statuses {
		a := homeApp{
			Name: st.Name, Title: st.Name, State: st.State, DesiredState: st.DesiredState,
			Healthy: st.Healthy, Deploying: st.Deploying, UpdateAvailable: st.UpdateAvailable,
			OpenErrors: st.OpenErrors,
		}
		if e, ok := listed[st.Name]; ok {
			a.Title, a.Icon = e.Title, e.Logo
		}
		if len(st.Hosts) > 0 {
			a.URL = "https://" + st.Hosts[0]
		}
		a.Contracts = contracts[st.ID]
		v.Apps = append(v.Apps, a)
	}

	// The documented home shape has no feedback counts; the rest is the
	// Inbox summary as the badge reads it.
	sum, err := s.inboxSummary()
	if err != nil {
		return nil, err
	}
	v.Inbox = homeInbox{Errors: sum.Errors, Issues: sum.Issues, Notices: sum.Notices, FogUpdate: sum.FogUpdate}

	if s.metrics != nil {
		if m, ok := s.metrics.Latest(); ok {
			v.Metrics = &homeMetrics{CPUPct: m.CPUPct, MemUsed: m.MemUsed, MemTotal: m.MemTotal, DiskUsed: m.DiskUsed, DiskTotal: m.DiskTotal}
		}
	}
	return v, nil
}
