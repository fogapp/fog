// Host access: which host paths a spec may bind, and on whose say.
//
// A list of dangerous paths can never be complete: /run/systemd/private is
// root on the machine through systemd's D-Bus, and so are a hundred paths
// nobody thought to write down. So the rule runs the other way round.
// fog's own storage — Drive's folders, the NAS shares the operator set up,
// the app's own data under fog's data directory — binds freely. Any other
// host path is the operator's escape hatch, and binds only when the spec
// names it in AcknowledgeHostAccess: an explicit, per-path statement shown in
// every review and kept in every revision. A short list is never bound,
// acknowledged or not: the whole filesystem, the kernel's and devices'
// trees, a container runtime's socket, and fog's own data directory beyond
// the app's own corner of it.
package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/store"
)

// DefaultDataDir is fog's data directory on an ordinary install
// (config.Default). Validate has no materializer to ask, so it protects
// this one; Render protects the directory the running fogd actually uses.
const DefaultDataDir = "/var/lib/fog"

// refusedHostPaths are never bound: binding one, anything inside one, or a
// directory that contains one hands the app the machine whatever anyone
// acknowledged.
var refusedHostPaths = []struct{ path, why string }{
	{"/proc", "the host's process table"},
	{"/sys", "the host's kernel and device controls"},
	{"/dev", "the host's devices, disks included"},
	{"/var/run/docker.sock", "Docker's control socket, which is root on this machine"},
	{"/run/docker.sock", "Docker's control socket, which is root on this machine"},
	{"/run/docker", "Docker's runtime state, which includes its control socket"},
	{"/var/run/docker", "Docker's runtime state, which includes its control socket"},
	{"/var/lib/docker", "Docker's own storage — every container's files"},
	{"/run/containerd", "containerd's control socket, which is root on this machine"},
	{"/var/run/containerd", "containerd's control socket, which is root on this machine"},
	{"/var/lib/containerd", "containerd's own storage — every container's files"},
	{"/run/podman", "Podman's control socket, which is root on this machine"},
	{"/var/run/podman", "Podman's control socket, which is root on this machine"},
	{"/run/crio", "CRI-O's control socket, which is root on this machine"},
}

// sensitiveSocketNames are control sockets wherever they live — a runtime
// relocated under /srv is still root on the machine.
var sensitiveSocketNames = []string{"docker.sock", "containerd.sock", "podman.sock", "crio.sock", "dockershim.sock"}

// readOnlyExempt are files compose files bind all the time that carry
// nothing: the host's time zone, and the zone database /etc/localtime
// links into. Read-only only; a folder covers what is inside it.
var readOnlyExempt = []string{"/etc/localtime", "/etc/timezone", "/usr/share/zoneinfo"}

func readOnlySafe(p string) bool {
	return slices.ContainsFunc(readOnlyExempt, func(e string) bool { return hostWithin(p, e) })
}

// noChownPaths are system trees fog never re-owns, acknowledged or not: an
// acknowledgement covers a bind, and re-owning /etc breaks the machine for
// everyone, not just the app.
var noChownPaths = []string{"/etc", "/root", "/home", "/boot", "/usr", "/bin", "/sbin", "/lib", "/lib64",
	"/var/log", "/var/lib", "/var/spool", "/run", "/var/run", "/opt", "/srv", "/mnt", "/media"}

// noChown says why fog won't chown p, or "".
func noChown(p string) string {
	p = cleanHostPath(p)
	for _, n := range noChownPaths {
		// The tree itself, or anything containing it; a folder inside
		// /srv or /mnt (a share) is the operator's to own.
		if hostWithin(n, p) || (p == n) || (hostWithin(p, n) && !slices.Contains([]string{"/srv", "/mnt", "/media", "/opt", "/home"}, n)) {
			return p + " is a system folder"
		}
	}
	return ""
}

// unmanagedWhy is what a host path outside fog's storage is, for the
// acknowledgement it needs.
const unmanagedWhy = "a folder on the host outside fog's own storage (Drive, NAS shares, the app's own data), which only you can say an app may use"

// cleanHostPath is the one spelling every comparison uses: "//", trailing
// slashes, "." and ".." collapsed, so /var/run/../run/docker.sock is
// /run/docker.sock and "//" is "/".
func cleanHostPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean("/" + strings.TrimSpace(p))
}

// hostWithin reports whether p is base or inside it.
func hostWithin(p, base string) bool {
	return p == base || base == "/" || strings.HasPrefix(p, base+"/")
}

