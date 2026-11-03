package files

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/store"
)

// The files backup: hardlink snapshots, Time-Machine style. Every run
// produces a complete tree under <backupDir>/files/<stamp>/ in which a
// file unchanged since the previous snapshot is a hardlink to that
// snapshot's inode and a changed or new one is a fresh copy — so each
// night is a full, cp -a-restorable copy that costs only what changed. A
// manifest beside the tree lists every file with its digest, which is what
// verification checks and what lets an unchanged file's digest be carried
// forward instead of recomputed.
//
// Targets without hardlinks (CIFS, typically) get a single rolling mirror
// instead, with deletions parked under .deleted/<stamp>/ for a month: a
// full copy with no history, and a notice that says so.

const (
	// SettingFilesBackup is "on" (or empty — the default) or "off".
	SettingFilesBackup = "files_backup"

	NoticeFilesBackupStale   = "files-backup-stale"
	NoticeFilesBackupNoLinks = "files-backup-nolinks"
	NoticeFilesBackupCorrupt = "files-backup-corrupt"
	NoticeFilesBackupNoSpace = "files-backup-nospace"
	// NoticeFilesTooDeep: the last snapshot left out folders nested past
	// MaxDepth.
	NoticeFilesTooDeep = "files-backup-toodeep"
	// NoticeFilesCollided: the last snapshot left out entries whose names
	// the backup disk folds together.
	NoticeFilesCollided = "files-backup-collided"

	snapshotManifest = ".fog-snapshot.json"
	snapshotFormat   = "fog-files-snapshot/1"
	mirrorDir        = "mirror"
	mirrorDeleted    = ".deleted"

	// staleAfter mirrors the app backups' dead-man's switch.
	staleAfter = 26 * time.Hour
	// verifyEvery is how often the newest snapshot is sample-rehashed, and
	// verifySampleMax the most files one check rehashes.
	verifyEvery     = 7 * 24 * time.Hour
	verifySampleMax = 200
	// mirrorDeletedKeep is how long a mirror keeps what the root deleted.
	mirrorDeletedKeep = 30 * 24 * time.Hour
	// minFreeBytes and minFreeFrac: a snapshot stops before it leaves the
	// backup disk with less than the larger of these, so a Drive that has
	// outgrown its target fails one run instead of filling the disk the
	// apps, their backups and the offsite spool all share.
	minFreeBytes = 2 << 30
	minFreeFrac  = 0.05
	// defaultRetention matches backup_retention's default.
	defaultRetention = 7
)

var ErrSnapshotRunning = errors.New("a files snapshot is already running")
var ErrBackupsOff = errors.New("files backups are off")
var ErrLastSnapshot = errors.New("this is the only snapshot; it stays")
var ErrBackupDiskFull = errors.New("the backup disk is nearly full")

// BackupEnabled reads files_backup: on unless explicitly off.
func (m *Manager) BackupEnabled() bool {
	v, _ := m.Store.GetSetting(SettingFilesBackup)
	return v != "off"
}

func (m *Manager) retention() int {
	if v, _ := m.Store.GetSetting(backup.SettingBackupRetention); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultRetention
}

// snapshotsDir is where snapshots live: <backupDir>/files.
func (m *Manager) snapshotsDir() (string, error) {
	if m.BackupRoot == nil {
		return "", errors.New("no backup directory is configured")
	}
	dir := m.BackupRoot()
	if name, reason := m.shareState(dir); reason != "" {
		return "", fmt.Errorf("the backup directory %s is on the NAS share %q, which is %s — refusing to write to the local disk in its place", dir, name, reason)
	}
	return filepath.Join(dir, backup.FilesDirName), nil
}

type snapFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"` // unix nanos
	Inode  int64  `json:"inode"`
	SHA256 string `json:"sha256"`
	// Linked: carried over from the previous snapshot as a hardlink.
	Linked bool `json:"linked,omitempty"`
}

type snapManifest struct {
	Format    string     `json:"format"`
	CreatedAt string     `json:"created_at"`
	Mode      string     `json:"mode"`
	Root      string     `json:"root"`
	Files     []snapFile `json:"files"`
	Excluded  []string   `json:"excluded,omitempty"`
	Torn      []string   `json:"torn,omitempty"`
	// Collided lists what was left out because the backup disk could not
	// hold it beside another entry: "X" next to "x/" on a case-insensitive
	// target. One app's names must not stop everyone's backup.
	Collided []string `json:"collided,omitempty"`
	Bytes    int64    `json:"bytes"`
	NewBytes int64    `json:"new_bytes"`
}

func readManifest(dir string) (*snapManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, snapshotManifest))
	if err != nil {
		return nil, err
	}
	var mf snapManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return nil, err
	}
	if mf.Format != snapshotFormat {
		return nil, fmt.Errorf("unexpected manifest format %q", mf.Format)
	}
	return &mf, nil
}

// writeManifest encodes straight to the staged file, compact: indenting a
// million-entry manifest cost a third of its size and a second in-memory
// copy. The format stays fog-files-snapshot/1 — offsite reads it too, and
// an older fogd after a rollback must still find its previous snapshot.
func writeManifest(dir string, mf *snapManifest) error {
	return fsx.WriteAtomic(filepath.Join(dir, snapshotManifest), 0o600, func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 1<<16)
		if err := json.NewEncoder(bw).Encode(mf); err != nil {
			return err
		}
		return bw.Flush()
	})
}

// sampleManifest picks up to n random entries of a manifest's files by
// reservoir sampling as it streams, never holding the whole list — the
// weekly verify needs two hundred entries, not a million. Returns the
// sample and how many files the manifest lists.
func sampleManifest(dir string, rng *rand.Rand, n func(total int) int, cap int) ([]snapFile, int, error) {
	f, err := os.Open(filepath.Join(dir, snapshotManifest))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<16))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, 0, fmt.Errorf("manifest is not an object")
	}
	var format string
	var res []snapFile
	total := 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, 0, err
		}
		switch t {
		case "format":
			if err := dec.Decode(&format); err != nil {
				return nil, 0, err
			}
		case "files":
			if t, err := dec.Token(); err != nil || t != json.Delim('[') {
				if t == nil && err == nil {
					continue // "files": null
				}
				return nil, 0, fmt.Errorf("manifest files is not a list")
			}
			for dec.More() {
				var sf snapFile
				if err := dec.Decode(&sf); err != nil {
					return nil, 0, err
				}
				total++
				if len(res) < cap {
					res = append(res, sf)
				} else if j := rng.Intn(total); j < cap {
					res[j] = sf
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, 0, err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, 0, err
			}
		}
	}
	if format != snapshotFormat {
		return nil, 0, fmt.Errorf("unexpected manifest format %q", format)
	}
	// The reservoir holds a uniform sample of cap; the run wants n of
	// them, which is a uniform sample too.
	if k := n(total); k < len(res) {
		rng.Shuffle(len(res), func(i, j int) { res[i], res[j] = res[j], res[i] })
		res = res[:k]
	}
	return res, total, nil
}

