package filesfed

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// The apps lens, wherever a copy lands.
//
// A share with apps: denied is for people here, not for this household's
// apps. MountPolicy refuses a deploy that would mount such a copy, the
// drive, Peers, or the peer's folder around it — but a mount already in
// place sees whatever arrives under it later: a new subscription, or an
// existing copy whose lens narrows to denied. So a copy is also held to its
// lens when it lands: one that an app's volume would reach is refused at
// subscribe, and a live one whose lens turns to denied while an app reaches
// it is taken off the disk and its subscription paused, with a notice, until
// the operator changes that app's volume and resumes it.

// NoticeAppsMount is the notice a copy withdrawn from an app's reach raises.
const NoticeAppsMount = "files-fed-apps-mount"

// ErrReachedByApps: an app's volume reaches where an apps-denied copy would
// live.
var ErrReachedByApps = errors.New("shared for people, not for apps here, but an app's volume reaches it")

// appsReaching names the apps whose volumes reach folder: mounted at it,
// inside it, or at any folder above it — the drive itself included.
func (s *Service) appsReaching(folder string) []string {
	if s.Files.MountedPaths == nil {
		return nil
	}
	root := filepath.Clean(s.Files.Root())
	var out []string
	for _, mp := range s.Files.MountedPaths() {
		p := filepath.Clean(mp.Path)
		var rel string
		switch {
		case p == root:
			rel = ""
		case strings.HasPrefix(p, root+string(filepath.Separator)):
			rel = filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		default:
			continue
		}
		if (rel == "" || under(folder, rel) || under(rel, folder)) && !slices.Contains(out, mp.App) {
			out = append(out, mp.App)
		}
	}
	return out
}

// guardAppsLens enforces sub's apps lens against the volumes in place now,
// reporting whether the copy may stay. One that may not is removed from the
// disk — every file and folder the subscription placed — and paused.
func (s *Service) guardAppsLens(sub *store.FileFedSubscription) bool {
	if sub.AppsAllowed {
		return true
	}
	apps := s.appsReaching(sub.Folder)
	if len(apps) == 0 {
		return true
	}
	// Everything the manifest placed goes — off the disk, not to the trash,
	// which would keep it there — and the folders made on the way once
	// empty. The subscriber's own files in the folder are theirs, and stay.
	s.removeCopy(sub, false)
	s.Files.EnsureDir(sub.Folder)
	sub.Status = StatusPaused
	sub.LastError = fmt.Sprintf("%s; an app's volume reaches it (%s)", ErrReachedByApps, strings.Join(apps, ", "))
	sub.ManifestETag = ""
	s.updateSubscription(sub)
	name := s.peerName(sub.PeerID, "the peer")
	s.Store.CreateNotice(NoticeAppsMount, strconv.FormatInt(sub.ID, 10),
		fmt.Sprintf("%s from %s was taken out of reach of apps", sub.Title, name),
		fmt.Sprintf("%s shares %q for people, not for apps here, and %s mounts a folder that contains Files/%s. "+
			"The copy was removed and the subscription paused. Change that app's volume, then resume the subscription on the Connect page.",
			name, sub.Title, strings.Join(apps, ", "), sub.Folder))
	s.Files.Kick()
	return false
}
