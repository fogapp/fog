// Package apps holds the app model — the single declarative shape shared by
// image-source and (Phase 2) git-source apps — and the engine that reconciles
// it into running containers via materialized compose files.
package apps

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/fogports"
	"gitea.lan/matthew/fog/internal/signin"
)

// AppSpec is one app's desired state. It is stored as JSON in app_revisions
// and NEVER contains secret values — only the names of secrets, whose values
// live encrypted in the secrets table.
type AppSpec struct {
	Source  Source            `json:"source"`
	Env     map[string]string `json:"env,omitempty"`
	Secrets []string          `json:"secrets,omitempty"` // env var names whose values come from the vault
	Volumes []Volume          `json:"volumes,omitempty"`
	Files   []File            `json:"files,omitempty"`
	Routes  []Route           `json:"routes,omitempty"`
	// Ports publishes raw container ports on the host — for non-HTTP
	// protocols the reverse proxy can't carry (DNS, SMTP). HTTP UIs belong
	// in Routes. Published ports force the "restart" deploy strategy: two
	// swap replicas can't bind the same host port.
	Ports   []Port   `json:"ports,omitempty"`
	Health  *Health  `json:"health,omitempty"`
	Command []string `json:"command,omitempty"`
	// Processes are additional long-running processes sharing the app's
	// image, env, and volumes (e.g. Fin's solid_queue worker).
	Processes []Process `json:"processes,omitempty"`
	// Aux are sidecar containers the app needs beside it — its own database,
	// a Redis broker, an ML worker. They run their own images on a private
	// per-app network (reachable from the app by aux name, invisible to
	// other apps) and share the app's lifecycle, compose file, and backups.
	Aux []AuxService `json:"aux,omitempty"`
	// Release runs once per deploy, from the new image, before processes
	// switch over — the migrations slot. Serialized by the app lock.
	Release []string `json:"release,omitempty"`
	// DeployStrategy: "swap" (default; start new web container, health-check,
	// flip the network alias, drain old — near-zero downtime) or "restart"
	// (compose recreate; simpler, brief downtime). Swap requires a health
	// spec and applies to the main process only.
	DeployStrategy string `json:"deployStrategy,omitempty" ts:"\"swap\" | \"restart\""`
	// Needs declares managed datastores Fog provisions before deploying.
	Needs *Needs `json:"needs,omitempty"`
	// Update policy for image-source apps: "manual" (default), "notify"
	// (badge + notification when the remote digest moves), or "auto"
	// (snapshot, apply, verify health, notify either way).
	UpdatePolicy string `json:"updatePolicy,omitempty" ts:"\"manual\" | \"notify\" | \"auto\""`
	// Backup: what a snapshot of this app includes. Managed volumes are
	// always included; databases adds pg_dump of managed databases.
	Backup *BackupSpec `json:"backup,omitempty"`
	// Notes: operator markdown shown on the app page (post-install steps,
	// runbook pointers).
	Notes string `json:"notes,omitempty"`
	// Resources: conservative by design — memory limits only, no CPU limits
	// (long ffmpeg-style encodes must not be throttled into failure).
	MemoryLimit string `json:"memoryLimit,omitempty"` // e.g. "2g"
	// Uses is what this app may read from fog's app-facing surface beyond
	// its own (docs/events-contract.md): "events" for the household event
	// stream, and the names of other apps whose Connection it is handed.
	// Installing an app with a uses list is the operator's grant — it sits
	// in the spec, so the app page shows it and every change is a revision.
	Uses []string `json:"uses,omitempty"`
	// Reach names the other apps this app may call on the network
	// (netwire.go): the operator's approval, one provider at a time.
	// Connecting an integration adds its provider; an app in uses is
	// reached too. An env URL naming an app not approved here wires
	// nothing — the issues sweep asks instead.
	Reach []string `json:"reach,omitempty"`
	// Connection is how another app reaches this one: its port on the fog
	// network and the vault secret holding the credential a consumer needs
	// (an API key, a long-lived token). Only apps that name this one in
	// their uses list ever see it.
	Connection *Connection `json:"connection,omitempty"`
	// Wants are the other apps this app can connect to
	// (docs/integrations-contract.md): its Integrations tab lists one entry
	// per app they name, and connecting one sets up everything its wants of
	// that app ask for. The operator still connects each.
	Wants []Want `json:"wants,omitempty"`
	// Signin makes this app a client of fog's OpenID provider
	// (docs/signin-contract.md): the same block a git app puts in its
	// fog.yaml, for apps fog installs from an image (the catalog's
	// Paperless). Env values may name {{oidc.issuer}}, {{oidc.client_id}}
	// and {{oidc.client_secret}}, filled in at deploy.
	Signin *signin.Manifest `json:"signin,omitempty"`
	// Network is which network the app lives on (docs/sealed-network.md):
	// empty for the shared fog network, or "sealed" for fog-sealed alone,
	// which has no route out of the machine. A sealed app's sidecars lose
	// their route too, and it can't publish host ports.
	Network string `json:"network,omitempty" ts:"\"sealed\""`
	// SealedPeer lets a sealed app be wired to this non-sealed one (by its
	// uses list or an env URL naming it; netwire.go), so it can reach it by
	// name. It keeps its way out, which makes it part of what a sealed app
	// trusts: anything it relays leaves the machine.
	SealedPeer bool `json:"sealedPeer,omitempty"`
	// AcknowledgeHostAccess names, one by one, the host paths outside fog's
	// own storage (Drive, NAS shares, the app's data) this app binds
	// (hostaccess.go). Each must be spelled as the volume spells it; a
	// volume binding such a path without an entry here is refused, and an
	// entry that no volume binds is too. A few paths are never bound at
	// all: "/", /proc, /sys, /dev, a container runtime's socket, fog's data.
	AcknowledgeHostAccess []string `json:"acknowledgeHostAccess,omitempty"`
	// AcknowledgeRawSockets gives the app's containers CAP_NET_RAW, which
	// fog drops from every container by default: raw sockets forge
	// addresses on the app's network (ARP-spoofing its gateway, then
	// speaking as it to fog's proxy). Ping needs none — ICMP datagram
	// sockets don't. A DHCP server or a network scanner does.
	AcknowledgeRawSockets bool `json:"acknowledgeRawSockets,omitempty"`
}

