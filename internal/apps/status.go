// What the UI polls: one status row per app, merged from the store, Docker,
// the update checker, and the health and stats samplers.
package apps

import (
	"context"
	"strings"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- status ----

type AppStatus struct {
	// ID is the app's store row, so a caller joining other per-app reads
	// needn't list the apps again to map names to IDs.
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DesiredState string `json:"desiredState"`
	State        string `json:"state"`  // running | exited | missing | …
	Detail       string `json:"detail"` // "Up 2 hours"
	Healthy      *bool  `json:"healthy,omitempty"`
	// Summed over all of the app's running containers (see AppUsage), CPU
	// as a share of the whole host. Nil until the background sampler has a
	// fresh reading for one of them. Zero would be a claim — "this app is
	// using no CPU" — and the UI must be able to tell that apart from "we
	// haven't measured yet".
	CPUPercent *float64 `json:"cpuPercent,omitempty"`
	MemUsage   *uint64  `json:"memUsage,omitempty"`
	// The containers' memory ceilings summed: the spec's limit where one is
	// set, otherwise the host's RAM, and never more than the host has —
	// the honest denominator for "1.4 / 8 GB".
	MemLimit *uint64 `json:"memLimit,omitempty"`
	// Bytes per second between the sampler's last two readings, summed like
	// the rest. Nil until two readings exist for one of the containers.
	NetInBps        *float64 `json:"netInBps,omitempty"`
	NetOutBps       *float64 `json:"netOutBps,omitempty"`
	DiskReadBps     *float64 `json:"diskReadBps,omitempty"`
	DiskWriteBps    *float64 `json:"diskWriteBps,omitempty"`
	CurrentRev      int64    `json:"currentRev"`
	Image           string   `json:"image,omitempty"`
	UpdateAvailable bool     `json:"updateAvailable"`
	// Short human-readable versions when an update is available: the running
	// commit/digest and the one waiting on the remote.
	UpdateFrom string `json:"updateFrom,omitempty"`
	UpdateTo   string `json:"updateTo,omitempty"`
	Deploying  bool   `json:"deploying"`
	// An armed restore is staged in the app's volumes: it either has not been
	// consumed yet or the app is failing to consume it and retrying on every
	// boot. Reported from disk, so it survives a fogd restart — otherwise a
	// restore that outlives the process that started it becomes invisible,
	// and there is nothing on screen to cancel.
	RestorePending bool `json:"restorePending"`
	HostPort       int  `json:"hostPort"`
	// Every hostname the proxy serves this app on, resolved against the base
	// domain. Sent because a route may override the <app>.<domain> default —
	// callers that guess from the name alone are wrong for those apps. Empty
	// when the app has no routes or no base domain is configured yet.
	Hosts []string `json:"hosts,omitempty"`
	// Open+regressed Flare error groups. Nil when the count query failed —
	// absent, not zero, so the dashboard can't show a clean slate it hasn't
	// earned.
	OpenErrors *int64 `json:"openErrors,omitempty"`
}

// ShortRef trims a commit sha or "sha256:…" digest to a display length —
// the one rule, so a version reads identically wherever it is shown.
func ShortRef(ref string) string {
	if i := strings.Index(ref, ":"); i >= 0 {
		ref = ref[i+1:]
	}
	if len(ref) > 12 {
		ref = ref[:12]
	}
	return ref
}

// Statuses returns live container status for all apps in one Docker call.
// Resource figures come from the background sampler (see stats.go), so this
// stays a single round-trip no matter how many apps are running.
func (e *Engine) Statuses(ctx context.Context, withStats bool) ([]AppStatus, error) {
	list, err := e.Store.ListApps()
	if err != nil {
		return nil, err
	}
	containers, err := e.AppContainers(ctx)
	if err != nil {
		containers = nil // Docker down: report all apps as missing
	}
	// Status reflects the main container — never a worker or aux sidecar —
	// but resource figures are the whole app's, sidecars included.
	byApp := dockerx.MainContainers(containers)
	var usages map[string]AppUsage
	if withStats {
		usages = e.appUsages(containers)
	}
	health := e.healthSnapshot()
	// One query for every app's latest spec — this runs on the dashboard's
	// poll interval, and per-app LatestSpec calls made the poll O(apps) SQL
	// round-trips. A failed batch degrades to no Hosts, like a failed
	// per-app read used to.
	specs, _ := LatestSpecs(e.Store)
	updates, _ := e.Store.AppUpdates()
	// Error-group counts feed the dashboard badges. On failure the field
	// stays nil for every app — unknown, not "no errors".
	errCounts, errCountsErr := e.Store.CountOpenErrorGroups()
	domain, _ := e.Store.GetSetting(store.SettingBaseDomain)

	out := make([]AppStatus, 0, len(list))
	for _, a := range list {
		st := AppStatus{ID: a.ID, Name: a.Name, DesiredState: a.DesiredState, State: "missing", CurrentRev: a.CurrentRev, HostPort: a.HostPort}
		if spec := specs[a.ID]; spec != nil {
			for _, r := range spec.Routes {
				if h := RouteHost(a.Name, r, domain); h != "" {
					st.Hosts = append(st.Hosts, h)
				}
			}
		}
		if errCountsErr == nil {
			n := errCounts[a.ID]
			st.OpenErrors = &n
		}
		if u, ok := updates[a.ID]; ok && u.Available {
			st.UpdateAvailable = true
			st.UpdateFrom = ShortRef(u.LocalDigest)
			st.UpdateTo = ShortRef(u.RemoteDigest)
		}
		// Never successfully deployed = still in setup, not "missing".
		if a.CurrentRev == 0 {
			st.State = "new"
		}
		st.Deploying = e.Deploying(a.Name)
		if e.RestoreStaged != nil {
			st.RestorePending = e.RestoreStaged(a.Name)
		}
		if c, ok := byApp[a.Name]; ok {
			st.State = c.State
			st.Detail = c.Status
			st.Image = c.Image
			if u := usages[a.Name]; withStats && c.State == "running" && u.Measured {
				cpu, mem := u.CPUPercent, u.MemUsage
				st.CPUPercent, st.MemUsage = &cpu, &mem
				if u.MemLimit > 0 {
					lim := u.MemLimit
					st.MemLimit = &lim
				}
				if r := u.Rates; r != nil {
					in, out, rd, wr := r.NetIn, r.NetOut, r.DiskRead, r.DiskWrite
					st.NetInBps, st.NetOutBps, st.DiskReadBps, st.DiskWriteBps = &in, &out, &rd, &wr
				}
			}
		}
		if h, ok := health[a.Name]; ok {
			healthy := h
			st.Healthy = &healthy
		}
		out = append(out, st)
	}
	return out, nil
}
