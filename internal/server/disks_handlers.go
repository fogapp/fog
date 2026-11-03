package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/nas"
)

// Settings › Storage › Disks & shares: every filesystem fog puts bytes on, as one
// list. The system disk first — the one fog's data directory lives on —
// then each NAS share (mounted or not), then any other filesystem a fog
// path happens to land on: a USB disk someone pointed a volume at. Each
// entry says what depends on it — fog's own data, the Drive, the backup
// directory, the apps whose volumes resolve there — so unmounting a share
// or watching a disk fill is a decision made with the consequences in view.

// diskUses is what lives on a disk. Fog is the data directory itself: the
// database, builds, and every managed app volume.
type diskUses struct {
	Fog     bool     `json:"fog"`
	Drive   bool     `json:"drive"`
	Backups bool     `json:"backups"`
	Apps    []string `json:"apps"`
}

type diskEntry struct {
	// ID is stable across polls: "local", "nas:<share>", or "fs:<mount>".
	ID         string `json:"id"`
	Kind       string `json:"kind" ts:"\"local\" | \"nas\" | \"other\""` // local | nas | other
	Name       string `json:"name"`
	MountPoint string `json:"mountPoint"`
	// Share carries the stored share for a nas entry — the edit form and
	// the mount/unmount buttons key on it.
	Share     *nas.ShareStatus `json:"share,omitempty"`
	Mounted   bool             `json:"mounted"`
	Stale     bool             `json:"stale,omitempty"`
	LastError string           `json:"lastError,omitempty"`
	// Figures are absent (zero) when the filesystem isn't there to ask.
	TotalBytes uint64   `json:"totalBytes,omitempty"`
	UsedBytes  uint64   `json:"usedBytes,omitempty"`
	FreeBytes  uint64   `json:"freeBytes,omitempty"`
	Uses       diskUses `json:"uses"`
	// Usage splits UsedBytes by what fog knows is there. Absent until the
	// first sizing pass has finished, and on a disk with no reading.
	Usage *diskBreakdown `json:"usage,omitempty"`
}

// diskBreakdown is the used space of one disk by category, in the fixed order
// the bar draws them. "other" is whatever fog didn't size: the OS on the
// system disk, everyone else's files on a share.
type diskBreakdown struct {
	At       time.Time     `json:"at"`
	Segments []diskSegment `json:"segments"`
}

type diskSegment struct {
	Key   string `json:"key" ts:"\"apps\" | \"drive\" | \"backups\" | \"postgres\" | \"fog\" | \"docker\" | \"other\""`
	Label string `json:"label"`
	Bytes uint64 `json:"bytes"`
	// Reclaimable is the part of Bytes a cleanup would free — only the
	// Docker segment says: images no container uses and idle build cache.
	Reclaimable uint64 `json:"reclaimable,omitempty"`
}

// segmentOrder is the categories in bar order; the label is what the
// legend says.
var segmentOrder = []diskSegment{
	{Key: "apps", Label: "Apps"},
	{Key: "drive", Label: "Drive"},
	{Key: "backups", Label: "Backups"},
	{Key: "postgres", Label: "Postgres"},
	{Key: "fog", Label: "Fog"},
	{Key: "docker", Label: "Docker"},
}

// sizedPath is one directory fog owns, its category, and what the last
// sizing pass measured. Docker's store is one too — its path the daemon's
// data-root, its bytes Docker's own count — and the only one that knows
// how much of itself is reclaimable.
type sizedPath struct {
	path        string
	category    string
	bytes       int64
	reclaimable int64
}

// diskPaths is everything fog knows about where its bytes go.
type diskPaths struct {
	dataDir   string
	filesRoot string
	// placed are where Apps and Content live when not inside filesRoot.
	placed    []string
	backupDir string
	// apps maps an app name to the host paths its volumes bind from.
	apps map[string][]string
	// dockerRoot is the daemon's data-root when the last sizing pass
	// learned it; sized by Docker, never walked.
	dockerRoot string
}

// appVolumePaths lists where every volume of an app — its own and its
// sidecars' — binds from on the host. A managed volume is under the data
// directory; a Files volume resolves against the Drive root.
func appVolumePaths(name string, spec *apps.AppSpec, dataDir, filesRoot string) []string {
	var out []string
	add := func(vols []apps.Volume) {
		for _, v := range vols {
			if v.Managed() {
				out = append(out, filepath.Join(dataDir, "data", name))
			} else if p := v.HostPath(filesRoot); p != "" {
				out = append(out, filepath.Clean(p))
			}
		}
	}
	add(spec.Volumes)
	for _, a := range spec.Aux {
		add(a.Volumes)
	}
	return out
}

