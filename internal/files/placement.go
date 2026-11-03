package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"gitea.lan/matthew/fog/internal/hostpkg"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/nas"
)

// Apps and Content can each live somewhere other than the Drive root — the
// apps' own data on a small fast disk, the libraries on a big slow one —
// and still be the same two folders in Drive. Fog bind-mounts the chosen
// location over <root>/Apps or <root>/Content, so every path anything
// already holds (a spec's Files volume, a backup's volume path, an index
// row, a share) keeps meaning what it meant; only the filesystem under the
// folder changes. Like the NAS shares, the bind is fog's to make at every
// start, not fstab's.
//
// Two folders on two filesystems means a rename between them can fail with
// EXDEV, so the things Drive stages before a rename — an upload's part
// file, an entry sent to the trash — stage inside the folder itself, under
// <folder>/.fog, never in the root's .fog on another disk.

// Settings keys: an absolute host path, or empty for "inside the Drive root".
const (
	SettingAppsLocation    = "files_apps_location"
	SettingContentLocation = "files_content_location"
)

// placeable are the folders that can live elsewhere, in the order the
// settings page lists them.
var placeable = []string{AppsDir, ContentDir}

func isPlaceable(folder string) bool { return folder == AppsDir || folder == ContentDir }

func placementSetting(folder string) string {
	if folder == AppsDir {
		return SettingAppsLocation
	}
	return SettingContentLocation
}

// Test seams: a bind mount needs root and a Linux mount namespace.
var (
	bindSupported = runtime.GOOS == "linux"
	bindMount     = func(ctx context.Context, src, dst string) error {
		return hostMount(ctx, "mount", "--bind", src, dst)
	}
	unbindMount = func(ctx context.Context, dst string) error {
		if err := hostMount(ctx, "umount", dst); err == nil {
			return nil
		}
		// Busy: a shell or a scan holds it open. Detaching lazily is safe
		// here — the path reverts to the empty folder underneath at once,
		// and the bytes stay where they are.
		return hostMount(ctx, "umount", "-l", dst)
	}
)

func hostMount(ctx context.Context, name string, args ...string) error {
	// In the host's namespace, not fogd's private one: a bind made inside
	// the sandbox would be invisible to Docker (see hostpkg.Cmd).
	if out, err := hostpkg.Run(ctx, nil, name, args...); err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Placement is where one of the two folders lives, as the settings page
// shows it — or the backup directory, which the page lists beside them
// (backupmove.go).
type Placement struct {
	Folder string `json:"folder" ts:"\"Apps\" | \"Content\" | \"Backups\""`
	// Location is the host path the folder is bound from; empty when it is
	// an ordinary folder inside the Drive root. For Backups it is always
	// the directory in effect.
	Location string `json:"location,omitempty"`
	// Default (Backups only): Location is fog's default, under its data
	// directory.
	Default bool `json:"default,omitempty"`
	// Bound: the location is mounted over the folder right now. A placed
	// folder that is not bound is unavailable, and Error says why.
	Bound bool   `json:"bound"`
	Error string `json:"error,omitempty"`
	// FsMount is the mount point of the filesystem the folder's bytes are
	// on; "/" means the system disk.
	FsMount    string `json:"fsMount"`
	TotalBytes uint64 `json:"totalBytes,omitempty"`
	FreeBytes  uint64 `json:"freeBytes,omitempty"`
}

// Location is folder's configured location, or "" for inside the root.
func (m *Manager) Location(folder string) string {
	if v, _ := m.Store.GetSetting(placementSetting(folder)); v != "" {
		return filepath.Clean(v)
	}
	return ""
}

// placementOffline reports why a placed folder is unavailable, or "".
func (m *Manager) placementOffline(folder string) string {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	return m.offline[folder]
}

func (m *Manager) setOffline(folder, reason string) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.offline == nil {
		m.offline = map[string]string{}
	}
	if reason == "" {
		delete(m.offline, folder)
	} else {
		m.offline[folder] = reason
	}
}

