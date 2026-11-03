package server

// Wiring apps to the folders they share (docs/files-contract.md,
// "Wiring"). An app that uses another (spec.uses) must see the folders
// that one shares (connection.shares): Hook must see SABnzbd's downloads
// and Stream's libraries. Fog mounts what is missing when either side is
// installed, keeps a volume that follows another app's folder in step with
// it, tells a consumer when its providers' folders change, and hands it the
// path translations with its connections — so nobody types a path mapping.

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// ReasonRewired is the files notice a consumer gets when a provider it
// uses was installed, removed, or changed its folders: fetch
// /app/v1/connections again and re-derive anything built from its paths.
const ReasonRewired = "rewired"

// installedSpecs answers every installed app's latest spec, by name.
func (s *Server) installedSpecs() (map[string]*apps.AppSpec, []store.App, error) {
	all, err := apps.WithSpecs(s.store)
	if err != nil {
		return nil, nil, err
	}
	// An app held for review is nobody's provider and nobody's follower
	// target until it is confirmed: a held import named like a removed
	// provider must not repoint its followers.
	held, _ := s.store.HeldApps()
	out := make(map[string]*apps.AppSpec, len(all))
	list := make([]store.App, 0, len(all))
	for _, a := range all {
		if held[a.Name] {
			continue
		}
		list = append(list, a.App)
		if a.Spec != nil {
			out[a.Name] = a.Spec
		}
	}
	return out, list, nil
}

func (s *Server) filesRootPath() string {
	if s.files == nil {
		return ""
	}
	return s.files.Root()
}

// wireSpec brings spec — the app name is about to have — in line with the
// apps it uses: followed volumes take their target's folder, and each
// installed provider's shared folders it cannot see are mounted. Answers
// what it added, for the audit trail.
func (s *Server) wireSpec(name string, spec *apps.AppSpec) ([]string, bool) {
	specs, _, err := s.installedSpecs()
	if err != nil {
		return nil, false
	}
	lookup := func(app string) *apps.AppSpec {
		if app == name {
			return nil
		}
		return specs[app]
	}
	changed := apps.ResolveFollows(spec, lookup)
	var added []string
	root := s.filesRootPath()
	for _, u := range spec.Uses {
		p := lookup(u)
		if u == apps.UsesEvents || p == nil {
			continue
		}
		added = append(added, apps.WireShares(spec, u, p, root)...)
	}
	return added, changed || len(added) > 0
}

// rewireApp revises an installed app's spec through wireSpec (full) or
// only its followed volumes, and redeploys it if it is running. Answers
// the volumes it mounted.
func (s *Server) rewireApp(ctx context.Context, a store.App, full bool, actor string) []string {
	_, spec, err := apps.SpecAt(s.store, a.ID, 0)
	if err != nil {
		return nil
	}
	var added []string
	changed := false
	if full {
		added, changed = s.wireSpec(a.Name, spec)
	} else {
		specs, _, err := s.installedSpecs()
		if err != nil {
			return nil
		}
		changed = apps.ResolveFollows(spec, func(app string) *apps.AppSpec { return specs[app] })
	}
	if !changed {
		return nil
	}
	comment := "wiring: follow the folders of the apps it uses"
	if len(added) > 0 {
		comment = "wiring: mount " + strings.Join(added, ", ")
	}
	if err := s.engine.UpdateSpec(ctx, a.Name, spec, nil, comment, actor); err != nil {
		s.log.Warn("wiring: revise", "app", a.Name, "err", err)
		return nil
	}
	s.auditAs(actor, "app.wired", a.Name, map[string]any{"added": added})
	if a.DesiredState == "running" && a.CurrentRev > 0 {
		s.deployAsync("deploy after wiring", a.Name, 0, actor)
	}
	return added
}

// handleAppWire is the app-unwired issue's fix: mount every shared folder
// of the apps it uses that it cannot see, and redeploy.
func (s *Server) handleAppWire(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.AppByName(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	added := s.rewireApp(r.Context(), *a, true, s.actor(r))
	if added == nil {
		added = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added})
}

// wireConsumersOf runs after provider is installed: every app that uses it
// gets the folders it shares mounted, and hears that its wiring changed.
func (s *Server) wireConsumersOf(ctx context.Context, provider, actor string) {
	specs, list, err := s.installedSpecs()
	if err != nil {
		return
	}
	for _, a := range list {
		if sp := specs[a.Name]; a.Name != provider && sp != nil && sp.UsesGrant(provider) {
			s.rewireApp(ctx, a, true, actor)
		}
	}
}

// onSpecChanged runs after any app's spec is created, revised, or deleted:
// volumes that follow its folders catch up, and the apps that use it are
// told to fetch their connections again.
func (s *Server) onSpecChanged(ctx context.Context, name string) {
	specs, list, err := s.installedSpecs()
	if err != nil {
		return
	}
	notices := map[string]filesNotice{}
	contracted := map[int64]bool{}
	if withContract, err := s.store.AppsWithContract(ContractFilesV1); err == nil {
		for _, a := range withContract {
			contracted[a.ID] = true
		}
	}
	for _, a := range list {
		sp := specs[a.Name]
		if a.Name == name || sp == nil {
			continue
		}
		if sp.FollowsApp(name) {
			s.rewireApp(ctx, a, false, "system")
		}
		if sp.UsesGrant(name) && contracted[a.ID] {
			notices[strconv.FormatInt(a.ID, 10)+"\x00"+ReasonRewired] = filesNotice{app: a, reason: ReasonRewired}
		}
	}
	if len(notices) > 0 {
		s.filesNotify.queue(notices, filesNotifyQuiet, func() { s.goSafe("files notify", s.flushFilesNotices) })
	}
}

// connectionPaths fills a connection's path translations and the shared
// folders the consumer cannot see.
func (s *Server) connectionPaths(c *appConnection, consumer, provider *apps.AppSpec) {
	root := s.filesRootPath()
	c.Paths = apps.TranslatePaths(provider, consumer, root)
	for _, name := range apps.UnseenShares(provider, consumer, root) {
		c.Unseen = append(c.Unseen, name)
	}
}