// assembleDisks builds the list from the stored shares, the host's mount
// table, and a statfs. A path under a share's mount point belongs to that
// share whether or not it is mounted right now — the dependency is on the
// share, and an unmounted one is exactly the case worth showing.
//
// sized is the last sizing pass, nil until one has finished; with it every
// disk that has a reading also gets its usage breakdown.
func assembleDisks(p diskPaths, shares []nas.ShareStatus, mounts map[string]bool, usage func(string) (total, free uint64, err error), sized []sizedPath, sizedAt time.Time) []diskEntry {
	localMount := fsMountOf(p.dataDir, mounts)
	entries := []diskEntry{{ID: "local", Kind: "local", Name: "System disk", MountPoint: localMount, Mounted: true}}
	byID := map[string]int{"local": 0}
	for i := range shares {
		sh := shares[i]
		e := diskEntry{
			ID: "nas:" + sh.Name, Kind: "nas", Name: sh.Name, MountPoint: sh.MountPoint, Share: &sh,
			Mounted: sh.Mounted, Stale: sh.Stale, LastError: sh.LastError,
			TotalBytes: sh.TotalBytes, FreeBytes: sh.FreeBytes,
		}
		if sh.TotalBytes >= sh.FreeBytes {
			e.UsedBytes = sh.TotalBytes - sh.FreeBytes
		}
		byID[e.ID] = len(entries)
		entries = append(entries, e)
	}

	// resolve names the entry a host path lands on, adding an "other"
	// entry for a filesystem fog has no record of.
	resolve := func(path string) int {
		best, bestLen := -1, -1
		for _, sh := range shares {
			if files.IsUnder(path, sh.MountPoint) && len(sh.MountPoint) > bestLen {
				best, bestLen = byID["nas:"+sh.Name], len(sh.MountPoint)
			}
		}
		if best >= 0 {
			return best
		}
		mp := fsMountOf(path, mounts)
		if mp == localMount {
			return 0
		}
		id := "fs:" + mp
		if i, ok := byID[id]; ok {
			return i
		}
		byID[id] = len(entries)
		entries = append(entries, diskEntry{ID: id, Kind: "other", Name: filepath.Base(mp), MountPoint: mp, Mounted: true})
		return byID[id]
	}

	entries[resolve(p.dataDir)].Uses.Fog = true
	if p.filesRoot != "" {
		entries[resolve(p.filesRoot)].Uses.Drive = true
	}
	for _, loc := range p.placed {
		entries[resolve(loc)].Uses.Drive = true
	}
	if p.backupDir != "" {
		entries[resolve(p.backupDir)].Uses.Backups = true
	}
	names := make([]string, 0, len(p.apps))
	for name := range p.apps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		seen := map[int]bool{}
		for _, path := range p.apps[name] {
			i := resolve(path)
			if !seen[i] {
				seen[i] = true
				entries[i].Uses.Apps = append(entries[i].Uses.Apps, name)
			}
		}
	}

	// Every sized directory lands on the disk that holds it, by the same
	// resolution as the dependencies above.
	byCat := map[int]map[string]uint64{}
	reclaimable := map[int]map[string]uint64{}
	for _, sp := range sized {
		i := resolve(sp.path)
		if byCat[i] == nil {
			byCat[i] = map[string]uint64{}
			reclaimable[i] = map[string]uint64{}
		}
		if sp.bytes > 0 {
			byCat[i][sp.category] += uint64(sp.bytes)
		}
		if sp.reclaimable > 0 {
			reclaimable[i][sp.category] += uint64(sp.reclaimable)
		}
	}

	for i := range entries {
		e := &entries[i]
		if e.Uses.Apps == nil {
			e.Uses.Apps = []string{}
		}
		if e.Kind != "nas" && usage != nil {
			if total, free, err := usage(e.MountPoint); err == nil && total > 0 {
				e.TotalBytes, e.FreeBytes = total, free
				if total >= free {
					e.UsedBytes = total - free
				}
			}
		}
		if sized == nil || e.TotalBytes == 0 {
			continue
		}
		u := &diskBreakdown{At: sizedAt, Segments: []diskSegment{}}
		var known uint64
		for _, seg := range segmentOrder {
			if b := byCat[i][seg.Key]; b > 0 {
				u.Segments = append(u.Segments, diskSegment{Key: seg.Key, Label: seg.Label, Bytes: b, Reclaimable: min(reclaimable[i][seg.Key], b)})
				known += b
			}
		}
		// Walked sizes and the filesystem's own accounting never agree to
		// the byte; the rest is whatever fog didn't size, floored at zero.
		if e.UsedBytes > known {
			u.Segments = append(u.Segments, diskSegment{Key: "other", Label: "Other", Bytes: e.UsedBytes - known})
		}
		e.Usage = u
	}
	// Other filesystems after the shares, in a stable order.
	others := entries[1+len(shares):]
	sort.SliceStable(others, func(a, b int) bool { return others[a].MountPoint < others[b].MountPoint })
	return entries
}