// placedFolder is the placed folder rel lies in (or is), if that folder
// is bound right now — the folder whose own .fog stages rel's renames.
func (m *Manager) placedFolder(rel string) string {
	top, _, _ := strings.Cut(rel, "/")
	if !isPlaceable(top) || m.Location(top) == "" || m.placementOffline(top) != "" {
		return ""
	}
	return top
}

// stagingDir is the reserved directory a write or trash at rel stages in:
// the root's, or a placed folder's own.
func stagingDir(folder string) string {
	if folder == "" {
		return ReservedDir
	}
	return folder + "/" + ReservedDir
}

// placementGuard is a write guard: nothing is written into a placed folder
// that is not bound, because it would land in the empty mount point on the
// disk underneath; and a placed folder itself is not renamed or trashed.
func (m *Manager) placementGuard(rel string) error {
	top, rest, _ := strings.Cut(rel, "/")
	if !isPlaceable(top) {
		return nil
	}
	loc := m.Location(top)
	if loc == "" {
		return nil
	}
	if reason := m.placementOffline(top); reason != "" {
		return fmt.Errorf("%s lives at %s, which is not available: %s", top, loc, reason)
	}
	if rest == "" {
		return fmt.Errorf("%s lives at %s; change that in Settings › Storage › Drive", top, loc)
	}
	return nil
}

// errPlacementOffline is what a read of an unavailable placed folder says
// instead of listing the empty mount point underneath as if it were empty.
func (m *Manager) errPlacementOffline(rel string) error {
	top, _, _ := strings.Cut(rel, "/")
	if !isPlaceable(top) {
		return nil
	}
	if reason := m.placementOffline(top); reason != "" {
		return fmt.Errorf("%w: %s lives at %s, which is not available: %s", ErrNotConfigured, top, m.Location(top), reason)
	}
	return nil
}

// Placements describes both folders.
func (m *Manager) Placements() []Placement {
	root := m.Root()
	mounts := mountedPoints()
	out := make([]Placement, 0, len(placeable))
	for _, folder := range placeable {
		p := Placement{Folder: folder, Location: m.Location(folder)}
		at := filepath.Join(root, folder)
		if p.Location != "" {
			at = p.Location
			// What the last bind attempt said, then what is true now: a
			// share that went away since, or a bind nobody has made yet.
			p.Error = m.placementOffline(folder)
			if p.Error == "" {
				if name, reason := m.shareState(p.Location); reason != "" {
					p.Error = fmt.Sprintf("it is on the NAS share %q, which is %s", name, reason)
				} else if !sameDir(p.Location, filepath.Join(root, folder)) {
					p.Error = "not mounted yet"
				}
			}
			p.Bound = p.Error == ""
		}
		p.FsMount = "/"
		for mp := range mounts {
			if IsUnder(at, mp) && len(mp) > len(p.FsMount) {
				p.FsMount = mp
			}
		}
		if p.Error == "" {
			if fs, stale, err := nas.Statfs(at); err == nil && !stale {
				p.TotalBytes = uint64(fs.Blocks) * uint64(fs.Bsize)
				p.FreeBytes = uint64(fs.Bavail) * uint64(fs.Bsize)
			}
		}
		out = append(out, p)
	}
	return out
}

// EnsurePlacements binds every placed folder that is not bound yet and
// returns the host paths of those it bound just now: an app started before
// the bind is bound to the empty folder underneath and needs a restart.
// Cheap when everything is already in place, so it runs at start, after a
// NAS share mounts late, before an app deploys, and after the root moves.
func (m *Manager) EnsurePlacements(ctx context.Context) []string {
	m.placeMu.Lock()
	defer m.placeMu.Unlock()
	root := m.Root()
	var fresh []string
	for _, folder := range placeable {
		loc := m.Location(folder)
		if loc == "" {
			m.setOffline(folder, "")
			continue
		}
		mp := filepath.Join(root, folder)
		bound, err := m.bind(ctx, loc, mp)
		if err != nil {
			m.setOffline(folder, err.Error())
			m.Log.Warn("files: placed folder is not available", "folder", folder, "location", loc, "err", err)
			continue
		}
		m.setOffline(folder, "")
		if bound {
			m.Log.Info("files: placed folder bound", "folder", folder, "location", loc)
			fresh = append(fresh, mp)
		}
	}
	return fresh
}