// NetworkSealed is the Network value that seals an app.
const NetworkSealed = "sealed"

// Sealed reports whether the app has no route out of the machine.
func (s *AppSpec) Sealed() bool { return s.Network == NetworkSealed }

// UsesEvents is the uses entry that grants reading the event stream.
const UsesEvents = "events"

// UsesHome is the uses entry that grants reading the home view
// (/app/v1/home): the app directory, the Inbox summary and the host's
// vitals. A start page asks for it; nothing else needs to.
const UsesHome = "home"

// UsesChat is the uses entry that grants posting into chat through fog's
// bot (/app/v1/chat): the rooms the bot is in, each post shown as this
// app's, never as fog's.
const UsesChat = "chat"

// UsesDrive is the uses entry that grants searching Drive by name for the
// operator while they are at the app's door (/app/v1/files/search, with a
// Fog-Assertion): names, paths, sizes and dates, never contents, and never
// other apps' own folders or a peer's files. A start page's search box asks
// for it; nothing else needs to.
const UsesDrive = "drive"

// SurfaceGrant reports whether a uses entry grants part of fog's own
// surface rather than naming another app.
func SurfaceGrant(u string) bool {
	return u == UsesEvents || u == UsesHome || u == UsesChat || u == UsesDrive
}

// Connection is what /app/v1/connections hands a consumer: the URL is
// http://<app>:<port><path> on the fog network, the credential is the
// app's own vault secret named by Secret.
type Connection struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
	// Secret names one of this app's vault secrets. It need not be in
	// Secrets: a credential minted inside the app (Home Assistant's
	// long-lived token) is pasted into the vault afterwards, and listing it
	// in Secrets would fail every deploy until then.
	Secret string `json:"secret,omitempty"`
	// Shares names this app's volumes a consumer must be able to see to
	// work with it — SABnzbd's downloads folder, Stream's libraries. Fog
	// mounts them into an app that uses this one when it is wired, and
	// hands the consumer the path translations between the two
	// (docs/files-contract.md, "Wiring").
	Shares []string `json:"shares,omitempty"`
	// Offers are what another app can connect to on its Integrations tab
	// (docs/integrations-contract.md): fog asks this app to mint a
	// credential for that one consumer and sets it in the consumer's
	// environment. Unlike Secret, every consumer gets its own.
	Offers []Offer `json:"offers,omitempty"`
}

// Offer is one integration a provider makes (fog-integration/1). Env is set
// in the consumer's spec as written, with {{url}} as the provider's
// connection URL; Secrets name what the provider's mint answers, which fog
// keeps in the consumer's vault.
type Offer struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Blurb   string            `json:"blurb,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Secrets []string          `json:"secrets,omitempty"`
	// Capabilities say what a connected app gets, one short line each, for
	// the Integrations tab's list.
	Capabilities []string `json:"capabilities,omitempty"`
	// Consent: a person approves each connection at the provider before
	// fog can claim the credential — one minted in their name.
	Consent bool `json:"consent,omitempty"`
}

// Want is one thing this app can take from another app. Either Offer names
// an offer, as "<provider>/<offer id>", minted for this app alone; or App
// names a provider whose connection (address, shared credential, shared
// folders) this app is handed, which connecting grants through Uses.
// As renames a variable the offer sets to the one this app reads, and
// Capabilities, when set, say what connecting gives this app in place of
// the offer's own.
type Want struct {
	Offer        string            `json:"offer,omitempty"`
	App          string            `json:"app,omitempty"`
	As           map[string]string `json:"as,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
}

// Provider is the app the want names.
func (w Want) Provider() string {
	if w.App != "" {
		return w.App
	}
	provider, _, _ := strings.Cut(w.Offer, "/")
	return provider
}

// OfferID is the offer the want names, or "" for an App want.
func (w Want) OfferID() string {
	_, offer, _ := strings.Cut(w.Offer, "/")
	return offer
}

// key tells wants apart: two wants of the same key are the same want.
func (w Want) key() string {
	if w.App != "" {
		return "app:" + w.App
	}
	return w.Offer
}

// FindWant finds what wants says of provider's offer, or nil.
func FindWant(wants []Want, provider, offer string) *Want {
	for i := range wants {
		if wants[i].Offer == provider+"/"+offer {
			return &wants[i]
		}
	}
	return nil
}

// Name is the variable this app reads for name, an offer's.
func (w *Want) Name(name string) string {
	if w != nil && w.As[name] != "" {
		return w.As[name]
	}
	return name
}

// URLTemplate in an offer's env is the provider's ConnectionURL.
const URLTemplate = "{{url}}"

// ConnectionURL is where a consumer reaches name on the fog network.
func (c *Connection) ConnectionURL(name string) string {
	host := "http://" + name
	if c.Port != 80 {
		host += ":" + strconv.Itoa(c.Port)
	}
	return host + c.Path
}

// Offer finds the offer with id, or nil.
func (c *Connection) Offer(id string) *Offer {
	if c == nil {
		return nil
	}
	for i := range c.Offers {
		if c.Offers[i].ID == id {
			return &c.Offers[i]
		}
	}
	return nil
}

// UsesGrant reports whether the spec grants want ("events", "home", or an
// app name).
func (s *AppSpec) UsesGrant(want string) bool {
	for _, u := range s.Uses {
		if u == want {
			return true
		}
	}
	return false
}

