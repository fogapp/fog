// Feedback: the header's bug button. A person writes what surprised them or
// what they wish were different; fog files it with a dossier of what it
// knew at that moment, and sends it on to the peers this household reports
// to. Peers holding a feedback.ingest grant deliver theirs here. What
// becomes of a report travels back the way its origin dials: in the
// heartbeat, where the receiver's word on each report it holds rides along
// and the origin mirrors it onto the row the person can see.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/feedback"
	"gitea.lan/matthew/fog/internal/logring"
	"gitea.lan/matthew/fog/internal/store"
)

// Dossier budgets. The whole thing must fit feedback.MaxDossier, which is
// also what a peer accepts; the parts are sized so it rarely has to shrink.
const (
	dossierLogTail      = 200
	dossierWarnings     = 50
	dossierAppLogLines  = "150"
	dossierAppLogBytes  = 64 << 10
	dossierClientBytes  = 256 << 10
	dossierListLimit    = 25
	dossierAuditLimit   = 30
	feedbackSendTimeout = 5 * time.Second
	// feedbackStatusLimit bounds what one heartbeat says about a peer's
	// reports: the newest changes. Older ones have long since been mirrored.
	feedbackStatusLimit = 500
)

var appPathRe = regexp.MustCompile(`^/apps/([^/]+)`)

type feedbackCreateReq struct {
	Body string        `json:"body"`
	Page feedback.Page `json:"page"`
	// Client is the browser's own account of itself (lib/clientTrail.ts).
	Client json.RawMessage `json:"client"`
}

// feedbackDeliveryView names the peer a delivery is for, so the page never
// has to join against the federation list to say where a report went.
type feedbackDeliveryView struct {
	store.FeedbackDelivery
	Peer string `json:"peer"`
}

type feedbackView struct {
	store.Feedback
	// Origin is the relaying peer's display name; empty for local reports.
	Origin string `json:"origin,omitempty"`
	// StatusBy is the display name of the peer whose word set the status;
	// empty when it was set here.
	StatusBy   string                 `json:"statusBy,omitempty"`
	Deliveries []feedbackDeliveryView `json:"deliveries,omitempty"`
}

// feedbackAttached is what the create response says was captured, so the
// dialog can confirm it in words without fetching the dossier back.
type feedbackAttached struct {
	App      string   `json:"app,omitempty"`
	Logs     int      `json:"logs"`
	Warnings int      `json:"warnings"`
	Errors   int      `json:"errors"`
	Issues   int      `json:"issues"`
	Browser  bool     `json:"browser"`
	Gaps     []string `json:"gaps,omitempty"`
}

func (s *Server) handleCreateFeedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackCreateReq
	if !readJSON(w, r, &req) {
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		badRequest(w, "write something first")
		return
	}
	if utf8.RuneCountInString(body) > feedback.MaxBody {
		badRequest(w, "that's longer than "+strconv.Itoa(feedback.MaxBody)+" characters")
		return
	}
	page := req.Page.Path + req.Page.Search
	if len(page) > feedback.MaxPage {
		page = page[:feedback.MaxPage]
	}
	// Where it goes is read before anything is written: a report must not
	// be filed under a consent that could not be read.
	dests, err := feedback.Destinations(s.store)
	if err != nil {
		writeError(w, err)
		return
	}
	uid, err := fed.NewULID()
	if err != nil {
		writeError(w, err)
		return
	}

	d := s.buildDossier(r.Context(), req)
	raw, err := json.Marshal(d)
	if err != nil {
		writeError(w, err)
		return
	}

	f, err := s.store.CreateFeedback(store.Feedback{
		UID: uid, Author: s.actor(r), Body: body, Page: page,
		FogVersion: config.Version, Dossier: string(raw),
	}, dests)
	if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "feedback.create", strconv.FormatInt(f.ID, 10), map[string]any{"page": page, "peers": len(dests)})

	// Try the sends while the person is still looking at the dialog, so it
	// can say "delivered" rather than "queued" in the common case. Bounded:
	// a peer that is away costs a few seconds, then the loop takes over.
	if len(dests) > 0 && s.feedbackFwd != nil {
		ctx, cancel := context.WithTimeout(r.Context(), feedbackSendTimeout)
		s.feedbackFwd.SendNow(ctx, f.ID)
		cancel()
	}

	view, err := s.feedbackView(f)
	if err != nil {
		writeError(w, err)
		return
	}
	view.Dossier = ""
	app := ""
	if d.App != nil {
		app = d.App.Name
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"feedback": view,
		"attached": feedbackAttached{
			App: app, Logs: len(d.Logs), Warnings: len(d.Warnings), Errors: len(d.Errors),
			Issues: len(d.Issues), Browser: len(d.Client) > 0, Gaps: d.Gaps,
		},
	})
}

