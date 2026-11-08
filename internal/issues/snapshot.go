// The sweep snapshot: one consistent read of the world, built once per
// sweep so every check sees the same apps, specs, containers, and shares —
// and so check tests construct it by hand instead of mocking docker.
package issues

import (
	"context"
	"errors"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/store"
)

type appInfo struct {
	app  store.App
	spec *apps.AppSpec // nil when the stored spec fails to load
	// specUnreadable: the app has a revision, and it doesn't decode.
	specUnreadable bool
	deploying      bool // sampled once; probe checks skip these apps
	main           *dockerx.ContainerSummary
	lastBackup     *store.Backup
}

type snapshot struct {
	apps []appInfo
	// dockerOK is false when the container listing failed this sweep: every
	// appInfo.main is then nil for lack of evidence, not because containers
	// are gone. Checks that read main return an empty report instead —
	// issues survive rather than resolving (or filing) on missing evidence.
	dockerOK bool
	// nasOK is false when share statuses could not be read this sweep; the
	// storage checks return an empty report rather than treating the empty
	// shares list as "everything mounted".
	nasOK      bool
	shares     []nas.ShareStatus
	backupDir  string
	filesRoot  string
	baseDomain string
	// backupHourGlobal is the fleet-wide default backup hour setting
	// ("" = scheduled backups off unless a spec overrides).
	backupHourGlobal string
	dataDir          string
	// pgMajor is the major the shared cluster's data directory actually is
	// (0 = unknown or no cluster yet). The database checks compare against
	// what runs, not against what fog would like to run.
	pgMajor int
	// Files across peers: every subscription, and peer names for the
	// titles. now is the sweep's clock (tests pin it).
	fileSubs  []store.FileFedSubscription
	peerNames map[int64]string
	// offsiteTargets: every target, active or retired, for the offsite
	// checks.
	offsiteTargets []store.OffsiteTarget
	// connected is each app's connected providers, by app id; connectedOK
	// is false when they could not be read, and the wiring checks that read
	// it skip the sweep rather than resolve on missing evidence.
	connected   map[int64]map[string]bool
	connectedOK bool
	now         time.Time
}

func (sc *Scanner) buildSnapshot(ctx context.Context) (*snapshot, error) {
	list, err := sc.Store.ListApps()
	if err != nil {
		return nil, err
	}

	dockerOK := true
	var containers []dockerx.ContainerSummary
	listContainers := sc.AppContainers
	if listContainers == nil && sc.Docker != nil {
		listContainers = func(ctx context.Context) ([]dockerx.ContainerSummary, error) {
			return sc.Docker.ListByLabel(ctx, "fog.app")
		}
	}
	if listContainers != nil {
		if cs, err := listContainers(ctx); err == nil {
			containers = cs
		} else {
			// A transient daemon hiccup must not read as "no containers" —
			// that would file drift for every running app, then resolve it
			// all next sweep. Flag it; the container checks skip this sweep.
			dockerOK = false
			sc.Log.Warn("issues snapshot: docker list", "err", err)
		}
	}
	mains := map[string]*dockerx.ContainerSummary{}
	for app, c := range dockerx.MainContainers(containers) {
		mains[app] = &c
	}

	sn := &snapshot{dataDir: sc.DataDir, dockerOK: dockerOK, nasOK: true}
	if sc.PG != nil {
		if st, err := sc.PG.Inspect(); err == nil && !st.Fresh {
			sn.pgMajor = st.Major
		} else if err != nil {
			sc.Log.Warn("issues snapshot: postgres data dir", "err", err)
		}
	}
	// Specs and newest backups are read for the whole fleet at once; an app
	// missing from either map is what a failed per-app read used to leave.
	specs, err := apps.LatestSpecs(sc.Store)
	if err != nil {
		sc.Log.Warn("issues snapshot: specs", "err", err)
	}
	latest, _ := sc.Store.LatestBackups()
	for _, a := range list {
		info := appInfo{app: a, main: mains[a.Name]}
		if sc.Deploying != nil {
			info.deploying = sc.Deploying(a.Name)
		}
		if spec := specs[a.ID]; spec != nil {
			info.spec = spec
		} else if err == nil {
			// No revision yet is a just-created app; one that doesn't
			// decode is the proxy leaving it out.
			if _, rerr := apps.LatestSpec(sc.Store, a.ID); rerr != nil && !errors.Is(rerr, store.ErrNotFound) {
				info.specUnreadable = true
				sc.Log.Warn("issues snapshot: spec", "app", a.Name, "err", rerr)
			}
		}
		if b, ok := latest[a.ID]; ok {
			info.lastBackup = &b
		}
		sn.apps = append(sn.apps, info)
	}

	if sc.Nas != nil {
		if shares, err := sc.Nas.Statuses(); err == nil {
			sn.shares = shares
		} else {
			// Same rule as dockerOK: unreadable statuses are not "no shares
			// in trouble" — the storage checks skip this sweep instead of
			// resolving open issues and re-filing (and re-pushing) them on
			// the next good read.
			sn.nasOK = false
			sc.Log.Warn("issues snapshot: nas", "err", err)
		}
	}
	if sc.BackupDir != nil {
		sn.backupDir = sc.BackupDir()
	}
	if sc.FilesRoot != nil {
		sn.filesRoot = sc.FilesRoot()
	}
	sn.baseDomain, _ = sc.Store.GetSetting(store.SettingBaseDomain)
	sn.backupHourGlobal, _ = sc.Store.GetSetting(backup.SettingBackupHourUTC)
	sn.fileSubs, _ = sc.Store.ListFileFedSubscriptions(0)
	sn.peerNames = map[int64]string{}
	if len(sn.fileSubs) > 0 {
		peers, _ := sc.Store.ListFedPeers()
		for _, p := range peers {
			sn.peerNames[p.ID] = p.DisplayName()
		}
	}
	sn.offsiteTargets, _ = sc.Store.ListOffsiteTargets()
	sn.connected = map[int64]map[string]bool{}
	if conns, err := sc.Store.ConnectedIntegrations(); err == nil {
		sn.connectedOK = true
		for _, in := range conns {
			if sn.connected[in.AppID] == nil {
				sn.connected[in.AppID] = map[string]bool{}
			}
			sn.connected[in.AppID][strings.ToLower(in.Provider)] = true
		}
	} else {
		sc.Log.Warn("issues snapshot: integrations", "err", err)
	}
	sn.now = time.Now()
	return sn, nil
}
