package apps

import (
	"encoding/json"
	"fmt"

	"gitea.lan/matthew/fog/internal/store"
)

// LatestSpec decodes an app's newest revision.
//
// Nine call sites across six packages were doing this by hand, and not
// identically: some skipped the app when the JSON failed to parse, some
// carried on with a zero spec, and one used a zero spec to tear an app down —
// which would have silently skipped its Postgres deprovisioning. Having one
// answer to "what does this app currently say it is" removes the chance of
// those drifting further apart.
func LatestSpec(st *store.Store, appID int64) (*AppSpec, error) {
	_, spec, err := SpecAt(st, appID, 0)
	return spec, err
}

// SpecAt loads and decodes one revision of an app — rev 0 means the newest —
// returning the row too, for callers that need its number or id.
func SpecAt(st *store.Store, appID, rev int64) (*store.AppRevision, *AppSpec, error) {
	var r *store.AppRevision
	var err error
	if rev == 0 {
		r, err = st.LatestRevision(appID)
	} else {
		r, err = st.RevisionByRev(appID, rev)
	}
	if err != nil {
		return nil, nil, err
	}
	spec, err := ParseSpec(r.Spec)
	if err != nil {
		return nil, nil, fmt.Errorf("app %d rev %d: %w", appID, r.Rev, err)
	}
	return r, spec, nil
}

// ParseSpec decodes a stored revision's spec JSON. A row that doesn't parse
// is an error, never a zero spec: an empty spec rendered, saved back or
// deployed would quietly undo the real configuration.
func ParseSpec(specJSON string) (*AppSpec, error) {
	spec := &AppSpec{}
	if err := json.Unmarshal([]byte(specJSON), spec); err != nil {
		return nil, fmt.Errorf("stored configuration is not valid JSON: %w", err)
	}
	return spec, nil
}

// LatestSpecs is LatestSpec for every app at once — one SQL round-trip
// instead of one per app, for the callers that sweep the whole app list on a
// timer (the dashboard's 5s status poll, the health tick, directory
// heartbeats). Per app it answers exactly what LatestSpec answers: an app
// with no revisions or with unparseable JSON — LatestSpec's two error cases —
// simply has no entry, so `specs[id]` being nil means what `err != nil` means
// there.
func LatestSpecs(st *store.Store) (map[int64]*AppSpec, error) {
	raw, err := st.LatestRevisionSpecs()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*AppSpec, len(raw))
	for appID, specJSON := range raw {
		spec, err := ParseSpec(specJSON)
		if err != nil {
			continue
		}
		out[appID] = spec
	}
	return out, nil
}

// AppWithSpec is an app row next to its newest spec. Spec is nil exactly
// where LatestSpec would error: no revision yet, or one that doesn't parse.
type AppWithSpec struct {
	store.App
	Spec *AppSpec
}

// WithSpecs lists every app, in ListApps order, each with its newest spec —
// two queries whatever the app count. Sweeping the app list and calling
// LatestSpec per app was written out by hand at a score of call sites, some
// of them on timers, and each cost a round-trip per app.
func WithSpecs(st *store.Store) ([]AppWithSpec, error) {
	list, err := st.ListApps()
	if err != nil {
		return nil, err
	}
	specs, err := LatestSpecs(st)
	if err != nil {
		return nil, err
	}
	out := make([]AppWithSpec, len(list))
	for i, a := range list {
		out[i] = AppWithSpec{App: a, Spec: specs[a.ID]}
	}
	return out, nil
}
