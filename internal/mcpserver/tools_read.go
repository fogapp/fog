// The read half of the tool surface: statuses, logs, issues, backups,
// audit, system health. Outputs are compact views — statuses and
// summaries, never full spec dumps and never secrets — because every byte
// lands in an LLM's context window.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/issues"
	"gitea.lan/matthew/fog/internal/metrics"
	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/store"
)

func (s *server) addReadTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_apps_list",
		Description: "List every app Fog manages with its live status: state, health, current revision, resource use, pending updates, open error count."},
		s.appsList)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_app_get",
		Description: "Detail for one app: status, source (image or git repo), latest revision, in-flight deploy progress, and recent deploy history with a diagnosis when the last deploy failed."},
		s.appGet)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_app_logs",
		Description: "The last N lines of an app's container logs (no streaming)."},
		s.appLogs)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_issues_list",
		Description: "List configuration issues found by Fog's scanner. Default shows active (open + acknowledged) issues with severity, detail, and the suggested fix."},
		s.issuesList)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_issues_summary",
		Description: "Open issue counts by severity (critical/warning/info) and when the last scan ran."},
		s.issuesSummary)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_system_status",
		Description: "Fog version, app counts, open issue counts, and the latest host metrics sample (CPU, memory, disk, load)."},
		s.systemStatus)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_backups_list",
		Description: "List an app's recorded backups, newest first."},
		s.backupsList)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_audit_recent",
		Description: "Recent entries from Fog's audit log: who did what, when."},
		s.auditRecent)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_offsite_status",
		Description: "Offsite backup targets: each bucket's immutability grade, what it holds and costs, when it last synced, and any offsite alarms (stale copies, failed syncs or verifies)."},
		s.offsiteStatus)
}

// ---- offsite ----

type offsiteTargetView struct {
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	Bucket      string  `json:"bucket"`
	Role        string  `json:"role"`
	Grade       string  `json:"grade"`
	GradeLabel  string  `json:"gradeLabel"`
	Fix         string  `json:"fix,omitempty"`
	Objects     int64   `json:"objects"`
	StoredBytes int64   `json:"storedBytes"`
	HiddenBytes int64   `json:"hiddenBytes"`
	MonthlyCost float64 `json:"monthlyCostUSD"`
	LastSyncAt  string  `json:"lastSyncAt,omitempty"`
	LastError   string  `json:"lastError,omitempty"`
}

type offsiteStatusOut struct {
	Targets []offsiteTargetView `json:"targets"`
	// Alarms are the active offsite notices: stale copies, failed syncs,
	// failed verifies.
	Alarms []string `json:"alarms"`
}

func (s *server) offsiteStatus(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, offsiteStatusOut, error) {
	out := offsiteStatusOut{Targets: []offsiteTargetView{}, Alarms: []string{}}
	targets, err := s.Store.ListOffsiteTargets()
	if err != nil {
		return nil, out, err
	}
	// A failed read leaves every target's accounting at zero, as a failed
	// per-target read did.
	allTotals, totalsErr := s.Store.AllOffsiteCopyTotals()
	for _, t := range targets {
		v := offsiteTargetView{Name: t.Name, Provider: t.Provider, Bucket: t.Bucket, Role: t.Role, Grade: t.Grade,
			GradeLabel: offsite.GradeLabel(t.Grade), LastSyncAt: t.LastSyncAt, LastError: t.LastError}
		if p := offsite.ParseProbe(t.Probe); p != nil {
			v.Fix = p.Fix
		}
		if totalsErr == nil {
			totals := allTotals[t.ID]
			v.Objects, v.StoredBytes, v.HiddenBytes = totals.Stored, totals.StoredBytes, totals.HiddenBytes
			v.MonthlyCost = float64(totals.StoredBytes+totals.HiddenBytes) / 1e12 * t.PricePerTB
		}
		out.Targets = append(out.Targets, v)
	}
	if notices, err := s.Store.ActiveNotices(); err == nil {
		for _, n := range notices {
			if strings.HasPrefix(n.Kind, "offsite-") {
				out.Alarms = append(out.Alarms, n.Title)
			}
		}
	}
	return nil, out, nil
}

// ---- apps ----

type appsListOut struct {
	Apps []apps.AppStatus `json:"apps"`
}

func (s *server) appsList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, appsListOut, error) {
	if s.Engine == nil {
		return nil, appsListOut{}, errNoEngine
	}
	statuses, err := s.Engine.Statuses(ctx, true)
	if err != nil {
		return nil, appsListOut{}, err
	}
	if statuses == nil {
		statuses = []apps.AppStatus{}
	}
	return nil, appsListOut{Apps: statuses}, nil
}

type appArg struct {
	Name string `json:"name" jsonschema:"the app's name"`
}

