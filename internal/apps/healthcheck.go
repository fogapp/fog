package apps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// HealthCheckResult is one on-demand probe of an app's health path, reported
// in full so the operator sees what the monitor sees — not just pass/fail.
type HealthCheckResult struct {
	OK          bool   `json:"ok"`
	URL         string `json:"url,omitempty"`
	Status      int    `json:"status,omitempty"`
	StatusText  string `json:"statusText,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Body        string `json:"body,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	DurationMs  int64  `json:"durationMs"`
	Error       string `json:"error,omitempty"`
}

const healthCheckBodyLimit = 4096

// CheckHealth probes the app's health path once, the way the monitor does
// (main container, fog network, HealthPort; <500 counts as healthy). path
// overrides the saved spec's path so an unsaved edit can be tried first.
func (e *Engine) CheckHealth(ctx context.Context, name, path string) HealthCheckResult {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return HealthCheckResult{Error: "no such app"}
	}
	spec, err := LatestSpec(e.Store, app.ID)
	if err != nil {
		return HealthCheckResult{Error: err.Error()}
	}
	if path == "" && spec.Health != nil {
		path = spec.Health.Path
	}
	if path == "" {
		return HealthCheckResult{Error: "no health check path is set"}
	}
	if !strings.HasPrefix(path, "/") {
		return HealthCheckResult{Error: "health path must start with /"}
	}
	base, err := e.AppBaseURL(ctx, name, spec)
	if err != nil {
		return HealthCheckResult{Error: err.Error()}
	}
	res := HealthCheckResult{URL: base + path}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", res.URL, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	start := time.Now()
	resp, err := dockerx.ProbeClient.Do(req)
	res.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, healthCheckBodyLimit+1))
	if len(body) > healthCheckBodyLimit {
		body, res.Truncated = body[:healthCheckBodyLimit], true
	}
	res.Status = resp.StatusCode
	res.StatusText = http.StatusText(resp.StatusCode)
	res.ContentType = resp.Header.Get("Content-Type")
	res.Body = string(body)
	res.OK = resp.StatusCode < 500
	if !res.OK {
		res.Error = fmt.Sprintf("responded %d", resp.StatusCode)
	}
	return res
}
