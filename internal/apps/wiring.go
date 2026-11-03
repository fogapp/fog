package apps

// Wiring: apps that work on the same files (a downloader and the app that
// files what it downloads, a library and the app that delivers into it)
// each see a shared folder at their own container path. Fog knows both
// sides — every volume resolves to a host path — so it, not a person, works
// out how a path one app reports reads in the other, and which of a
// provider's shared folders a consumer cannot see at all
// (docs/files-contract.md, "Wiring").

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// PathRow translates a provider's container path prefix (Remote) into the
// consumer's (Local) — the same shape apps already keep for a download
// client's "remote path mappings".
type PathRow struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

// SharedMountRoot is where wiring mounts a provider's folder in a consumer
// that had no view of it: /shared/<provider>/<volume>.
const SharedMountRoot = "/shared"

// parseFollows splits a Volume.Follows value, "sabnzbd/media".
func parseFollows(f string) (app, volume string, err error) {
	app, volume, ok := strings.Cut(f, "/")
	if !ok || !nameRe.MatchString(app) || !nameRe.MatchString(volume) {
		return "", "", fmt.Errorf("follows %q must name an app and one of its volumes, like sabnzbd/media", f)
	}
	return app, volume, nil
}

func (s *AppSpec) hasVolume(name string) bool { return s.volume(name) != nil }

// volume finds a volume by name across the app and its sidecars.
func (s *AppSpec) volume(name string) *Volume {
	for i := range s.Volumes {
		if s.Volumes[i].Name == name {
			return &s.Volumes[i]
		}
	}
	for a := range s.Aux {
		for i := range s.Aux[a].Volumes {
			if s.Aux[a].Volumes[i].Name == name {
				return &s.Aux[a].Volumes[i]
			}
		}
	}
	return nil
}

// within reports whether p is base or below it.
func within(p, base string) bool {
	return p == base || strings.HasPrefix(p, strings.TrimSuffix(base, "/")+"/")
}

