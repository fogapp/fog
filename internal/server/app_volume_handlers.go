package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/jobs"
)

// Moving one app volume to another folder in Drive.
//
// From the Drive tab this is one sentence — "put this app's media in
// Content/Movies" — and underneath it is four things that must happen
// together: stop the app, move the bytes, rewrite the spec, start it
// again. Doing them from the outside (edit the spec, then drag the folder)
// is impossible: Drive refuses to move a folder an app mounts, and rightly.
// So fog does the whole sequence itself, as one job the page can watch.

type appVolumeMoveReq struct {
	// The sidecar that mounts the volume; "" for the app's own container.
	Service string `json:"service"`
	// Where it should live, relative to the Drive root ("Content/Movies").
	To string `json:"to"`
	// "move" carries the files there; "adopt" leaves them and points the
	// app at what is already in the destination.
	Mode string `json:"mode"`
}

func (s *Server) handleAppVolumeMove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	volName := r.PathValue("volume")
	var req appVolumeMoveReq
	if !readJSON(w, r, &req) {
		return
	}
	if s.files == nil {
		badRequest(w, "Drive is not available on this server")
		return
	}
	filesRoot := s.files.Root()
	if filesRoot == "" {
		badRequest(w, "Drive is not usable right now — fix it in Settings → Storage → Drive first")
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "move"
	}
	if mode != "move" && mode != "adopt" {
		badRequest(w, "mode must be move or adopt")
		return
	}
	to := strings.Trim(strings.TrimSpace(req.To), "/")
	if err := apps.ValidateFilesRel(to); err != nil {
		badRequest(w, fmt.Sprintf("Drive folder %s", err))
		return
	}

	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	spec, err := apps.LatestSpec(s.store, app.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	vol, ok := findVolume(spec, req.Service, volName)
	if !ok {
		notFound(w, "no such volume")
		return
	}
	if vol.Files == to {
		badRequest(w, "that volume is already there")
		return
	}
	if vol.ReadOnly && vol.Files != "" {
		// A copy of a peer's share is theirs; moving it would only make a
		// second copy fog then has to keep in step with the first.
		badRequest(w, "that folder is a copy of a peer's share — manage it on the Connect page")
		return
	}

	from := s.volumeHostPath(name, vol, filesRoot)
	actor := s.actor(r)
	label := fmt.Sprintf("Move %s's %s to Drive/%s", name, vol.Name, to)
	if mode == "adopt" {
		label = fmt.Sprintf("Point %s's %s at Drive/%s", name, vol.Name, to)
	}
	job, ok := s.files.Jobs.StartExclusive(volumeMoveKind, label, func(ctx context.Context, p *jobs.Progress) error {
		return s.moveAppVolume(ctx, p, moveArgs{
			app: name, service: req.Service, volume: vol.Name,
			from: from, to: to, mode: mode, actor: actor,
		})
	})
	if !ok {
		badRequest(w, "another volume move is already running")
		return
	}
	s.auditAs(actor, "app.volume.move", name, map[string]any{"volume": vol.Name, "to": to, "mode": mode})
	writeJSON(w, http.StatusAccepted, job)
}

const volumeMoveKind = "app-volume-move"

type moveArgs struct {
	app, service, volume string
	from, to, mode       string
	actor                string
}

// moveAppVolume is the sequence itself. The order is the point: the app is
// stopped before its files are touched, the spec is rewritten only once the
// files are where it will say they are, and the app comes back only after
// both are true. A failure anywhere leaves the app stopped with its data
// intact at one end or the other, never running against a folder that is
// not there.
func (s *Server) moveAppVolume(ctx context.Context, p *jobs.Progress, a moveArgs) error {
	app, err := s.store.AppByName(a.app)
	if err != nil {
		return err
	}
	wasRunning := app.DesiredState == "running"
	if wasRunning {
		p.Current("stopping " + a.app)
		if err := s.engine.Stop(ctx, a.app, a.actor); err != nil {
			return fmt.Errorf("stop %s before moving its files: %w", a.app, err)
		}
	}

	if a.mode == "adopt" {
		p.Current("preparing " + a.to)
		if err := s.files.AdoptFolder(a.to); err != nil {
			return err
		}
	} else {
		p.Current("moving files")
		if err := s.files.MoveVolume(ctx, a.from, a.to, p); err != nil {
			if errors.Is(err, files.ErrDestNotEmpty) {
				return fmt.Errorf("Drive/%s already has files in it — choose an empty folder, or use what is already there", a.to)
			}
			return err
		}
	}

	p.Current("updating " + a.app)
	spec, err := apps.LatestSpec(s.store, app.ID)
	if err != nil {
		return err
	}
	if !repointVolume(spec, a.service, a.volume, a.to) {
		return fmt.Errorf("%s no longer has a volume named %s", a.app, a.volume)
	}
	comment := fmt.Sprintf("moved %s to Drive/%s", a.volume, a.to)
	if err := s.engine.UpdateSpec(ctx, a.app, spec, nil, comment, a.actor); err != nil {
		return err
	}
	if !wasRunning {
		return nil
	}
	p.Current("starting " + a.app)
	return s.engine.Deploy(ctx, a.app, 0, a.actor)
}

// findVolume resolves a volume by the service that mounts it and its name.
func findVolume(spec *apps.AppSpec, service, name string) (apps.Volume, bool) {
	if service == "" {
		for _, v := range spec.Volumes {
			if v.Name == name {
				return v, true
			}
		}
		return apps.Volume{}, false
	}
	for _, aux := range spec.Aux {
		if aux.Name != service {
			continue
		}
		for _, v := range aux.Volumes {
			if v.Name == name {
				return v, true
			}
		}
	}
	return apps.Volume{}, false
}

// repointVolume rewrites one volume's placement in place, reporting whether
// it found it. A host path is cleared: a volume lives in one place.
func repointVolume(spec *apps.AppSpec, service, name, rel string) bool {
	place := func(vols []apps.Volume) bool {
		for i := range vols {
			if vols[i].Name == name {
				vols[i].Files, vols[i].Host = rel, ""
				return true
			}
		}
		return false
	}
	if service == "" {
		return place(spec.Volumes)
	}
	for i := range spec.Aux {
		if spec.Aux[i].Name == service {
			return place(spec.Aux[i].Volumes)
		}
	}
	return false
}

// volumeHostPath is where a volume's files are right now: its Drive folder,
// its host path, or the managed directory fog keeps for it.
func (s *Server) volumeHostPath(app string, v apps.Volume, filesRoot string) string {
	if p := v.HostPath(filesRoot); p != "" {
		return p
	}
	return filepath.Join(s.engine.Mat.DataPath(app), v.Name)
}