// probeLinks reports whether dir's filesystem supports hardlinks.
func probeLinks(dir string) bool {
	f, err := os.CreateTemp(dir, ".fog-linkprobe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	link := name + ".link"
	defer os.Remove(link)
	return os.Link(name, link) == nil
}

// previous finds the newest snapshot whose directory and manifest still
// exist, to link against.
func (m *Manager) previous(mode string) (dir string, mf *snapManifest) {
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return "", nil
	}
	for _, r := range rows {
		if r.Mode != mode {
			continue
		}
		if p, err := readManifest(r.Path); err == nil {
			return r.Path, p
		}
	}
	return "", nil
}

// RunSnapshot writes one snapshot now. It is the body of the "snapshot"
// job; callers go through StartSnapshot, whose exclusive job is what keeps
// two runs from overlapping.
func (m *Manager) RunSnapshot(ctx context.Context, p *jobs.Progress) (store.FileSnapshot, error) {
	base, err := m.snapshotsDir()
	if err != nil {
		return store.FileSnapshot{}, err
	}
	root, err := m.openRoot()
	if err != nil {
		return store.FileSnapshot{}, err
	}
	// A placed folder that is not bound reads as empty: a snapshot of it
	// would record everything in it as gone (and a mirror would set it all
	// aside as deleted). Better no snapshot tonight than that one.
	for _, folder := range placeable {
		if reason := m.placementOffline(folder); reason != "" {
			return store.FileSnapshot{}, fmt.Errorf("%s lives at %s, which is not available (%s); skipping the snapshot", folder, m.Location(folder), reason)
		}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return store.FileSnapshot{}, err
	}
	// Only one snapshot job runs at a time, so any .partial here is a run
	// that died without cleaning up (fogd stopped mid-copy) and nothing will
	// ever finish it.
	m.sweepPartials(base)
	links := probeLinks(base)
	mode := "links"
	if !links {
		mode = "mirror"
		m.Store.CreateNotice(NoticeFilesBackupNoLinks, "",
			"Drive backups have no history on this target",
			"The backup directory does not support hardlinks (SMB/CIFS shares usually do not), so fog keeps one rolling mirror of "+
				"Drive there instead of nightly snapshots. It is a full, restorable copy, but a file changed or deleted today is not "+
				"recoverable from yesterday. An NFS share or a local disk gives fog hardlinks and full history.")
	} else {
		m.Store.DismissNoticeByKind(NoticeFilesBackupNoLinks, "")
	}
	prevDir, prev := m.previous(mode)
	prevFiles := map[string]snapFile{}
	if prev != nil {
		for _, f := range prev.Files {
			prevFiles[f.Path] = f
		}
	}
	es := m.excludes()

	// Progress against the index's idea of the total; exact enough.
	if ix, err := m.Index(); err == nil {
		if st, err := ix.Stats(); err == nil {
			p.Set("starting", 0, st.Bytes)
		}
	}

	// Plan before copying: a snapshot that cannot fit fails here, having
	// written nothing — not after filling the disk to its floor and taking it
	// back, which leaves every other writer there (the apps' own backups) a
	// disk with no room for the minutes it takes.
	if err := m.preflight(ctx, root, base, links, prevFiles, es); err != nil {
		return store.FileSnapshot{}, err
	}

	// Two runs inside one second (a double-click on Snapshot now) must not
	// collide on the directory name.
	stamp := fsx.FreeStamp(time.Now(), func(s string) bool {
		_, err := os.Stat(filepath.Join(base, s))
		return err == nil
	})
	var dest string
	if links {
		dest = filepath.Join(base, stamp+".partial")
		os.RemoveAll(dest)
		if err := os.Mkdir(dest, 0o755); err != nil {
			return store.FileSnapshot{}, err
		}
	} else {
		dest = filepath.Join(base, mirrorDir)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return store.FileSnapshot{}, err
		}
	}

	mf := &snapManifest{Format: snapshotFormat, CreatedAt: time.Now().UTC().Format(time.RFC3339), Mode: mode, Root: m.Root(), Excluded: es.sorted}
	seen := map[string]bool{}
	gauge := &spaceGauge{dir: dest}
	var tooDeep []string
	// On a case-insensitive backup disk, "X" and "x" are one name. Each
	// entry's folded name is claimed by the first to reach it; a later one
	// is left out and reported (Collided), never written over the first or
	// allowed to fail the run.
	folding := caseInsensitive(dest)
	claimed := map[string]string{}
	held := map[[2]uint64]string{}
	hold := func(rel, out string) {
		if st, err := os.Lstat(out); err == nil {
			held[devIno(st)] = rel
		}
	}
	collide := func(rel string, isDir bool) error {
		mf.Collided = append(mf.Collided, rel)
		m.Log.Warn("files snapshot: left out an entry the backup disk cannot hold beside another", "path", DisplayName(rel))
		if isDir {
			return fs.SkipDir
		}
		return nil
	}
	walkErr := m.snapWalk(ctx, root, "", func(p string) { tooDeep = append(tooDeep, p) }, func(rel string, info fs.FileInfo) error {
		if es.prunes(rel) || (m.BackupSkip != nil && rel != "" && m.BackupSkip(rel)) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if folding && rel != "" {
			key := strings.ToLower(rel)
			if other, ok := claimed[key]; ok && other != rel {
				return collide(rel, info.IsDir())
			}
			claimed[key] = rel
		}
		out := filepath.Join(dest, filepath.FromSlash(rel))
		if rel != "" {
			// The backstop, exact for whatever the backup disk folds
			// together (case, Unicode normalisation, a share's own rules):
			// the disk itself says whether this name lands on an entry
			// another Drive entry already placed this run. A key-based fold
			// can only approximate the target's rules; the inode cannot be
			// wrong.
			if st, err := os.Lstat(out); err == nil {
				if other, ok := held[devIno(st)]; ok && other != rel {
					return collide(rel, info.IsDir())
				} else if !ok && links {
					return collide(rel, info.IsDir()) // a fresh snapshot holds nothing it did not place
				}
			}
		}
		if info.IsDir() {
			if rel == "" {
				return nil
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, fs.ErrExist) {
					return collide(rel, true)
				}
				return err
			}
			hold(rel, out)
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks and specials are not backed up
		}
		seen[rel] = true
		p.Current(rel)
		sf := snapFile{Path: rel, Size: info.Size(), Mtime: info.ModTime().UnixNano(), Inode: inodeOf(info)}
		if old, ok := prevFiles[rel]; ok && old.Size == sf.Size && old.Mtime == sf.Mtime && old.Inode == sf.Inode && old.SHA256 != "" {
			sf.SHA256 = old.SHA256
			if links {
				if err := os.Link(filepath.Join(prevDir, filepath.FromSlash(rel)), out); err == nil {
					hold(rel, out)
					sf.Linked = true
					mf.Files = append(mf.Files, sf)
					mf.Bytes += sf.Size
					p.Add(sf.Size, 1)
					return nil
				}
				// The previous copy is gone or unlinkable: copy afresh.
			} else {
				// Mirror: unchanged file already there.
				if st, err := os.Lstat(out); err == nil && st.Size() == sf.Size {
					hold(rel, out)
					sf.Linked = true
					mf.Files = append(mf.Files, sf)
					mf.Bytes += sf.Size
					p.Add(sf.Size, 1)
					return nil
				}
			}
		}
		if err := gauge.reserve(sf.Size); err != nil {
			m.diskFull(sf.Size)
			m.Store.CreateNotice(NoticeFilesBackupNoSpace, "", "Drive backup stopped: the backup disk is nearly full",
				err.Error()+". Exclude large folders from Drive backups, free space there, or move the backup directory to a bigger disk.")
			return err
		}
		sum, torn, err := copyOut(ctx, root, rel, out, info, p)
		if errors.Is(err, ErrNotFound) {
			// Deleted between the listing and the copy (an app's cache
			// churning): it is simply not in this snapshot.
			delete(seen, rel)
			return nil
		}
		if err != nil {
			if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) {
				delete(seen, rel)
				return collide(rel, false)
			}
			return fmt.Errorf("copy %s: %w", rel, err)
		}
		hold(rel, out)
		sf.SHA256 = sum
		if torn {
			mf.Torn = append(mf.Torn, rel)
		}
		mf.Files = append(mf.Files, sf)
		mf.Bytes += sf.Size
		mf.NewBytes += sf.Size
		p.Add(0, 1)
		return nil
	})
	if walkErr != nil {
		if links {
			os.RemoveAll(dest)
		}
		return store.FileSnapshot{}, walkErr
	}

	if !links {
		// Mirror: park what the root no longer has.
		for rel := range prevFiles {
			if seen[rel] || es.prunes(rel) {
				continue
			}
			src := filepath.Join(dest, filepath.FromSlash(rel))
			// On a disk that folds names, rel may now name the copy another
			// entry placed this run: never park that.
			if st, err := os.Lstat(src); err == nil {
				if _, ok := held[devIno(st)]; ok {
					continue
				}
			}
			park := filepath.Join(dest, mirrorDeleted, stamp, filepath.FromSlash(rel))
			os.MkdirAll(filepath.Dir(park), 0o755)
			os.Rename(src, park)
		}
		m.pruneMirrorDeleted(dest)
	}

	if err := writeManifest(dest, mf); err != nil {
		if links {
			os.RemoveAll(dest)
		}
		return store.FileSnapshot{}, err
	}
	final := dest
	if links {
		final = filepath.Join(base, stamp)
		if err := os.Rename(dest, final); err != nil {
			os.RemoveAll(dest)
			return store.FileSnapshot{}, err
		}
		latest := filepath.Join(base, "latest")
		os.Remove(latest)
		os.Symlink(stamp, latest)
	}
	id, err := m.Store.CreateFileSnapshot(final, mode, int64(len(mf.Files)), mf.Bytes, mf.NewBytes, len(mf.Torn))
	if err != nil {
		return store.FileSnapshot{}, err
	}
	m.Store.DismissNoticeByKind(NoticeFilesBackupStale, "")
	m.Store.DismissNoticeByKind(NoticeFilesBackupNoSpace, "")
	m.noteTooDeep(tooDeep)
	m.noteCollided(mf.Collided)
	m.pruneSnapshots(base)
	row, err := m.Store.FileSnapshotByID(id)
	if err != nil {
		return store.FileSnapshot{}, err
	}
	m.Log.Info("files snapshot written", "path", final, "files", len(mf.Files), "bytes", mf.Bytes, "new", mf.NewBytes, "torn", len(mf.Torn), "mode", mode)
	return *row, nil
}