type Source struct {
	Type  string `json:"type" ts:"\"image\" | \"git\""` // "image" | "git"
	Image string `json:"image,omitempty"`               // e.g. lscr.io/linuxserver/sonarr:latest
	// git source: built on the box with docker build.
	Repo       string            `json:"repo,omitempty"`   // ssh or https clone URL
	Branch     string            `json:"branch,omitempty"` // default "main"
	Dockerfile string            `json:"dockerfile,omitempty"`
	BuildArgs  map[string]string `json:"buildArgs,omitempty"` // values may use {{commit}}
}

// BranchOrDefault is the git branch to deploy: Branch, or "main" when the
// spec leaves it out.
func (s Source) BranchOrDefault() string {
	if s.Branch == "" {
		return "main"
	}
	return s.Branch
}

type Process struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

// AuxService is a sidecar container. Env values are rendered inline in the
// compose file (not the shared .env), so `${VAR}` references the app's env —
// that's how a database sidecar shares a generated password with the app
// (e.g. POSTGRES_PASSWORD: ${DB_PASSWORD}) without storing it twice.
// Volumes share the app's namespace: same data dir, same backup coverage.
type AuxService struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Env     map[string]string `json:"env,omitempty"`
	Volumes []Volume          `json:"volumes,omitempty"`
	Command []string          `json:"command,omitempty"`
	ShmSize string            `json:"shmSize,omitempty"` // e.g. "128mb" (Immich's postgres)
}

type Needs struct {
	Postgres *PostgresNeed `json:"postgres,omitempty"`
}

type BackupSpec struct {
	Enabled bool `json:"enabled"`
	// BeforeUpdate overrides the fleet-wide backup_before_update setting
	// for this app: snapshot (true) or not (false) before an update or a
	// rollback. nil follows the setting.
	BeforeUpdate *bool `json:"beforeUpdate,omitempty"`
	// Mode selects how a snapshot is produced. "volumes" (default) tars
	// managed volumes and pg_dumps managed databases from the outside.
	// "delegated" asks the app to produce a single artifact via the
	// fog-backup/1 HTTP contract (docs/backup-contract.md) — the app owns
	// consistency, fog owns scheduling, verification, and retention.
	Mode string `json:"mode,omitempty"`
	// Timeout bounds a delegated backup end to end (produce + download).
	// Go duration string, e.g. "45m". Default 30m.
	Timeout string `json:"timeout,omitempty"`
	// HourUTC overrides the global backup hour for this app (0–23).
	HourUTC *int `json:"hourUTC,omitempty"`
	// MaxBytes caps the size of a delegated artifact fog will download, so a
	// misbehaving or hostile app cannot fill the backup disk. 0 follows the
	// fleet setting, then a generous default.
	MaxBytes int64 `json:"maxBytes,omitempty"`
}

// The backup modes a spec can name; "" means BackupModeVolumes.
const (
	BackupModeVolumes   = "volumes"
	BackupModeDelegated = "delegated"
)

// BackupsEnabled reports whether the app is backed up at all. A nil spec
// (an app with no revision yet) is not.
func (s *AppSpec) BackupsEnabled() bool {
	return s != nil && s.Backup != nil && s.Backup.Enabled
}

// DelegatedBackups reports whether the app's snapshots are produced by the
// app itself over the fog-backup/1 contract rather than tarred from
// outside. It says nothing about whether backups are on; that is
// BackupsEnabled.
func (s *AppSpec) DelegatedBackups() bool {
	return s != nil && s.Backup != nil && s.Backup.Mode == BackupModeDelegated
}

// PostgresNeed provisions role + database(s) + extensions in Fog's managed
// Postgres and injects connection details as env vars. Inject maps env var
// name -> field (host|port|user|password|dbname).
type PostgresNeed struct {
	Databases  []string          `json:"databases"`
	Extensions []string          `json:"extensions,omitempty"`
	Inject     map[string]string `json:"inject,omitempty"`
}

// Volume is one bind mount. Where it lives on the host is one of three
// things, in order of how much the operator can see of it:
//
//   - managed (neither Host nor Files): a directory under
//     /var/lib/fog/data/<app>/<name>, private to the app and included in
//     its backups. Where sidecar state lives; a new app's own volumes are
//     given a Files folder instead (PlaceAppData);
//   - Files: a folder in fog's own Files tree, named relative to the Files
//     root — "Apps/Paperless/config" for the app's own data,
//     "Content/Movies" for a library it merely reads. Resolved against the
//     live root at deploy time, so the spec survives the root moving; the
//     folder is created on demand and is browsable and uploadable from the
//     Drive page. The app's own folder under Apps travels in its backups
//     (IsAppData); a content library is a shared pool, covered by the
//     nightly Files snapshot rather than any one app's snapshot;
//   - Host: an absolute path anywhere on the host — a NAS share, a media
//     pool — the escape hatch.
type Volume struct {
	Name  string `json:"name"`  // dir under /var/lib/fog/data/<app>/
	Mount string `json:"mount"` // container path
	// Files names a folder in Drive, relative to its root — the app's own
	// "Apps/Paperless/config", or a shared "Content/Movies". Exclusive
	// with Host.
	Files string `json:"files,omitempty"`
	Host  string `json:"host,omitempty"` // explicit host path escape hatch (shared media pools)
	// "uid:gid" enforced on the volume's directory, e.g. "1000:1000". A
	// managed volume is fog's own tree and gets it recursively; a host path
	// is someone else's, so only the mount root is chowned.
	Owner string `json:"owner,omitempty"`
	// ReadOnly mounts the path :ro. A copy of a peer's share is always
	// read-only (the materializer's MountPolicy says so), whatever the spec.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Follows names another app's volume, "sabnzbd/media": this volume is
	// whatever folder that one is. Fog copies the target's Files folder (or
	// host path) into this volume when the app is wired and again whenever
	// the target's folder changes, so the two never drift apart — the
	// volume itself stays an ordinary Files or host volume everywhere else.
	Follows string `json:"follows,omitempty"`
}

