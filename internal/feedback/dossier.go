package feedback

import (
	"encoding/json"

	"gitea.lan/matthew/fog/internal/logring"
	"gitea.lan/matthew/fog/internal/metrics"
	"gitea.lan/matthew/fog/internal/store"
)

// Dossier is everything fog knew when a report was written. Each part is
// read at that moment and never refreshed: a dossier is a photograph, and
// the report page shows it as one. A part fog could not read is named in
// Gaps with the reason, so an empty list always means "there was nothing",
// never "we didn't manage to look".
type Dossier struct {
	CapturedAt string `json:"capturedAt"`
	Page       Page   `json:"page"`
	Fog        Fog    `json:"fog"`
	// Host is the newest utilization sample, when the sampler has one.
	Host *metrics.Sample `json:"host,omitempty"`
	// Client is what the browser saw: its environment and its recent trail
	// of console errors, failed requests and navigations. Passed through as
	// the browser sent it, bounded in size.
	Client json.RawMessage `json:"client,omitempty" ts:"import(\"./lib/clientTrail\").ClientSnapshot"`
	// App is the app on screen, when the page was an app's.
	App *App `json:"app,omitempty"`
	// Logs is fogd's recent tail; Warnings is every warning and error still
	// in the ring, which reaches further back than the tail does.
	Logs     []logring.Record   `json:"logs"`
	Warnings []logring.Record   `json:"warnings"`
	Errors   []Error            `json:"errors"`
	Issues   []Issue            `json:"issues"`
	Notices  []Notice           `json:"notices"`
	Audit    []store.AuditEntry `json:"audit"`
	Peers    []Peer             `json:"peers"`
	Gaps     []string           `json:"gaps,omitempty"`
}

type Page struct {
	Path   string `json:"path"`
	Search string `json:"search,omitempty"`
	Title  string `json:"title,omitempty"`
}

type Fog struct {
	Version    string `json:"version"`
	ServerName string `json:"serverName,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	OS         string `json:"os"`
	GoVersion  string `json:"goVersion"`
	StartedAt  string `json:"startedAt,omitempty"`
	Goroutines int    `json:"goroutines"`
	HeapBytes  uint64 `json:"heapBytes"`
}

type App struct {
	Name           string         `json:"name"`
	DesiredState   string         `json:"desiredState"`
	CurrentRev     int64          `json:"currentRev"`
	DeployedCommit string         `json:"deployedCommit,omitempty"`
	Containers     []Container    `json:"containers"`
	Logs           string         `json:"logs,omitempty"`
	Deploys        []store.Deploy `json:"deploys"`
}

type Container struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
}

type Error struct {
	App      string `json:"app"`
	Origin   string `json:"origin,omitempty"`
	Title    string `json:"title"`
	Culprit  string `json:"culprit,omitempty"`
	Level    string `json:"level"`
	Status   string `json:"status"`
	Events   int64  `json:"events"`
	LastSeen string `json:"lastSeen"`
	Release  string `json:"release,omitempty"`
}

type Issue struct {
	App      string `json:"app,omitempty"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	LastSeen string `json:"lastSeen"`
}

type Notice struct {
	Title     string `json:"title"`
	App       string `json:"app,omitempty"`
	CreatedAt string `json:"createdAt"`
}

type Peer struct {
	Name       string `json:"name"`
	FogVersion string `json:"fogVersion,omitempty"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
	UnpairedAt string `json:"unpairedAt,omitempty"`
}