// EnsurePlacementsFor is the deploy-time check: every placed folder one of
// hostPaths lies in must be bound, or the app would start against the empty
// folder underneath. It binds what it can and names what it can't.
func (m *Manager) EnsurePlacementsFor(ctx context.Context, hostPaths []string) error {
	root := m.Root()
	need := false
	for _, folder := range placeable {
		if m.Location(folder) == "" {
			continue
		}
		mp := filepath.Join(root, folder)
		for _, p := range hostPaths {
			if IsUnder(p, mp) || IsUnder(mp, p) {
				need = true
			}
		}
	}
	if !need {
		return nil
	}
	m.EnsurePlacements(ctx)
	for _, folder := range placeable {
		mp := filepath.Join(root, folder)
		for _, p := range hostPaths {
			if !IsUnder(p, mp) && !IsUnder(mp, p) {
				continue
			}
			if reason := m.placementOffline(folder); reason != "" {
				return fmt.Errorf("Drive's %s folder lives at %s, which is not available: %s", folder, m.Location(folder), reason)
			}
		}
	}
	return nil
}

// bind makes loc appear at mp. bound is false when it already did.
func (m *Manager) bind(ctx context.Context, loc, mp string) (bound bool, err error) {
	if !bindSupported {
		return false, errors.New("placing a folder on another disk needs a Linux host")
	}
	if name, reason := m.shareState(loc); reason != "" {
		return false, fmt.Errorf("it is on the NAS share %q, which is %s", name, reason)
	}
	if name, reason := m.shareState(mp); reason != "" {
		return false, fmt.Errorf("the Drive root is on the NAS share %q, which is %s", name, reason)
	}
	if m.isShareMountPoint(mp) {
		return false, fmt.Errorf("%s is a NAS share's own mount point; remove that share or point it elsewhere", mp)
	}
	if err := os.MkdirAll(loc, 0o755); err != nil {
		return false, fmt.Errorf("cannot create %s: %w", loc, err)
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return false, fmt.Errorf("cannot create %s: %w", mp, err)
	}
	if sameDir(loc, mp) {
		return false, nil
	}
	if mountedPoints()[mp] {
		// An earlier location is still bound there.
		if err := unbindMount(ctx, mp); err != nil {
			return false, err
		}
	}
	if hidden, _ := hasContent(mp); hidden {
		// Files written into the bare folder while it was unbound (an app
		// Docker restarted at boot, before fogd). They stay, hidden under
		// the mount, exactly as a NAS share hides its bare mount point.
		m.Log.Warn("files: binding over a folder that has files in it; they are hidden, not removed", "path", mp)
	}
	if err := bindMount(ctx, loc, mp); err != nil {
		return false, err
	}
	if !sameDir(loc, mp) {
		return false, fmt.Errorf("bound %s at %s, but the folder there is still another one", loc, mp)
	}
	return true, nil
}

// unbind removes a placement's bind, if one is there.
func (m *Manager) unbind(ctx context.Context, mp string) error {
	if !mountedPoints()[mp] || m.isShareMountPoint(mp) {
		return nil
	}
	return unbindMount(ctx, mp)
}

// unbindAll removes every placement's bind under root.
func (m *Manager) unbindAll(ctx context.Context, root string) error {
	m.placeMu.Lock()
	defer m.placeMu.Unlock()
	for _, folder := range placeable {
		if m.Location(folder) == "" {
			continue
		}
		if err := m.unbind(ctx, filepath.Join(root, folder)); err != nil {
			return fmt.Errorf("unmounting %s: %w", folder, err)
		}
		m.setOffline(folder, "moving")
	}
	return nil
}

