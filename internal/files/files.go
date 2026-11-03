package files

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/store"
)

// MountedPath is one app volume that points at a host path.
type MountedPath struct {
	Path string
	App  string
	// Mount is the container path the app sees the folder at; ReadOnly
	// says the spec mounts it :ro. Both are for the Drive page's badge.
	Mount    string
	ReadOnly bool
}

// AppsDir is the folder at the root that fog keeps for apps: one named
// subfolder per app — Apps/Paperless, Apps/Jellyfin — holding that app's
// own data and nothing else. It is an ordinary folder in every other way
// (browse, upload, back up); what makes it fog's is that apps find their
// folders here by name and the page says which app sees each one. The
// same constant as apps.FilesAppsDir.
const AppsDir = "Apps"

// ContentDir is the other half of that split: the folder that holds
// generalized content — Photos, Movies, Music — the libraries that belong
// to the household rather than to one app, and that several apps read at
// once. An app's own data goes under AppsDir; what an app merely reads or
// files for you goes here. The same constant as apps.FilesContentDir.
const ContentDir = "Content"

// contentSeed is the set of libraries fog creates inside Content the first
// time it prepares a root, so there is somewhere obvious to put a film
// before any app that plays films is installed. Only ever created once per
// root: a folder deleted afterwards stays deleted.
var contentSeed = []string{"Photos", "Movies", "TV", "Music", "Books", "Documents"}

// Settings keys. Like backup_dir, an empty root means "derive from the
// data dir" and is never written back.
const (
	SettingRoot  = "files_root"  // default <DataDir>/files
	SettingOwner = "files_owner" // "uid:gid" applied to what fog creates; default DefaultOwner

	// DefaultOwner matches the uid every managed app volume runs as, so a
	// folder handed to an app as a volume works without a chown.
	DefaultOwner = "1000:1000"
)

// Test seams, the same ones internal/backup uses: real mounts need a Linux
// host, and these let the share guard be exercised on a dev machine.
var (
	mountedPoints = nas.MountedPoints
	unresponsive  = nas.Unresponsive
)

// Manager owns the root and the index. One per fogd.
type Manager struct {
	DataDir string
	Store   *store.Store
	Log     *slog.Logger
	// BackupRoot returns the backup directory, one of the trees the root
	// may not sit inside. Optional (tests).
	BackupRoot func() string
	// HoldBackupDir holds the backup directory for one piece of work and
	// LockBackupDir takes it for a move (backup.Manager.HoldDir, LockDir):
	// a snapshot, a restore or a verify never runs while the directory is
	// half in one place and half in another. Optional (tests).
	HoldBackupDir func() (release func(), err error)
	LockBackupDir func(ctx context.Context) (unlock func(), err error)
	// MountedPaths returns every host path an app spec mounts as a volume,
	// with the app; a folder under the root that an app depends on refuses
	// to be moved or trashed, and shows who mounts it. Optional (tests).
	MountedPaths func() []MountedPath
	// Notify sends an operator notification (ntfy); optional.
	Notify func(title, msg string, ok bool)
	// OnChange is called (synchronously, briefly) whenever a listing refresh
	// or a verb finds a directory changed; rel is the directory. Federation
	// uses it to mark shares dirty. Optional.
	OnChange func(rel string)
	// OnRootMoved is called once the root setting points at a new tree,
	// so apps whose volumes name a Files folder can be re-rendered against
	// it. Optional.
	OnRootMoved func()
	// StopAppsUnder stops the running apps with a volume at, under or
	// above a host path and names them; StartApps starts them again. A
	// placement move brackets its work with them. Optional (tests).
	StopAppsUnder func(ctx context.Context, path string) ([]string, error)
	StartApps     func(ctx context.Context, names []string) error
	// Annotators stamp extra facts onto listing entries (shared-with, kept,
	// replica provenance) — registered by the subsystems that own them.
	Annotators []func(nodes []Node)
	// Guards refuse a write at rel (create, rename, move, trash, upload,
	// paste) with a reason: a copy of a peer's share is read-only. The
	// subsystems that own such folders write through Materialize, which
	// does not consult them.
	Guards []func(rel string) error
	// BackupSkip says a folder is left out of the nightly snapshot for a
	// reason other than the operator's exclusion (a replica not chosen for
	// backup). Optional.
	BackupSkip func(rel string) bool
	// InstallTools lets Loop put host tools the page wants (ffmpeg, for
	// video thumbnails) on the box with apt-get when they are missing.
	// Off in tests and on dev machines.
	InstallTools bool

	Jobs *jobs.Jobs

	// Snapshot state: the scheduler's retry and alert throttles.
	lastSnapAttempt time.Time
	// snapFullAt and snapFullNeed: the last run refused for want of space,
	// and what it needed (runDue backs off until there is room).
	snapFullMu     sync.Mutex
	snapFullAt     time.Time
	snapFullNeed   int64
	lastStaleAlert time.Time

	imu     sync.Mutex
	openErr error
	idx     *Index

	rmu      sync.Mutex
	rootPath string
	root     *os.Root

	// placeMu serializes binding and moving the placed folders; offline
	// (under pmu) is each placed folder that is not bound, with why.
	placeMu sync.Mutex
	pmu     sync.Mutex
	offline map[string]string

	walkMu sync.Mutex
	// frozen are the Drive folders a move between disks is copying: fog
	// refuses writes under them until the move ends (freeze), so nothing
	// written during the copy is lost when the source is removed.
	frozenMu sync.Mutex
	frozen   map[string]int
	walking  atomic.Bool
	hashKick chan struct{}
}

func New(dataDir string, st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{DataDir: dataDir, Store: st, Log: log, Jobs: jobs.New(), hashKick: make(chan struct{}, 1)}
	m.Guards = append(m.Guards, m.placementGuard)
	return m
}

// IndexPath is where the index lives: beside fog's own state, not under
// the root, so the root stays pure data.
func (m *Manager) IndexPath() string { return filepath.Join(m.DataDir, "files-index.db") }