// Managed reports whether the volume lives in fog's private data dir for
// the app — the kind its backups cover and the janitor accounts for.
func (v Volume) Managed() bool { return v.Host == "" && v.Files == "" }

// HostPath is where the volume binds from, given the current Files root:
// the explicit host path, the Files folder resolved against the root, or
// "" for a managed volume (whose path is the materializer's to derive). A
// Files volume with no root to resolve against also answers "" — the
// materializer refuses it rather than binding a bare relative path.
func (v Volume) HostPath(filesRoot string) string {
	switch {
	case v.Host != "":
		return v.Host
	case v.Files != "" && filesRoot != "":
		return filepath.Join(filesRoot, filepath.FromSlash(v.Files))
	}
	return ""
}

// FilesAppsDir is the folder at the Files root that fog keeps for apps:
// one named subfolder per app ("Apps/Paperless"), holding that app's own
// data. FilesContentDir is its counterpart for the household's own
// libraries ("Content/Movies"), which several apps read at once and no
// single app owns. Both are the same constants as files.AppsDir and
// files.ContentDir; they live here so a catalog entry can name one without
// importing files.
const (
	FilesAppsDir    = "Apps"
	FilesContentDir = "Content"
)

// AppDataFolder is the folder in Drive that holds one app's own data:
// Apps/<App>, the app's name with its first letter up ("paperless" →
// "Apps/Paperless").
func AppDataFolder(app string) string {
	name := strings.TrimSpace(app)
	if name == "" {
		return FilesAppsDir
	}
	return FilesAppsDir + "/" + strings.ToUpper(name[:1]) + name[1:]
}

// IsAppData reports whether this volume is the app's own data in Drive —
// its folder under Apps, or something inside it. It is the line the app's
// backups draw: what an app owns travels in its snapshot, while a content
// library (Content/Movies), a folder belonging to another app, and any
// host path are shared pools that deliberately do not.
func (v Volume) IsAppData(app string) bool {
	if v.Files == "" {
		return false
	}
	base := AppDataFolder(app)
	return strings.EqualFold(v.Files, base) ||
		strings.HasPrefix(strings.ToLower(v.Files), strings.ToLower(base)+"/")
}

// filesReservedDir mirrors files.ReservedDir: the one top-level name in
// Files that is fog's own and never an app's folder.
const filesReservedDir = ".fog"

// ValidateFilesRel checks a Volume.Files value: a clean, root-relative
// slash path naming a folder — no leading slash, no ".", "..", or empty
// segments, not the reserved directory. It is the spec-side twin of
// files.CleanRel (a test keeps them in agreement); the rule is here
// because files depends on apps, not the other way round.
func ValidateFilesRel(rel string) error {
	if rel == "" {
		return fmt.Errorf("names no folder")
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("must be relative to the Drive root, e.g. %s/Books", FilesContentDir)
	}
	if strings.ContainsRune(rel, 0) || !utf8.ValidString(rel) {
		return fmt.Errorf("is not a valid path")
	}
	segs := strings.Split(rel, "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("must be a clean path with no empty, \".\" or \"..\" segments")
		}
		if len(seg) > 255 {
			return fmt.Errorf("has a segment longer than 255 bytes")
		}
	}
	if IsReservedFilesRel(rel) {
		return fmt.Errorf("%s is reserved for fog", filesReservedDir)
	}
	return nil
}

// IsReservedFilesRel reports whether a Drive-relative path is in, or is,
// fog's reserved directory: the root's .fog, or Apps' or Content's own
// (either may live on another disk, and stages its uploads and trash
// there). Case-insensitively, as files.CleanRel does: on a macOS or CIFS
// root ".FOG/trash" is fog's trash.
func IsReservedFilesRel(rel string) bool {
	segs := strings.Split(strings.Trim(rel, "/"), "/")
	if strings.EqualFold(segs[0], filesReservedDir) {
		return true
	}
	return len(segs) > 1 && strings.EqualFold(segs[1], filesReservedDir) &&
		(strings.EqualFold(segs[0], FilesAppsDir) || strings.EqualFold(segs[0], FilesContentDir))
}

// File is a UI-edited config file rendered to disk and bind-mounted read-only.
// Content lives in the spec so it is versioned with everything else.
type File struct {
	Name    string `json:"name"`  // filename under the app's files/ dir
	Mount   string `json:"mount"` // container path
	Content string `json:"content"`
	// Exec renders the file 0755 — for an image that runs what it finds at
	// the mount point (a LinuxServer /custom-cont-init.d hook), where a
	// plain 0644 config file is silently skipped.
	Exec bool `json:"exec,omitempty"`
}

type Route struct {
	Host string `json:"host,omitempty"` // defaults to <app>.<base_domain>
	Port int    `json:"port"`           // container port
	// NoBuffer disables proxy buffering — required for media endpoints that
	// stream or serve Range requests (Hub podcast audio).
	NoBuffer bool `json:"noBuffer,omitempty"`
	// Auth gates the route behind Fog's session (Phase 4 forward_auth).
	Auth bool `json:"auth,omitempty"`
	// LanOnly answers only clients in private address ranges, and 403
	// everyone else (docs/sealed-network.md). It costs the route its direct
	// port, which goes around the proxy.
	LanOnly bool `json:"lanOnly,omitempty"`
}

// Port is a raw host-published port (compose "host:container/proto").
type Port struct {
	Port     int    `json:"port"`                                      // container port
	HostPort int    `json:"hostPort,omitempty"`                        // defaults to Port
	Protocol string `json:"protocol,omitempty" ts:"\"tcp\" | \"udp\""` // "tcp" (default) | "udp"
}

type Health struct {
	Path string `json:"path"`           // HTTP path, e.g. /ping
	Port int    `json:"port,omitempty"` // defaults to first route port
}