// buildDossier reads every source at once. Nothing here fails the report:
// a source that can't be read becomes a named gap.
func (s *Server) buildDossier(ctx context.Context, req feedbackCreateReq) *feedback.Dossier {
	d := &feedback.Dossier{
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Page:       req.Page,
		Logs:       []logring.Record{},
		Warnings:   []logring.Record{},
		Errors:     []feedback.Error{},
		Issues:     []feedback.Issue{},
		Notices:    []feedback.Notice{},
		Audit:      []store.AuditEntry{},
		Peers:      []feedback.Peer{},
	}
	gap := func(what string, err error) { d.Gaps = append(d.Gaps, what+": "+err.Error()) }

	// fogd itself.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	d.Fog = feedback.Fog{
		Version: config.Version, OS: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version(),
		Goroutines: runtime.NumGoroutine(), HeapBytes: mem.HeapAlloc,
	}
	d.Fog.ServerName, _ = s.store.GetSetting(store.SettingServerName)
	d.Fog.Hostname, _ = os.Hostname()
	if !s.startedAt.IsZero() {
		d.Fog.StartedAt = s.startedAt.UTC().Format(time.RFC3339)
	}
	if s.metrics != nil {
		if series := s.metrics.Series(); len(series) > 0 {
			d.Host = &series[len(series)-1]
		}
	}

	// The browser's account, passed through when it is well-formed and sane.
	if len(req.Client) > 0 && string(req.Client) != "null" {
		switch {
		case len(req.Client) > dossierClientBytes:
			d.Gaps = append(d.Gaps, "browser trail: too large to attach, left out")
		case !json.Valid(req.Client):
			d.Gaps = append(d.Gaps, "browser trail: not valid JSON, left out")
		default:
			d.Client = req.Client
		}
	}

	// fogd's log tail.
	if s.logRing == nil {
		d.Gaps = append(d.Gaps, "fogd logs: not captured by this process")
	} else {
		d.Logs = s.logRing.Recent(dossierLogTail)
		for _, rec := range s.logRing.Recent(0) {
			if rec.Level == "WARN" || rec.Level == "ERROR" {
				d.Warnings = append(d.Warnings, rec)
			}
		}
		if n := len(d.Warnings); n > dossierWarnings {
			d.Warnings = d.Warnings[n-dossierWarnings:]
		}
	}

	// What the Inbox holds right now.
	peerNames := map[int64]string{}
	if peers, err := s.store.ListFedPeers(); err != nil {
		gap("peers", err)
	} else {
		for _, p := range peers {
			peerNames[p.ID] = p.DisplayName()
			d.Peers = append(d.Peers, feedback.Peer{Name: p.DisplayName(), FogVersion: p.FogVersion,
				LastSeenAt: p.LastSeenAt, UnpairedAt: p.UnpairedAt})
		}
	}
	if groups, err := s.store.ListOpenErrorGroupsAll(dossierListLimit); err != nil {
		gap("errors", err)
	} else {
		for _, g := range groups {
			d.Errors = append(d.Errors, feedback.Error{App: g.AppName, Origin: peerNames[g.PeerID], Title: g.Title,
				Culprit: g.Culprit, Level: g.Level, Status: g.Status, Events: g.EventsCount,
				LastSeen: g.LastSeen, Release: g.LastRelease})
		}
	}
	if issues, err := s.store.ListIssues([]string{"open"}, dossierListLimit); err != nil {
		gap("issues", err)
	} else {
		for _, i := range issues {
			d.Issues = append(d.Issues, feedback.Issue{App: i.AppName, Severity: i.Severity, Title: i.Title,
				Detail: i.Detail, LastSeen: i.LastSeen})
		}
	}
	if notices, err := s.store.ActiveNotices(); err != nil {
		gap("notices", err)
	} else {
		for _, n := range notices {
			d.Notices = append(d.Notices, feedback.Notice{Title: n.Title, App: n.App, CreatedAt: n.CreatedAt})
		}
	}
	if audit, err := s.store.RecentAudit(dossierAuditLimit); err != nil {
		gap("audit log", err)
	} else if audit != nil {
		d.Audit = audit
	}

	// The app on screen.
	if m := appPathRe.FindStringSubmatch(req.Page.Path); m != nil && m[1] != "new" {
		s.dossierApp(ctx, d, m[1])
	}

	s.fitDossier(d)
	return d
}

