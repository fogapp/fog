package apps

// Approvals: what an app may do beyond fog's own storage and ports is the
// operator's say, kept as records of the operator's (store.Approval) — not
// fields of the spec, which an imported manifest writes and a rollback
// brings back. A spec's acknowledgeHostAccess and acknowledgeRawSockets
// are what it asks for; they become approvals only
// when an operator saves them in fog's UI or API, or confirms them by
// reading an import's review. Every deploy, of any revision, checks what
// that revision needs against the approvals as they stand.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// Approval kinds.
const (
	ApproveHost = "host"
	ApproveRaw  = "raw"
)

// specApprovals is what a spec's own acknowledgements ask for.
func specApprovals(s *AppSpec) []store.Approval {
	var out []store.Approval
	for _, p := range s.AcknowledgeHostAccess {
		out = append(out, store.Approval{Kind: ApproveHost, Value: ackKey(p)})
	}
	if s.AcknowledgeRawSockets {
		out = append(out, store.Approval{Kind: ApproveRaw})
	}
	return out
}

// ApproveSpec records, as the operator's approvals of app, exactly what the
// spec they just saved acknowledges — withdrawals included. Called only on
// an operator's own save, never on a revision an import, a repo, an
// integration, a wiring change or an assistant made.
func (e *Engine) ApproveSpec(name string, spec *AppSpec, actor string) error {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	return e.Store.SetAppApprovals(app.ID, specApprovals(spec), actor)
}

// requiredApprovals is what deploying spec as app needs approved: each host
// path outside fog's storage, and raw sockets. (A host port fog holds is
// no approval's to give: fogPortErr refuses it outright.)
func (e *Engine) requiredApprovals(name string, spec *AppSpec) []store.Approval {
	var out []store.Approval
	if e.Mat != nil {
		h := e.storageFor(name, e.approvedUses(name, spec))
		for _, hv := range spec.hostVolumes() {
			p := cleanHostPath(hv.Host)
			if _, ack := h.HostPathVerdict(p, hv.ReadOnly); ack != "" {
				out = append(out, store.Approval{Kind: ApproveHost, Value: p})
			}
		}
		// A Drive folder that isn't the app's to bind: approved under its
		// Drive-relative spelling.
		for _, dv := range spec.driveVolumes() {
			if rel := cleanDriveRel(dv.Files); h.driveWhy(rel) != "" {
				out = append(out, store.Approval{Kind: ApproveHost, Value: rel})
			}
		}
	}
	if spec.AcknowledgeRawSockets {
		out = append(out, store.Approval{Kind: ApproveRaw})
	}
	return out
}

// checkApprovals refuses deploying spec as app unless the operator has
// approved everything it needs — whatever the revision says of itself.
func (e *Engine) checkApprovals(app *store.App, spec *AppSpec) error {
	have, err := e.Store.AppApprovals(app.ID)
	if err != nil {
		return err
	}
	var missing []string
	for _, a := range e.requiredApprovals(app.Name, spec) {
		if have[a] {
			continue
		}
		switch a.Kind {
		case ApproveHost:
			if strings.HasPrefix(a.Value, "/") {
				missing = append(missing, "host folder "+a.Value)
			} else {
				missing = append(missing, "Drive folder "+a.Value)
			}
		case ApproveRaw:
			missing = append(missing, "raw sockets")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s needs what no operator has approved: %s. Approve it in the app's settings", app.Name, strings.Join(missing, ", "))
	}
	return nil
}

// checkReviewedRev refuses a revision of an imported app earlier than the
// one its review was confirmed at: nobody read it.
func (e *Engine) checkReviewedRev(app *store.App, rev int64) error {
	reviewed, err := e.Store.AppReviewedRev(app.ID)
	if err != nil {
		return err
	}
	if reviewed > 0 && rev < reviewed {
		return fmt.Errorf("revision %d of %s is older than the one its review was confirmed at (%d); confirm it again to deploy it", rev, app.Name, reviewed)
	}
	return nil
}

// securityView is everything in a spec that decides what the app can do
// or reach: what runs (image or repo, commands, processes, release,
// sidecars), what it binds (volumes, Drive folders, follows, files), what
// it publishes and serves (ports, routes and their sign-in), who it reaches
// and is reached by (uses, reach, wants, connection, network), its
// environment and secrets, sign-in, its database, and what it is
// acknowledged to do, and its resource limits (a limit lifted starves
// every other app). Notes, backups, health and the update policy are left
// out: rolling those back changes nothing an app could turn against anyone.
type securityView struct {
	Source                Source
	Env                   map[string]string
	Secrets               []string
	Volumes               []Volume
	Files                 []File
	Routes                []Route
	Ports                 []Port
	Command               []string
	Processes             []Process
	Aux                   []AuxService
	Release               []string
	Needs                 *Needs
	Uses, Reach           []string
	Connection            *Connection
	Wants                 []Want
	Signin                any
	Network               string
	SealedPeer            bool
	AcknowledgeHostAccess []string
	AcknowledgeRawSockets bool
	MemoryLimit           string
}

func viewOf(s *AppSpec) []byte {
	b, _ := json.Marshal(securityView{
		Source: s.Source, Env: s.Env, Secrets: s.Secrets, Volumes: s.Volumes, Files: s.Files, Routes: s.Routes,
		Ports: s.Ports, Command: s.Command, Processes: s.Processes, Aux: s.Aux, Release: s.Release, Needs: s.Needs,
		Uses: s.Uses, Reach: s.Reach, Connection: s.Connection, Wants: s.Wants, Signin: s.Signin,
		Network: s.Network, SealedPeer: s.SealedPeer,
		AcknowledgeHostAccess: s.AcknowledgeHostAccess, AcknowledgeRawSockets: s.AcknowledgeRawSockets,
		MemoryLimit: s.MemoryLimit,
	})
	return b
}

// SameAccessAs reports whether two revisions are alike in everything
// security-relevant (securityView) — what an assistant's deploy of an
// older revision must hold to.
func SameAccessAs(a, b *AppSpec) bool {
	return bytes.Equal(viewOf(a), viewOf(b))
}

// FogdPort is the port fogd listens on, 0 when unknown.
func (e *Engine) FogdPort() int {
	if _, port, ok := strings.Cut(e.FogHostAddr, ":"); ok {
		p, _ := strconv.Atoi(port)
		return p
	}
	return 0
}

// CheckRollbackSameAccess refuses rev of app unless it is the latest's
// equal in everything security-relevant (securityView) — for a deploy made
// by anyone but an operator in fog's UI.
func (e *Engine) CheckRollbackSameAccess(name string, rev int64) error {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	_, old, err := SpecAt(e.Store, app.ID, rev)
	if err != nil {
		return err
	}
	latest, err := LatestSpec(e.Store, app.ID)
	if err != nil {
		return err
	}
	if !SameAccessAs(old, latest) {
		return fmt.Errorf("revision %d of %s differs from the latest in what it runs, binds, publishes, reaches or is allowed; only an operator can deploy it, from the app's History", rev, name)
	}
	return nil
}