// sharedHostPath is where a volume binds from, cleaned, or "" for a
// managed volume — fog's private data dir, which no other app shares.
func sharedHostPath(v Volume, filesRoot string) string {
	p := v.HostPath(filesRoot)
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// TranslatePaths answers how the provider's container paths read in the
// consumer, for every folder the two both see: equal folders map mount to
// mount, and a folder one of them mounts inside the other's maps through
// the relative path between them. Longest remote first, so the first match
// is the most specific.
func TranslatePaths(provider, consumer *AppSpec, filesRoot string) []PathRow {
	seen := map[PathRow]bool{}
	rows := []PathRow{}
	for _, pv := range provider.AllVolumes() {
		ph := sharedHostPath(pv, filesRoot)
		if ph == "" {
			continue
		}
		for _, cv := range consumer.AllVolumes() {
			ch := sharedHostPath(cv, filesRoot)
			if ch == "" {
				continue
			}
			var row PathRow
			switch {
			case within(ch, ph):
				row = PathRow{Remote: joinRel(pv.Mount, ph, ch), Local: path.Clean(cv.Mount)}
			case within(ph, ch):
				row = PathRow{Remote: path.Clean(pv.Mount), Local: joinRel(cv.Mount, ch, ph)}
			default:
				continue
			}
			if !seen[row] {
				seen[row] = true
				rows = append(rows, row)
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if len(rows[i].Remote) != len(rows[j].Remote) {
			return len(rows[i].Remote) > len(rows[j].Remote)
		}
		return rows[i].Remote < rows[j].Remote
	})
	return rows
}

// joinRel is mount plus the path from base down to p (p within base).
func joinRel(mount, base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." {
		return path.Clean(mount)
	}
	return path.Join(mount, filepath.ToSlash(rel))
}

// UnseenShares lists the provider's shared volumes (Connection.Shares)
// whose whole folder no consumer volume covers, by volume name.
func UnseenShares(provider, consumer *AppSpec, filesRoot string) []string {
	if provider.Connection == nil {
		return nil
	}
	var out []string
	for _, name := range provider.Connection.Shares {
		pv := provider.volume(name)
		if pv == nil {
			continue
		}
		ph := sharedHostPath(*pv, filesRoot)
		if ph == "" || !covers(consumer, ph, filesRoot) {
			out = append(out, name)
		}
	}
	return out
}

func covers(consumer *AppSpec, hostPath, filesRoot string) bool {
	for _, cv := range consumer.AllVolumes() {
		if ch := sharedHostPath(cv, filesRoot); ch != "" && within(hostPath, ch) {
			return true
		}
	}
	return false
}

// WireShares adds to consumer a volume for each of the provider's shared
// folders it cannot see, following the provider's volume so it tracks the
// folder if it ever moves. A share the provider keeps in fog's private data
// dir cannot be followed and is left for the operator (it stays unseen).
// Answers the names of the volumes it added.
func WireShares(consumer *AppSpec, providerName string, provider *AppSpec, filesRoot string) []string {
	var added []string
	for _, name := range UnseenShares(provider, consumer, filesRoot) {
		pv := provider.volume(name)
		if pv.Managed() {
			continue
		}
		follows := providerName + "/" + name
		if existing := consumer.following(follows); existing != nil {
			// Already wired, just stale: bring it back in line.
			existing.Files, existing.Host = pv.Files, pv.Host
			added = append(added, existing.Name)
			continue
		}
		consumer.Volumes = append(consumer.Volumes, Volume{
			Name:    consumer.freeVolumeName(providerName + "-" + name),
			Mount:   consumer.freeMount(path.Join(SharedMountRoot, providerName, name)),
			Files:   pv.Files,
			Host:    pv.Host,
			Follows: follows,
		})
		added = append(added, consumer.Volumes[len(consumer.Volumes)-1].Name)
	}
	return added
}

// ResolveFollows copies each followed volume's folder into the volume that
// follows it. lookup answers an installed app's current spec, or nil. A
// target that is gone, or kept in fog's private data dir, leaves the
// follower where it was.
//
// Only a folder the target shares (connection.shares) with an app that
// uses it is ever copied. "follows: vault/data" in a spec that merely names
// another app would otherwise mount that app's own data — its credential
// store — and keep tracking it wherever it moved. Reports whether anything
// changed.
func ResolveFollows(s *AppSpec, lookup func(app string) *AppSpec) bool {
	changed := false
	resolve := func(v *Volume) {
		if v.Follows == "" {
			return
		}
		app, name, err := parseFollows(v.Follows)
		if err != nil {
			return
		}
		t := lookup(app)
		if t == nil {
			return
		}
		tv := t.volume(name)
		if tv == nil || tv.Managed() || !t.shares(name) || !s.UsesGrant(app) {
			return
		}
		if v.Files != tv.Files || v.Host != tv.Host {
			v.Files, v.Host = tv.Files, tv.Host
			changed = true
		}
	}
	for i := range s.Volumes {
		resolve(&s.Volumes[i])
	}
	for a := range s.Aux {
		for i := range s.Aux[a].Volumes {
			resolve(&s.Aux[a].Volumes[i])
		}
	}
	return changed
}

// FollowsApp reports whether any of the spec's volumes follows one of app's.
func (s *AppSpec) FollowsApp(app string) bool {
	for _, v := range s.AllVolumes() {
		if a, _, err := parseFollows(v.Follows); err == nil && a == app {
			return true
		}
	}
	return false
}

// shares reports whether the app shares volume name with the apps that use
// it.
func (s *AppSpec) shares(name string) bool {
	return s.Connection != nil && slices.Contains(s.Connection.Shares, name)
}

func (s *AppSpec) following(follows string) *Volume {
	for i := range s.Volumes {
		if s.Volumes[i].Follows == follows {
			return &s.Volumes[i]
		}
	}
	return nil
}

func (s *AppSpec) freeVolumeName(base string) string {
	name := base
	for i := 2; s.hasVolume(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func (s *AppSpec) freeMount(base string) string {
	taken := map[string]bool{}
	for _, v := range s.AllVolumes() {
		taken[path.Clean(v.Mount)] = true
	}
	m := base
	for i := 2; taken[m]; i++ {
		m = fmt.Sprintf("%s-%d", base, i)
	}
	return m
}