func (p Port) EffectiveHostPort() int {
	if p.HostPort != 0 {
		return p.HostPort
	}
	return p.Port
}

func (p Port) EffectiveProtocol() string {
	if p.Protocol == "" {
		return "tcp"
	}
	return p.Protocol
}

// DirectPort reports whether the app gets a stable port on the host, served
// by fogd's forwarder for tunnels and no-domain setups. The forwarder goes
// around Caddy, so a first route that Caddy has to guard — the login gate, or
// the LAN check — gets none.
//
// The direct port forwards to the first route's container port, so it is
// refused when ANY route Caddy guards serves that port: an open route and a
// gated one on the same port are the same service, and the forward would
// be the gated one's way around the gate.
func (s *AppSpec) DirectPort() bool {
	if len(s.Routes) == 0 {
		return false
	}
	port := s.Routes[0].Port
	for _, r := range s.Routes {
		if r.Port == port && (r.Auth || r.LanOnly) {
			return false
		}
	}
	return true
}

// HealthPort resolves the port fog reaches the app on — the health spec's
// explicit port, else the first route's. This is the one answer to "where do
// I probe this app": the health monitor, the swap gate, and fog→app API
// calls all use it, so they can never drift onto different ports. 0 means
// the spec offers no way in.
func (s *AppSpec) HealthPort() int {
	if s.Health != nil && s.Health.Port != 0 {
		return s.Health.Port
	}
	if len(s.Routes) > 0 {
		return s.Routes[0].Port
	}
	return 0
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// reserved names collide with Fog's own containers or DNS, or with the
// directories fog keeps beside the per-app ones under the backups root
// (backup.ReservedDirs; backup imports apps, so they are spelled out here
// and a backup test holds the two lists together).
var reserved = map[string]bool{
	"fog": true, "caddy": true, "fog-postgres": true, "postgres": true,
	"fog-recovery": true, "files": true, "fog-store": true,
}

// AllVolumes is the app's volumes followed by every sidecar's — one
// namespace, as Validate enforces.
func (s *AppSpec) AllVolumes() []Volume {
	out := append([]Volume{}, s.Volumes...)
	for _, a := range s.Aux {
		out = append(out, a.Volumes...)
	}
	return out
}

// HostPaths returns every host path this spec bind-mounts (app and aux
// volumes), Files folders resolved against filesRoot — what must exist and
// be mounted before containers start. A Files volume with no root to
// resolve against is left out; the materializer is where that fails.
func (s *AppSpec) HostPaths(filesRoot string) []string {
	var out []string
	for _, v := range s.AllVolumes() {
		if p := v.HostPath(filesRoot); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FilesVolumes returns the volumes that live in Files.
func (s *AppSpec) FilesVolumes() []Volume {
	var out []Volume
	for _, v := range s.AllVolumes() {
		if v.Files != "" {
			out = append(out, v)
		}
	}
	return out
}

// fogProjects are the compose projects of fog's own: an app with one of
// these names would be that project — and its `up --remove-orphans` would
// remove fog's proxy as an orphan, and take its ports.
var fogProjects = map[string]bool{"fog-caddy": true, "fog-wire": true, "fog-postgres": true}

// fogPortErr refuses host port p when fog holds it (fogports): Caddy's
// ports, fogd's own (0 when unknown), the direct ports fogd forwards. No
// acknowledgement lets an app have one. An app publishing it races fog for
// it at every boot — Docker starts containers before fogd — and the winner
// answers in fog's place: for fog.<domain> on Caddy's, with the operator's
// sign-in on fogd's, for another app on its direct port.
func fogPortErr(p, fogd int) error {
	if owner := fogports.Owner(p, fogd); owner != "" {
		return fmt.Errorf("host port %d is %s; publish another", p, owner)
	}
	return nil
}

func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name must be a short lowercase slug (letters, digits, hyphens)")
	}
	if reserved[name] {
		return fmt.Errorf("name %q is reserved", name)
	}
	// An app's Postgres role is its name with '-' as '_' (pg.RoleName). It
	// must not be one of fog's own roles: the restore roles fog replays
	// untrusted dumps as (fog_restore_scratch, fog_scratch_<random>).
	if role := strings.ReplaceAll(name, "-", "_"); role == "fog_restore_scratch" || strings.HasPrefix(role, "fog_scratch_") {
		return fmt.Errorf("name %q is reserved: its database role would be one of fog's own", name)
	}
	return nil
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var pgIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// pgExtRe: extension names are quoted at CREATE EXTENSION time, so a hyphen
// is fine there ("uuid-ossp") — database names never get one.
var pgExtRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// hostRe matches a lowercase DNS hostname — one or more labels. A route host
// reaches the proxy config verbatim, so anything that isn't a hostname (a
// scheme, a path, a space) has to be refused before it can render a broken
// Caddyfile.
var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func (s *AppSpec) Validate() error {
	if s.Signin != nil {
		if !s.Signin.Declared() {
			return fmt.Errorf("signin.contract must be %s", signin.Contract)
		}
		if err := s.Signin.Validate(); err != nil {
			return err
		}
	}
	switch s.Source.Type {
	case "image":
		if strings.TrimSpace(s.Source.Image) == "" {
			return fmt.Errorf("image is required")
		}
	case "git":
		if err := validateGitRepo(s.Source.Repo); err != nil {
			return err
		}
		// The Dockerfile is read from the checkout, and nothing else: a
		// path out of it (absolute, "..") named a host file, which docker
		// read into the build and its log.
		if d := s.Source.Dockerfile; d != "" {
			if filepath.IsAbs(d) || strings.HasPrefix(d, "-") || filepath.Clean(d) != d || d == ".." || strings.HasPrefix(d, "../") || strings.Contains(d, "/../") {
				return fmt.Errorf("dockerfile %q must be a plain path inside the repository, like docker/Dockerfile", d)
			}
		}
		// The branch reaches git's command line; one git would read as an
		// option, or that is no ref at all, is refused here, at save.
		if s.Source.Branch != "" {
			if err := build.ValidateBranch(s.Source.Branch); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown source type %q", s.Source.Type)
	}
	switch s.DeployStrategy {
	case "", "swap", "restart":
	default:
		return fmt.Errorf("deployStrategy must be swap or restart")
	}
	switch s.Network {
	case "":
	case NetworkSealed:
		if s.SealedPeer {
			return fmt.Errorf("a sealed app is already on the sealed network — sealedPeer is for an app that keeps its way out")
		}
		// Docker doesn't publish a port from an internal network, so the
		// spec would promise a port nothing answers on.
		if len(s.Ports) > 0 {
			return fmt.Errorf("a sealed app can't publish host ports — it is reached through its routes")
		}
	default:
		return fmt.Errorf("network must be sealed, or left out for the shared fog network")
	}
	switch s.UpdatePolicy {
	case "", "manual", "notify", "auto":
	default:
		return fmt.Errorf("updatePolicy must be manual, notify, or auto")
	}
	for _, p := range s.Processes {
		if !nameRe.MatchString(p.Name) || p.Name == "main" {
			return fmt.Errorf("invalid process name %q", p.Name)
		}
		if len(p.Command) == 0 {
			return fmt.Errorf("process %q needs a command", p.Name)
		}
	}
	seenAux := map[string]bool{}
	for _, p := range s.Processes {
		seenAux[p.Name] = true
	}
	for _, a := range s.Aux {
		if !nameRe.MatchString(a.Name) || a.Name == "main" {
			return fmt.Errorf("invalid aux service name %q", a.Name)
		}
		if seenAux[a.Name] {
			return fmt.Errorf("duplicate aux/process name %q", a.Name)
		}
		seenAux[a.Name] = true
		if strings.TrimSpace(a.Image) == "" {
			return fmt.Errorf("aux service %q needs an image", a.Name)
		}
		for k := range a.Env {
			if !envKeyRe.MatchString(k) {
				return fmt.Errorf("aux service %q: invalid env var name %q", a.Name, k)
			}
			if err := checkToolName(k); err != nil {
				return fmt.Errorf("aux service %q: %w", a.Name, err)
			}
		}
	}
	if s.Needs != nil && s.Needs.Postgres != nil {
		pg := s.Needs.Postgres
		if len(pg.Databases) == 0 {
			return fmt.Errorf("postgres need requires at least one database")
		}
		for _, d := range pg.Databases {
			if !pgIdentRe.MatchString(d) {
				return fmt.Errorf("invalid postgres identifier %q", d)
			}
		}
		for _, x := range pg.Extensions {
			if !pgExtRe.MatchString(x) {
				return fmt.Errorf("invalid postgres extension %q", x)
			}
		}
		for envVar, field := range pg.Inject {
			if !envKeyRe.MatchString(envVar) {
				return fmt.Errorf("invalid inject env var %q", envVar)
			}
			if err := checkToolName(envVar); err != nil {
				return err
			}
			switch field {
			case "host", "port", "user", "password", "dbname":
			default:
				return fmt.Errorf("invalid inject field %q (host|port|user|password|dbname)", field)
			}
		}
	}
	for k := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid env var name %q", k)
		}
		if err := checkToolName(k); err != nil {
			return err
		}
	}
	for _, k := range s.Secrets {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid secret name %q", k)
		}
		if err := checkFogSecretName(k); err != nil {
			return err
		}
		if err := checkToolName(k); err != nil {
			return err
		}
	}
	for _, u := range s.Uses {
		if u != UsesEvents && u != UsesHome && !nameRe.MatchString(u) {
			return fmt.Errorf("invalid uses entry %q (events, home, chat, drive, or an app name)", u)
		}
	}
	for _, r := range s.Reach {
		if !nameRe.MatchString(r) || SurfaceGrant(r) {
			return fmt.Errorf("invalid reach entry %q (an app name)", r)
		}
	}
	if err := validateWants(s.Wants); err != nil {
		return err
	}
	if c := s.Connection; c != nil {
		if c.Port < 1 || c.Port > 65535 {
			return fmt.Errorf("connection port %d out of range", c.Port)
		}
		if c.Path != "" && !strings.HasPrefix(c.Path, "/") {
			return fmt.Errorf("connection path %q must start with /", c.Path)
		}
		if c.Secret != "" && !envKeyRe.MatchString(c.Secret) {
			return fmt.Errorf("invalid connection secret name %q", c.Secret)
		}
		for _, name := range c.Shares {
			if !s.hasVolume(name) {
				return fmt.Errorf("connection shares %q, which is not one of this app's volumes", name)
			}
		}
		if err := validateOffers(c.Offers); err != nil {
			return err
		}
	}
	// Aux volumes share the app's data-dir namespace, so names must be
	// unique across the app and every sidecar.
	seen := map[string]bool{}
	for _, v := range s.AllVolumes() {
		if !nameRe.MatchString(v.Name) {
			return fmt.Errorf("invalid volume name %q", v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("duplicate volume %q", v.Name)
		}
		seen[v.Name] = true
		if !strings.HasPrefix(v.Mount, "/") {
			return fmt.Errorf("volume %q mount must be absolute", v.Name)
		}
		if v.Host != "" && !strings.HasPrefix(v.Host, "/") {
			return fmt.Errorf("volume %q host path must be absolute", v.Name)
		}
		// Compose would read "${…}" in it as a variable (materialize writes
		// it literally all the same): refused outright, so no check of a
		// host path is ever about a string compose reads differently.
		if strings.ContainsAny(v.Host, "$") {
			return fmt.Errorf("volume %q host path may not contain $", v.Name)
		}
		if v.Host != "" && v.Files != "" {
			return fmt.Errorf("volume %q names both a host path and a Drive folder — pick one", v.Name)
		}
		if v.Files != "" {
			if err := ValidateFilesRel(v.Files); err != nil {
				return fmt.Errorf("volume %q: Drive folder %q %v", v.Name, v.Files, err)
			}
			// Like a host path: no string any tool on the way (compose,
			// systemd) reads as a variable.
			if strings.Contains(v.Files, "$") {
				return fmt.Errorf("volume %q: Drive folder %q may not contain $", v.Name, v.Files)
			}
		}
		if v.Follows != "" {
			if _, _, err := parseFollows(v.Follows); err != nil {
				return fmt.Errorf("volume %q: %v", v.Name, err)
			}
		}
	}
	if err := s.ValidateHostAccess(); err != nil {
		return err
	}
	for _, f := range s.Files {
		if strings.Contains(f.Name, "/") || strings.Contains(f.Name, "..") || f.Name == "" {
			return fmt.Errorf("invalid file name %q", f.Name)
		}
		if !strings.HasPrefix(f.Mount, "/") {
			return fmt.Errorf("file %q mount must be absolute", f.Name)
		}
	}
	seenHosts := map[string]bool{}
	for _, r := range s.Routes {
		if r.Port <= 0 || r.Port > 65535 {
			return fmt.Errorf("route port %d out of range", r.Port)
		}
		if r.Host == "" {
			continue
		}
		if len(r.Host) > 253 || !hostRe.MatchString(r.Host) {
			return fmt.Errorf("route host %q must be a lowercase hostname — letters, digits, hyphens, and dots", r.Host)
		}
		// Two routes on one hostname would render two proxy entries for the
		// same name; the second silently never matches.
		if seenHosts[r.Host] {
			return fmt.Errorf("two routes both claim the hostname %q", r.Host)
		}
		seenHosts[r.Host] = true
	}
	seenPorts := map[string]bool{}
	for _, p := range s.Ports {
		if p.Port <= 0 || p.Port > 65535 {
			return fmt.Errorf("published port %d out of range", p.Port)
		}
		if p.HostPort < 0 || p.HostPort > 65535 {
			return fmt.Errorf("published host port %d out of range", p.HostPort)
		}
		switch p.Protocol {
		case "", "tcp", "udp":
		default:
			return fmt.Errorf("published port protocol must be tcp or udp")
		}
		key := fmt.Sprintf("%d/%s", p.EffectiveHostPort(), p.EffectiveProtocol())
		if seenPorts[key] {
			return fmt.Errorf("host port %s published twice", key)
		}
		seenPorts[key] = true
	}
	for _, p := range s.Ports {
		if err := fogPortErr(p.EffectiveHostPort(), 0); err != nil {
			return err
		}
	}
	if len(s.Ports) > 0 && s.DeployStrategy == "swap" {
		return fmt.Errorf("published ports require the restart deploy strategy — two swap replicas can't bind the same host port")
	}
	if s.Health != nil && !strings.HasPrefix(s.Health.Path, "/") {
		return fmt.Errorf("health path must start with /")
	}
	if s.Backup != nil {
		switch s.Backup.Mode {
		case "", BackupModeVolumes, BackupModeDelegated:
		default:
			return fmt.Errorf("backup mode must be volumes or delegated")
		}
		if s.Backup.Timeout != "" {
			if _, err := time.ParseDuration(s.Backup.Timeout); err != nil {
				return fmt.Errorf("backup timeout %q is not a duration (e.g. \"45m\")", s.Backup.Timeout)
			}
		}
		if h := s.Backup.HourUTC; h != nil && (*h < 0 || *h > 23) {
			return fmt.Errorf("backup hourUTC must be 0–23")
		}
		if s.Backup.MaxBytes < 0 {
			return fmt.Errorf("backup maxBytes must not be negative")
		}
		// A delegated backup is an HTTP conversation — fog needs a port to
		// reach the app on, the same one the health prober uses.
		if s.DelegatedBackups() && s.Health == nil && len(s.Routes) == 0 {
			return fmt.Errorf("delegated backup needs a health check or route to reach the app")
		}
	}
	return nil
}

// Neither the user nor the host may start with "-": ssh would read either
// as an option (CVE-2017-1000117).
var scpStyleRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*@[A-Za-z0-9._\[\]][A-Za-z0-9._\-\[\]]*:[^\s]+$`)

// ValidateGitRepo is validateGitRepo for callers outside the package (the
// catalog's source settings).
func ValidateGitRepo(repo string) error { return validateGitRepo(repo) }

// validateGitRepo checks a repository URL is a shape Fog can actually clone,
// with a specific message per common mistake. A private repository is
// cloned over SSH with the app's deploy key; a public one may instead be
// cloned over https://, which needs no key at all — that is how a
// published app installs on a fog whose key no git host has heard of.
func validateGitRepo(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fmt.Errorf("git repository URL is required")
	}
	if strings.ContainsAny(repo, " \t\n") {
		return fmt.Errorf("the repository URL contains whitespace — check for a stray space")
	}
	if strings.HasPrefix(repo, "http://") {
		return fmt.Errorf("plain http:// is unencrypted — use https:// for a public repository, or the SSH form with a deploy key for a private one, e.g. ssh://gitea@host/owner/repo.git")
	}
	if strings.HasPrefix(repo, "https://") {
		u, err := url.Parse(repo)
		if err != nil || u.Host == "" || strings.HasPrefix(u.Host, "-") {
			return fmt.Errorf("not a valid https:// URL — expected https://host/owner/repo.git")
		}
		// A token in the URL would be written into the clone's .git/config,
		// inside the build context. Private repositories use a deploy key.
		if u.User != nil {
			return fmt.Errorf("no credentials in the URL — https:// is for public repositories; clone a private one over SSH with the deploy key")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("the https:// URL has a query or fragment — use the plain clone URL, e.g. https://%s/owner/repo.git", u.Host)
		}
		if strings.Trim(u.Path, "/") == "" {
			return fmt.Errorf("the https:// URL is missing the repository path — e.g. https://%s/owner/repo.git", u.Host)
		}
		return nil
	}
	if strings.HasPrefix(repo, "git://") {
		return fmt.Errorf("git:// URLs are unauthenticated and unsupported — use the SSH form, e.g. ssh://gitea@host/owner/repo.git")
	}
	if strings.HasPrefix(repo, "ssh://") {
		u, err := url.Parse(repo)
		if err != nil || u.Host == "" {
			return fmt.Errorf("not a valid ssh:// URL — expected ssh://user@host/owner/repo.git")
		}
		if strings.HasPrefix(u.Host, "-") || (u.User != nil && strings.HasPrefix(u.User.Username(), "-")) {
			return fmt.Errorf("the SSH URL's user or host starts with \"-\", which ssh would read as an option")
		}
		if u.User == nil || u.User.Username() == "" {
			return fmt.Errorf("the SSH URL is missing its user — most git hosts need one, e.g. ssh://gitea@%s%s", u.Host, u.Path)
		}
		if strings.Trim(u.Path, "/") == "" {
			return fmt.Errorf("the SSH URL is missing the repository path — e.g. ssh://%s@%s/owner/repo.git", u.User.Username(), u.Host)
		}
		return nil
	}
	// Absolute paths (a repo on this server) and scp-style user@host:path
	// are both valid git remotes.
	if strings.HasPrefix(repo, "/") {
		return nil
	}
	if scpStyleRe.MatchString(repo) {
		return nil
	}
	return fmt.Errorf("unrecognized repository URL — use ssh://user@host/owner/repo.git (or scp-style user@host:owner/repo.git), or https://host/owner/repo.git for a public repository")
}

// RouteHost resolves a route's hostname, applying the <app>.<domain> default.
//
// A Host with no dot is a subdomain label rather than a finished hostname:
// "claw" resolves to claw.<base domain>. Storing the label instead of the
// full name means a custom address follows the base domain if it ever
// changes, the same way the app-name default already does — a spec pinned to
// claw.old.example.com would just be stranded there. A dotted Host is taken
// verbatim, so an off-domain name stays an escape hatch.
func RouteHost(appName string, r Route, baseDomain string) string {
	if r.Host != "" {
		if strings.Contains(r.Host, ".") {
			return r.Host
		}
		if baseDomain == "" {
			return ""
		}
		return r.Host + "." + baseDomain
	}
	if baseDomain == "" {
		return ""
	}
	return appName + "." + baseDomain
}

func validateWants(wants []Want) error {
	seen := map[string]bool{}
	for _, w := range wants {
		switch {
		case w.App != "" && w.Offer != "":
			return fmt.Errorf("a want names an offer (%s) or an app (%s), not both", w.Offer, w.App)
		case w.App != "":
			if !nameRe.MatchString(w.App) || SurfaceGrant(w.App) {
				return fmt.Errorf("invalid wants app %q", w.App)
			}
			if len(w.As) > 0 {
				return fmt.Errorf("wants app %s: as renames an offer's variables, and an app want has none", w.App)
			}
		default:
			provider, offer, ok := strings.Cut(w.Offer, "/")
			if !ok || !nameRe.MatchString(provider) || !nameRe.MatchString(offer) {
				return fmt.Errorf("invalid wants offer %q (provider/offer, like glass/monitoring)", w.Offer)
			}
		}
		if seen[w.key()] {
			return fmt.Errorf("wants %s twice", w.Offer+w.App)
		}
		seen[w.key()] = true
		for from, to := range w.As {
			if !envKeyRe.MatchString(from) || !envKeyRe.MatchString(to) {
				return fmt.Errorf("wants %s: invalid rename %s → %s", w.Offer, from, to)
			}
			// What the rename lands on is set from the provider's answer.
			if err := CheckOfferedName(to); err != nil {
				return fmt.Errorf("wants %s: %w", w.Offer, err)
			}
		}
		for _, c := range w.Capabilities {
			if strings.TrimSpace(c) == "" {
				return fmt.Errorf("wants %s: an empty capability", w.Offer+w.App)
			}
		}
	}
	return nil
}

func validateOffers(offers []Offer) error {
	ids := map[string]bool{}
	for _, o := range offers {
		if !nameRe.MatchString(o.ID) {
			return fmt.Errorf("invalid offer id %q", o.ID)
		}
		if ids[o.ID] {
			return fmt.Errorf("duplicate offer %q", o.ID)
		}
		ids[o.ID] = true
		if strings.TrimSpace(o.Title) == "" {
			return fmt.Errorf("offer %q needs a title", o.ID)
		}
		if len(o.Env) == 0 && len(o.Secrets) == 0 {
			return fmt.Errorf("offer %q sets no env and no secrets", o.ID)
		}
		for _, c := range o.Capabilities {
			if strings.TrimSpace(c) == "" {
				return fmt.Errorf("offer %q: an empty capability", o.ID)
			}
		}
		names := map[string]bool{}
		for k := range o.Env {
			if !envKeyRe.MatchString(k) {
				return fmt.Errorf("offer %q: invalid env var name %q", o.ID, k)
			}
			if err := CheckOfferedName(k); err != nil {
				return fmt.Errorf("offer %q: %w", o.ID, err)
			}
			names[k] = true
		}
		for _, k := range o.Secrets {
			if !envKeyRe.MatchString(k) {
				return fmt.Errorf("offer %q: invalid secret name %q", o.ID, k)
			}
			if err := CheckOfferedName(k); err != nil {
				return fmt.Errorf("offer %q: %w", o.ID, err)
			}
			if names[k] {
				return fmt.Errorf("offer %q names %s twice", o.ID, k)
			}
			names[k] = true
		}
	}
	return nil
}