func (m *Manager) isShareMountPoint(p string) bool {
	shares, err := m.Store.ListNasShares()
	if err != nil {
		return false
	}
	for _, sh := range shares {
		if sh.MountPoint == p {
			return true
		}
	}
	return false
}

// sameDir: a and b are the same directory — what a bind mount looks like.
// A var so tests can fake the bind.
var sameDir = func(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}

// mountsUnder lists the mount points strictly inside dir: a tree with one
// in it must not be removed as part of a move, or the delete would reach
// into the other filesystem.
func mountsUnder(dir string) []string {
	var out []string
	for mp := range mountedPoints() {
		if mp != dir && IsUnder(mp, dir) {
			out = append(out, mp)
		}
	}
	return out
}

// hasFiles reports whether dir holds a regular file anywhere beneath it;
// empty folders don't count — Content's starter libraries are just names.
func hasFiles(dir string) bool {
	found := errors.New("found")
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return found
		}
		return nil
	})
	return errors.Is(err, found)
}

// validatePlacement checks a new location for folder.
func (m *Manager) validatePlacement(folder, loc string) error {
	if !filepath.IsAbs(loc) {
		return errors.New("the location must be an absolute path")
	}
	root := m.Root()
	if IsUnder(loc, root) || IsUnder(root, loc) {
		return errors.New("the location must be outside the Drive location, and not contain it")
	}
	for _, other := range placeable {
		if other == folder {
			continue
		}
		if o := m.Location(other); o != "" && (IsUnder(loc, o) || IsUnder(o, loc)) {
			return fmt.Errorf("that overlaps where %s lives (%s)", other, o)
		}
	}
	if !bindSupported {
		return errors.New("placing a folder on another disk needs a Linux host")
	}
	return m.validateDir(loc, "the location")
}