func (s *Server) dossierApp(ctx context.Context, d *feedback.Dossier, rawName string) {
	name := rawName
	if u, err := url.PathUnescape(rawName); err == nil {
		name = u
	}
	app, err := s.store.AppByName(name)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			d.Gaps = append(d.Gaps, "app "+name+": "+err.Error())
		}
		return
	}
	a := &feedback.App{Name: app.Name, DesiredState: app.DesiredState, CurrentRev: app.CurrentRev,
		DeployedCommit: app.DeployedCommit, Containers: []feedback.Container{}, Deploys: []store.Deploy{}}
	d.App = a
	// Through the engine, which redacts what old logs may hold; without one
	// (a fogd still starting), no logs rather than unredacted ones.
	if s.engine == nil {
		d.Gaps = append(d.Gaps, "deploys: the app engine is not available")
	} else if deploys, err := s.engine.DeploySummaries(app.ID, 3, 4000); err != nil {
		d.Gaps = append(d.Gaps, "deploys: "+err.Error())
	} else if deploys != nil {
		a.Deploys = deploys
	}
	if s.docker == nil {
		d.Gaps = append(d.Gaps, "containers: docker is not wired in this process")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	containers, err := s.docker.ListByLabel(cctx, "fog.app="+app.Name)
	if err != nil {
		d.Gaps = append(d.Gaps, "containers: "+err.Error())
		return
	}
	for _, c := range containers {
		cname := c.ID
		if len(c.Names) > 0 {
			cname = strings.TrimPrefix(c.Names[0], "/")
		}
		a.Containers = append(a.Containers, feedback.Container{Name: cname, Image: c.Image, State: c.State, Status: c.Status})
	}
	target, ok := dockerx.ContainerForProcess(containers, "")
	if !ok {
		return
	}
	rc, err := s.docker.Logs(cctx, target.ID, false, dossierAppLogLines, true)
	if err != nil {
		d.Gaps = append(d.Gaps, "container logs: "+err.Error())
		return
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, dossierAppLogBytes)); err != nil && buf.Len() == 0 {
		d.Gaps = append(d.Gaps, "container logs: "+err.Error())
		return
	}
	a.Logs = buf.String()
}

// fitDossier sheds the bulkiest parts, in order, until the dossier fits
// what a peer accepts — and says what it shed.
func (s *Server) fitDossier(d *feedback.Dossier) {
	size := func() int { b, _ := json.Marshal(d); return len(b) }
	steps := []struct {
		what string
		cut  func() bool
	}{
		{"app container logs", func() bool {
			if d.App == nil || d.App.Logs == "" {
				return false
			}
			d.App.Logs = ""
			return true
		}},
		{"older fogd log lines", func() bool {
			if len(d.Logs) <= 50 {
				return false
			}
			d.Logs = d.Logs[len(d.Logs)-50:]
			return true
		}},
		{"browser trail", func() bool {
			if len(d.Client) == 0 {
				return false
			}
			d.Client = nil
			return true
		}},
		{"deploy logs", func() bool {
			if d.App == nil || len(d.App.Deploys) == 0 {
				return false
			}
			for i := range d.App.Deploys {
				d.App.Deploys[i].Log = ""
			}
			return true
		}},
	}
	for _, st := range steps {
		if size() <= feedback.MaxDossier {
			return
		}
		if st.cut() {
			d.Gaps = append(d.Gaps, st.what+": left out to keep the report under 1 MB")
		}
	}
}

