// The main container's lifecycle facts, for the app page's "Container"
// group and its vitals: what Docker itself knows about the process fog
// runs — never anything about what the app does inside it.
package apps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
)

type ContainerFacts struct {
	// Found is false when no main container exists for the app (never
	// deployed, or removed). Every other field is then meaningless.
	Found bool   `json:"found"`
	ID    string `json:"id,omitempty"`
	// Image is the reference the container runs (repo:tag); ImageID the
	// resolved digest-style ID behind it.
	Image   string `json:"image,omitempty"`
	ImageID string `json:"imageId,omitempty"`
	State   string `json:"state,omitempty"`
	Running bool   `json:"running"`
	// RFC3339; StartedAt is the current run's start, FinishedAt the last
	// exit (empty while it has never exited).
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// The last exit code. Only meaningful once FinishedAt is set; a
	// container that has never stopped reports 0 with no exit behind it.
	ExitCode      int    `json:"exitCode"`
	OOMKilled     bool   `json:"oomKilled"`
	RestartCount  int    `json:"restartCount"`
	RestartPolicy string `json:"restartPolicy,omitempty"`
	// The health check fog runs against it, and the day of results.
	Health        *Health       `json:"health,omitempty"`
	HealthEvery   float64       `json:"healthEverySeconds"`
	HealthProbes  []HealthProbe `json:"healthProbes"`
	HealthRetainH float64       `json:"healthRetainHours"`
}

// HealthInterval is the monitor loop's cadence, set by main so the page
// can say how coarse the history is.
var HealthInterval = 30 * time.Second

// Container inspects the app's main container. Docker being down is an
// error; no container is a found=false answer.
func (e *Engine) Container(ctx context.Context, name string) (*ContainerFacts, error) {
	// The same pick as logs and exec: the running main container, or the
	// stopped one when nothing runs, so a crash keeps its exit code visible.
	main, err := e.Docker.ProcessContainer(ctx, name, "")
	found := err == nil
	if err != nil && !errors.Is(err, dockerx.ErrNoContainer) && !errors.Is(err, dockerx.ErrNoProcess) {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	facts := &ContainerFacts{
		HealthEvery:   HealthInterval.Seconds(),
		HealthProbes:  e.probeHistory(name),
		HealthRetainH: healthKeep.Hours(),
	}
	if facts.HealthProbes == nil {
		facts.HealthProbes = []HealthProbe{}
	}
	if app, err := e.Store.AppByName(name); err == nil {
		if spec, err := LatestSpec(e.Store, app.ID); err == nil && spec.Health != nil {
			h := *spec.Health
			facts.Health = &h
		}
	}
	if !found {
		return facts, nil
	}
	d, err := e.Docker.Inspect(ctx, main.ID)
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	facts.Found = true
	facts.ID = d.ID
	facts.Image = d.Config.Image
	facts.ImageID = d.Image
	facts.State = d.State.Status
	facts.Running = d.State.Running
	facts.StartedAt = zeroTime(d.State.StartedAt)
	facts.FinishedAt = zeroTime(d.State.FinishedAt)
	facts.ExitCode = d.State.ExitCode
	facts.OOMKilled = d.State.OOMKilled
	facts.RestartCount = d.RestartCount
	facts.RestartPolicy = d.HostConfig.RestartPolicy.Name
	return facts, nil
}

// Docker reports "never" as the zero time; the page must not read that as
// 1 January 0001.
func zeroTime(s string) string {
	if strings.HasPrefix(s, "0001-01-01") {
		return ""
	}
	return s
}
