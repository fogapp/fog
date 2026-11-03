// The Content folder: where the household's own libraries live — Photos,
// Movies, Music — as opposed to Apps, where one app's own data lives. The
// split is the whole point of both names, so Content has to be there before
// anything is installed: a person who has just set fog up should be able to
// drop a film somewhere sensible without first working out which app will
// eventually play it.
//
// Seeding happens once per root and is remembered in settings. A library
// the operator deletes stays deleted, and a root that already has a Content
// folder is left exactly as it is.
package files

import (
	"os"
	"path/filepath"
)

// SettingContentSeeded records the root fog has already made Content in, so
// the seed runs once rather than on every open — and runs again for a root
// the operator moves Drive to.
const SettingContentSeeded = "files_content_seeded"

// EnsureContent makes Content and its starter libraries the first time fog
// prepares a root — at startup, and again after Drive moves to a new one.
// Every failure is silent by design: an unwritable or half-made Content is
// a folder the operator can make themselves, not a reason to refuse to
// open Drive.
func (m *Manager) EnsureContent() {
	if m.Store == nil {
		return
	}
	root := m.Root()
	if seeded, _ := m.Store.GetSetting(SettingContentSeeded); seeded == root {
		return
	}
	// A root on a share that is unmounted or hung is a mount point, not the
	// tree: seeding it would put six folders on the system disk underneath.
	if _, reason := m.shareState(root); reason != "" {
		return
	}
	// Likewise a Content that lives on another disk and is not bound.
	if m.placementOffline(ContentDir) != "" {
		return
	}
	base := filepath.Join(root, ContentDir)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return
	}
	m.chownPath(base)
	for _, name := range contentSeed {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o755); err == nil {
			m.chownPath(p)
		}
	}
	m.Store.SetSetting(SettingContentSeeded, root)
}

// chownPath applies files_owner to one absolute path. The Manager's own
// chown works through the open root, which is not yet open when the seed
// runs, so this is the same rule spelled against the filesystem.
func (m *Manager) chownPath(path string) {
	if uid, gid, ok := m.Owner(); ok {
		os.Chown(path, uid, gid)
	}
}
