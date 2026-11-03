package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/nas"
)

// The backup directory is the third folder the Drive settings page places,
// beside Apps and Content. It is not inside Drive and nothing binds it: the
// backup manager reads backup_dir and writes there. What it shares with the
// other two is the job that changes it — move the files or adopt what is
// already at the new place — because a bare setting write would leave every
// snapshot fog has taken behind, the Drive snapshots and recovery kits out
// of sight at once. The job lives here because the tree-moving machinery
// does.
//
// Unlike Apps and Content, app backups and Drive snapshots are recorded by
// absolute path, so the records follow the files: re-pointed together with
// the setting in one transaction, after the copy is verified and before
// the old copy is removed.

// BackupsFolder is the name the settings page lists the backup directory
// under.
const BackupsFolder = "Backups"

// backupMoveJob is the move's job kind; a Drive snapshot, a root move and
// a placement move each refuse to start beside it.
const backupMoveJob = "backup-move"

// holdBackups holds the backup directory for one piece of work.
func (m *Manager) holdBackups() (func(), error) {
	if m.HoldBackupDir == nil {
		return func() {}, nil
	}
	return m.HoldBackupDir()
}

// DefaultBackupDir is where backups go when backup_dir is empty.
func (m *Manager) DefaultBackupDir() string { return filepath.Join(m.DataDir, "backups") }

// backupDir is the backup directory in effect.
func (m *Manager) backupDir() string {
	if m.BackupRoot != nil {
		if d := m.BackupRoot(); d != "" {
			return filepath.Clean(d)
		}
	}
	return m.DefaultBackupDir()
}

// BackupPlacement describes the backup directory the way Placements
// describes Apps and Content. Location is always the directory in effect;
// Default says it is fog's own default.
func (m *Manager) BackupPlacement() Placement {
	dir := m.backupDir()
	set, _ := m.Store.GetSetting(backup.SettingBackupDir)
	p := Placement{Folder: BackupsFolder, Location: dir, Default: set == ""}
	switch {
	case m.Jobs.Running(backupMoveJob):
		p.Error = "it is being moved"
	default:
		if name, reason := m.shareState(dir); reason != "" {
			p.Error = fmt.Sprintf("it is on the NAS share %q, which is %s", name, reason)
		}
	}
	p.Bound = p.Error == ""
	p.FsMount = "/"
	for mp := range mountedPoints() {
		if IsUnder(dir, mp) && len(mp) > len(p.FsMount) {
			p.FsMount = mp
		}
	}
	if p.Error == "" {
		if fs, stale, err := nas.Statfs(dir); err == nil && !stale {
			p.TotalBytes = uint64(fs.Blocks) * uint64(fs.Bsize)
			p.FreeBytes = uint64(fs.Bavail) * uint64(fs.Bsize)
		}
	}
	return p
}