// snapWalk visits the root pre-order with lstat info, skipping .fog.
// deep, when set, is told of each folder left out for being past MaxDepth.
func (m *Manager) snapWalk(ctx context.Context, root *os.Root, rel string, deep func(rel string), fn func(rel string, info fs.FileInfo) error) error {
	info, err := lstatNF(root, rel)
	if err != nil {
		return err
	}
	// A directory that vanished mid-walk is not a failed backup, and nor is
	// one nested past MaxDepth: an app can build a tree no walk finishes,
	// and that must not stop everyone else's files being backed up. It is
	// left out, and said so: here in the log, and by the run in the Inbox
	// (noteTooDeep).
	return walkTreeOpts(ctx, root, rel, info, walkOpts{vanishedOK: true, tooDeep: func(p string) {
		if deep != nil {
			deep(p)
		}
		m.Log.Warn("files snapshot: folder too deep to back up, left out", "path", DisplayName(p), "max_depth", MaxDepth)
	}}, fn)
}

// copyOut copies one file from the root to an absolute destination,
// hashing as it goes, and re-checks the source afterwards: a file that
// changed while it was being read is copied again (three tries), and
// reported as torn if it never holds still. Every copy, torn ones included,
// goes through a temporary name and a rename so a crash leaves nothing
// under the final one.
func copyOut(ctx context.Context, root *os.Root, rel, out string, info fs.FileInfo, p *jobs.Progress) (sum string, torn bool, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		in, _, err := openFileNF(root, rel)
		if err != nil {
			return "", false, err
		}
		var moved fs.FileInfo
		sum, err := atomicWrite(osFS{}, out, in, writeOpts{
			perm: 0o644, mtime: info.ModTime(), hash: true, p: p,
			beforeRename: func(string) error {
				after, err := lstatNF(root, rel)
				if err != nil {
					return err
				}
				if after.Size() != info.Size() || after.ModTime() != info.ModTime() {
					moved = after
					return errMovedUnderneath
				}
				return nil
			},
		})
		in.Close()
		if errors.Is(err, errMovedUnderneath) {
			// Moved underneath us; try again against what is there now.
			info = moved
			if p != nil {
				p.Add(-info.Size(), 0)
			}
			continue
		}
		return sum, false, err
	}
	// Never held still: keep the last copy, say so.
	in, _, err := openFileNF(root, rel)
	if err != nil {
		return "", true, err
	}
	defer in.Close()
	sum, err = atomicWrite(osFS{}, out, in, writeOpts{perm: 0o644, hash: true})
	return sum, true, err
}