// Index opens the index on first use. Tests construct servers that never
// touch Files; they should not pay for a database file each.
//
// An index SQLite has called damaged is replaced here: the file (and its
// WAL) is moved aside, a fresh one opened, and a walk refills it. The
// damaged copy is kept beside it for a look, not reused.
func (m *Manager) Index() (*Index, error) {
	m.imu.Lock()
	defer m.imu.Unlock()
	if m.idx == nil && m.openErr == nil {
		m.idx, m.openErr = OpenIndex(m.IndexPath())
	}
	if m.idx != nil && m.idx.Corrupt() {
		m.replaceCorruptIndex()
	}
	return m.idx, m.openErr
}

// replaceCorruptIndex runs under imu. A listing that already holds the old
// *Index fails that one call with "database is closed"; the next call
// through Index gets the fresh one.
func (m *Manager) replaceCorruptIndex() {
	m.idx.Close()
	m.idx = nil
	aside := m.IndexPath() + ".corrupt-" + time.Now().UTC().Format("20060102-150405")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(m.IndexPath()+suffix, aside+suffix); err != nil && !os.IsNotExist(err) {
			m.Log.Warn("files index: moving the damaged index aside", "err", err)
		}
	}
	m.idx, m.openErr = OpenIndex(m.IndexPath())
	m.Log.Warn("files index was damaged; started a fresh one and rebuilding it", "kept", aside, "err", m.openErr)
	if m.openErr == nil {
		go func() {
			if err := m.Walk(context.Background()); err != nil && !errors.Is(err, ErrWalking) {
				m.Log.Warn("files walk", "err", err)
			}
		}()
	}
}

// Close releases the index and the open root.
func (m *Manager) Close() {
	m.rmu.Lock()
	if m.root != nil {
		m.root.Close()
		m.root = nil
	}
	m.rmu.Unlock()
	m.imu.Lock()
	if m.idx != nil {
		m.idx.Close()
	}
	m.imu.Unlock()
}

// DefaultRoot is what an empty setting means.
func (m *Manager) DefaultRoot() string { return filepath.Join(m.DataDir, "files") }

// Root is the configured root, defaulted.
func (m *Manager) Root() string {
	if v, _ := m.Store.GetSetting(SettingRoot); v != "" {
		return filepath.Clean(v)
	}
	return m.DefaultRoot()
}

// Owner parses files_owner; ok is false when fog should leave ownership
// alone (a blank setting, or not running as root — chown would fail).
func (m *Manager) Owner() (uid, gid int, ok bool) {
	v, _ := m.Store.GetSetting(SettingOwner)
	if v == "" {
		v = DefaultOwner
	}
	uid, gid, ok = fsx.ParseOwner(v)
	if !ok || os.Geteuid() != 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

// ValidOwnerSetting is the settings-table validator.
func ValidOwnerSetting(v string) bool {
	if v == "" {
		return true
	}
	_, _, ok := fsx.ParseOwner(v)
	return ok
}

// chown applies files_owner to a path fog just created. Best effort: a
// filesystem that refuses (a CIFS mount with fixed ownership) is not a
// reason to fail the operation that created the file.
func (m *Manager) chown(root *os.Root, rel string) {
	if uid, gid, ok := m.Owner(); ok {
		nf(root).Lchown(osPath(rel), uid, gid)
	}
}

// managedTrees are the directories under DataDir that belong to other
// subsystems. A root inside one would be indexed by Files and, worse,
// backed up or reported as an orphan by the subsystem that owns it.
var managedTrees = []string{"apps", "data", "builds", "postgres", "caddy", "docker-config", "docker-root", "uploads", "restore-pending", "backups"}

// Validate checks a candidate root before it is used: absolute
// and clean, not one of fog's own trees or an ancestor of them, on a share
// that is actually mounted, and writable in practice.
func (m *Manager) Validate(root string) error {
	if root == "" {
		root = m.DefaultRoot()
	}
	return m.validateDir(root, "the files root")
}

// validateDir is Validate for any directory fog would keep Drive's files
// in — the root, or where Apps or Content lives; what names it in errors.
func (m *Manager) validateDir(root, what string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%s must be an absolute path", what)
	}
	root = filepath.Clean(root)
	if root == "/" {
		return fmt.Errorf("%s cannot be the filesystem root", what)
	}
	data := filepath.Clean(m.DataDir)
	if IsUnder(data, root) {
		return fmt.Errorf("%s cannot contain fog's own data directory", what)
	}
	for _, t := range managedTrees {
		p := filepath.Join(data, t)
		if IsUnder(root, p) {
			return fmt.Errorf("%s cannot be inside %s, which belongs to fog", what, p)
		}
	}
	if m.BackupRoot != nil {
		if b := m.BackupRoot(); b != "" && (IsUnder(root, b) || IsUnder(b, root)) {
			return fmt.Errorf("%s and the backup directory cannot contain each other", what)
		}
	}
	if name, reason := m.shareState(root); reason != "" {
		return fmt.Errorf("%s is on the NAS share %q, which is %s — fix the share in Settings → Storage → Disks & shares first", root, name, reason)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", root, err)
	}
	// The unit's ProtectSystem/ProtectHome make whole trees read-only to
	// fogd without any error at path-checking time; only a write proves
	// the directory is usable.
	probe, err := os.CreateTemp(root, ".fog-probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable by fog: %w", root, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

// ValidRootSetting is the cheap, synchronous validator the settings table
// runs; the full Validate happens in the handler before the value is stored.
func ValidRootSetting(v string) bool {
	return v == "" || (strings.HasPrefix(v, "/") && !strings.ContainsRune(v, 0))
}

// shareState is nas.ShareState for path: a root under a stored NAS share
// that is unmounted or hung is unusable, and saying so beats writing into
// the empty mount point on the system disk.
func (m *Manager) shareState(path string) (name, reason string) {
	shares, err := m.Store.ListNasShares()
	if err != nil || len(shares) == 0 {
		return "", ""
	}
	return nas.ShareState(shares, path, mountedPoints(), unresponsive)
}

// openRoot returns the cached *os.Root for the current setting, opening
// (and creating the reserved directories) on first use or after the root
// changed. Sharing one *os.Root across goroutines is fine — it is a
// directory file descriptor.
func (m *Manager) openRoot() (*os.Root, error) {
	path := m.Root()
	m.rmu.Lock()
	defer m.rmu.Unlock()
	if m.root != nil && m.rootPath == path {
		return m.root, nil
	}
	if name, reason := m.shareState(path); reason != "" {
		return nil, fmt.Errorf("%w: %s is on the NAS share %q, which is %s", ErrNotConfigured, path, name, reason)
	}
	if err := m.ensureReserved(path); err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrNotConfigured, err)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotConfigured, err)
	}
	if m.root != nil {
		m.root.Close()
	}
	m.root, m.rootPath = r, path
	return r, nil
}

