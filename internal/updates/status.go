// The fleet view of app updates: what the last check recorded for every
// app, with the policy that decides what happens about it. Stored facts,
// not a fresh check — CheckAll is the verb, this is the noun.
package updates

import (
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

type AppUpdateStatus struct {
	App    string `json:"app"`
	Source string `json:"source" ts:"\"image\" | \"git\" | \"\""` // image | git | "" when the spec is unreadable
	// Policy is manual, notify, or auto — the spec's empty value normalized
	// to manual, the engine's default.
	Policy string `json:"policy" ts:"\"manual\" | \"notify\" | \"auto\""`
	// Current and Available are the short refs the last check compared;
	// both empty until a check has run.
	Current         string `json:"current,omitempty"`
	Available       string `json:"available,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	// Comparable is false when the last check had nothing to compare (image
	// never pulled, app never deployed) — neither an update nor "up to date".
	Comparable bool   `json:"comparable"`
	CheckedAt  string `json:"checkedAt,omitempty"`
}

func FleetStatus(st *store.Store) ([]AppUpdateStatus, error) {
	list, err := apps.WithSpecs(st)
	if err != nil {
		return nil, err
	}
	ups, err := st.AppUpdates()
	if err != nil {
		return nil, err
	}
	out := make([]AppUpdateStatus, 0, len(list))
	for _, a := range list {
		row := AppUpdateStatus{App: a.Name, Policy: "manual"}
		if spec := a.Spec; spec != nil {
			row.Source = spec.Source.Type
			if spec.UpdatePolicy != "" {
				row.Policy = spec.UpdatePolicy
			}
		}
		if u, ok := ups[a.ID]; ok {
			row.Current, row.Available = apps.ShortRef(u.LocalDigest), apps.ShortRef(u.RemoteDigest)
			row.Comparable = u.LocalDigest != "" && u.RemoteDigest != ""
			row.UpdateAvailable = u.Available
			row.CheckedAt = u.CheckedAt
		}
		out = append(out, row)
	}
	return out, nil
}
