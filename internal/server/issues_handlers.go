// Issues: the configuration scanner's findings (the Inbox's Issues tab). The
// scanner does the detecting; these handlers only list, acknowledge, and
// trigger a manual sweep.
package server

import (
	"context"
	"net/http"

	"gitea.lan/matthew/fog/internal/issues"
	"gitea.lan/matthew/fog/internal/store"
)

// issueView decorates a stored issue with its display category.
type issueView struct {
	store.Issue
	Category string `json:"category"`
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	var statuses []string
	switch r.URL.Query().Get("status") {
	case "", "active":
		statuses = []string{"open", "acked"} // the triage default
	case "open":
		statuses = []string{"open"}
	case "acked":
		statuses = []string{"acked"}
	case "resolved":
		statuses = []string{"resolved"}
	// The Inbox's Resolved tab: everything no longer asking for attention,
	// whether a human silenced it or the world stopped reproducing it.
	case "done":
		statuses = []string{"acked", "resolved"}
	case "all":
		statuses = nil
	default:
		badRequest(w, "status must be one of active, open, acked, resolved, done, all")
		return
	}
	list, err := s.store.ListIssues(statuses, 500)
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]issueView, 0, len(list))
	for _, i := range list {
		views = append(views, issueView{Issue: i, Category: issues.Categories[i.Kind]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issues":     views,
		"lastScanAt": s.issuesLastScan(),
	})
}

func (s *Server) handleIssuesSummary(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.CountOpenIssues()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"critical":   counts["critical"],
		"warning":    counts["warning"],
		"info":       counts["info"],
		"lastScanAt": s.issuesLastScan(),
	})
}

func (s *Server) handleAckIssue(w http.ResponseWriter, r *http.Request) {
	s.issueAction(w, r, "issue.ack", s.store.AckIssue)
}

func (s *Server) handleUnackIssue(w http.ResponseWriter, r *http.Request) {
	s.issueAction(w, r, "issue.unack", s.store.UnackIssue)
}

func (s *Server) issueAction(w http.ResponseWriter, r *http.Request, action string, fn func(int64) error) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := issues.Mark(s.store, s.actor(r), action, id, fn); err != nil {
		badRequest(w, err.Error())
		return
	}
	writeOK(w)
}

func (s *Server) handleIssuesScan(w http.ResponseWriter, r *http.Request) {
	if s.issues == nil {
		writeErr(w, http.StatusServiceUnavailable, "the issues scanner is not running")
		return
	}
	s.audit(r, "issues.scan", "", nil)
	s.goSafe("issues scan", func() {
		// Detached from the request: closing the tab must not abort a sweep
		// mid-reconcile. The scanner's own mutex makes double-fire harmless.
		s.issues.Sweep(context.WithoutCancel(r.Context()))
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

func (s *Server) issuesLastScan() string {
	if s.issues == nil {
		return ""
	}
	return s.issues.LastScan()
}