// RootChanged is called after files_root is stored: the open root and the
// index both describe the old tree, and the new one may never have been
// prepared.
func (m *Manager) RootChanged() {
	defer m.EnsureContent()
	m.rmu.Lock()
	if m.root != nil {
		m.root.Close()
		m.root = nil
		m.rootPath = ""
	}
	m.rmu.Unlock()
	if ix, err := m.Index(); err == nil {
		if err := ix.Reset(); err != nil {
			m.Log.Warn("files index reset", "err", err)
		}
	}
	// Apps and Content, where they live elsewhere, bind over the new tree
	// before its Content is seeded.
	m.EnsurePlacements(context.Background())
}

// Status is the Storage page's view of Files.
type Status struct {
	Root        string `json:"root"`
	DefaultRoot string `json:"defaultRoot"`
	Configured  bool   `json:"configured"` // the setting is non-empty
	Owner       string `json:"owner"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	// FsMount is the mount point of the filesystem holding the root; "/"
	// means the system disk.
	FsMount    string `json:"fsMount"`
	TotalBytes uint64 `json:"totalBytes,omitempty"`
	FreeBytes  uint64 `json:"freeBytes,omitempty"`
	Stats      Stats  `json:"stats"`
	WalkedAt   string `json:"walkedAt,omitempty"`
	Walking    bool   `json:"walking"`
	// Placements says where Apps and Content live: inside the root, or
	// bound from another disk (placement.go); then where backups are kept
	// (backupmove.go).
	Placements []Placement `json:"placements"`
}

func (m *Manager) Status() Status {
	st := Status{Root: m.Root(), DefaultRoot: m.DefaultRoot(), Walking: m.walking.Load()}
	if v, _ := m.Store.GetSetting(SettingRoot); v != "" {
		st.Configured = true
	}
	if v, _ := m.Store.GetSetting(SettingOwner); v != "" {
		st.Owner = v
	} else {
		st.Owner = DefaultOwner
	}
	if _, err := m.openRoot(); err != nil {
		st.Error = err.Error()
	} else {
		st.OK = true
	}
	mounts := mountedPoints()
	st.FsMount = "/"
	for mp := range mounts {
		if IsUnder(st.Root, mp) && len(mp) > len(st.FsMount) {
			st.FsMount = mp
		}
	}
	// The root may be a NAS share: a dead one leaves the sizes blank
	// rather than hanging the status page.
	if fs, stale, err := nas.Statfs(st.Root); err == nil && !stale {
		st.TotalBytes = uint64(fs.Blocks) * uint64(fs.Bsize)
		st.FreeBytes = uint64(fs.Bavail) * uint64(fs.Bsize)
	}
	if ix, err := m.Index(); err == nil {
		st.Stats, _ = ix.Stats()
		st.WalkedAt = ix.getMeta("walked_at")
	}
	st.Placements = append(m.Placements(), m.BackupPlacement())
	return st
}

// Listing is one page of a directory as the API returns it.
type Listing struct {
	Path    string `json:"path"`
	Parent  string `json:"parent"`
	Root    bool   `json:"root"`
	Dir     Node   `json:"dir"`
	Entries []Node `json:"entries"`
	Next    string `json:"next,omitempty"`
	Total   int    `json:"total"`
	Sort    Sort   `json:"sort"`
	Desc    bool   `json:"desc"`
}

// List refreshes rel against disk (see refreshDir) and returns a page.
func (m *Manager) List(rel string, o ListOpts) (Listing, error) {
	root, err := m.openRoot()
	if err != nil {
		return Listing{}, err
	}
	if err := m.errPlacementOffline(rel); err != nil {
		return Listing{}, err
	}
	ix, err := m.Index()
	if err != nil {
		return Listing{}, err
	}
	dir, err := m.refreshDir(root, ix, rel, false)
	if err != nil {
		return Listing{}, err
	}
	if o.Sort == "" {
		o.Sort = SortName
	}
	o.ShowHidden, o.ShowSystem = m.visible()
	page, err := ix.List(rel, o)
	if err != nil {
		return Listing{}, err
	}
	es := m.excludes()
	es.stamp(page.Entries)
	if ex, by := es.check(rel); ex && rel != "" {
		dir.Excluded = true
		if by != rel {
			dir.ExcludedBy = by
		}
	}
	// The folder itself rides along with its entries, so each decoration
	// (and the queries behind it) runs once per listing, not twice.
	all := append(page.Entries, dir)
	m.stampMounted(all)
	stampRoles(all)
	for _, a := range m.Annotators {
		a(all)
	}
	entries, dir := all[:len(all)-1], all[len(all)-1]
	l := Listing{Path: rel, Root: rel == "", Dir: dir, Entries: entries, Next: page.Next, Total: page.Total, Sort: o.Sort, Desc: o.Desc}
	if rel != "" {
		l.Parent, _ = Split(rel)
	}
	return l, nil
}

// Detail is a node plus what the detail pane wants to say about it.
type Detail struct {
	Node
	Duplicates []string `json:"duplicates,omitempty"`
	// Images: pixel size from the header.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// (MountedBy on Detail comes from the embedded Node.)

// KindOf says what is at rel on disk right now — "file", "dir", or
// "other" (a link is "other": Drive does not follow one) — without
// refreshing the index, for callers that only need to
// know whether a path still leads somewhere. ErrNotFound when it doesn't.
func (m *Manager) KindOf(rel string) (string, error) {
	root, err := m.openRoot()
	if err != nil {
		return "", err
	}
	fi, err := lstatNF(root, rel)
	if err != nil {
		return "", err
	}
	switch {
	case fi.IsDir():
		return "dir", nil
	case fi.Mode().IsRegular():
		return "file", nil
	}
	return "other", nil
}

// Stat returns one entry, refreshed against disk first so a file just
// written by another tool is described accurately: a file by its own
// lstat, anything else by refreshing its parent directory.
func (m *Manager) Stat(rel string) (Detail, error) {
	root, err := m.openRoot()
	if err != nil {
		return Detail{}, err
	}
	ix, err := m.Index()
	if err != nil {
		return Detail{}, err
	}
	if rel == "" {
		n, err := m.refreshDir(root, ix, "", false)
		return Detail{Node: n}, err
	}
	n, err := m.refreshFile(root, ix, rel)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Node: n}
	if n.SHA256 != "" {
		d.Duplicates, _ = ix.Duplicates(n.SHA256, rel, 10)
	}
	if n.Kind == "file" {
		d.Width, d.Height, _ = m.Dimensions(rel)
	}
	es := m.excludes()
	es.stamp([]Node{d.Node})
	nodes := []Node{d.Node}
	m.stampMounted(nodes)
	stampRoles(nodes)
	for _, a := range m.Annotators {
		a(nodes)
	}
	d.Node = nodes[0]
	if ex, by := es.check(rel); ex {
		d.Excluded = true
		if by != rel {
			d.ExcludedBy = by
		}
	}
	return d, nil
}

// Search is name search over the index.
func (m *Manager) Search(q, under, kind string, limit int) ([]Node, error) {
	ix, err := m.Index()
	if err != nil {
		return nil, err
	}
	var o ListOpts
	o.ShowHidden, o.ShowSystem = m.visible()
	return ix.Search(q, under, kind, limit, o)
}

// SearchScoped is Search narrowed to what one reader may see (an app
// searching for the person at its door; see SearchScope).
func (m *Manager) SearchScoped(q string, limit int, sc SearchScope) ([]Node, error) {
	ix, err := m.Index()
	if err != nil {
		return nil, err
	}
	var o ListOpts
	o.ShowHidden, o.ShowSystem = m.visible()
	return ix.search(q, "", "", limit, o, &sc)
}

// OpenFile opens a regular file for reading with its index row.
func (m *Manager) OpenFile(rel string) (*os.File, Node, error) {
	root, err := m.openRoot()
	if err != nil {
		return nil, Node{}, err
	}
	if rel == "" {
		return nil, Node{}, ErrIsDirectory
	}
	// Never through a link, and never anything but a regular file: what a
	// share link, WebDAV or a peer is served is the file at this path.
	f, info, err := openFileNF(root, rel)
	if err != nil {
		return nil, Node{}, err
	}
	n := Node{Path: rel, Kind: "file", Size: info.Size(), mtime: info.ModTime().UnixNano(), Modified: info.ModTime().UTC().Format(time.RFC3339)}
	_, n.Name = Split(rel)
	n.Mime = MimeFor(n.Name)
	if ix, err := m.Index(); err == nil {
		if row, err := ix.Get(rel); err == nil && row.Size == info.Size() && row.mtime == n.mtime {
			n.SHA256 = row.SHA256
		}
	}
	return f, n, nil
}

func mapOSError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, os.ErrExist):
		return ErrExists
	case errors.Is(err, syscall.ENOTDIR):
		return ErrNotDirectory
	case errors.Is(err, syscall.EISDIR):
		return ErrIsDirectory
	case errors.Is(err, syscall.ENAMETOOLONG):
		return ErrNameTooLong
	}
	// os.Root reports an escape as a *PathError wrapping "path escapes from parent".
	if strings.Contains(err.Error(), "escapes from parent") {
		return ErrEscapes
	}
	return err
}

// Loop runs the periodic work: a full walk shortly after boot and then
// daily, with the hasher draining between walks.
func (m *Manager) Loop(ctx context.Context) {
	if root, err := m.openRoot(); err == nil {
		m.restoreAside(root)
	}
	go m.hashLoop(ctx)
	go m.ensureFFmpeg(ctx)
	loop.Every(ctx, 45*time.Second, 24*time.Hour, func(ctx context.Context) {
		if err := m.Walk(ctx); err != nil && !errors.Is(err, ErrWalking) {
			m.Log.Warn("files walk", "err", err)
		}
		m.sweep()
		m.sweepThumbs()
	})
}

// stampMounted notes, on each folder, the apps whose volumes point at it
// or inside it — the badge that explains why it will not move or trash.
func (m *Manager) stampMounted(nodes []Node) {
	ms := m.Mounts()
	if len(ms.list) == 0 {
		return
	}
	for i := range nodes {
		if nodes[i].Kind != "dir" {
			continue
		}
		abs := filepath.Join(ms.root, filepath.FromSlash(nodes[i].Path))
		var by []string
		var mountsHere []Mount
		for _, mp := range ms.Under(nodes[i].Path) {
			if !slices.Contains(by, mp.App) {
				by = append(by, mp.App)
			}
			// The exact folder: the app sees it at this path. A mount
			// deeper inside counts for the badge, not the sentence.
			if mp.Path == abs {
				mountsHere = append(mountsHere, Mount{App: mp.App, Mount: mp.Mount, ReadOnly: mp.ReadOnly})
			}
		}
		nodes[i].MountedBy = by
		nodes[i].Mounts = mountsHere
	}
}

// stampRoles marks the folders fog gives a meaning to: the two reserved
// folders at the root, and each folder directly inside one of them — an
// app's own folder in Apps, a library in Content.
func stampRoles(nodes []Node) {
	for i := range nodes {
		if nodes[i].Kind != "dir" {
			continue
		}
		switch {
		case nodes[i].Path == AppsDir:
			nodes[i].Role = RoleApps
		case nodes[i].Path == ContentDir:
			nodes[i].Role = RoleContent
		case directlyIn(nodes[i].Path, AppsDir):
			nodes[i].Role = RoleAppFolder
		case directlyIn(nodes[i].Path, ContentDir):
			nodes[i].Role = RoleLibrary
		}
	}
}

// directlyIn reports whether rel is a child of dir with nothing between.
func directlyIn(rel, dir string) bool {
	return strings.HasPrefix(rel, dir+"/") && !strings.Contains(rel[len(dir)+1:], "/")
}

// WalkTree visits rel and everything beneath it with lstat info, never
// following symlinks and never entering the reserved directory.
func (m *Manager) WalkTree(rel string, fn func(rel string, fi fs.FileInfo) error) error {
	root, err := m.openRoot()
	if err != nil {
		return err
	}
	info, err := lstatNF(root, rel)
	if err != nil {
		return err
	}
	return walkTree(root, rel, info, fn)
}

// Lstat is one entry's lstat through the root; a link among rel's folders
// is ErrSymlink.
func (m *Manager) Lstat(rel string) (fs.FileInfo, error) {
	root, err := m.openRoot()
	if err != nil {
		return nil, err
	}
	return lstatNF(root, rel)
}

// IndexedHash returns the digest the index holds for rel if it describes
// the file as it is now, else "".
func (m *Manager) IndexedHash(rel string) string {
	ix, err := m.Index()
	if err != nil {
		return ""
	}
	info, err := m.Lstat(rel)
	if err != nil {
		return ""
	}
	if row, err := ix.Get(rel); err == nil && row.Size == info.Size() && row.mtime == info.ModTime().UnixNano() {
		return row.SHA256
	}
	return ""
}

// IndexedFile is what the index holds for one file: its digest, and the
// size and mtime that digest describes.
type IndexedFile struct {
	size, mtime int64
	sha256      string
}

// HashFor returns the digest if it describes fi as it is now, else "".
func (f IndexedFile) HashFor(fi fs.FileInfo) string {
	if f.size != fi.Size() || f.mtime != fi.ModTime().UnixNano() {
		return ""
	}
	return f.sha256
}

// IndexedDir returns what the index holds for dir's files, keyed by name:
// one query for a caller that already has each file's lstat (a walk),
// where IndexedHash would cost it a query and an Lstat per file.
func (m *Manager) IndexedDir(dir string) (map[string]IndexedFile, error) {
	ix, err := m.Index()
	if err != nil {
		return nil, err
	}
	kids, err := ix.children(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]IndexedFile, len(kids))
	for name, n := range kids {
		if n.Kind == "file" && n.SHA256 != "" {
			out[name] = IndexedFile{size: n.Size, mtime: n.mtime, sha256: n.SHA256}
		}
	}
	return out, nil
}

// QueueHashes gets digests on their way for files in dirs the index lacks
// one for: each directory's rows are brought in line with disk (a file the
// index never saw, or one rewritten in place, is re-rowed without its old
// digest) and the background hasher is woken. For a caller that must not
// hash inline.
func (m *Manager) QueueHashes(dirs []string) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return
	}
	for _, dir := range dirs {
		// A directory gone since the caller saw it has nothing to hash.
		m.refreshDir(root, ix, dir, true)
	}
	m.kickHasher()
}

// HashNow returns rel's digest, computing and recording it when the index
// does not have it yet.
func (m *Manager) HashNow(rel string) (string, error) {
	if sha := m.IndexedHash(rel); sha != "" {
		return sha, nil
	}
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return "", err
	}
	info, err := lstatNF(root, rel)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: not a regular file", ErrBadName)
	}
	if _, err := ix.Get(rel); err != nil {
		parent, _ := Split(rel)
		m.refreshDir(root, ix, parent, false)
	}
	sha, err := hashFile(root, Node{Path: rel, Size: info.Size(), mtime: info.ModTime().UnixNano()})
	if err != nil {
		return "", err
	}
	ix.SetHash(rel, info.Size(), info.ModTime().UnixNano(), sha)
	return sha, nil
}

// ErrReadOnly is a write refused by a guard.
var ErrReadOnly = errors.New("read-only here")

// guardWrite refuses a path that runs through a link (nofollow.go), then
// runs every registered guard. A write that takes an entry
// away from its place goes through guardRemove, which adds the
// mounted-folder rule.
func (m *Manager) guardWrite(rel string) error {
	if err := m.frozenAt(rel); err != nil {
		return err
	}
	if root, err := m.openRoot(); err == nil {
		if err := checkNoLinks(root, rel); err != nil {
			return err
		}
	}
	for _, g := range m.Guards {
		if err := g(rel); err != nil {
			return fmt.Errorf("%w: %v", ErrReadOnly, err)
		}
	}
	return nil
}

// ErrDigestMismatch: bytes handed to Materialize did not hash to the digest
// the caller required. Nothing was placed.
var ErrDigestMismatch = errors.New("bytes do not match the expected digest")

// Materialize places bytes at rel on behalf of a subsystem that owns the
// folder (a peer sync): written to staging on the same filesystem,
// digested on the way, fsynced, given the stated mtime, renamed into
// place, and indexed. Guards are not consulted. Returns the digest.
//
// A non-empty wantSHA is checked before the rename, so bytes that don't
// match never replace what is already at rel.
func (m *Manager) Materialize(rel string, src io.Reader, mtime time.Time, wantSHA string) (string, error) {
	if err := m.frozenAt(rel); err != nil {
		return "", err
	}
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return "", err
	}
	parent, sum, err := m.writeInPlace(root, rel, src, mtime, wantSHA)
	if err != nil {
		return "", err
	}
	if _, err := m.refreshDir(root, ix, parent, false); err != nil {
		return sum, err
	}
	if n, err := ix.Get(rel); err == nil {
		ix.SetHash(rel, n.Size, n.mtime, sum)
	}
	// New bytes for the same entry, put in place by its owner: a link to it
	// stays a link to it.
	m.rebindShares(rel)
	return sum, nil
}

// ErrNotPlaced: what holds the name is not what the owning subsystem put
// there — the operator saved a file under it — so it was left alone.
var ErrNotPlaced = errors.New("a file the sync did not place is in the way; it was kept")

// MaterializeOwned is Materialize for an owner that may replace only what
// it placed: mine says whether what holds rel is that. Whatever holds the
// name at the moment of placing is checked — moved aside in one rename,
// checked, and put back untouched if it isn't mine (ErrNotPlaced) — so a
// file saved there at any moment before is never overwritten. With nothing
// there the name is claimed in one rename that refuses to replace.
func (m *Manager) MaterializeOwned(rel string, src io.Reader, mtime time.Time, wantSHA string, mine func(fs.FileInfo) bool) (string, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return "", err
	}
	parent, sum, err := m.writeInPlaceAs(root, rel, src, mtime, wantSHA, mine)
	if err != nil {
		return "", err
	}
	if _, err := m.refreshDir(root, ix, parent, false); err != nil {
		return sum, err
	}
	if n, err := ix.Get(rel); err == nil {
		ix.SetHash(rel, n.Size, n.mtime, sum)
	}
	m.rebindShares(rel)
	return sum, nil
}

// ErrPutBackFailed: a file moved aside to be checked could not go back to
// its name; the error says where it is now.
var ErrPutBackFailed = errors.New("putting the file back failed")

// asideReceipt is what a file moved aside carries beside it — its name —
// so a start after a crash can put it back (restoreAside).
type asideReceipt struct {
	Path string `json:"path"`
}

// placeOwned puts tmp at name for an owner that may replace only what mine
// accepts. What holds the name is moved aside with a receipt naming where
// it came from, checked, and put back if it isn't mine; a crash in between
// is undone at the next start.
func placeOwned(root *os.Root, tmp, name string, mine func(fs.FileInfo) bool) error {
	err := nf(root).RenameNoReplace(tmp, name)
	if err == nil || !errors.Is(err, ErrExists) {
		return err
	}
	aside := tmp + ".was"
	receipt, _ := json.Marshal(asideReceipt{Path: name})
	if err := writeReceipt(root, aside+".json", receipt); err != nil {
		return err
	}
	if err := nf(root).RenameNoReplace(name, aside); err != nil {
		nf(root).Remove(aside + ".json")
		return err
	}
	fi, err := lstatNF(root, aside)
	if err != nil || !mine(fi) {
		if where, perr := putBack(root, aside, name); perr != nil {
			return fmt.Errorf("%w: %w (%v); it is at %s", ErrNotPlaced, ErrPutBackFailed, perr, where)
		} else if where != name {
			return fmt.Errorf("%w: %w; it is at %s", ErrNotPlaced, ErrPutBackFailed, where)
		}
		return ErrNotPlaced
	}
	if err := nf(root).RenameNoReplace(tmp, name); err != nil {
		if where, perr := putBack(root, aside, name); perr != nil || where != name {
			return fmt.Errorf("%w: %v; the file that was there is at %s", ErrPutBackFailed, err, where)
		}
		return err
	}
	nf(root).Remove(aside)
	nf(root).Remove(aside + ".json")
	return nil
}

// writeReceipt writes a move-aside receipt as a new file. The staging
// folder it lands in can sit inside a folder an app mounts (Content/.fog),
// so whatever holds the name already — a link an app planted to make fog
// write through it, a stale receipt — is unlinked as itself first, and the
// receipt is created fresh (O_EXCL, no link followed): it can only ever be
// written where it is named.
func writeReceipt(root *os.Root, rel string, data []byte) error {
	// Nothing there is the usual case. Remove reports it as the raw
	// ENOENT on Linux, so both spellings of "not there" are accepted.
	if err := nf(root).Remove(rel); err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := nf(root).OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		nf(root).Remove(rel)
	}
	return err
}

// putBack returns a file moved aside to name, or — when something has
// taken the name meanwhile — to a free name beside it, and drops its
// receipt once it is somewhere. It reports where the file is.
func putBack(root *os.Root, aside, name string) (string, error) {
	err := nf(root).RenameNoReplace(aside, name)
	if err == nil {
		nf(root).Remove(aside + ".json")
		return name, nil
	}
	if !errors.Is(err, ErrExists) {
		return aside, err
	}
	dir, base := Split(name)
	other := Join(dir, uniqueName(root, dir, base))
	if err := nf(root).RenameNoReplace(aside, other); err != nil {
		return aside, err
	}
	nf(root).Remove(aside + ".json")
	return other, nil
}

// restoreAside puts back every file a crash left moved aside, at the start:
// to its name if that is free, else beside it under a free one. A file
// whose receipt is gone is left where it is, never swept.
func (m *Manager) restoreAside(root *os.Root) {
	// Each staging folder, with the tree its files were taken from: a file
	// staged in Content/.fog came from Content and goes back nowhere else.
	dirs := []string{uploadsDir}
	trees := map[string]string{uploadsDir: ""}
	for _, folder := range placeable {
		if m.placedFolder(folder) != "" {
			dirs = append(dirs, stagedUploads(folder))
			trees[stagedUploads(folder)] = folder
		}
	}
	for _, dir := range dirs {
		f, err := openDirNF(root, dir)
		if err != nil {
			continue
		}
		names, err := f.Readdirnames(-1)
		f.Close()
		if err != nil {
			continue
		}
		for _, n := range names {
			if !strings.HasSuffix(n, ".was") {
				continue
			}
			aside := dir + "/" + n
			raw, err := nf(root).ReadFile(aside + ".json")
			var rc asideReceipt
			if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &rc) != nil || rc.Path == "" {
				m.Log.Warn("files: a file moved aside has no receipt; left in place", "path", aside)
				continue
			}
			// The receipt sits where an app may have written it, so the
			// path it names is checked: a clean Drive path inside the tree
			// the staging folder serves, never fog's own reserved folders.
			if !m.asideTargetOK(rc.Path, trees[dir]) {
				m.Log.Warn("files: a receipt names a path outside the folder it was staged for; left in place",
					"path", aside, "names", DisplayName(rc.Path))
				continue
			}
			where, err := putBack(root, aside, rc.Path)
			if err != nil {
				m.Log.Warn("files: putting back a file moved aside", "path", rc.Path, "at", aside, "err", err)
				continue
			}
			m.Log.Info("files: put back a file moved aside before a restart", "path", where)
		}
	}
}

// asideTargetOK says a moved-aside file may go back to rel: a clean path
// (CleanRel refuses escapes and the reserved folders) inside tree, the
// placed folder the staging area belongs to, or — for the root's own
// staging area — anywhere not in a placed folder (those stage their own).
func (m *Manager) asideTargetOK(rel, tree string) bool {
	c, err := CleanRel(rel)
	if err != nil || c != rel || c == "" {
		return false
	}
	if tree != "" {
		return IsUnder(c, tree) && c != tree
	}
	return m.placedFolder(c) == ""
}

// writeInPlace is Materialize's disk half: stage, digest, check, rename.
// Returns rel's parent and the digest; the index is the caller's.
func (m *Manager) writeInPlace(root *os.Root, rel string, src io.Reader, mtime time.Time, wantSHA string) (string, string, error) {
	return m.writeInPlaceAs(root, rel, src, mtime, wantSHA, nil)
}

// writeInPlaceAs is writeInPlace placing through placeOwned when mine is
// set.
func (m *Manager) writeInPlaceAs(root *os.Root, rel string, src io.Reader, mtime time.Time, wantSHA string, mine func(fs.FileInfo) bool) (string, string, error) {
	if rel == "" {
		return "", "", ErrBadName
	}
	parent, name := Split(rel)
	if err := ValidateName(parent, name); err != nil {
		return "", "", err
	}
	if parent != "" {
		if err := mkdirAllNF(root, parent, 0o755); err != nil {
			return "", "", err
		}
		m.chownNewDirs(root, parent)
	}
	// Staged on the destination's own filesystem (see placement.go).
	staging := stagedUploads(m.placedFolder(rel))
	if err := mkdirAllNF(root, staging, 0o700); err != nil {
		if err = m.retryReserved(err, func() error { return mkdirAllNF(root, staging, 0o700) }); err != nil {
			return "", "", mapOSError(err)
		}
	}
	sum, err := atomicWrite(nf(root), rel, src, writeOpts{
		tmp:     staging + "/" + jobs.NewID() + ".part",
		perm:    0o644,
		mtime:   mtime,
		hash:    true,
		wantSHA: wantSHA,
		beforeRename: func(tmp string) error {
			m.chown(root, tmp)
			return nil
		},
		place: placeFor(root, mine),
	})
	if err != nil {
		if errors.Is(err, ErrNotPlaced) {
			return "", "", err
		}
		return "", "", mapOSError(err)
	}
	return parent, sum, nil
}

func placeFor(root *os.Root, mine func(fs.FileInfo) bool) func(tmp, name string) error {
	if mine == nil {
		return nil
	}
	return func(tmp, name string) error { return placeOwned(root, tmp, name, mine) }
}

// EnsureDir creates a folder (and parents) on behalf of an owning
// subsystem; guards are not consulted.
func (m *Manager) EnsureDir(rel string) error {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return err
	}
	if rel == "" {
		return nil
	}
	// Folder by folder, never through a link: the folder an app's volume
	// names is the one that gets bound into its container (an app that can
	// write next to it must not be able to turn it into a link to another
	// app's data first).
	if err := mkdirAllNF(root, rel, 0o755); err != nil {
		return err
	}
	m.chownNewDirs(root, rel)
	parent, _ := Split(rel)
	_, err = m.refreshDir(root, ix, parent, false)
	return err
}

// RemovePermanently deletes rel (a file or a whole folder) without the
// trash, on behalf of an owning subsystem: a replica leaving is not
// something to undo. Guards are not consulted.
func (m *Manager) RemovePermanently(rel string) error {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return err
	}
	if rel == "" {
		return ErrBadName
	}
	if err := checkNoLinks(root, rel); err != nil {
		return err
	}
	if err := nf(root).RemoveAll(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return mapOSError(err)
	}
	m.Store.DeleteFileSharesUnder(rel)
	ix.wmu.Lock()
	ix.dropDehydratedLocked(rel)
	ix.wmu.Unlock()
	parent, _ := Split(rel)
	_, err = m.refreshDir(root, ix, parent, false)
	return err
}

// Batch places many entries for an owning subsystem (a peer sync filling a
// folder) and defers the index's directory refreshes to Flush, which does
// each touched directory once. Placing entry by entry refreshed the parent
// after every file — reading the whole folder back each time, quadratic in
// its size. Until Flush, listings may lag the disk; nothing else does.
//
// Safe for concurrent use: a sync fetches several files at once.
type Batch struct {
	m     *Manager
	root  *os.Root
	ix    *Index
	mu    sync.Mutex
	dirty map[string]bool
	// hashes are digests computed while writing, recorded once the rows
	// exist.
	hashes map[string]string
}

// NewBatch starts a batch; the caller must Flush it.
func (m *Manager) NewBatch() (*Batch, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return nil, err
	}
	return &Batch{m: m, root: root, ix: ix, dirty: map[string]bool{}, hashes: map[string]string{}}, nil
}

func (b *Batch) touch(dir string) {
	b.mu.Lock()
	b.dirty[dir] = true
	b.mu.Unlock()
}

// EnsureDir is Manager.EnsureDir with the refresh deferred.
func (b *Batch) EnsureDir(rel string) error {
	if rel == "" {
		return nil
	}
	if err := mkdirAllNF(b.root, rel, 0o755); err != nil {
		return err
	}
	b.m.chownNewDirs(b.root, rel)
	parent, _ := Split(rel)
	b.touch(parent)
	return nil
}

// PlaceDehydrated lists a file the disk does not hold: an entry of a
// previews subscription, described by the origin's size, time and digest.
// Listings show it with a cloud; opening it fetches the bytes. Whatever
// was at rel is removed first — for this name the row is the truth.
func (b *Batch) PlaceDehydrated(rel string, size int64, mtime time.Time, sha, mime string) error {
	if rel == "" {
		return ErrBadName
	}
	parent, name := Split(rel)
	if err := ValidateName(parent, name); err != nil {
		return err
	}
	if err := b.EnsureDir(parent); err != nil {
		return err
	}
	b.ix.wmu.Lock()
	if err := nf(b.root).Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		b.ix.wmu.Unlock()
		return mapOSError(err)
	}
	err := b.ix.putDehydratedLocked(rel, size, mtime.UnixNano(), sha, mime)
	b.ix.wmu.Unlock()
	if err != nil {
		return err
	}
	b.touch(parent)
	return nil
}

// MaterializeOwned is Manager.MaterializeOwned with the refresh deferred.
func (b *Batch) MaterializeOwned(rel string, src io.Reader, mtime time.Time, wantSHA string, mine func(fs.FileInfo) bool) (string, error) {
	parent, sum, err := b.m.writeInPlaceAs(b.root, rel, src, mtime, wantSHA, mine)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.dirty[parent] = true
	b.hashes[rel] = sum
	b.mu.Unlock()
	return sum, nil
}

// Materialize is Manager.Materialize with the refresh deferred; the digest
// is recorded on the row at Flush.
func (b *Batch) Materialize(rel string, src io.Reader, mtime time.Time, wantSHA string) (string, error) {
	parent, sum, err := b.m.writeInPlace(b.root, rel, src, mtime, wantSHA)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.dirty[parent] = true
	b.hashes[rel] = sum
	b.mu.Unlock()
	return sum, nil
}

// Flush refreshes every directory the batch touched, shallowest first, then
// records the digests of the bytes it wrote. It returns the first error but
// tries every directory.
func (b *Batch) Flush() error {
	b.mu.Lock()
	dirs := make([]string, 0, len(b.dirty))
	for d := range b.dirty {
		dirs = append(dirs, d)
	}
	hashes := b.hashes
	b.dirty, b.hashes = map[string]bool{}, map[string]string{}
	b.mu.Unlock()
	slices.SortFunc(dirs, func(x, y string) int {
		return cmp.Compare(strings.Count(x, "/"), strings.Count(y, "/"))
	})
	var first error
	for _, d := range dirs {
		if _, err := b.m.refreshDir(b.root, b.ix, d, false); err != nil && first == nil {
			first = err
		}
	}
	for rel, sum := range hashes {
		if n, err := b.ix.Get(rel); err == nil {
			b.ix.SetHash(rel, n.Size, n.mtime, sum)
		}
		b.m.rebindShares(rel)
	}
	return first
}

// Dehydrate drops a file's bytes and keeps its row — "free up space" on a
// previews subscription. The row and the removal happen under one lock so
// a listing refresh in between cannot mistake the pair for a vanished
// file.
func (m *Manager) Dehydrate(rel string) error {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return err
	}
	if rel == "" {
		return ErrBadName
	}
	if err := checkNoLinks(root, rel); err != nil {
		return err
	}
	ix.wmu.Lock()
	err = ix.markDehydratedLocked(rel)
	if err == nil {
		if rerr := nf(root).Remove(rel); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = mapOSError(rerr)
		}
	}
	ix.wmu.Unlock()
	if err != nil {
		return err
	}
	parent, _ := Split(rel)
	_, err = m.refreshDir(root, ix, parent, false)
	return err
}

// Node returns the index row for rel without touching the disk: the way
// to learn about a dehydrated entry, which Stat would also describe but
// only after a directory refresh.
func (m *Manager) Node(rel string) (Node, error) {
	ix, err := m.Index()
	if err != nil {
		return Node{}, err
	}
	return ix.Get(rel)
}

// Exists reports whether rel is present.
func (m *Manager) Exists(rel string) bool {
	root, err := m.openRoot()
	if err != nil {
		return false
	}
	return existsNF(root, rel)
}

// ErrMoving is a write refused while the folder it lands in is being moved
// between disks.
var ErrMoving = fmt.Errorf("%w: this folder is being moved to another disk; try again when the move finishes", ErrReadOnly)

// freeze refuses writes at and under rel ("" is all of Drive) until the
// returned func is called.
func (m *Manager) freeze(rel string) func() {
	m.frozenMu.Lock()
	if m.frozen == nil {
		m.frozen = map[string]int{}
	}
	m.frozen[rel]++
	m.frozenMu.Unlock()
	return func() {
		m.frozenMu.Lock()
		if m.frozen[rel]--; m.frozen[rel] <= 0 {
			delete(m.frozen, rel)
		}
		m.frozenMu.Unlock()
	}
}

// frozenAt is ErrMoving when rel is at or under a frozen folder.
func (m *Manager) frozenAt(rel string) error {
	m.frozenMu.Lock()
	defer m.frozenMu.Unlock()
	for f := range m.frozen {
		if IsUnder(rel, f) {
			return ErrMoving
		}
	}
	return nil
}