// dockerDiskUsage is the sizer's Docker probe: where the daemon's store
// is and what it holds, by Docker's own count.
func (s *Server) dockerDiskUsage(ctx context.Context) (*dockerUsage, error) {
	if s.docker == nil {
		return nil, errors.New("no docker client")
	}
	info, err := s.docker.Info(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.docker.DiskUsage(ctx)
	if err != nil {
		return nil, err
	}
	return &dockerUsage{root: filepath.Clean(info.DockerRootDir), bytes: u.Bytes, reclaimable: u.Reclaimable}, nil
}

func (s *Server) handleSystemDisks(w http.ResponseWriter, r *http.Request) {
	var shares []nas.ShareStatus
	if s.nas != nil {
		st, err := s.nas.Statuses()
		if err != nil {
			writeError(w, err)
			return
		}
		shares = st
	} else {
		stored, err := s.store.ListNasShares()
		if err != nil {
			writeError(w, err)
			return
		}
		for _, sh := range stored {
			shares = append(shares, nas.ShareStatus{NasShare: sh})
		}
	}
	list, err := apps.WithSpecs(s.store)
	if err != nil {
		writeError(w, err)
		return
	}
	p := diskPaths{dataDir: filepath.Clean(s.cfg.DataDir), filesRoot: s.files.Root(), apps: map[string][]string{}}
	// A path in Apps or Content placed elsewhere is on that location's
	// disk; the bind over the folder is not a disk of its own.
	binds := map[string]string{}
	for _, folder := range []string{files.AppsDir, files.ContentDir} {
		if loc := s.files.Location(folder); loc != "" {
			p.placed = append(p.placed, loc)
			binds[filepath.Join(p.filesRoot, folder)] = loc
		}
	}
	unbind := func(path string) string {
		for mp, loc := range binds {
			if files.IsUnder(path, mp) {
				return loc + strings.TrimPrefix(path, mp)
			}
		}
		return path
	}
	if d, _ := s.store.GetSetting(backup.SettingBackupDir); d != "" {
		p.backupDir = filepath.Clean(d)
	} else {
		p.backupDir = filepath.Join(p.dataDir, "backups")
	}
	for _, a := range list {
		spec := a.Spec
		if spec == nil {
			continue
		}
		if paths := appVolumePaths(a.Name, spec, p.dataDir, p.filesRoot); len(paths) > 0 {
			for i := range paths {
				paths[i] = unbind(paths[i])
			}
			p.apps[a.Name] = paths
		}
	}
	// Docker's root comes from the last pass, so the first poll after boot
	// lists it for walking; the pass leaves it out, and the next poll
	// knows better.
	p.dockerRoot = s.diskSizer.dockerRoot()
	owned := ownedPaths(p)
	paths := make([]string, len(owned))
	for i, sp := range owned {
		paths[i] = sp.path
	}
	pass := s.diskSizer.sizes(s.bgContext(), paths, func(fn func()) { s.goSafe("disk sizes", fn) })
	var sized []sizedPath
	var at time.Time
	if pass != nil {
		at = pass.at
		sized = []sizedPath{}
		for _, sp := range owned {
			if b, ok := pass.sizes[sp.path]; ok {
				sp.bytes = b
				sized = append(sized, sp)
			}
		}
		if d := pass.docker; d != nil && d.root != "" {
			sized = append(sized, sizedPath{path: filepath.Clean(d.root), category: "docker", bytes: d.bytes, reclaimable: d.reclaimable})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"disks":   assembleDisks(p, shares, nas.MountedPoints(), diskUsage, sized, at),
		"helpers": nas.HelpersInstalled(),
		// Non-empty when no share can ever be mounted by fogd itself —
		// shown on the form before the first attempt rather than after.
		"mountsBlocked": nas.MountsBlocked(),
	})
}

// ownedPaths lists every directory fog can put a name to, each with its
// category: the data directory's top-level entries (app data, Postgres,
// and the rest of fog's bookkeeping), the backup directory and the Drive
// root wherever they are, and every host path an app's volume binds
// from. A path inside one already listed is left out so nothing is
// counted twice, and Docker's data-root is Docker's to size.
func ownedPaths(p diskPaths) []sizedPath {
	var out []sizedPath
	covered := func(path string) bool {
		for _, sp := range out {
			if files.IsUnder(path, sp.path) {
				return true
			}
		}
		return false
	}
	add := func(path, cat string) {
		if path == "" || covered(path) {
			return
		}
		out = append(out, sizedPath{path: path, category: cat})
	}
	if ents, err := os.ReadDir(p.dataDir); err == nil {
		for _, e := range ents {
			path := filepath.Join(p.dataDir, e.Name())
			if path == p.dockerRoot {
				continue
			}
			cat := "fog"
			switch {
			case e.Name() == "data":
				cat = "apps"
			case e.Name() == "postgres":
				cat = "postgres"
			case path == p.backupDir:
				cat = "backups"
			case path == p.filesRoot:
				cat = "drive"
			}
			add(path, cat)
		}
	}
	add(p.backupDir, "backups")
	add(p.filesRoot, "drive")
	for _, loc := range p.placed {
		add(loc, "drive")
	}
	names := make([]string, 0, len(p.apps))
	for name := range p.apps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, path := range p.apps[name] {
			add(path, "apps")
		}
	}
	return out
}