func (s *Server) feedbackView(f *store.Feedback) (feedbackView, error) {
	v := feedbackView{Feedback: *f}
	names, err := s.peerDisplayNames()
	if err != nil {
		return v, err
	}
	v.StatusBy = names[f.StatusByPeerID]
	if f.PeerID != 0 {
		v.Origin = names[f.PeerID]
		return v, nil
	}
	ds, err := s.store.FeedbackDeliveries(f.ID)
	if err != nil {
		return v, err
	}
	for _, d := range ds {
		v.Deliveries = append(v.Deliveries, feedbackDeliveryView{FeedbackDelivery: d, Peer: names[d.PeerID]})
	}
	return v, nil
}

func (s *Server) peerDisplayNames() (map[int64]string, error) {
	peers, err := s.store.ListFedPeers()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(peers))
	for _, p := range peers {
		out[p.ID] = p.DisplayName()
	}
	return out, nil
}

func (s *Server) handleListFeedback(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && status != "open" && status != "closed" {
		badRequest(w, "status must be open or closed")
		return
	}
	list, err := s.store.ListFeedback(status, 200)
	if err != nil {
		writeError(w, err)
		return
	}
	names, err := s.peerDisplayNames()
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]feedbackView, 0, len(list))
	for _, f := range list {
		out = append(out, feedbackView{Feedback: f, Origin: names[f.PeerID], StatusBy: names[f.StatusByPeerID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedback": out})
}

func (s *Server) feedbackFromPath(w http.ResponseWriter, r *http.Request) (*store.Feedback, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		if f, ferr := s.store.FeedbackByID(id); ferr == nil {
			return f, true
		} else if !errors.Is(ferr, sql.ErrNoRows) {
			writeError(w, ferr)
			return nil, false
		}
	}
	notFound(w, "feedback not found")
	return nil, false
}

func (s *Server) handleGetFeedback(w http.ResponseWriter, r *http.Request) {
	f, ok := s.feedbackFromPath(w, r)
	if !ok {
		return
	}
	v, err := s.feedbackView(f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSetFeedbackStatus(w http.ResponseWriter, r *http.Request) {
	f, ok := s.feedbackFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Status != "open" && req.Status != "closed" {
		badRequest(w, "status must be open or closed")
		return
	}
	if err := s.store.SetFeedbackStatus(f.ID, req.Status); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "feedback."+statusVerb(req.Status), strconv.FormatInt(f.ID, 10), nil)
	updated, err := s.store.FeedbackByID(f.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if f.PeerID != 0 {
		s.tellFeedbackOrigin(f.PeerID)
	}
	v, err := s.feedbackView(updated)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func statusVerb(status string) string {
	if status == "closed" {
		return "close"
	}
	return "reopen"
}

// tellFeedbackOrigin gets a status change to the peer that relayed the
// report. The word travels in the heartbeat, so kick one now rather than
// leave the person who wrote the report waiting out the ticker. A
// dial-in-only origin (no address here) hears it on its next dial: the
// reply payload is computed fresh.
func (s *Server) tellFeedbackOrigin(peerID int64) {
	peer, err := s.store.FedPeerByID(peerID)
	if err != nil || peer.URL == "" {
		return
	}
	p := *peer
	s.goSafe("feedback status heartbeat", func() {
		ctx, cancel := context.WithTimeout(s.bgContext(), 30*time.Second)
		defer cancel()
		if err := s.fed.HeartbeatPeer(ctx, &p); err != nil {
			s.log.Debug("fed heartbeat after feedback status change", "peer", p.Name, "err", err)
		}
	})
}

// feedbackStatusesFor is the heartbeat hook: what this instance has done
// with the reports one peer relayed here.
func (s *Server) feedbackStatusesFor(peer *store.FedPeer) []store.FeedbackStatus {
	out, err := s.store.PeerFeedbackStatuses(peer.ID, feedbackStatusLimit)
	if err != nil {
		s.log.Warn("feedback statuses for heartbeat", "peer", peer.Name, "err", err)
		return nil
	}
	return out
}

// applyPeerFeedbackStatuses is the other end: a peer's verified heartbeat
// said what it did with reports written here. Each is mirrored onto its
// row, and a change of status — someone in another household dealt with
// what this household's person wrote — reaches the phone.
func (s *Server) applyPeerFeedbackStatuses(peerID int64, statuses []store.FeedbackStatus) {
	peer, err := s.store.FedPeerByID(peerID)
	if err != nil {
		return
	}
	for _, st := range statuses {
		if (st.Status != "open" && st.Status != "closed") || st.ChangedAt == "" {
			continue
		}
		f, prev, err := s.store.ApplyPeerFeedbackStatus(peer.ID, st)
		if err != nil {
			s.log.Warn("peer feedback status not applied", "peer", peer.Name, "uid", st.UID, "err", err)
			continue
		}
		if f == nil {
			continue
		}
		s.auditAs(store.FederationActor(peer.Name), "feedback."+statusVerb(f.Status), strconv.FormatInt(f.ID, 10), nil)
		if prev != f.Status {
			s.notifyFeedbackStatus(peer, f)
		}
	}
}

func (s *Server) notifyFeedbackStatus(peer *store.FedPeer, f *store.Feedback) {
	if s.pushNotify == nil {
		return
	}
	title := peer.DisplayName() + " resolved your report"
	if f.Status == "open" {
		title = peer.DisplayName() + " reopened your report"
	}
	s.pushNotify(title, feedbackHeadline(f.Body), f.Status == "closed")
}

// feedbackHeadline is a report's first line, cut to a notification's width.
func feedbackHeadline(body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	if utf8.RuneCountInString(line) > 140 {
		line = string([]rune(line)[:140]) + "…"
	}
	return line
}

func (s *Server) handleDeleteFeedback(w http.ResponseWriter, r *http.Request) {
	f, ok := s.feedbackFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteFeedback(f.ID); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "feedback.delete", strconv.FormatInt(f.ID, 10), nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFedFeedback receives a peer's batch of reports. The caller is
// authenticated by its request signature and must hold a feedback.ingest
// grant here. Every uid in the batch is answered — accepted (including a
// retry of one already stored) or refused with the reason — and the answer
// is signed, because the sender settles its deliveries on it for good.
func (s *Server) handleFedFeedback(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedAuthed(w, r, 8<<20)
	if !ok {
		return
	}
	if _, granted, _ := s.store.FedCapabilityConfig(peer.ID, store.CapFeedbackIngest, store.DirGrant); !granted {
		forbidden(w, "this peer is not granted feedback.ingest here")
		return
	}
	if !limit(w, s.flareLimiter, "feedback:"+peer.Name, "rate limited") {
		return
	}
	var batch feedback.Batch
	if !fedDecode(w, body, &batch) {
		return
	}
	ans := feedback.Answer{Accepted: []string{}, Refused: map[string]string{}}
	fresh := 0
	var first string
	for _, it := range batch.Items {
		if err := it.Validate(); err != nil {
			if it.UID != "" {
				ans.Refused[it.UID] = err.Error()
			}
			continue
		}
		inserted, err := s.store.IngestPeerFeedback(peer.ID, store.Feedback{
			UID: it.UID, Author: it.Author, Body: strings.TrimSpace(it.Body), Page: it.Page,
			FogVersion: it.FogVersion, Dossier: string(it.Dossier), CreatedAt: it.CreatedAt,
		})
		if err != nil {
			// Not refused: a store hiccup here is ours, and the sender should
			// try again rather than give up on the report.
			s.log.Warn("peer feedback ingest", "peer", peer.Name, "err", err)
			continue
		}
		ans.Accepted = append(ans.Accepted, it.UID)
		if inserted {
			fresh++
			if first == "" {
				first = it.Body
			}
		}
	}
	s.store.TouchFedPeerSeen(peer.ID)
	if fresh > 0 {
		s.notifyFeedback(peer, fresh, first)
	}
	s.writeFedSignedJSON(w, r, ans)
}

// notifyFeedback pushes a peer's new reports to the operator's phone: someone
// in another household took the time to write, which is worth a buzz.
func (s *Server) notifyFeedback(peer *store.FedPeer, n int, first string) {
	if s.pushNotify == nil {
		return
	}
	title := "Feedback from " + peer.DisplayName()
	if n > 1 {
		title = strconv.Itoa(n) + " reports from " + peer.DisplayName()
	}
	s.pushNotify(title, feedbackHeadline(first), false)
}