var errMovedUnderneath = errors.New("file changed while being copied")

// sweepPartials removes abandoned in-progress snapshot directories.
func (m *Manager) sweepPartials(base string) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		p := filepath.Join(base, e.Name())
		if err := os.RemoveAll(p); err != nil {
			m.Log.Warn("files snapshot: removing abandoned partial", "path", p, "err", err)
			continue
		}
		m.Log.Info("files snapshot: removed abandoned partial", "path", p)
	}
}

// preflight walks the root as the run will, totals what it would copy
// (everything not carried over from the previous snapshot), and refuses the
// run if that would leave the backup disk below its floor. The refusal
// names the largest top-level folders, which is what the operator excludes.
func (m *Manager) preflight(ctx context.Context, root *os.Root, base string, links bool, prevFiles map[string]snapFile, es excludeSet) error {
	var need int64
	byTop := map[string]int64{}
	err := m.snapWalk(ctx, root, "", nil, func(rel string, info fs.FileInfo) error {
		if es.prunes(rel) || (m.BackupSkip != nil && rel != "" && m.BackupSkip(rel)) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		if old, ok := prevFiles[rel]; ok && old.Size == info.Size() && old.Mtime == info.ModTime().UnixNano() && old.Inode == inodeOf(info) && old.SHA256 != "" {
			if links {
				return nil // a hardlink costs nothing
			}
			if st, err := os.Lstat(filepath.Join(base, mirrorDir, filepath.FromSlash(rel))); err == nil && st.Size() == info.Size() {
				return nil
			}
		}
		need += info.Size()
		top, _, _ := strings.Cut(rel, "/")
		byTop[top] += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	if err := checkSpace(base, need); err != nil {
		m.diskFull(need)
		tops := make([]string, 0, len(byTop))
		for t := range byTop {
			tops = append(tops, t)
		}
		slices.SortFunc(tops, func(a, b string) int { return cmp.Compare(byTop[b], byTop[a]) })
		var biggest []string
		for _, t := range tops[:min(3, len(tops))] {
			biggest = append(biggest, fmt.Sprintf("%s (%s)", t, fsx.HumanBytes(byTop[t])))
		}
		m.Store.CreateNotice(NoticeFilesBackupNoSpace, "", "Drive backup can't fit on the backup disk",
			fmt.Sprintf("Tonight's snapshot would copy %s, and the backup disk doesn't have it: %s. The most to copy is in %s. "+
				"Exclude large folders from Drive backups, or move the backup directory to a bigger disk. "+
				"fog tries again tomorrow night, or sooner once there is room.",
				fsx.HumanBytes(need), err.Error(), strings.Join(biggest, ", ")))
		return err
	}
	return nil
}

// diskFull remembers that a run could not fit and how much it needed, so
// the scheduler stops retrying hourly: free space does not grow by itself,
// and each doomed attempt costs a walk of the whole Drive.
func (m *Manager) diskFull(need int64) {
	m.snapFullMu.Lock()
	defer m.snapFullMu.Unlock()
	m.snapFullAt, m.snapFullNeed = time.Now(), need
}

// roomNow says whether the backup disk has since gained room for what the
// last refused run needed.
func (m *Manager) roomNow() bool {
	m.snapFullMu.Lock()
	need := m.snapFullNeed
	m.snapFullMu.Unlock()
	base, err := m.snapshotsDir()
	if err != nil {
		return false
	}
	return checkSpace(base, need) == nil
}

// checkSpace refuses a copy of size bytes into dir that would leave its
// filesystem below the free-space floor.
func checkSpace(dir string, size int64) error {
	avail, total, ok := diskSpace(dir)
	if !ok {
		return nil // can't tell; the write itself will say if it fails
	}
	if floor := freeFloor(total); avail-size < floor {
		return diskFullError(dir, avail, size, floor)
	}
	return nil
}

func freeFloor(total int64) int64 {
	return max(int64(minFreeBytes), int64(float64(total)*minFreeFrac))
}

func diskFullError(dir string, avail, size, floor int64) error {
	return fmt.Errorf("%w: %s free on %s, and the next file needs %s while %s must stay free",
		ErrBackupDiskFull, fsx.HumanBytes(avail), dir, fsx.HumanBytes(size), fsx.HumanBytes(floor))
}

// spaceRecheck is how much a snapshot copies between looks at the backup
// disk's free space.
const spaceRecheck = 256 << 20

// spaceGauge keeps a snapshot from filling the backup disk past its floor
// without asking the filesystem before every file: a statfs on a NAS is a
// goroutine and a timer (nas.Statfs), and a Drive is mostly small files.
// It reads the free space once, counts down what each copy takes from it,
// and reads again after spaceRecheck bytes — other writers share the disk
// — or when the next file is bigger than the room it last saw.
type spaceGauge struct {
	dir   string
	read  bool
	avail int64 // as of the last read, less what was copied since
	floor int64
	since int64 // bytes copied since the last read
	blind bool  // the last read could not tell
}

// reserve refuses a copy of size bytes that would take the disk below its
// floor, and otherwise counts it as taken.
func (g *spaceGauge) reserve(size int64) error {
	if !g.read || g.since >= spaceRecheck || (!g.blind && g.avail-size < g.floor) {
		avail, total, ok := diskSpace(g.dir)
		g.read, g.since, g.blind = true, 0, !ok
		g.avail, g.floor = avail, freeFloor(total)
	}
	if g.blind {
		g.since += size
		return nil // can't tell; the write itself will say if it fails
	}
	if g.avail-size < g.floor {
		return diskFullError(g.dir, g.avail, size, g.floor)
	}
	g.avail -= size
	g.since += size
	return nil
}

// diskSpace reports the bytes available to us and the filesystem's size.
// The backup directory is often on a NAS, so it goes through nas.Statfs: a
// dead hard mount answers "can't tell" instead of hanging the snapshot.
// A variable so tests decide how full the disk is, not the machine running
// them.
var diskSpace = func(dir string) (avail, total int64, ok bool) {
	st, stale, err := nas.Statfs(dir)
	if err != nil || stale {
		return 0, 0, false
	}
	bsize := int64(st.Bsize)
	return int64(st.Bavail) * bsize, int64(st.Blocks) * bsize, true
}

// pruneSnapshots keeps the newest retention() link snapshots (mirror rows
// beyond retention are just records) and never removes the last one. A
// link row under base whose directory is gone is dropped: it can't be
// browsed or restored, and listing it would promise a backup that isn't
// there.
func (m *Manager) pruneSnapshots(base string) {
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return
	}
	keep := m.retention()
	var links []store.FileSnapshot
	for _, r := range rows {
		if r.Mode != "links" {
			continue
		}
		if filepath.Dir(r.Path) == base {
			if _, err := os.Stat(r.Path); errors.Is(err, os.ErrNotExist) {
				m.Log.Warn("files snapshot: dropping a record whose directory is gone", "path", r.Path)
				m.Store.DeleteFileSnapshot(r.ID)
				continue
			}
		}
		links = append(links, r)
	}
	for i, r := range links {
		if i < keep || i == 0 {
			continue
		}
		if err := os.RemoveAll(r.Path); err != nil {
			m.Log.Warn("files snapshot prune", "path", r.Path, "err", err)
			continue
		}
		m.Store.DeleteFileSnapshot(r.ID)
	}
	// Mirror rows: keep the newest few as history of runs.
	n := 0
	for _, r := range rows {
		if r.Mode != "mirror" {
			continue
		}
		n++
		if n > keep {
			m.Store.DeleteFileSnapshot(r.ID)
		}
	}
}

func (m *Manager) pruneMirrorDeleted(mirror string) {
	ents, err := os.ReadDir(filepath.Join(mirror, mirrorDeleted))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-mirrorDeletedKeep)
	for _, e := range ents {
		t, ok := fsx.ParseStamp(e.Name())
		if ok && t.Before(cutoff) {
			os.RemoveAll(filepath.Join(mirror, mirrorDeleted, e.Name()))
		}
	}
}