type deployView struct {
	Rev        int64  `json:"rev"`
	Status     string `json:"status"`
	Actor      string `json:"actor"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

type appDetailOut struct {
	Status    apps.AppStatus `json:"status"`
	Source    apps.Source    `json:"source"`
	LatestRev int64          `json:"latestRev"`
	CreatedAt string         `json:"createdAt"`
	// InFlight is the live deploy stage when one is running.
	InFlight string       `json:"inFlight,omitempty"`
	Deploys  []deployView `json:"deploys"`
	// When the most recent deploy failed: the log tail and, when the log
	// matches a known failure shape, a plain-English diagnosis.
	LastFailureLog       string          `json:"lastFailureLog,omitempty"`
	LastFailureDiagnosis *apps.Diagnosis `json:"lastFailureDiagnosis,omitempty"`
}

func (s *server) appGet(ctx context.Context, _ *mcp.CallToolRequest, in appArg) (*mcp.CallToolResult, appDetailOut, error) {
	var out appDetailOut
	if s.Engine == nil {
		return nil, out, errNoEngine
	}
	app, err := s.Store.AppByName(in.Name)
	if err != nil {
		return nil, out, fmt.Errorf("no such app %q", in.Name)
	}
	statuses, err := s.Engine.Statuses(ctx, true)
	if err != nil {
		return nil, out, err
	}
	for _, st := range statuses {
		if st.Name == app.Name {
			out.Status = st
			break
		}
	}
	out.CreatedAt = app.CreatedAt
	if rev, err := s.Store.LatestRevision(app.ID); err == nil {
		out.LatestRev = rev.Rev
		if spec, err := apps.ParseSpec(rev.Spec); err == nil {
			// Source only — the full spec carries env values and host paths
			// the model doesn't need to see for status work.
			out.Source = sourceView(spec.Source)
		}
	}
	out.InFlight = s.Engine.DeployStage(app.Name)
	deploys, _ := s.Engine.DeploySummaries(app.ID, 5, 4000)
	out.Deploys = make([]deployView, 0, len(deploys))
	for _, d := range deploys {
		out.Deploys = append(out.Deploys, deployView{
			Rev: d.Rev, Status: d.Status, Actor: d.Actor,
			StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		})
	}
	if len(deploys) > 0 && deploys[0].Status == "failed" {
		out.LastFailureLog = apps.LogTail(deploys[0].Log, 4000)
		out.LastFailureDiagnosis = apps.Diagnose(deploys[0].Log)
	}
	return nil, out, nil
}

// sourceView is an app's source as an assistant sees it: no build args
// (they carry tokens), and no password in the repo URL — an ssh:// URL can
// hold one, validation lets it through, and the model has no use for it.
func sourceView(src apps.Source) apps.Source {
	src.BuildArgs = nil
	if u, err := url.Parse(src.Repo); err == nil && u.User != nil {
		if _, has := u.User.Password(); has {
			src.Repo = u.Redacted()
		}
	}
	return src
}

// maxLogBytes bounds one logs call. Enough to hold any useful tail, small
// enough that a chatty container can't flood the client's context.
const maxLogBytes = 256 * 1024

type appLogsIn struct {
	Name    string `json:"name" jsonschema:"the app's name"`
	Tail    int    `json:"tail,omitempty" jsonschema:"how many trailing lines to return (default 200, max 1000)"`
	Process string `json:"process,omitempty" jsonschema:"a worker or sidecar process name; omit for the app's main container"`
}

type appLogsOut struct {
	Container string `json:"container"`
	Logs      string `json:"logs"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (s *server) appLogs(ctx context.Context, _ *mcp.CallToolRequest, in appLogsIn) (*mcp.CallToolResult, appLogsOut, error) {
	var out appLogsOut
	if s.Docker == nil {
		return nil, out, errNoDocker
	}
	tail := in.Tail
	if tail <= 0 {
		tail = 200
	}
	if tail > 1000 {
		tail = 1000
	}
	// Main container unless a process is named — same selection as the web
	// log and exec endpoints, so a pg/worker sidecar never masquerades as
	// the app on Docker list-order luck.
	target, err := s.Docker.ProcessContainer(ctx, in.Name, in.Process)
	if errors.Is(err, dockerx.ErrNoProcess) {
		return nil, out, fmt.Errorf("no process %q for app %q", in.Process, in.Name)
	}
	if err != nil {
		return nil, out, fmt.Errorf("no container for app %q", in.Name)
	}
	rc, err := s.Docker.Logs(ctx, target.ID, false, fmt.Sprint(tail), false)
	if err != nil {
		return nil, out, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxLogBytes+1))
	if err != nil {
		return nil, out, err
	}
	out.Container = target.ID
	if len(b) > maxLogBytes {
		b, out.Truncated = b[:maxLogBytes], true
	}
	out.Logs = string(b)
	return nil, out, nil
}

// ---- issues ----

type issuesListIn struct {
	Status string `json:"status,omitempty" jsonschema:"filter: active (open + acked, the default), open, acked, resolved, or all"`
}

// issueView is the store row minus internals (hashes, notify bookkeeping,
// raw data payload) plus the display category the UI shows.
type issueView struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Category  string `json:"category,omitempty"`
	Severity  string `json:"severity"`
	Status    string `json:"status"`
	App       string `json:"app,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Fix       string `json:"fix,omitempty"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	AckedAt   string `json:"ackedAt,omitempty"`
}