// MovePlacement starts the job that changes where folder lives. to is a
// host path, or "" for back inside the Drive root. mode "move" carries the
// folder's contents there; "adopt" uses what is already at to and leaves
// the old place alone — allowed only when leaving it alone hides nothing.
//
// Apps whose volumes are in the folder are stopped for the job and started
// after it: a container keeps the mount it was created with, so one left
// running would go on writing to the old place.
func (m *Manager) MovePlacement(folder, to, mode string) (jobs.Job, error) {
	if !isPlaceable(folder) {
		return jobs.Job{}, fmt.Errorf("only %s and %s can live elsewhere", AppsDir, ContentDir)
	}
	if mode != "move" && mode != "adopt" {
		return jobs.Job{}, errors.New("mode must be move or adopt")
	}
	if to != "" {
		to = filepath.Clean(to)
	}
	cur := m.Location(folder)
	if to == cur {
		return jobs.Job{}, fmt.Errorf("%s already lives there", folder)
	}
	if to != "" {
		if err := m.validatePlacement(folder, to); err != nil {
			return jobs.Job{}, err
		}
	} else if !bindSupported && cur != "" {
		return jobs.Job{}, errors.New("placing a folder on another disk needs a Linux host")
	}
	root := m.Root()
	mp := filepath.Join(root, folder)
	if cur != "" && mode == "move" {
		if reason := m.placementOffline(folder); reason != "" {
			return jobs.Job{}, fmt.Errorf("%s's current location, %s, is not available (%s); adopt the new place instead, or fix the old one first", folder, cur, reason)
		}
	}
	switch {
	case mode == "move" && to != "":
		if has, _ := hasContent(to); has {
			return jobs.Job{}, fmt.Errorf("%s is not empty; choose Adopt to use what is there, or pick an empty folder", to)
		}
	case mode == "adopt" && cur == "":
		// Adopting would hide what the folder holds now under the mount.
		if hasFiles(mp) {
			return jobs.Job{}, fmt.Errorf("Drive's %s folder has files in it; move them, or empty it first", folder)
		}
	}
	src := mp
	if cur != "" {
		src = cur
	}
	if mode == "move" {
		if under := mountsUnder(src); len(under) > 0 {
			return jobs.Job{}, fmt.Errorf("%s has another disk mounted inside it (%s); unmount it first", src, under[0])
		}
	}

	label := fmt.Sprintf("Adopt %s at %s", folder, to)
	switch {
	case mode == "move" && to == "":
		label = fmt.Sprintf("Move %s back into Drive", folder)
	case mode == "move":
		label = fmt.Sprintf("Move %s to %s", folder, to)
	case to == "":
		label = fmt.Sprintf("Keep %s inside Drive again", folder)
	}
	job, ok := m.Jobs.StartExclusive("placement", label, func(ctx context.Context, p *jobs.Progress) (err error) {
		if m.StopAppsUnder != nil {
			p.Current("stopping the apps that use it")
			stopped, serr := m.StopAppsUnder(ctx, mp)
			defer func() {
				if len(stopped) > 0 && m.StartApps != nil {
					p.Current("starting the apps again")
					if serr := m.StartApps(context.WithoutCancel(ctx), stopped); serr != nil && err == nil {
						err = fmt.Errorf("moved, but some apps did not start: %w", serr)
					}
				}
			}()
			if serr != nil {
				return serr
			}
		}
		unfreeze := m.freeze(folder)
		defer unfreeze()
		m.placeMu.Lock()
		err = m.relocate(ctx, folder, cur, to, mode, mp, p)
		m.placeMu.Unlock()
		err = m.moveRefused("Moving "+folder, err)
		if err != nil {
			// Whatever happened, the setting still names the tree that
			// holds the folder; put the bind back over it.
			m.EnsurePlacements(context.WithoutCancel(ctx))
			return err
		}
		m.Store.Audit("system", "files.placement", folder, mode+" "+to)
		m.EnsurePlacements(ctx)
		if reason := m.placementOffline(folder); reason != "" {
			return fmt.Errorf("%s now lives at %s, but binding it failed: %s", folder, to, reason)
		}
		if mode == "move" {
			// The copy gave everything a new inode; links follow it.
			m.rebindShares(folder)
		}
		p.Current("indexing")
		return m.Walk(ctx)
	}, "root-move", backupMoveJob)
	if !ok {
		return jobs.Job{}, errors.New("a Drive move is already running")
	}
	return job, nil
}

// relocate is the job's body under placeMu: move the bytes (for "move"),
// take the old bind down, and flip the setting. The new bind is the
// caller's, so a failure here can be answered with a bind of whatever the
// setting still says.
func (m *Manager) relocate(ctx context.Context, folder, cur, to, mode, mp string, p *jobs.Progress) error {
	if cur != "" {
		if err := m.unbind(ctx, mp); err != nil {
			return fmt.Errorf("unmounting %s: %w", cur, err)
		}
		m.setOffline(folder, "moving")
	}
	if mode == "move" {
		from, dest := mp, to
		if cur != "" {
			from = cur
		}
		if to == "" {
			dest = mp
			// Back into the root: the bare folder that was under the mount
			// receives the tree, so it must not hold anything of its own.
			if has, _ := hasContent(mp); has {
				return fmt.Errorf("Drive's own %s folder, under the mount, has files in it; empty %s first", folder, mp)
			}
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		// The folder's own .fog (its trash, its staged uploads) travels
		// with it.
		if err := moveTree(ctx, from, dest, "", p); err != nil {
			return err
		}
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return err
		}
		m.chownPath(mp)
	}
	if err := m.Store.SetSetting(placementSetting(folder), to); err != nil {
		return err
	}
	if to == "" {
		m.setOffline(folder, "")
	}
	return nil
}