// StartSnapshot runs a snapshot as a job.
func (m *Manager) StartSnapshot(label string) (jobs.Job, error) {
	if _, err := m.snapshotsDir(); err != nil {
		return jobs.Job{}, err
	}
	job, ok := m.Jobs.StartExclusive("snapshot", label, func(ctx context.Context, p *jobs.Progress) error {
		release, err := m.holdBackups()
		if err != nil {
			return err
		}
		defer release()
		row, err := m.RunSnapshot(ctx, p)
		if err != nil {
			m.Log.Warn("files snapshot failed", "err", err)
			if m.Notify != nil {
				// The error can carry a path, and a path is whatever an
				// app named its files: shown as DisplayName, never raw.
				m.Notify("Drive backup failed", DisplayName(err.Error()), false)
			}
			return err
		}
		p.Result(row)
		return nil
	}, backupMoveJob)
	if !ok {
		return jobs.Job{}, ErrSnapshotRunning
	}
	return job, nil
}

// SnapshotLoop is the scheduler: every five minutes, if backups are on and
// an hour is set and no snapshot has landed since it was due, run one.
// Due-ness comes from the store, so a restart neither skips nor repeats a
// night. Files go after the apps' own hour so an app backup never queues
// behind a large file copy: the check simply starts fifteen minutes later.
func (m *Manager) SnapshotLoop(ctx context.Context) {
	loop.Every(ctx, 90*time.Second, 5*time.Minute, func(ctx context.Context) {
		m.runDue(ctx, time.Now().UTC())
	})
}

func (m *Manager) runDue(ctx context.Context, now time.Time) {
	m.checkStaleness(now)
	if !m.BackupEnabled() {
		return
	}
	hour, ok := m.scheduledHour()
	if !ok {
		return
	}
	due := time.Date(now.Year(), now.Month(), now.Day(), hour, 15, 0, 0, time.UTC)
	if due.After(now) {
		due = due.Add(-24 * time.Hour)
	}
	last, _ := m.Store.LatestFileSnapshotAt()
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && !t.Before(due) {
			return
		}
	}
	// The backup directory is moving; the next tick after it is due still.
	if m.Jobs.Running(backupMoveJob) {
		return
	}
	// One attempt per hour when it keeps failing (a NAS that is away).
	if !m.lastSnapAttempt.IsZero() && now.Sub(m.lastSnapAttempt) < time.Hour {
		return
	}
	// A run that could not fit since this night's due waits for tomorrow's,
	// unless room has been made since.
	m.snapFullMu.Lock()
	fullAt := m.snapFullAt
	m.snapFullMu.Unlock()
	if !fullAt.IsZero() && !fullAt.Before(due) && !m.roomNow() {
		return
	}
	m.lastSnapAttempt = now
	if _, err := m.StartSnapshot("Nightly files snapshot"); err != nil && !errors.Is(err, ErrSnapshotRunning) {
		m.Log.Warn("files snapshot", "err", err)
	}
}

func (m *Manager) scheduledHour() (int, bool) {
	v, _ := m.Store.GetSetting(backup.SettingBackupHourUTC)
	if v == "" {
		return 0, false
	}
	h, err := strconv.Atoi(v)
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	return h, true
}

// checkStaleness is the dead-man's switch: scheduled, and the newest
// snapshot missing or older than staleAfter, raises a notice (and one
// ntfy a day). Nothing to say when there is nothing to back up yet.
func (m *Manager) checkStaleness(now time.Time) {
	if !m.BackupEnabled() {
		m.Store.DismissNoticeByKind(NoticeFilesBackupStale, "")
		return
	}
	if _, ok := m.scheduledHour(); !ok {
		return
	}
	last, _ := m.Store.LatestFileSnapshotAt()
	stale := last == ""
	if !stale {
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			stale = now.Sub(t) > staleAfter
		}
	}
	if !stale {
		m.Store.DismissNoticeByKind(NoticeFilesBackupStale, "")
		return
	}
	// A fresh install with an empty root has nothing overdue.
	if last == "" {
		if has, _ := hasContent(m.Root()); !has {
			return
		}
	}
	m.Store.CreateNotice(NoticeFilesBackupStale, "", "Drive backup is overdue",
		"The nightly Drive snapshot has not landed in over a day. If the backup directory is on a NAS, check that the share is mounted (Settings → Storage → Disks & shares).")
	if m.Notify != nil && (m.lastStaleAlert.IsZero() || now.Sub(m.lastStaleAlert) > 24*time.Hour) {
		m.lastStaleAlert = now
		m.Notify("Drive backup is overdue", "No Drive snapshot in over a day.", false)
	}
}