type issuesListOut struct {
	LastScanAt string      `json:"lastScanAt,omitempty"`
	Issues     []issueView `json:"issues"`
}

func (s *server) issuesList(_ context.Context, _ *mcp.CallToolRequest, in issuesListIn) (*mcp.CallToolResult, issuesListOut, error) {
	var out issuesListOut
	var statuses []string
	switch in.Status {
	case "", "active":
		statuses = []string{"open", "acked"}
	case "open", "acked", "resolved":
		statuses = []string{in.Status}
	case "all":
		statuses = nil
	default:
		return nil, out, fmt.Errorf("status must be one of active, open, acked, resolved, all")
	}
	list, err := s.Store.ListIssues(statuses, 100)
	if err != nil {
		return nil, out, err
	}
	out.Issues = make([]issueView, 0, len(list))
	for _, i := range list {
		out.Issues = append(out.Issues, issueView{
			ID: i.ID, Kind: i.Kind, Category: issues.Categories[i.Kind],
			Severity: i.Severity, Status: i.Status, App: i.AppName,
			Subject: i.Subject, Title: i.Title, Detail: i.Detail, Fix: i.Fix,
			FirstSeen: i.FirstSeen, LastSeen: i.LastSeen, AckedAt: i.AckedAt,
		})
	}
	out.LastScanAt = s.lastScan()
	return nil, out, nil
}

type issuesSummaryOut struct {
	Critical   int64  `json:"critical"`
	Warning    int64  `json:"warning"`
	Info       int64  `json:"info"`
	LastScanAt string `json:"lastScanAt,omitempty"`
}

func (s *server) issuesSummary(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, issuesSummaryOut, error) {
	counts, err := s.Store.CountOpenIssues()
	if err != nil {
		return nil, issuesSummaryOut{}, err
	}
	return nil, issuesSummaryOut{
		Critical:   counts["critical"],
		Warning:    counts["warning"],
		Info:       counts["info"],
		LastScanAt: s.lastScan(),
	}, nil
}

func (s *server) lastScan() string {
	if s.Issues == nil {
		return ""
	}
	return s.Issues.LastScan()
}

// ---- system ----

type systemStatusOut struct {
	Version     string           `json:"version"`
	AppsTotal   int              `json:"appsTotal"`
	AppsRunning int              `json:"appsRunning"`
	Issues      map[string]int64 `json:"openIssues"`
	// Latest host metrics sample; absent until the sampler has one.
	Metrics *metrics.Sample `json:"metrics,omitempty"`
}

func (s *server) systemStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, systemStatusOut, error) {
	out := systemStatusOut{Version: config.Version}
	if s.Engine != nil {
		if statuses, err := s.Engine.Statuses(ctx, false); err == nil {
			out.AppsTotal = len(statuses)
			for _, st := range statuses {
				if st.State == "running" {
					out.AppsRunning++
				}
			}
		}
	}
	if counts, err := s.Store.CountOpenIssues(); err == nil {
		out.Issues = counts
	}
	if s.Metrics != nil {
		if series := s.Metrics.Series(); len(series) > 0 {
			latest := series[len(series)-1]
			out.Metrics = &latest
		}
	}
	return nil, out, nil
}

// ---- backups + audit ----

type backupView struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	SizeBytes  int64  `json:"sizeBytes"`
	CreatedAt  string `json:"createdAt"`
	Format     string `json:"format,omitempty"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
}

type backupsListIn struct {
	App string `json:"app" jsonschema:"the app whose backups to list"`
}

type backupsListOut struct {
	Backups []backupView `json:"backups"`
}

func (s *server) backupsList(_ context.Context, _ *mcp.CallToolRequest, in backupsListIn) (*mcp.CallToolResult, backupsListOut, error) {
	var out backupsListOut
	app, err := s.Store.AppByName(in.App)
	if err != nil {
		return nil, out, fmt.Errorf("no such app %q", in.App)
	}
	backups, err := s.Store.ListBackups(app.ID, 20)
	if err != nil {
		return nil, out, err
	}
	out.Backups = make([]backupView, 0, len(backups))
	for _, b := range backups {
		out.Backups = append(out.Backups, backupView{
			ID: b.ID, Kind: b.Kind, SizeBytes: b.SizeBytes,
			CreatedAt: b.CreatedAt, Format: b.Format, VerifiedAt: b.VerifiedAt,
		})
	}
	return nil, out, nil
}

type auditRecentIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many entries to return (default 50, max 200)"`
}

type auditRecentOut struct {
	Entries []store.AuditEntry `json:"entries"`
}

func (s *server) auditRecent(_ context.Context, _ *mcp.CallToolRequest, in auditRecentIn) (*mcp.CallToolResult, auditRecentOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	entries, err := s.Store.RecentAudit(limit)
	if err != nil {
		return nil, auditRecentOut{}, err
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	return nil, auditRecentOut{Entries: entries}, nil
}
