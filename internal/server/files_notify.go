package server

// The Files half of the app-facing surface (docs/files-contract.md). An
// app that mounts a Files folder — its own, or a copy of a peer's share —
// learns that the folder changed under it: a sync landed, a file was
// fetched, the share was revoked, the operator adopted the copy, or a
// local write happened. The notice carries no listing; the app re-reads
// the mount. GET /app/v1/files/mounts tells an app where its folders come
// from, so it can say "this album is dave's; it is read-only here" without
// fog proxying anything.

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"time"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// ContractFilesV1 is what an app declares (repo fog.yaml: files.contract)
	// to receive change notices.
	ContractFilesV1 = "fog-files/1"
	// AppFilesCallbackPath is the optional endpoint a contracted app serves.
	AppFilesCallbackPath = "/fog/files/changed"
	// filesNotifyQuiet coalesces a burst (a sync landing fifty files) into
	// one notice per app and mount.
	filesNotifyQuiet = 3 * time.Second
)

type filesNotice struct {
	app      store.App
	mount    string
	hostPath string
	reason   string
}

// appFilesMount is one of an app's Files-backed volumes with provenance.
type appFilesMount struct {
	Mount        string `json:"mount"`
	HostPath     string `json:"hostPath"`
	Kind         string `json:"kind"` // local | peer
	Peer         string `json:"peer,omitempty"`
	Share        string `json:"share,omitempty"`
	ReadOnly     bool   `json:"readOnly"`
	Hydrated     bool   `json:"hydrated"`
	LastSyncedAt string `json:"lastSyncedAt,omitempty"`
	Status       string `json:"status,omitempty"`
}

// filesMountsFor lists an app's volumes that point into the Files root.
func (s *Server) filesMountsFor(app *store.App) ([]appFilesMount, error) {
	root := ""
	if s.files != nil {
		root = s.files.Root()
	}
	if root == "" {
		return []appFilesMount{}, nil
	}
	rev, err := s.store.LatestRevision(app.ID)
	if err != nil {
		return []appFilesMount{}, nil
	}
	spec, err := apps.ParseSpec(rev.Spec)
	if err != nil {
		return nil, err
	}
	out := filesVolumes(spec, root)
	subs, _ := s.store.ListFileFedSubscriptions(0)
	for i := range out {
		m := &out[i]
		rel := strings.TrimPrefix(strings.TrimPrefix(m.HostPath, root), "/")
		for _, sub := range subs {
			if files.IsUnder(rel, sub.Folder) {
				m.Kind, m.ReadOnly, m.Share, m.Status, m.LastSyncedAt = "peer", true, sub.Title, sub.Status, sub.LastSyncedAt
				m.Hydrated = sub.DetailWanted == filesfed.DetailFull
				if p, err := s.store.FedPeerByID(sub.PeerID); err == nil {
					m.Peer = p.DisplayName()
				}
				break
			}
		}
	}
	return out, nil
}

// filesVolumes lists a spec's volumes that point into the Files root, as
// local mounts — the federation provenance filesMountsFor adds is not
// needed to tell whether a change touches them.
func filesVolumes(spec *apps.AppSpec, root string) []appFilesMount {
	out := []appFilesMount{}
	for _, v := range spec.AllVolumes() {
		abs := v.HostPath(root)
		if abs == "" {
			continue
		}
		abs = filepath.Clean(abs)
		if !files.IsUnder(abs, root) {
			continue
		}
		out = append(out, appFilesMount{Mount: v.Mount, HostPath: abs, Kind: "local", ReadOnly: v.ReadOnly, Hydrated: true})
	}
	return out
}

func (s *Server) handleAppFilesMounts(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	out, err := s.filesMountsFor(app)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mounts": out})
}

// notifyAppsFilesChanged queues a notice for every contracted app whose
// mount overlaps rel (the folder is the mount, inside it, or above it).
// Cheap to call from every change; the quiet timer does the coalescing.
func (s *Server) notifyAppsFilesChanged(rel, reason string) {
	root := ""
	if s.files != nil {
		root = s.files.Root()
	}
	if root == "" {
		return
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	// Two queries whatever the app count: this runs on every file change a
	// sync or hydration lands, so it must not cost a round-trip per app.
	list, err := s.store.AppsWithContract(ContractFilesV1)
	if err != nil || len(list) == 0 {
		return
	}
	specs, err := apps.LatestSpecs(s.store)
	if err != nil {
		return
	}
	notices := map[string]filesNotice{}
	for _, a := range list {
		spec := specs[a.ID]
		if spec == nil {
			continue
		}
		for _, m := range filesVolumes(spec, root) {
			if !files.IsUnder(abs, m.HostPath) && !files.IsUnder(m.HostPath, abs) {
				continue
			}
			notices[strconv.FormatInt(a.ID, 10)+"\x00"+m.Mount] = filesNotice{app: a, mount: m.Mount, hostPath: m.HostPath, reason: reason}
		}
	}
	s.filesNotify.queue(notices, filesNotifyQuiet, func() { s.goSafe("files notify", s.flushFilesNotices) })
}

// flushFilesNotices sends the queued notices. An app with several changed
// mounts gets them one after another, the apps all at once, so one hung
// app delays only its own notices.
func (s *Server) flushFilesNotices() {
	notices := s.filesNotify.take()
	byApp := map[int64][]filesNotice{}
	var list []store.App
	for _, n := range notices {
		if _, seen := byApp[n.app.ID]; !seen {
			list = append(list, n.app)
		}
		byApp[n.app.ID] = append(byApp[n.app.ID], n)
	}
	s.fanOutApps(s.bgContext(), list, dirNotifyCallTimeout, func(ctx context.Context, t *appcall.Target) {
		for _, n := range byApp[t.App.ID] {
			body := map[string]string{
				"mount":     n.mount,
				"hostPath":  n.hostPath,
				"reason":    n.reason,
				"changedAt": time.Now().UTC().Format(time.RFC3339),
			}
			if _, err := t.Call(ctx, "POST", AppFilesCallbackPath, body); err != nil {
				s.log.Debug("files: app notify", "app", t.App.Name, "mount", n.mount, "err", err)
			}
		}
	})
}