// BackupStatus is the Files row of the fleet backup view.
type BackupStatus struct {
	Enabled        bool   `json:"enabled"`
	HourUTC        *int   `json:"hourUTC"`
	NextAt         string `json:"nextAt,omitempty"`
	LastAt         string `json:"lastAt,omitempty"`
	LastBytes      int64  `json:"lastBytes,omitempty"`
	LastNewBytes   int64  `json:"lastNewBytes,omitempty"`
	LastFiles      int64  `json:"lastFiles,omitempty"`
	LastMode       string `json:"lastMode,omitempty" ts:"\"links\" | \"mirror\""`
	LastWarnings   int    `json:"lastWarnings,omitempty"`
	LastVerifiedAt string `json:"lastVerifiedAt,omitempty"`
	Stale          bool   `json:"stale"`
	Running        bool   `json:"running"`
	Snapshots      int    `json:"snapshots"`
	Excluded       int    `json:"excluded"`
	// Target is the snapshots directory, for the operator's orientation.
	Target string `json:"target,omitempty"`
	Error  string `json:"error,omitempty"`
	// Offsite is the newest snapshot's state in the offsite targets.
	Offsite string `json:"offsite,omitempty" ts:"\"\" | \"verified\" | \"stored\" | \"uploading\" | \"pending\""`
}

func (m *Manager) BackupStatus(now time.Time) BackupStatus {
	st := BackupStatus{Enabled: m.BackupEnabled(), Running: m.Jobs.Running("snapshot")}
	if dir, err := m.snapshotsDir(); err == nil {
		st.Target = dir
	} else {
		st.Error = err.Error()
	}
	rows, _ := m.Store.ListFileSnapshots()
	st.Snapshots = len(rows)
	if len(rows) > 0 {
		r := rows[0]
		st.LastAt, st.LastBytes, st.LastNewBytes, st.LastFiles = r.CreatedAt, r.Bytes, r.NewBytes, r.Files
		st.LastMode, st.LastWarnings, st.LastVerifiedAt = r.Mode, r.Warnings, r.VerifiedAt
		offsite, _ := m.Store.OffsiteStatesOf(store.OffsiteKindFilesSnapshot)
		st.Offsite = offsite.Of(strconv.FormatInt(r.ID, 10))
	}
	if ex, err := m.Store.ListFileExcludes(); err == nil {
		st.Excluded = len(ex)
	}
	if st.Enabled {
		if hour, ok := m.scheduledHour(); ok {
			h := hour
			st.HourUTC = &h
			next := time.Date(now.Year(), now.Month(), now.Day(), hour, 15, 0, 0, time.UTC)
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			st.NextAt = next.Format(time.RFC3339)
			st.Stale = st.LastAt == ""
			if !st.Stale {
				if t, err := time.Parse(time.RFC3339, st.LastAt); err == nil {
					st.Stale = now.Sub(t) > staleAfter
				}
			}
			if st.LastAt == "" {
				if has, _ := hasContent(m.Root()); !has {
					st.Stale = false
				}
			}
		}
	}
	return st
}

// ListSnapshots returns the recorded snapshots that are still on disk.
func (m *Manager) ListSnapshots() ([]store.FileSnapshot, error) {
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return nil, err
	}
	offsite, _ := m.Store.OffsiteStatesOf(store.OffsiteKindFilesSnapshot)
	out := []store.FileSnapshot{}
	for _, r := range rows {
		if _, err := os.Stat(filepath.Join(r.Path, snapshotManifest)); err == nil {
			r.Offsite = offsite.Of(strconv.FormatInt(r.ID, 10))
			out = append(out, r)
		}
	}
	return out, nil
}

// DeleteSnapshot removes one snapshot for good, never the last.
func (m *Manager) DeleteSnapshot(id int64) error {
	release, err := m.holdBackups()
	if err != nil {
		return err
	}
	defer release()
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return err
	}
	links := 0
	var row *store.FileSnapshot
	for i := range rows {
		if rows[i].Mode == "links" {
			links++
		}
		if rows[i].ID == id {
			row = &rows[i]
		}
	}
	if row == nil {
		return ErrNotFound
	}
	if row.Mode == "mirror" {
		return errors.New("a mirror is the only copy; it cannot be deleted from here")
	}
	if links <= 1 {
		return ErrLastSnapshot
	}
	if err := os.RemoveAll(row.Path); err != nil {
		return err
	}
	return m.Store.DeleteFileSnapshot(id)
}

// ---- browsing a snapshot ----

