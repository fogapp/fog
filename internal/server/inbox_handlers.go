// The Inbox summary: how much is waiting for a human, in one shape, for
// the sidebar badge and the header bell. It reads the same stores the
// Inbox tabs read, so the count and the page can never disagree.
package server

import (
	"net/http"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// inboxSummary is how much is waiting for a human. Its fields are in
// alphabetical order, so /api/inbox/summary reads byte for byte as it did
// when it was built as a map.
type inboxSummary struct {
	Errors int64 `json:"errors"`
	// Open reports, and how many of them came from peers — someone else's
	// words waiting on this household, which is what the bell counts. A
	// report written here is a note, not a to-do.
	Feedback  inboxFeedback   `json:"feedback"`
	FogUpdate fogUpdateView   `json:"fogUpdate"`
	Issues    issueSeverities `json:"issues"`
	Notices   int64           `json:"notices"`
}

type inboxFeedback struct {
	FromPeers int64 `json:"fromPeers"`
	Open      int64 `json:"open"`
}

// issueSeverities counts open configuration issues by severity.
type issueSeverities struct {
	Critical int64 `json:"critical"`
	Info     int64 `json:"info"`
	Warning  int64 `json:"warning"`
}

// fogUpdateView is the fog release row. Without an update checker there
// is nothing to say beyond "no update", so only Available is sent; with
// one, every field is, empty or not — hence pointers rather than plain
// omitempty, which would drop an empty notes or a false ignored.
type fogUpdateView struct {
	Available      bool    `json:"available"`
	CurrentVersion *string `json:"currentVersion,omitempty"`
	// Ignored: the operator dismissed this release.
	Ignored       *bool   `json:"ignored,omitempty"`
	LatestVersion *string `json:"latestVersion,omitempty"`
	Notes         *string `json:"notes,omitempty"`
}

// inboxSummary reads what the badge, the bell and the home view all show.
func (s *Server) inboxSummary() (inboxSummary, error) {
	var sum inboxSummary
	var err error
	if sum.Errors, err = s.store.CountOpenErrorGroupsAll(); err != nil {
		return sum, err
	}
	counts, err := s.store.CountOpenIssues()
	if err != nil {
		return sum, err
	}
	sum.Issues = issueSeverities{Critical: counts["critical"], Info: counts["info"], Warning: counts["warning"]}
	if sum.Notices, err = s.store.CountActiveNotices(); err != nil {
		return sum, err
	}
	if sum.Feedback.Open, sum.Feedback.FromPeers, err = s.store.CountOpenFeedback(); err != nil {
		return sum, err
	}
	sum.FogUpdate = s.fogUpdateSummary()
	return sum, nil
}

func (s *Server) handleInboxSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.inboxSummary()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// fogUpdateSummary is the fog release row, for the Inbox and the home view.
// It rides the same cache the update banner did — refreshed in the
// background when stale, never on the request path.
func (s *Server) fogUpdateSummary() fogUpdateView {
	if s.updater == nil {
		return fogUpdateView{}
	}
	s.updater.MaybeRefresh(10 * time.Minute)
	res := s.updater.Cached()
	ignored, _ := s.store.GetSetting(store.SettingIgnoredUpdateVersion)
	isIgnored := res.LatestVersion != "" && ignored == res.LatestVersion
	return fogUpdateView{
		Available:      res.UpdateAvailable,
		CurrentVersion: &res.CurrentVersion,
		Ignored:        &isIgnored,
		LatestVersion:  &res.LatestVersion,
		Notes:          &res.Notes,
	}
}
