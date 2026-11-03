// The manage half of the tool surface: app lifecycle, deploys, issue
// acknowledgement, update checks. Every mutation runs as actor
// "mcp:<token name>" so the audit log and the ActivityTab attribute it
// honestly — down to which client did it — exactly like a UI user or a
// webhook.
package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/issues"
	"gitea.lan/matthew/fog/internal/updates"
)

func (s *server) addManageTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_app_control",
		Description: "Start, stop, or restart an app."},
		s.appControl)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_app_deploy",
		Description: "Deploy an app: rev 0 (or omitted) deploys the latest revision, a past rev is a rollback. Returns immediately; poll fog_app_get for progress."},
		s.appDeploy)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_app_backup_set",
		Description: "Change an app's backup configuration: enabled, mode (volumes | delegated), per-app hour override, before-update snapshot, delegated timeout. Only the fields given change. Writes a new spec revision without deploying; the backup scheduler picks it up immediately. Delegated mode requires the app itself to implement the fog-backup/1 contract."},
		s.appBackupSet)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_issue_ack",
		Description: "Acknowledge an issue: keeps it visible but marks it as seen and silences notifications for it."},
		s.issueAck)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_issue_unack",
		Description: "Remove an issue's acknowledgement, returning it to open."},
		s.issueUnack)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_issues_scan",
		Description: "Trigger a configuration scan now instead of waiting for the hourly sweep. Returns immediately; poll fog_issues_list for results."},
		s.issuesScan)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_updates_check",
		Description: "Check for app updates against registries/git remotes — one app, or every app when no name is given. Reports what it found; applies nothing."},
		s.updatesCheck)
}

type okOut struct {
	Ok bool `json:"ok"`
}

type appControlIn struct {
	Name   string `json:"name" jsonschema:"the app's name"`
	Action string `json:"action" jsonschema:"one of start, stop, restart"`
}

func (s *server) appControl(ctx context.Context, _ *mcp.CallToolRequest, in appControlIn) (*mcp.CallToolResult, okOut, error) {
	if s.Engine == nil {
		return nil, okOut{}, errNoEngine
	}
	var err error
	actor := actorFor(ctx)
	switch in.Action {
	case "start":
		err = s.Engine.Start(ctx, in.Name, actor)
	case "stop":
		err = s.Engine.Stop(ctx, in.Name, actor)
	case "restart":
		err = s.Engine.Restart(ctx, in.Name, actor)
	default:
		err = fmt.Errorf("action must be one of start, stop, restart")
	}
	if err != nil {
		return nil, okOut{}, err
	}
	return nil, okOut{Ok: true}, nil
}

type appDeployIn struct {
	Name string `json:"name" jsonschema:"the app's name"`
	Rev  int64  `json:"rev,omitempty" jsonschema:"revision to deploy; 0 or omitted deploys the latest, a past rev rolls back"`
}

type startedOut struct {
	Started bool   `json:"started"`
	Note    string `json:"note,omitempty"`
}

func (s *server) appDeploy(ctx context.Context, _ *mcp.CallToolRequest, in appDeployIn) (*mcp.CallToolResult, startedOut, error) {
	if s.Engine == nil {
		return nil, startedOut{}, errNoEngine
	}
	if _, err := s.Store.AppByName(in.Name); err != nil {
		return nil, startedOut{}, fmt.Errorf("no such app %q", in.Name)
	}
	// Refuse rather than queue: the engine would serialize a second deploy
	// behind the first, which reads as "started" while doing nothing yet.
	if s.Engine.Deploying(in.Name) {
		return nil, startedOut{}, fmt.Errorf("a deploy is already in flight for %q", in.Name)
	}
	// An imported app deploys only once the operator confirms its review in
	// fog's UI; an assistant can't confirm it for them.
	if held, _ := s.Engine.ReviewPending(in.Name); held {
		return nil, startedOut{}, &apps.ErrReviewPending{App: in.Name}
	}
	// An assistant may deploy an older revision only when it is the
	// latest's equal in everything security-relevant — image or repo,
	// commands, sidecars, ports, routes, mounts and follows, uses and reach,
	// env, acknowledgements (apps.SameAccessAs). Any other is the
	// operator's, from the app's History in fog's UI.
	if in.Rev != 0 {
		if err := s.Engine.CheckRollbackSameAccess(in.Name, in.Rev); err != nil {
			return nil, startedOut{}, err
		}
	}
	// The deploy outlives this call, so the actor is read now, off the
	// request context, and carried into the detached one.
	name, rev, actor := in.Name, in.Rev, actorFor(ctx)
	s.detach("mcp deploy", func(ctx context.Context) {
		if err := s.Engine.Deploy(ctx, name, rev, actor); err != nil {
			s.Log.Error("mcp deploy", "app", name, "rev", rev, "err", err)
		}
	})
	return nil, startedOut{Started: true, Note: "deploy running in the background; poll fog_app_get for progress"}, nil
}

type appBackupSetIn struct {
	Name         string `json:"name" jsonschema:"the app's name"`
	Enabled      *bool  `json:"enabled,omitempty" jsonschema:"include the app in nightly backups"`
	BeforeUpdate string `json:"beforeUpdate,omitempty" jsonschema:"snapshot before every update and rollback: on, off, or default to follow the fleet-wide setting"`
	Mode         string `json:"mode,omitempty" jsonschema:"volumes (fog tars volumes and dumps databases) or delegated (the app produces a fog-backup/1 artifact; requires app-side support)"`
	HourUTC      *int   `json:"hourUTC,omitempty" jsonschema:"per-app backup hour override, 0-23; -1 clears the override so the global default applies"`
	Timeout      string `json:"timeout,omitempty" jsonschema:"delegated backup timeout as a Go duration (e.g. 45m); the word none clears it"`
}