// validateBackupDir checks a new backup directory: absolute, apart from
// the current one, from Drive and from the folders placed out of it, not
// one of fog's own trees (its own default excepted), on a share that is
// mounted, and writable.
func (m *Manager) validateBackupDir(to, cur string) error {
	if !filepath.IsAbs(to) {
		return errors.New("the backup directory must be an absolute path")
	}
	if to == "/" {
		return errors.New("the backup directory cannot be the filesystem root")
	}
	if IsUnder(to, cur) || IsUnder(cur, to) {
		return fmt.Errorf("the new place cannot be inside the current backup directory (%s), or contain it", cur)
	}
	if root := m.Root(); IsUnder(to, root) || IsUnder(root, to) {
		return errors.New("the backup directory must be outside the Drive location, and not contain it")
	}
	for _, folder := range placeable {
		if o := m.Location(folder); o != "" && (IsUnder(to, o) || IsUnder(o, to)) {
			return fmt.Errorf("that overlaps where %s lives (%s)", folder, o)
		}
	}
	data := filepath.Clean(m.DataDir)
	if IsUnder(data, to) {
		return errors.New("the backup directory cannot contain fog's own data directory")
	}
	if to != m.DefaultBackupDir() {
		for _, t := range managedTrees {
			if p := filepath.Join(data, t); IsUnder(to, p) {
				return fmt.Errorf("the backup directory cannot be inside %s, which belongs to fog", p)
			}
		}
	}
	if name, reason := m.shareState(to); reason != "" {
		return fmt.Errorf("%s is on the NAS share %q, which is %s — fix the share in Settings → Storage → Disks & shares first", to, name, reason)
	}
	if err := os.MkdirAll(to, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", to, err)
	}
	probe, err := os.CreateTemp(to, ".fog-probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable by fog: %w", to, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

// MoveBackups starts the job that changes where backups are kept. to is a
// host path, or "" for fog's default. mode "move" carries every snapshot,
// Drive snapshot and recovery kit there; "adopt" uses what is already at
// to — a copy made by hand, or a NAS that held this box's backups before —
// and leaves the old place alone, re-pointing only the records whose
// snapshot is there.
//
// Backups, restores and verifies wait for it: it starts once the ones
// running are done, and turns new ones away until it is.
func (m *Manager) MoveBackups(to, mode string) (jobs.Job, error) {
	if mode != "move" && mode != "adopt" {
		return jobs.Job{}, errors.New("mode must be move or adopt")
	}
	if m.LockBackupDir == nil {
		return jobs.Job{}, errors.New("the backup directory cannot be moved here")
	}
	setting := ""
	if to != "" {
		to = filepath.Clean(to)
		setting = to
	} else {
		to = m.DefaultBackupDir()
	}
	if to == m.DefaultBackupDir() {
		setting = ""
	}
	cur := m.backupDir()
	if to == cur {
		return jobs.Job{}, errors.New("backups are already kept there")
	}
	if err := m.validateBackupDir(to, cur); err != nil {
		return jobs.Job{}, err
	}
	if mode == "move" {
		if name, reason := m.shareState(cur); reason != "" {
			return jobs.Job{}, fmt.Errorf("the current backup directory is on the NAS share %q, which is %s; adopt the new place instead, or fix the share first", name, reason)
		}
		if ents, _ := os.ReadDir(to); len(ents) > 0 {
			return jobs.Job{}, fmt.Errorf("%s is not empty; choose Adopt to use what is there, or pick an empty folder", to)
		}
		if under := mountsUnder(cur); len(under) > 0 {
			return jobs.Job{}, fmt.Errorf("%s has another disk mounted inside it (%s); unmount it first", cur, under[0])
		}
	}

	label := fmt.Sprintf("Adopt backups at %s", to)
	if mode == "move" {
		label = fmt.Sprintf("Move backups to %s", to)
	}
	job, ok := m.Jobs.StartExclusive(backupMoveJob, label, func(ctx context.Context, p *jobs.Progress) error {
		p.Current("waiting for running backups to finish")
		unlock, err := m.LockBackupDir(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		if err := m.relocateBackups(ctx, cur, to, setting, mode, p); err != nil {
			return err
		}
		m.Store.Audit("system", "backup.dir", to, mode)
		return nil
	}, "snapshot", "root-move", "placement")
	if !ok {
		return jobs.Job{}, errors.New("a Drive snapshot or another move is running; try again when it is done")
	}
	return job, nil
}

// relocateBackups is the job's body, with the directory held: copy (or
// rename), re-point the records and the setting, then remove the old copy.
// A failure before the records change leaves fog where it was.
func (m *Manager) relocateBackups(ctx context.Context, cur, to, setting, mode string, p *jobs.Progress) error {
	if mode == "adopt" {
		keep := func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		}
		return m.Store.MoveBackupDir(backup.SettingBackupDir, setting, cur, to, keep)
	}
	if _, err := os.Stat(cur); errors.Is(err, os.ErrNotExist) {
		// Nothing has been backed up yet.
		return m.Store.MoveBackupDir(backup.SettingBackupDir, setting, cur, to, nil)
	}
	renamed, err := relocateTree(ctx, cur, to, "", p)
	if err != nil {
		return err
	}
	if err := m.Store.MoveBackupDir(backup.SettingBackupDir, setting, cur, to, nil); err != nil {
		if renamed {
			os.Rename(to, cur)
		}
		return err
	}
	if !renamed {
		p.Current("removing the old copy")
		if err := os.RemoveAll(cur); err != nil {
			return fmt.Errorf("backups now live at %s, but the old copy at %s could not be removed: %w", to, cur, err)
		}
	}
	return nil
}