// HostStorage is what fog counts as its own storage when judging a host
// path: the app it is for, fog's data directories, Drive's roots and the
// NAS shares' mount points.
type HostStorage struct {
	App      string
	DataDirs []string // DefaultDataDir always counts
	Managed  []string // NAS share mount points
	Drive    []string // Drive roots beyond <data>/files
	// Shared reports whether a Drive folder (relative to Drive's root) is
	// one an app this one uses shares with it (Connection.Shares). Nil:
	// none is.
	Shared func(rel string) bool
}

func (h HostStorage) dataDirs() []string {
	out := []string{DefaultDataDir}
	for _, d := range h.DataDirs {
		if d = cleanHostPath(d); d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func (h HostStorage) driveRoots() []string {
	var out []string
	for _, d := range h.dataDirs() {
		out = append(out, d+"/files")
	}
	for _, d := range h.Drive {
		if d = cleanHostPath(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// RefusedHostPath says why host path p may never be bound, or "".
func (h HostStorage) RefusedHostPath(p string) string {
	p = cleanHostPath(p)
	switch {
	case p == "":
		return ""
	case p == "/":
		return "the host's whole filesystem"
	}
	for _, s := range refusedHostPaths {
		if hostWithin(p, s.path) || hostWithin(s.path, p) {
			return s.why
		}
	}
	if slices.Contains(sensitiveSocketNames, filepath.Base(p)) {
		return "a container runtime's control socket, which is root on this machine"
	}
	for _, drive := range h.driveRoots() {
		// Drive is a folder of fog's that apps share (docs/files-contract.md):
		// a host path into it is a Drive folder by another name — all but
		// fog's own corner of it.
		if hostWithin(p, drive) && !IsReservedFilesRel(strings.TrimPrefix(p, drive)) {
			return ""
		}
	}
	for _, d := range h.dataDirs() {
		if h.App != "" && hostWithin(p, d+"/data/"+h.App) {
			continue // the app's own corner
		}
		if hostWithin(p, d) || hostWithin(d, p) {
			return "fog's own data directory (" + d + "), which holds every app's secrets and the vault key"
		}
	}
	return ""
}

// managed reports whether p is fog's own storage: a Drive folder (not a
// Drive root itself, which is every app's data at once), a NAS share, or
// the app's own data.
func (h HostStorage) managed(p string, readOnly bool) bool {
	p = cleanHostPath(p)
	if readOnly && readOnlySafe(p) {
		return true
	}
	for _, drive := range h.driveRoots() {
		if hostWithin(p, drive) {
			// A Drive folder is fog's storage, but not every one is this
			// app's to bind (driveWhy).
			return h.driveWhy(strings.TrimPrefix(p, drive)) == ""
		}
	}
	for _, m := range h.Managed {
		if m = cleanHostPath(m); m != "" && m != "/" && hostWithin(p, m) {
			return true
		}
	}
	if h.App != "" {
		for _, d := range h.dataDirs() {
			if hostWithin(p, d+"/data/"+h.App) {
				return true
			}
		}
	}
	return false
}

// FilesPeersDir is where folders paired households share land in Drive:
// the same constant as filesfed.PeersFolder.
const FilesPeersDir = "Peers"

// DriveWhy says why binding the Drive folder rel (relative to Drive's
// root) needs the operator's approval, or "" when it is the app's to bind:
// its own Apps/<App> folder, a Content library or any ordinary folder, or a
// folder an app it uses shares with it. Drive's root, Apps itself, another
// app's own folder, Peers and fog's own corner are not: each is other
// apps' data — credentials included — or other households'.
func (h HostStorage) driveWhy(rel string) string {
	rel = cleanDriveRel(rel)
	segs := strings.Split(rel, "/")
	switch {
	case rel == "":
		return "Drive's whole root — every app's data and every library at once"
	case strings.EqualFold(segs[0], filesReservedDir) || IsReservedFilesRel(rel):
		return "fog's own corner of Drive, where every folder's uploads and trash are kept"
	case strings.EqualFold(segs[0], FilesPeersDir):
		return "the folders paired households share with this one"
	case !strings.EqualFold(segs[0], FilesAppsDir):
		return ""
	case len(segs) == 1:
		return "every app's own data folder at once — their files, credentials included"
	case h.App != "" && strings.EqualFold(FilesAppsDir+"/"+segs[1], AppDataFolder(h.App)):
		return ""
	case h.Shared != nil && h.Shared(rel):
		return ""
	}
	return segs[1] + "'s own data folder — that app's files, credentials included"
}

// cleanDriveRel is the one spelling of a Drive-relative path comparisons
// use: no leading or trailing slash, "." and ".." collapsed.
func cleanDriveRel(rel string) string {
	return strings.Trim(filepath.ToSlash(filepath.Clean("/"+strings.TrimSpace(rel))), "/")
}

// HostPathVerdict judges a bind of p: refused (never), or needing the
// spec's acknowledgement (why), or "" for fog's own storage.
func (h HostStorage) HostPathVerdict(p string, readOnly bool) (refused, needsAck string) {
	if why := h.RefusedHostPath(p); why != "" {
		return why, ""
	}
	if h.managed(p, readOnly) {
		return "", ""
	}
	p = cleanHostPath(p)
	for _, drive := range h.driveRoots() {
		if hostWithin(p, drive) {
			return "", h.driveWhy(strings.TrimPrefix(p, drive))
		}
	}
	return "", unmanagedWhy
}

// DangerousHostPath says why binding p with nothing known of fog's storage
// beyond its data directories is more than a Drive folder: refused, or
// needing an acknowledgement. "" for a Drive folder.
func DangerousHostPath(p string, readOnly bool, dataDirs ...string) string {
	refused, ack := HostStorage{DataDirs: dataDirs}.HostPathVerdict(p, readOnly)
	if refused != "" {
		return refused
	}
	return ack
}

// acknowledged reports whether the spec names host path p in
// AcknowledgeHostAccess, compared cleaned.
func (s *AppSpec) acknowledged(p string) bool {
	p = cleanHostPath(p)
	for _, a := range s.AcknowledgeHostAccess {
		if strings.HasPrefix(strings.TrimSpace(a), "/") && cleanHostPath(a) == p {
			return true
		}
	}
	return false
}

// acknowledgedDrive reports whether the spec names the Drive folder rel —
// spelled as the volume's files: spells it, with no leading slash — in
// AcknowledgeHostAccess.
func (s *AppSpec) acknowledgedDrive(rel string) bool {
	rel = cleanDriveRel(rel)
	for _, a := range s.AcknowledgeHostAccess {
		if !strings.HasPrefix(strings.TrimSpace(a), "/") && cleanDriveRel(a) == rel {
			return true
		}
	}
	return false
}

// driveVolumes are the volumes in Drive, by files:, and who mounts them.
func (s *AppSpec) driveVolumes() []hostVolume {
	var out []hostVolume
	for _, v := range s.Volumes {
		if v.Files != "" {
			out = append(out, hostVolume{"", v})
		}
	}
	for _, a := range s.Aux {
		for _, v := range a.Volumes {
			if v.Files != "" {
				out = append(out, hostVolume{a.Name, v})
			}
		}
	}
	return out
}

// ackKey is an acknowledgement's spelling for comparison: a host path
// cleaned, a Drive folder cleaned as Drive-relative.
func ackKey(a string) string {
	if strings.HasPrefix(strings.TrimSpace(a), "/") {
		return cleanHostPath(a)
	}
	return cleanDriveRel(a)
}

// hostVolume is a host-path volume and who mounts it.
type hostVolume struct {
	Service string // "" for the app's own containers, else the aux name
	Volume
}

func (s *AppSpec) hostVolumes() []hostVolume {
	var out []hostVolume
	for _, v := range s.Volumes {
		if v.Host != "" {
			out = append(out, hostVolume{"", v})
		}
	}
	for _, a := range s.Aux {
		for _, v := range a.Volumes {
			if v.Host != "" {
				out = append(out, hostVolume{a.Name, v})
			}
		}
	}
	return out
}

// ValidateHostAccess is what a spec alone can be judged on: no refused
// path, no owner on one, and no acknowledgement of a path no volume binds
// (one waiting for a later edit to slip a path in under it). Whether a
// path is fog's storage or needs acknowledging depends on the NAS shares
// and the app — CheckHostAccess, at save and at render.
func (s *AppSpec) ValidateHostAccess(dataDirs ...string) error {
	h := HostStorage{DataDirs: dataDirs}
	bound := map[string]bool{}
	for _, hv := range s.hostVolumes() {
		p := cleanHostPath(hv.Host)
		bound[p] = true
		if why := h.RefusedHostPath(p); why != "" {
			return fmt.Errorf("volume %q mounts %s — %s. Fog never binds it", hv.Name, p, why)
		}
		if hv.Owner != "" {
			if why := noChown(p); why != "" {
				return fmt.Errorf("volume %q would chown %s — %s. Fog never changes its owner; drop the owner", hv.Name, p, why)
			}
		}
	}
	for _, dv := range s.driveVolumes() {
		bound[cleanDriveRel(dv.Files)] = true
	}
	for _, a := range s.AcknowledgeHostAccess {
		if !bound[ackKey(a)] {
			return fmt.Errorf("acknowledgeHostAccess names %s, which no volume mounts", a)
		}
	}
	return nil
}

// CheckHostAccess is the whole rule for one app: every host path refused
// is refused, and every one outside fog's storage is acknowledged.
func (s *AppSpec) CheckHostAccess(h HostStorage) error {
	for _, hv := range s.hostVolumes() {
		p := cleanHostPath(hv.Host)
		refused, ack := h.HostPathVerdict(p, hv.ReadOnly)
		if refused != "" {
			return fmt.Errorf("volume %q mounts %s — %s. Fog never binds it", hv.Name, p, refused)
		}
		if ack != "" && !s.acknowledged(p) {
			return fmt.Errorf("volume %q mounts %s — %s. If the app really needs it, add %q to acknowledgeHostAccess", hv.Name, p, ack, p)
		}
	}
	for _, dv := range s.driveVolumes() {
		rel := cleanDriveRel(dv.Files)
		if why := h.driveWhy(rel); why != "" && !s.acknowledgedDrive(rel) {
			return fmt.Errorf("volume %q mounts the Drive folder %s — %s. If the app really needs it, add %q to acknowledgeHostAccess", dv.Name, rel, why, rel)
		}
	}
	return nil
}

// resolveHostPath follows symlinks in the longest existing prefix of p, so
// /srv/sock → /var/run/docker.sock is judged by where it really points even
// when its last component doesn't exist yet.
func resolveHostPath(p string) string {
	p = cleanHostPath(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return cleanHostPath(filepath.Join(r, rest))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// storageFor is HostStorage as this materializer knows it.
func (m *Materializer) storageFor(app string) HostStorage {
	h := HostStorage{App: app}
	if m.DataDir != "" {
		h.DataDirs = append(h.DataDirs, m.DataDir, resolveHostPath(m.DataDir))
	}
	if m.FilesRoot != nil {
		if r := m.FilesRoot(); r != "" {
			h.Drive = append(h.Drive, r, resolveHostPath(r))
		}
	}
	if m.ManagedRoots != nil {
		for _, r := range m.ManagedRoots() {
			h.Managed = append(h.Managed, r, resolveHostPath(r))
		}
	}
	return h
}

// checkRenderHostPath is the render-time twin of CheckHostAccess, for one
// bind about to be written: it knows the data directory, Drive's root and
// the NAS shares this fogd really has, and judges both the path as written
// and where it resolves on this machine.
func (m *Materializer) checkRenderHostPath(app string, spec *AppSpec, v Volume, hostPath string) error {
	if v.Host == "" {
		return nil
	}
	h := m.storageFor(app)
	if m.SharesFor != nil {
		h.Shared = m.SharesFor(app, spec)
	}
	for _, p := range []string{hostPath, resolveHostPath(hostPath)} {
		refused, ack := h.HostPathVerdict(p, v.ReadOnly)
		if refused != "" {
			return fmt.Errorf("volume %s mounts %s — %s. Fog never binds it", v.Name, p, refused)
		}
		if ack != "" && !spec.acknowledged(v.Host) {
			return fmt.Errorf("volume %s mounts %s — %s. If the app really needs it, add %q to acknowledgeHostAccess", v.Name, p, ack, cleanHostPath(v.Host))
		}
	}
	return nil
}

// chownAllowed reports whether fog may re-own a host path for a volume's
// Owner: never one that is refused, whatever it resolves to.
func (m *Materializer) chownAllowed(app, hostPath string) error {
	h := m.storageFor(app)
	for _, p := range []string{hostPath, resolveHostPath(hostPath)} {
		if why := h.RefusedHostPath(p); why != "" {
			return fmt.Errorf("fog never changes the owner of %s — %s", p, why)
		}
		if why := noChown(p); why != "" {
			return fmt.Errorf("fog never changes the owner of %s — %s", p, why)
		}
	}
	return nil
}

// ---- review ----

// Review is everything about a spec that reaches past the app's own
// sandbox, laid out for an operator to read before the first deploy of an
// imported app: what it binds from the host, what it publishes, what it
// may read of other apps and of fog, and what it is acknowledged to do.
// Fog's spec has no privileged mode, devices, capabilities or host
// networking at all, so those never appear; anything that could stand in
// for them is here.
type Review struct {
	Source     string        `json:"source"` // image reference, or repo@branch
	Mounts     []ReviewMount `json:"mounts"`
	Ports      []ReviewPort  `json:"ports"`
	Routes     []ReviewRoute `json:"routes"`
	Network    string        `json:"network" ts:"\"shared\" | \"sealed\""`
	SealedPeer bool          `json:"sealedPeer,omitempty"`
	Secrets    []string      `json:"secrets"`
	// Env is the name of every variable the manifest sets — the app's, and
	// "<sidecar>: NAME" for a sidecar's. A name is what changes behaviour
	// (a preload, a proxy, a tool's setting), so it is shown.
	Env []string `json:"env"`
	// EnvLinks are the variables whose values name a place — a URL or a
	// host — as "NAME=value", any password in them masked: where the app
	// will try to connect, which the names alone don't say.
	EnvLinks []string `json:"envLinks"`
	// ReachAsked is the apps the manifest asks to call on the network
	// (AppSpec.Reach); confirming the review is the approval.
	ReachAsked  []string `json:"reachAsked"`
	Uses        []string `json:"uses"`
	Wants       []string `json:"wants"`
	Shares      []string `json:"shares"`
	Signin      bool     `json:"signin,omitempty"`
	Postgres    bool     `json:"postgres,omitempty"`
	ExecFiles   []string `json:"execFiles"`
	Release     []string `json:"release"`
	Acknowledge []string `json:"acknowledge"`
	// Reaches is the installed apps this one is wired to on the network —
	// its uses list and the apps an env URL names (Reaches in netwire.go)
	// — which is everything network isolation lets it talk to.
	Reaches []string `json:"reaches"`
	// RawSockets: the app keeps CAP_NET_RAW (acknowledgeRawSockets), with
	// which it can forge addresses on its network.
	RawSockets bool     `json:"rawSockets,omitempty"`
	Warnings   []string `json:"warnings"`
	// Leftovers is what an app deleted under this name, its data kept,
	// left behind — and this app would take over (Engine.Leftovers).
	// Confirming needs the operator's say-so to adopt it.
	Leftovers []string `json:"leftovers"`
}

// ReviewMount is one bind that leaves the app's own data: a host path or a
// Drive folder.
type ReviewMount struct {
	Service  string `json:"service"` // "" = the app itself, else a sidecar
	Volume   string `json:"volume"`
	Host     string `json:"host,omitempty"`
	Files    string `json:"files,omitempty"`
	Mount    string `json:"mount"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Follows  string `json:"follows,omitempty"`
	// OtherApp names the app whose own Drive data folder (Apps/<Name>)
	// this mounts — that app's files and, often, its credentials.
	OtherApp     string `json:"otherApp,omitempty"`
	Dangerous    string `json:"dangerous,omitempty"` // why, when it is
	Acknowledged bool   `json:"acknowledged,omitempty"`
}

type ReviewPort struct {
	HostPort int    `json:"hostPort"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	LowPort  bool   `json:"lowPort,omitempty"` // below 1024 on the host
}

type ReviewRoute struct {
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port"`
	Auth    bool   `json:"auth,omitempty"`
	LanOnly bool   `json:"lanOnly,omitempty"`
}

// BuildReview lays out what spec reaches. dataDirs are fog's data
// directories, as for ValidateHostAccess.
func BuildReview(app string, s *AppSpec, dataDirs ...string) Review {
	r := Review{
		Mounts: []ReviewMount{}, Ports: []ReviewPort{}, Routes: []ReviewRoute{},
		Secrets: append([]string{}, s.Secrets...), Uses: append([]string{}, s.Uses...),
		Wants: []string{}, Shares: []string{}, ExecFiles: []string{},
		Release: append([]string{}, s.Release...), Acknowledge: append([]string{}, s.AcknowledgeHostAccess...),
		Env:        reviewEnvNames(s),
		RawSockets: s.AcknowledgeRawSockets,
		EnvLinks:   reviewEnvLinks(s), ReachAsked: append([]string{}, s.Reach...),
		Reaches:  []string{},
		Warnings: []string{}, Leftovers: []string{}, Network: "shared", SealedPeer: s.SealedPeer,
		Signin: s.Signin != nil, Postgres: s.Needs != nil && s.Needs.Postgres != nil,
	}
	switch s.Source.Type {
	case "git":
		r.Source = s.Source.Repo + "@" + s.Source.BranchOrDefault()
	default:
		r.Source = s.Source.Image
	}
	if s.Sealed() {
		r.Network = "sealed"
	}
	add := func(service string, v Volume) {
		if v.Managed() || v.IsAppData(app) {
			return
		}
		m := ReviewMount{Service: service, Volume: v.Name, Host: v.Host, Files: v.Files, Mount: v.Mount,
			ReadOnly: v.ReadOnly, Owner: v.Owner, Follows: v.Follows}
		if v.Host != "" {
			m.Dangerous = DangerousHostPath(v.Host, v.ReadOnly, dataDirs...)
			m.Acknowledged = m.Dangerous != "" && s.acknowledged(v.Host)
			if m.Dangerous != "" {
				r.Warnings = append(r.Warnings, fmt.Sprintf("%s mounts %s — %s", nameOr(service, app), cleanHostPath(v.Host), m.Dangerous))
			}
		}
		if v.Files != "" {
			// Not the app's own folder or a library: shown as dangerous as a
			// host path is, and bound only once approved.
			m.Dangerous = HostStorage{App: app}.driveWhy(v.Files)
			m.Acknowledged = m.Dangerous != "" && s.acknowledgedDrive(v.Files)
		}
		if other := otherAppFolder(app, v.Files); other != "" {
			m.OtherApp = other
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s mounts %s's own data folder %s — that app's files, credentials included", nameOr(service, app), other, v.Files))
		} else if v.Files != "" && m.Dangerous != "" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s mounts the Drive folder %s — %s", nameOr(service, app), cleanDriveRel(v.Files), m.Dangerous))
		}
		if v.Follows != "" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s follows %s — it mounts whatever folder that app's volume is", nameOr(service, app), v.Follows))
		}
		r.Mounts = append(r.Mounts, m)
	}
	for _, v := range s.Volumes {
		add("", v)
	}
	for _, a := range s.Aux {
		for _, v := range a.Volumes {
			add(a.Name, v)
		}
	}
	for _, p := range s.Ports {
		rp := ReviewPort{HostPort: p.EffectiveHostPort(), Port: p.Port, Protocol: p.EffectiveProtocol(), LowPort: p.EffectiveHostPort() < 1024}
		r.Ports = append(r.Ports, rp)
	}
	for _, rt := range s.Routes {
		r.Routes = append(r.Routes, ReviewRoute{Host: rt.Host, Port: rt.Port, Auth: rt.Auth, LanOnly: rt.LanOnly})
	}
	for _, w := range s.Wants {
		if w.Offer != "" {
			r.Wants = append(r.Wants, w.Offer)
		} else {
			r.Wants = append(r.Wants, w.App)
		}
	}
	if s.Connection != nil {
		r.Shares = append(r.Shares, s.Connection.Shares...)
	}
	for _, f := range s.Files {
		if f.Exec {
			r.ExecFiles = append(r.ExecFiles, f.Mount)
		}
	}
	return r
}

// otherAppFolder names the app whose own Drive folder rel is in, when that
// is not app itself: "Apps/Glass/config" → "Glass".
func otherAppFolder(app, rel string) string {
	segs := strings.Split(rel, "/")
	if len(segs) < 2 || !strings.EqualFold(segs[0], FilesAppsDir) {
		return ""
	}
	if strings.EqualFold(FilesAppsDir+"/"+segs[1], AppDataFolder(app)) {
		return ""
	}
	return segs[1]
}

func nameOr(service, app string) string {
	if service != "" {
		return app + "'s " + service + " sidecar"
	}
	return app
}

// validateHostAccess is ValidateHostAccess against the data directory this
// fogd really uses, for the save paths — Validate alone knows only the
// default one.
func (e *Engine) validateHostAccess(name string, spec *AppSpec) error {
	return e.validateHostAccessWith(name, spec, spec.Uses)
}

// validateHostAccessWith is validateHostAccess with the folders the apps
// in uses share counting as the app's own.
func (e *Engine) validateHostAccessWith(name string, spec *AppSpec, uses []string) error {
	if e.Mat == nil || e.Mat.DataDir == "" {
		return nil
	}
	if err := spec.ValidateHostAccess(e.Mat.DataDir, resolveHostPath(e.Mat.DataDir)); err != nil {
		return err
	}
	return spec.CheckHostAccess(e.storageFor(name, uses))
}

// approvedUses is the uses that decide which apps' shared folders name
// binds: the latest revision's — the operator's say as it stands, as
// netwire reads reach and uses — not the running revision's. spec's, for
// an app not saved yet.
func (e *Engine) approvedUses(name string, spec *AppSpec) []string {
	if e.Store != nil {
		if a, err := e.Store.AppByName(name); err == nil {
			if latest, err := LatestSpec(e.Store, a.ID); err == nil {
				return latest.Uses
			}
		}
	}
	return spec.Uses
}

// storageFor is HostStorage as the engine knows it for app name: the
// materializer's, the NAS shares, and the folders the apps in uses share.
func (e *Engine) storageFor(name string, uses []string) HostStorage {
	h := e.Mat.storageFor(name)
	if e.Store != nil {
		if shares, err := e.Store.ListNasShares(); err == nil {
			for _, sh := range shares {
				h.Managed = append(h.Managed, sh.MountPoint)
			}
		}
		h.Shared = e.sharedBy(name, uses)
	}
	return h
}

// SharesFor answers whether a Drive folder is one an app that spec (as
// app name) uses shares with it: inside a volume the provider names in its
// Connection.Shares. Only an installed provider, not one held for review,
// and only through uses — the wiring the operator approved.
func (e *Engine) SharesFor(name string, spec *AppSpec) func(rel string) bool {
	return e.sharedBy(name, e.approvedUses(name, spec))
}

func (e *Engine) sharedBy(name string, uses []string) func(rel string) bool {
	return func(rel string) bool {
		if e.Store == nil {
			return false
		}
		rel = cleanDriveRel(rel)
		held, _ := e.Store.HeldApps()
		root := e.filesRoot()
		for _, u := range uses {
			if u == name || held[u] {
				continue
			}
			a, err := e.Store.AppByName(u)
			if err != nil {
				continue
			}
			p, err := LatestSpec(e.Store, a.ID)
			if err != nil || p.Connection == nil {
				continue
			}
			for _, sh := range p.Connection.Shares {
				v := p.volume(sh)
				if v == nil {
					continue
				}
				folder := ""
				switch {
				case v.Files != "":
					folder = cleanDriveRel(v.Files)
				case v.Host != "" && root != "" && hostWithin(cleanHostPath(v.Host), cleanHostPath(root)):
					folder = cleanDriveRel(strings.TrimPrefix(cleanHostPath(v.Host), cleanHostPath(root)))
				}
				if folder != "" && (strings.EqualFold(rel, folder) || strings.HasPrefix(strings.ToLower(rel), strings.ToLower(folder)+"/")) {
					return true
				}
			}
		}
		return false
	}
}

// ErrReviewPending is a deploy refused because the app was imported and
// nobody has confirmed its review yet.
type ErrReviewPending struct{ App string }

func (e *ErrReviewPending) Error() string {
	return e.App + " was imported and is waiting for review — read what it mounts and publishes, then confirm it on its page before it can deploy"
}

func (e *Engine) checkReviewHold(app *store.App) error {
	held, err := e.Store.AppReviewHold(app.ID)
	if err != nil {
		return err
	}
	if held {
		return &ErrReviewPending{App: app.Name}
	}
	return nil
}

// ReviewPending reports whether the named app is held for review.
func (e *Engine) ReviewPending(name string) (bool, error) {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return false, err
	}
	return e.Store.AppReviewHold(app.ID)
}

// AppReview lays out the app's latest revision for review, with the
// revision it describes: a confirmation names that revision, so an edit
// between reading and confirming can't ride along unread.
func (e *Engine) AppReview(name string) (Review, int64, error) {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return Review{}, 0, err
	}
	rev, err := e.Store.LatestRevision(app.ID)
	if err != nil {
		return Review{}, 0, err
	}
	spec, err := ParseSpec(rev.Spec)
	if err != nil {
		return Review{}, 0, err
	}
	var dirs []string
	if e.Mat != nil && e.Mat.DataDir != "" {
		dirs = append(dirs, e.Mat.DataDir, resolveHostPath(e.Mat.DataDir))
	}
	review := BuildReview(name, spec, dirs...)
	review.Leftovers = e.Leftovers(name)
	if list, err := e.Store.ListApps(); err == nil {
		installed := map[string]bool{}
		for _, a := range list {
			installed[a.Name] = true
		}
		review.Reaches = spec.Reaches(name, func(n string) bool { return installed[n] })
		if review.Reaches == nil {
			review.Reaches = []string{}
		}
	}
	return review, rev.Rev, nil
}

// ErrReviewStale is a confirmation of a revision that is no longer the
// latest: what was read is not what would deploy.
var ErrReviewStale = errors.New("the app changed since that review — read it again before confirming")

// ConfirmReview releases an imported app's hold, for the revision the
// operator read. It is the only way a held app becomes deployable.
func (e *Engine) ConfirmReview(name string, rev int64, adoptLeftovers bool, actor string) error {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	latest, err := e.Store.LatestRevision(app.ID)
	if err != nil {
		return err
	}
	if rev != latest.Rev {
		return ErrReviewStale
	}
	if left := e.Leftovers(name); len(left) > 0 && !adoptLeftovers {
		return &ErrLeftovers{App: name, What: left}
	}
	// What the reviewed revision acknowledges is what the operator just
	// approved, by reading it — that revision's, and no other's.
	_, spec, err := SpecAt(e.Store, app.ID, rev)
	if err != nil {
		return err
	}
	if err := e.Store.SetAppApprovals(app.ID, specApprovals(spec), actor); err != nil {
		return err
	}
	if err := e.Store.SetAppReviewedRev(app.ID, rev); err != nil {
		return err
	}
	if err := e.Store.SetAppReviewHold(app.ID, false); err != nil {
		return err
	}
	e.Store.Audit(actor, "app.review.confirm", name, fmt.Sprintf(`{"rev":%d,"adoptLeftovers":%v}`, rev, adoptLeftovers))
	return nil
}

// reviewEnvNames lists a spec's variable names, the app's then each
// sidecar's, sorted within each.
func reviewEnvNames(s *AppSpec) []string {
	out := sortedEnvNames(s.Env)
	for _, a := range s.Aux {
		for _, k := range sortedEnvNames(a.Env) {
			out = append(out, a.Name+": "+k)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// linkish finds a URL, or a host[:port] that is no plain word.
var linkish = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://\S+|\b[a-z0-9-]+(\.[a-z0-9-]+)+(:\d+)?\b|\b[a-z0-9-]+:\d{2,5}\b`)

// reviewEnvLinks lists the env values that name a place, the app's then
// each sidecar's, with a URL's password masked.
func reviewEnvLinks(s *AppSpec) []string {
	out := []string{}
	add := func(prefix string, env map[string]string) {
		for _, k := range sortedEnvNames(env) {
			if v := env[k]; linkish.MatchString(v) {
				out = append(out, prefix+k+"="+maskURLPasswords(v))
			}
		}
	}
	add("", s.Env)
	for _, a := range s.Aux {
		add(a.Name+": ", a.Env)
	}
	return out
}

// urlPassword is the password of a user:password@ in a URL.
var urlPassword = regexp.MustCompile(`(://[^:/@\s]*):[^@/\s]*@`)

func maskURLPasswords(v string) string { return urlPassword.ReplaceAllString(v, "${1}:•••@") }

// ErrLeftovers is a confirmation of an import that would take over what an
// app deleted under its name left behind, without the operator saying so.
type ErrLeftovers struct {
	App  string
	What []string
}

func (e *ErrLeftovers) Error() string {
	return e.App + " would take over what an earlier app named " + e.App + " left behind (" + strings.Join(e.What, "; ") +
		"). Confirm that it should, or remove that data first"
}

// Leftovers lists what an app deleted under name with its data kept left
// behind, which an app installed under the same name would take over: its
// data under fog's data directory, its own Drive folder, its Postgres role
// and the databases it owns. Only for an app that has never run — once it
// has, all of it is its own.
func (e *Engine) Leftovers(name string) []string {
	out := []string{}
	if app, err := e.Store.AppByName(name); err == nil && app.CurrentRev > 0 {
		return out
	}
	nonEmpty := func(dir string) bool {
		ents, err := os.ReadDir(dir)
		return err == nil && len(ents) > 0
	}
	if e.Mat != nil && e.Mat.DataDir != "" && nonEmpty(e.Mat.DataPath(name)) {
		out = append(out, "data in "+e.Mat.DataPath(name))
	}
	if root := e.filesRoot(); root != "" && nonEmpty(filepath.Join(root, filepath.FromSlash(AppDataFolder(name)))) {
		out = append(out, "the Drive folder "+AppDataFolder(name))
	}
	if e.PG != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if roles, err := e.PG.Roles(ctx); err == nil && slices.Contains(roles, pg.RoleName(name)) {
			out = append(out, "the Postgres role "+pg.RoleName(name)+" and every database it owns")
		}
	}
	return out
}