// snapshotRoot opens a snapshot's tree read-only through *os.Root.
func (m *Manager) snapshotRoot(id int64) (*os.Root, *store.FileSnapshot, error) {
	row, err := m.Store.FileSnapshotByID(id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	root, err := os.OpenRoot(row.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return root, row, nil
}

func snapNode(rel string, fi fs.FileInfo) Node {
	n := Node{Path: rel, Name: fi.Name(), Kind: kindOf(fi.Mode()), mtime: fi.ModTime().UnixNano()}
	n.Modified = fi.ModTime().UTC().Format(time.RFC3339)
	n.Mode = fi.Mode().String()
	if n.Kind == "file" {
		n.Size = fi.Size()
		n.Mime = MimeFor(n.Name)
	}
	return n
}

// SnapshotList lists one directory of a snapshot. No index: the tree is
// read and sorted in memory, and the cursor is an offset. Snapshot
// directories are as big as the root's, so this is fine for the same
// reason the root's are.
func (m *Manager) SnapshotList(id int64, rel string, o ListOpts) (Listing, error) {
	root, _, err := m.snapshotRoot(id)
	if err != nil {
		return Listing{}, err
	}
	defer root.Close()
	// No link followed, here as in the live root: a snapshot is a copy of
	// what apps wrote.
	info, err := lstatNF(root, rel)
	if err != nil {
		return Listing{}, mapOSError(err)
	}
	if !info.IsDir() {
		return Listing{}, ErrNotDirectory
	}
	dfd, err := openDirFD(root, rel)
	if err != nil {
		return Listing{}, mapOSError(err)
	}
	// Each entry lstat'ed through the folder's descriptor (readEntriesAt),
	// never through a path made from a descriptor's name.
	ents, err := readEntriesAt(dfd)
	unix.Close(dfd)
	if err != nil {
		return Listing{}, err
	}
	var nodes []Node
	for _, fi := range ents {
		if rel == "" && (fi.Name() == snapshotManifest || fi.Name() == mirrorDeleted) {
			continue
		}
		nodes = append(nodes, snapNode(Join(rel, fi.Name()), fi))
	}
	if o.Sort == "" {
		o.Sort = SortName
	}
	sortNodes(nodes, o.Sort, o.Desc)
	if o.Limit <= 0 || o.Limit > 2000 {
		o.Limit = 500
	}
	start, _ := strconv.Atoi(o.Cursor)
	if start < 0 || start > len(nodes) {
		start = 0
	}
	end := start + o.Limit
	next := ""
	if end < len(nodes) {
		next = strconv.Itoa(end)
	} else {
		end = len(nodes)
	}
	page := nodes[start:end]
	if page == nil {
		page = []Node{}
	}
	// Item counts for just the folders on this page: each is a directory
	// read, often on a NAS, and the rest of the listing never shows them.
	for i := range page {
		if page[i].Kind != "dir" {
			continue
		}
		if sub, err := openDirNF(root, page[i].Path); err == nil {
			if names, err := sub.Readdirnames(-1); err == nil {
				page[i].Entries = len(names)
			}
			sub.Close()
		}
	}
	dir := snapNode(rel, info)
	if rel == "" {
		dir.Name = ""
	}
	dir.Entries = len(nodes)
	l := Listing{Path: rel, Root: rel == "", Dir: dir, Entries: page, Next: next, Total: len(nodes), Sort: o.Sort, Desc: o.Desc}
	if rel != "" {
		l.Parent, _ = Split(rel)
	}
	return l, nil
}

func sortNodes(nodes []Node, s Sort, desc bool) {
	slices.SortFunc(nodes, func(a, b Node) int {
		if a.IsDir() != b.IsDir() {
			if a.IsDir() {
				return -1
			}
			return 1
		}
		var c int
		switch s {
		case SortMtime:
			c = cmp.Compare(a.mtime, b.mtime)
		case SortSize:
			c = cmp.Compare(a.Size, b.Size)
		case SortKind:
			c = cmp.Compare(strings.ToLower(a.Mime), strings.ToLower(b.Mime))
		default:
			c = cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
		if desc {
			c = -c
		}
		// Ties break by name, ascending either way — as the live listing
		// breaks them by id — so equal keys keep one order in both
		// directions instead of whatever the sort leaves them in.
		if c == 0 {
			c = cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
		}
		return c
	})
}

// SnapshotOpenFile opens one file of a snapshot for reading.
func (m *Manager) SnapshotOpenFile(id int64, rel string) (*os.File, Node, error) {
	root, _, err := m.snapshotRoot(id)
	if err != nil {
		return nil, Node{}, err
	}
	defer root.Close()
	if rel == "" {
		return nil, Node{}, ErrIsDirectory
	}
	f, info, err := openFileNF(root, rel)
	if err != nil {
		return nil, Node{}, mapOSError(err)
	}
	return f, snapNode(rel, info), nil
}

// RestoreSnapshot copies entries out of a snapshot into the root — to
// their original place, or into the folder to — as a job. Copies, never
// links: the snapshot stays what it was.
func (m *Manager) RestoreSnapshot(id int64, rels []string, to string, toSet bool, c Conflict) (_ jobs.Job, err error) {
	release, err := m.holdBackups()
	if err != nil {
		return jobs.Job{}, err
	}
	// Held until the job is done; released here only if it never starts.
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	snap, row, err := m.snapshotRoot(id)
	if err != nil {
		return jobs.Job{}, err
	}
	root, ix, err := m.rootAndIndex()
	if err != nil {
		snap.Close()
		return jobs.Job{}, err
	}
	if toSet {
		if err := requireDir(root, to); err != nil {
			snap.Close()
			return jobs.Job{}, err
		}
	}
	// Conflicts are decided up front for "ask".
	for _, rel := range rels {
		if !exists(snap, rel) && rel != "" {
			snap.Close()
			return jobs.Job{}, ErrNotFound
		}
		dest := restoreDest(rel, to, toSet)
		if c == ConflictAsk && dest != "" && exists(root, dest) {
			m.refreshDir(root, ix, parentOfRel(dest), false)
			n, _ := ix.Get(dest)
			snap.Close()
			return jobs.Job{}, &ConflictError{Path: dest, Existing: n}
		}
	}
	label := fmt.Sprintf("Restore from %s", row.CreatedAt[:10])
	started = true
	return m.Jobs.Start("restore", label, func(ctx context.Context, p *jobs.Progress) error {
		defer release()
		defer snap.Close()
		var total int64
		for _, rel := range rels {
			total += treeSize(snap, rel)
		}
		p.Set("", 0, total)
		var out []string
		om := &opMounts{}
		for _, rel := range rels {
			if err := ctx.Err(); err != nil {
				return err
			}
			dest := restoreDest(rel, to, toSet)
			if dest == "" {
				// The whole snapshot into the root: entry by entry.
				ents, err := readDir(snap, "")
				if err != nil {
					return err
				}
				for _, fi := range ents {
					if fi.Name() == snapshotManifest || fi.Name() == mirrorDeleted {
						continue
					}
					d, err := m.restoreOne(ctx, snap, root, ix, fi.Name(), "", fi.Name(), c, om, p)
					if err != nil {
						return err
					}
					if d != "" {
						out = append(out, d)
					}
				}
				continue
			}
			parent, name := Split(dest)
			d, err := m.restoreOne(ctx, snap, root, ix, rel, parent, name, c, om, p)
			if err != nil {
				return err
			}
			if d != "" {
				out = append(out, d)
			}
		}
		p.Result(map[string]any{"paths": out})
		m.kickHasher()
		return nil
	}), nil
}

func parentOfRel(rel string) string {
	p, _ := Split(rel)
	return p
}

func restoreDest(rel, to string, toSet bool) string {
	if !toSet {
		return rel
	}
	if rel == "" {
		return ""
	}
	_, name := Split(rel)
	return Join(to, name)
}

// restoreOne copies snapshot entry src into root at parent/name.
func (m *Manager) restoreOne(ctx context.Context, snap, root *os.Root, ix *Index, src, parent, name string, c Conflict, om *opMounts, p *jobs.Progress) (string, error) {
	// A restore writes into Drive like any other verb: not into a folder a
	// guard keeps read-only (a copy of a peer's share), not through a link.
	if err := m.guardWrite(Join(parent, name)); err != nil {
		return "", err
	}
	if parent != "" {
		if err := mkdirAllNF(root, parent, 0o755); err != nil {
			return "", err
		}
	}
	dest, err := m.resolveDest(root, ix, parent, name, c, om)
	if err != nil {
		return "", err
	}
	if dest == "" {
		return "", nil
	}
	p.Current(src)
	info, err := lstatNF(snap, src)
	if err != nil {
		return "", mapOSError(err)
	}
	err = walkTree(snap, src, info, func(rel string, fi fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := dest + strings.TrimPrefix(rel, src)
		if fi.IsDir() {
			if err := nf(root).Mkdir(target, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return mapOSError(err)
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if err := checkNoLinks(root, target); err != nil {
			return err
		}
		return copyBetweenRoots(snap, rel, root, target, fi, p)
	})
	if err != nil {
		nf(root).RemoveAll(dest)
		return "", err
	}
	m.chownTree(root, dest)
	if info.IsDir() {
		m.walkDir(ctx, root, ix, dest, time.Now().UnixNano())
	}
	m.refreshDir(root, ix, parent, false)
	return dest, nil
}

func copyBetweenRoots(src *os.Root, srcRel string, dst *os.Root, dstRel string, info fs.FileInfo, p *jobs.Progress) error {
	in, _, err := openFileNF(src, srcRel)
	if err != nil {
		return mapOSError(err)
	}
	defer in.Close()
	if _, err := atomicWrite(nf(dst), dstRel, in, writeOpts{perm: 0o644, mtime: info.ModTime(), p: p}); err != nil {
		return mapOSError(err)
	}
	p.Add(0, 1)
	return nil
}

// ---- verification ----

// VerifySnapshots sample-rehashes the newest snapshot once a week: a
// random one percent of its files (at least five, at most two hundred)
// against the manifest. A mismatch is storage corruption on the target
// and is said out loud.
func (m *Manager) VerifySnapshots(ctx context.Context, now time.Time) {
	release, err := m.holdBackups()
	if err != nil {
		return // moving; the next pass verifies at the new place
	}
	defer release()
	rows, err := m.Store.ListFileSnapshots()
	if err != nil || len(rows) == 0 {
		return
	}
	r := rows[0]
	if r.VerifiedAt != "" {
		if t, err := time.Parse(time.RFC3339, r.VerifiedAt); err == nil && now.Sub(t) < verifyEvery {
			return
		}
	}
	if _, err := m.snapshotsDir(); err != nil {
		return // target away; not corruption
	}
	rng := rand.New(rand.NewSource(now.UnixNano()))
	sample, _, err := sampleManifest(r.Path, rng, func(total int) int {
		return min(max(total/100, 5), verifySampleMax, total)
	}, verifySampleMax)
	if err != nil {
		m.Store.CreateNotice(NoticeFilesBackupCorrupt, "", "Drive snapshot is unreadable",
			fmt.Sprintf("The newest files snapshot at %s has no readable manifest: %v", r.Path, err))
		return
	}
	n := len(sample)
	var bad []string
	for _, f := range sample {
		if ctx.Err() != nil {
			return
		}
		got, err := fsx.FileSHA256(filepath.Join(r.Path, filepath.FromSlash(f.Path)))
		if err != nil || got != f.SHA256 {
			bad = append(bad, f.Path)
		}
	}
	if len(bad) > 0 {
		m.Store.CreateNotice(NoticeFilesBackupCorrupt, "", "Drive snapshot failed verification",
			fmt.Sprintf("%d of %d sampled files in the snapshot at %s no longer match their recorded digest (for example %s). The backup target may be failing; the next snapshot writes fresh copies of changed files, but consider checking the disk or share.", len(bad), n, r.Path, DisplayName(bad[0])))
		if m.Notify != nil {
			m.Notify("Drive snapshot failed verification", fmt.Sprintf("%d sampled files do not match at %s", len(bad), r.Path), false)
		}
		return
	}
	m.Store.DismissNoticeByKind(NoticeFilesBackupCorrupt, "")
	m.Store.SetFileSnapshotVerified(r.ID)
}

// VerifyLoop runs the sample check a few minutes after boot and every six
// hours after, like the app backups' verifier.
func (m *Manager) VerifyLoop(ctx context.Context) {
	loop.Every(ctx, 5*time.Minute, 6*time.Hour, func(ctx context.Context) {
		m.VerifySnapshots(ctx, time.Now().UTC())
	})
}

// noteTooDeep raises (or, when nothing was left out, clears) the Inbox
// issue for folders a snapshot could not back up for being nested past
// MaxDepth. A successful run that quietly skipped part of Drive would be a
// backup the operator trusts and cannot restore from.
func (m *Manager) noteTooDeep(paths []string) {
	if len(paths) == 0 {
		m.Store.DismissNoticeByKind(NoticeFilesTooDeep, "")
		return
	}
	shown := make([]string, 0, 5)
	for _, p := range paths[:min(len(paths), 5)] {
		shown = append(shown, "Files/"+shortPath(p))
	}
	more := ""
	if len(paths) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(paths)-len(shown))
	}
	m.Store.CreateNotice(NoticeFilesTooDeep, "", "Drive backup left out folders nested too deeply",
		fmt.Sprintf("The last Drive snapshot backed up everything else, but these folders are nested more than %d levels deep "+
			"and Drive goes no further, so nothing in them is backed up: %s%s. Usually an app made them; move what matters "+
			"higher up, or delete them.", MaxDepth, strings.Join(shown, ", "), more))
}

// caseInsensitive probes whether dir's filesystem folds case.
func caseInsensitive(dir string) bool {
	probe := filepath.Join(dir, ".fog-case-probe-X")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return false
	}
	defer os.Remove(probe)
	_, err := os.Lstat(filepath.Join(dir, ".fog-case-probe-x"))
	return err == nil
}

// noteCollided raises (or clears) the Inbox issue for entries a snapshot
// left out because the backup disk folds their names into another's.
func (m *Manager) noteCollided(paths []string) {
	if len(paths) == 0 {
		m.Store.DismissNoticeByKind(NoticeFilesCollided, "")
		return
	}
	shown := make([]string, 0, 5)
	for _, p := range paths[:min(len(paths), 5)] {
		shown = append(shown, "Files/"+shortPath(p))
	}
	more := ""
	if len(paths) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(paths)-len(shown))
	}
	m.Store.CreateNotice(NoticeFilesCollided, "", "Drive backup left out files whose names clash on the backup disk",
		fmt.Sprintf("The backup disk treats names that differ only in case (or accents' encoding) as the same, and these Drive entries "+
			"share a name that way with another, so the last snapshot backed up the other and left them out: %s%s. Rename one of each pair "+
			"to have both backed up, or use a case-sensitive backup disk.", strings.Join(shown, ", "), more))
}

// devIno is an lstat's device and inode.
func devIno(fi fs.FileInfo) [2]uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return [2]uint64{uint64(st.Dev), st.Ino}
	}
	return [2]uint64{}
}