type appBackupSetOut struct {
	Ok     bool             `json:"ok"`
	Backup *apps.BackupSpec `json:"backup"` // the stored result, for verification
}

// appBackupSet edits exactly spec.backup — a partial update over the latest
// revision, so an AI client can fix a backup misconfiguration without ever
// holding (or risking) the rest of the spec.
func (s *server) appBackupSet(ctx context.Context, _ *mcp.CallToolRequest, in appBackupSetIn) (*mcp.CallToolResult, appBackupSetOut, error) {
	if s.Engine == nil {
		return nil, appBackupSetOut{}, errNoEngine
	}
	app, err := s.Store.AppByName(in.Name)
	if err != nil {
		return nil, appBackupSetOut{}, fmt.Errorf("no such app %q", in.Name)
	}
	spec, err := apps.LatestSpec(s.Store, app.ID)
	if err != nil {
		return nil, appBackupSetOut{}, err
	}
	b := spec.Backup
	if b == nil {
		b = &apps.BackupSpec{}
	}
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	switch in.BeforeUpdate {
	case "":
	case "default":
		b.BeforeUpdate = nil
	case "on", "off":
		on := in.BeforeUpdate == "on"
		b.BeforeUpdate = &on
	default:
		return nil, appBackupSetOut{}, fmt.Errorf("beforeUpdate must be on, off, or default, not %q", in.BeforeUpdate)
	}
	if in.Mode != "" {
		b.Mode = in.Mode // junk is refused by spec validation below
	}
	if in.HourUTC != nil {
		if *in.HourUTC == -1 {
			b.HourUTC = nil
		} else {
			h := *in.HourUTC
			b.HourUTC = &h
		}
	}
	if in.Timeout != "" {
		if in.Timeout == "none" {
			b.Timeout = ""
		} else {
			b.Timeout = in.Timeout
		}
	}
	spec.Backup = b
	// Validate (mode, hour range, timeout syntax, delegated reachability)
	// happens inside UpdateSpec, against the whole spec.
	if err := s.Engine.UpdateSpec(ctx, in.Name, spec, nil, "backup config", actorFor(ctx)); err != nil {
		return nil, appBackupSetOut{}, err
	}
	return nil, appBackupSetOut{Ok: true, Backup: b}, nil
}

type issueIDIn struct {
	ID int64 `json:"id" jsonschema:"the issue's id (from fog_issues_list)"`
}

func (s *server) issueAck(ctx context.Context, _ *mcp.CallToolRequest, in issueIDIn) (*mcp.CallToolResult, okOut, error) {
	return s.issueAction(ctx, in.ID, "issue.ack", s.Store.AckIssue)
}

func (s *server) issueUnack(ctx context.Context, _ *mcp.CallToolRequest, in issueIDIn) (*mcp.CallToolResult, okOut, error) {
	return s.issueAction(ctx, in.ID, "issue.unack", s.Store.UnackIssue)
}

func (s *server) issueAction(ctx context.Context, id int64, action string, fn func(int64) error) (*mcp.CallToolResult, okOut, error) {
	if err := issues.Mark(s.Store, actorFor(ctx), action, id, fn); err != nil {
		if errors.Is(err, issues.ErrNoIssue) {
			return nil, okOut{}, fmt.Errorf("no issue with id %d", id)
		}
		return nil, okOut{}, err
	}
	return nil, okOut{Ok: true}, nil
}

func (s *server) issuesScan(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, startedOut, error) {
	if s.Issues == nil {
		return nil, startedOut{}, errNoIssues
	}
	s.Store.Audit(actorFor(ctx), "issues.scan", "", "")
	s.detach("mcp issues scan", func(ctx context.Context) {
		// The scanner's own mutex makes double-fire harmless.
		s.Issues.Sweep(ctx)
	})
	return nil, startedOut{Started: true, Note: "scan running; poll fog_issues_list for results"}, nil
}

type updatesCheckIn struct {
	App string `json:"app,omitempty" jsonschema:"check just this app; omit to check every app"`
}

type updatesCheckOut struct {
	Checked          int                   `json:"checked"`
	UpdatesAvailable int                   `json:"updatesAvailable"`
	Failed           int                   `json:"failed"`
	Results          []updates.CheckResult `json:"results"`
}

func (s *server) updatesCheck(ctx context.Context, _ *mcp.CallToolRequest, in updatesCheckIn) (*mcp.CallToolResult, updatesCheckOut, error) {
	var out updatesCheckOut
	if s.Updates == nil {
		return nil, out, errNoUpdates
	}
	// Manual check, like the UI's: refresh verdicts without firing auto
	// policies.
	if in.App != "" {
		res, err := s.Updates.CheckOne(ctx, in.App)
		if err != nil {
			return nil, out, fmt.Errorf("no such app %q", in.App)
		}
		out.Results = []updates.CheckResult{res}
	} else {
		out.Results = s.Updates.CheckAll(ctx, false)
	}
	out.Checked = len(out.Results)
	for _, res := range out.Results {
		switch {
		case res.Error != "":
			out.Failed++
		case res.UpdateAvailable:
			out.UpdatesAvailable++
		}
	}
	return nil, out, nil
}
