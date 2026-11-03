package apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
)

// Materializer renders an AppSpec into plain artifacts under
// <datadir>/apps/<name>/: compose.yaml, .env (0600), and files/. The
// invariant that makes Fog uninstall-safe: these artifacts are complete —
// `docker compose up -d` in that directory reproduces exactly what Fog runs.
type Materializer struct {
	DataDir string // e.g. /var/lib/fog
	// MountPolicy, when set, rules on a host-path volume at render time:
	// whether it must mount read-only, or may not mount at all (an error
	// fails the deploy with that reason). Wired to Files across peers.
	MountPolicy func(hostPath string) (readOnly bool, err error)
	// FilesRoot answers where Files lives right now; a volume that names a
	// Files folder resolves against it at render time. Unset (tests, a
	// fogd without Files) means such a volume cannot be rendered.
	FilesRoot func() string
	// EnsureFilesDir creates a Files folder on the app's behalf, through
	// the Files manager — so it is owned like everything else in Files,
	// indexed, and refused if Files itself is unusable (a stale share).
	// Optional: without it the directory is created directly.
	EnsureFilesDir func(rel string) error
	// Pin and Unpin pin each Drive and host-path volume at a path only fogd
	// writes, and compose binds that (pin.go). Nil (macOS development, a
	// fogd that can't mount) binds the paths as they are, and refuses a
	// volume nested in another the app can write.
	Pin   PinFunc
	Unpin UnpinFunc
	// ManagedRoots answers the NAS shares' mount points: host paths under
	// one are fog's storage, and bind without an acknowledgement.
	ManagedRoots func() []string
	// SharesFor answers the Drive folders the apps an app uses share with
	// it (Engine.SharesFor), which it binds like its own. Nil: none.
	SharesFor func(app string, spec *AppSpec) func(rel string) bool
}

// PinWarning is a pin that took, with something lesser that didn't (the
// owner of a share that refuses chown), for the deploy log.
type PinWarning struct{ Msg string }

func (w *PinWarning) Error() string { return w.Msg }

// resolveFiles turns a Files volume into the host path to bind, creating
// the folder on the way. The error names the volume and the reason: a
// deploy that fails here fails before a build, with the fix in the log.
func (m *Materializer) resolveFiles(v Volume) (string, error) {
	root := ""
	if m.FilesRoot != nil {
		root = m.FilesRoot()
	}
	if root == "" {
		return "", notReadyError{fmt.Errorf("volume %s lives in Drive (%s), but Drive is not available on this server", v.Name, v.Files)}
	}
	if m.EnsureFilesDir != nil {
		if err := m.EnsureFilesDir(v.Files); err != nil {
			return "", fmt.Errorf("volume %s: cannot prepare the Drive folder %s: %w", v.Name, v.Files, err)
		}
	} else if err := mkdirInRoot(root, filepath.FromSlash(v.Files)); err != nil {
		return "", fmt.Errorf("volume %s: cannot create the Drive folder %s: %w", v.Name, v.Files, err)
	}
	return v.HostPath(root), nil
}

func (m *Materializer) AppDir(name string) string   { return filepath.Join(m.DataDir, "apps", name) }
func (m *Materializer) DataPath(name string) string { return filepath.Join(m.DataDir, "data", name) }

// Render writes all artifacts for an app. env holds the merged plain+secret
// environment (secrets already decrypted — they land only in .env, mode 0600).
// warn receives non-fatal problems (a chown that couldn't be applied) so they
// land in the deploy log instead of vanishing. sealedGateway is the address
// of fog-sealed's gateway, which a sealed app has to be given and no other
// app reads.
func (m *Materializer) Render(name string, spec *AppSpec, env map[string]string, image, sealedGateway string, warn io.Writer) error {
	return m.RenderWith(name, spec, env, image, sealedGateway, nil, warn)
}

// RenderWith is Render onto planned networks: the app's own, and the wires
// it is on (netwire.go). nil means its own network alone.
func (m *Materializer) RenderWith(name string, spec *AppSpec, env map[string]string, image, sealedGateway string, nets *AppNetworks, warn io.Writer) error {
	if nets == nil {
		nets = defaultNetworks(name, spec)
	}
	if spec.Sealed() && nets.gatewayIP != "" {
		sealedGateway = nets.gatewayIP
	}
	if spec.Sealed() && sealedGateway == "" {
		return fmt.Errorf("%s is sealed, but fog doesn't know the sealed network's gateway", name)
	}
	dir := m.AppDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	filesRoot := ""
	if m.FilesRoot != nil {
		filesRoot = m.FilesRoot()
	}
	// Pinned or not: a writable volume holding another of the app's is
	// the app's to swap for a link.
	if err := checkNestedVolumes(spec, filesRoot); err != nil {
		return rulesError{err}
	}
	pinCtx, cancelPins := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelPins()
	pinned := []string{}

	// Volumes: create data dirs and enforce ownership.
	buildMounts := func(vols []Volume) ([]any, error) {
		var out []any
		for _, v := range vols {
			hostPath := v.Host
			if v.Files != "" {
				// A Drive folder that can't be made safely (a link where a
				// folder was) is as refused as a bind would be.
				p, err := m.resolveFiles(v)
				if err != nil {
					return nil, refusedOrNotReady(err)
				}
				hostPath = p
			}
			// A revision saved before a path was dangerous, or a path that
			// only resolves to one on this machine, is caught here, with the
			// data dir this fogd really uses (hostaccess.go).
			if err := m.checkRenderHostPath(name, spec, v, hostPath); err != nil {
				return nil, rulesError{err}
			}
			if v.Host != "" && v.Owner != "" {
				if err := m.chownAllowed(name, hostPath); err != nil {
					return nil, fmt.Errorf("volume %s: %w", v.Name, err)
				}
			}
			src := Volume{Files: v.Files, Host: v.Host}
			if v.Host != "" && m.Pin != nil {
				// No link is ever followed — but /etc/localtime's (and
				// /etc/timezone's) into the zone database, read-only.
				real, err := zoneinfoTarget(v)
				if err != nil {
					return nil, rulesError{fmt.Errorf("volume %s: %w", v.Name, err)}
				}
				src.Host = real
			}
			if root, rel, ok := pinSource(src, filesRoot); ok && m.Pin != nil {
				ro := v.ReadOnly
				if m.MountPolicy != nil {
					forced, err := m.MountPolicy(hostPath)
					if err != nil {
						return nil, fmt.Errorf("volume %s: %w", v.Name, err)
					}
					ro = ro || forced
				}
				owner := ""
				if v.Host != "" && v.Owner != "" {
					// Docker would make a missing bind source root:root;
					// fog makes the last folder itself (never through a
					// link) and the pin re-owns it.
					if _, err := ensureHostDir(hostPath); err != nil {
						fmt.Fprintf(warn, "warning: volume %s: %v\n", v.Name, err)
					}
					owner = v.Owner
				}
				target := m.PinPath(name, v.Name)
				if err := m.Pin(pinCtx, root, rel, target, owner); err != nil {
					var w *PinWarning
					if !errors.As(err, &w) {
						// Not bound by its pin is not bound at all: the app
						// doesn't run on what its old compose file says.
						return nil, refusedOrNotReady(fmt.Errorf("volume %s: %w", v.Name, err))
					}
					fmt.Fprintf(warn, "warning: volume %s: %v\n", v.Name, w)
				}
				pinned = append(pinned, v.Name)
				out = append(out, map[string]any{
					"type": "bind", "source": target, "target": v.Mount, "read_only": ro,
					// The pin exists only while fogd keeps it: Docker must
					// refuse to start rather than make an empty folder.
					"bind": map[string]any{"create_host_path": false},
				})
				continue
			}
			// Non-fatal by design, in both branches: chown fails on dev
			// machines (macOS) where container uid mapping doesn't matter.
			// It goes to the deploy log, not the void.
			switch {
			case hostPath == "":
				hostPath = filepath.Join(m.DataPath(name), v.Name)
				_, statErr := os.Stat(hostPath)
				created := os.IsNotExist(statErr)
				if err := os.MkdirAll(hostPath, 0o755); err != nil {
					return nil, err
				}
				if v.Owner != "" {
					if err := ensureVolumeOwner(hostPath, v.Owner, created, true); err != nil {
						fmt.Fprintf(warn, "warning: volume %s: %v\n", v.Name, err)
					}
				}
			case v.Host != "" && v.Owner != "":
				// An Owner on a host path used to be parsed, displayed in the
				// UI, and then silently ignored — so an app pointed at a NAS
				// directory that didn't exist yet got the one Docker makes on
				// its behalf, root:root 0755, and a container running as uid
				// 1000 couldn't write to the share it was just given.
				if created, err := ensureHostDir(hostPath); err != nil {
					fmt.Fprintf(warn, "warning: volume %s: %v\n", v.Name, err)
				} else if err := ensureVolumeOwner(hostPath, v.Owner, created, false); err != nil {
					fmt.Fprintf(warn, "warning: volume %s: %v\n", v.Name, err)
				}
			}
			ro := v.ReadOnly
			if !v.Managed() && m.MountPolicy != nil {
				// A host path may carry a rule of its own — a copy of a
				// peer's share mounts read-only or not at all.
				forced, err := m.MountPolicy(hostPath)
				if err != nil {
					return nil, fmt.Errorf("volume %s: %w", v.Name, err)
				}
				ro = ro || forced
			}
			mount := hostPath + ":" + v.Mount
			if ro {
				mount += ":ro"
			}
			out = append(out, mount)
		}
		return out, nil
	}
	mounts, err := buildMounts(spec.Volumes)
	if err != nil {
		return err
	}
	// Sidecar volumes are pinned too; then any pin of the app's that no
	// volume names any more goes.
	auxMountsOf := map[string][]any{}
	for _, a := range spec.Aux {
		am, err := buildMounts(a.Volumes)
		if err != nil {
			return err
		}
		auxMountsOf[a.Name] = am
	}
	if m.Unpin != nil {
		if err := m.Unpin(pinCtx, filepath.Join(m.BindsDir(), name), pinned...); err != nil {
			fmt.Fprintf(warn, "warning: removing pins no volume uses: %v\n", err)
		}
	}

	// Files: rendered read-only mounts, versioned via the spec.
	filesDir := filepath.Join(dir, "files")
	specFiles := map[string]bool{}
	if len(spec.Files) > 0 {
		if err := os.MkdirAll(filesDir, 0o755); err != nil {
			return err
		}
		for _, f := range spec.Files {
			specFiles[f.Name] = true
			p := filepath.Join(filesDir, f.Name)
			mode := os.FileMode(0o644)
			if f.Exec {
				mode = 0o755
			}
			if err := fsx.WriteFileAtomic(p, []byte(f.Content), mode); err != nil {
				return err
			}
			mounts = append(mounts, p+":"+f.Mount+":ro")
		}
	}
	// files/ mirrors the spec, so a File dropped from it must not linger as a
	// stale rendered copy. Only plain entries directly under files/ — the set
	// Render itself writes (names are validated flat) — are ever removed.
	if entries, err := os.ReadDir(filesDir); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() && !specFiles[ent.Name()] {
				os.Remove(filepath.Join(filesDir, ent.Name()))
			}
		}
	}

	// .env — the only place secret values ever touch disk.
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line, err := envFileLine(k, env[k])
		if err != nil {
			return err
		}
		b.WriteString(line)
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, ".env"), []byte(b.String()), 0o600); err != nil {
		return err
	}

	// Sidecars live on a private per-app network: the app reaches them by
	// their aux name (compose's service-name DNS), other apps can't see
	// them, and env references stay static regardless of the app's name.
	hasAux := len(spec.Aux) > 0

	// host-gateway resolves to the default docker bridge's gateway, which a
	// container on an internal network has no route to. A sealed app's
	// host.docker.internal is its own network's gateway instead, an address
	// on its own subnet, so FOG_API_URL and FLARE_DSN read the same for
	// every app (docs/sealed-network.md).
	hostGateway := "host.docker.internal:host-gateway"
	if spec.Sealed() {
		hostGateway = "host.docker.internal:" + sealedGateway
	}
	// attach is the networks a main or worker container joins: the app's own
	// network, which no other app's containers share (netwire.go). Only the
	// main service carries the app's names: its own, and the one fog's
	// proxies reach it by, "<app>.fog-app", which no other app can answer to.
	attach := func(aliases []string) map[string]any {
		out := map[string]any{}
		if aliases == nil {
			out[nets.Own] = map[string]any{}
		} else {
			out[nets.Own] = map[string]any{"aliases": append(aliases, AppUpstream(name))}
		}
		if hasAux {
			out["private"] = map[string]any{}
		}
		return out
	}

	// compose.yaml — main service plus worker processes sharing image, env,
	// and volumes. No container_name: the blue-green swap runs two replicas
	// of the main service briefly, which requires compose-managed names.
	base := func(labels map[string]string) map[string]any {
		s := map[string]any{
			"image":    image,
			"restart":  "unless-stopped",
			"networks": attach([]string{name}),
			"labels":   labels,
			// App → fogd reachability for error reporting (FLARE_DSN):
			// containers have no route to the host by default. host-gateway
			// pins host.docker.internal to the docker bridge gateway — the
			// same line Caddy's compose uses to proxy back to fogd.
			"extra_hosts": []string{hostGateway},
			"cap_drop":    dropCaps(spec),
		}
		if len(env) > 0 {
			s["env_file"] = []string{".env"}
		}
		if len(mounts) > 0 {
			s["volumes"] = mounts
		}
		if spec.MemoryLimit != "" {
			s["mem_limit"] = spec.MemoryLimit
		}
		return s
	}
	services := map[string]any{}
	main := base(fogLabels(name, "", ""))
	if len(spec.Command) > 0 {
		main["command"] = spec.Command
	}
	if len(spec.Ports) > 0 {
		// Raw host-published ports (DNS and friends). Main service only —
		// workers and sidecars never publish. Long syntax: the short form
		// "53:53/udp" is a YAML footpath (unquoted 53:53 parses as a sexagesimal
		// number in some parsers) and the long form is unambiguous.
		var ports []map[string]any
		for _, p := range spec.Ports {
			ports = append(ports, map[string]any{
				"target":    p.Port,
				"published": p.EffectiveHostPort(),
				"protocol":  p.EffectiveProtocol(),
			})
		}
		main["ports"] = ports
	}
	if hasAux {
		deps := make([]string, 0, len(spec.Aux))
		for _, a := range spec.Aux {
			deps = append(deps, a.Name)
		}
		main["depends_on"] = deps
	}
	services[name] = main
	for _, p := range spec.Processes {
		w := base(fogLabels(name, p.Name, ""))
		w["command"] = p.Command
		// Workers don't serve routes; drop the DNS alias to keep it unambiguous.
		w["networks"] = attach(nil)
		services[name+"-"+p.Name] = w
	}
	for _, a := range spec.Aux {
		if a.Name == name {
			return fmt.Errorf("aux service %q collides with the app name", a.Name)
		}
		auxMounts := auxMountsOf[a.Name]
		s := map[string]any{
			"image":    a.Image,
			"restart":  "unless-stopped",
			"networks": map[string]any{"private": map[string]any{}},
			"labels":   fogLabels(name, "", a.Name),
			"cap_drop": dropCaps(spec),
		}
		if len(a.Env) > 0 {
			// ${VAR} in a sidecar's env reads the app's environment — how a
			// sidecar shares a vault-generated secret. fog expands it here and
			// writes the result to the sidecar's own env file (0600, like
			// .env): compose interpolates nothing fog writes, and is never
			// handed the app's environment as its own settings (ComposeCmd).
			var b strings.Builder
			for _, k := range sortedEnvNames(a.Env) {
				line, err := envFileLine(k, expandEnvRefs(a.Env[k], env))
				if err != nil {
					return fmt.Errorf("sidecar %s: %w", a.Name, err)
				}
				b.WriteString(line)
			}
			file := "aux-" + a.Name + ".env"
			if err := fsx.WriteFileAtomic(filepath.Join(dir, file), []byte(b.String()), 0o600); err != nil {
				return err
			}
			s["env_file"] = []string{file}
		}
		if len(auxMounts) > 0 {
			s["volumes"] = auxMounts
		}
		if len(a.Command) > 0 {
			s["command"] = a.Command
		}
		if a.ShmSize != "" {
			s["shm_size"] = a.ShmSize
		}
		services[a.Name] = s
	}
	networks := map[string]any{nets.Own: map[string]any{"external": true}}
	if hasAux {
		// fog makes the sidecar network, on a subnet of its own choosing
		// (netwire.go): left to compose, each one took a slot of Docker's
		// default pools, about thirty networks for the whole machine. A
		// sealed app's is internal — it is on this one too, and a default
		// route here would be its way out — which fog fixes at creation and
		// the deploy's breach check verifies.
		networks["private"] = map[string]any{"external": true, "name": nets.aux()}
	}
	doc := map[string]any{
		"name":     name,
		"services": services,
		"networks": networks,
	}
	out, err := yaml.Marshal(composeLiteral(doc, true))
	if err != nil {
		return err
	}
	header := "# Generated by Fog. Edit through the Fog UI.\n# Recovery without Fog: docker compose up -d\n"
	// Every artifact lands whole (fsx.WriteFileAtomic), or the header's
	// promise breaks: after a crash mid-write, `docker compose up -d` would
	// run a torn compose.yaml or a half-rendered .env.
	return fsx.WriteFileAtomic(filepath.Join(dir, "compose.yaml"), []byte(header+string(out)), 0o644)
}

// dropCaps is what every container of an app gives up of Docker's default
// capabilities. NET_RAW: a raw socket forges any address on the app's
// network — an app ARP-spoofed its gateway and reached fog's proxy as it,
// past session pinning — and ping needs none (ICMP datagram sockets). MKNOD:
// device nodes an app has no use for. The rest of Docker's default set stays
// (chown, setuid and the like), which ordinary images need; an app asks for
// raw sockets back with acknowledgeRawSockets.
func dropCaps(spec *AppSpec) []string {
	if spec.AcknowledgeRawSockets {
		return []string{"MKNOD"}
	}
	return []string{"MKNOD", "NET_RAW"}
}

// fogLabels are the fog.* labels of one of an app's services, every one of
// them set — empty where it doesn't apply. Compose's labels override an
// image's, so an image's LABEL fog.system=caddy (or fog.aux, fog.process)
// can't make its container something fog would treat it as.
func fogLabels(app, process, aux string) map[string]string {
	return map[string]string{
		"fog.managed": "true", "fog.app": app, "fog.process": process, "fog.aux": aux, "fog.system": "",
		RulesLabel: RulesVersion,
	}
}

// RulesLabel says which rules a container was rendered under, for anyone
// reading `docker inspect`. fog never trusts it — an image can set any
// label on a container a render didn't — and goes by its own record of the
// containers it brought up instead (disarmOldContainers).
const (
	RulesLabel   = "fog.rules"
	RulesVersion = "2"
)

// composeLiteral returns v with every "$" doubled, keys included, so that
// docker compose reads every string fog writes as the string it is. Compose
// interpolates ${VAR} — and ${VAR-default}, ${VAR:?err} — throughout the
// file, after fog has checked it: a host path written as
// "/srv/x${UNSET-/var/run/docker.sock}" passed every check as a harmless
// literal and bound the Docker socket. No exception: a sidecar's ${VAR} is
// expanded by fog (expandEnvRefs), never by compose.
func composeLiteral(v any, top bool) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, "$", "$$")
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = strings.ReplaceAll(s, "$", "$$")
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = composeLiteral(e, false)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = composeLiteral(e, false)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, s := range x {
			out[strings.ReplaceAll(k, "$", "$$")] = strings.ReplaceAll(s, "$", "$$")
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[strings.ReplaceAll(k, "$", "$$")] = composeLiteral(e, false)
		}
		return out
	}
	return v
}

// Remove deletes materialized artifacts; app data under data/<name> is kept
// unless deleteData is set.
func (m *Materializer) Remove(name string, deleteData bool) error {
	if err := os.RemoveAll(m.AppDir(name)); err != nil {
		return err
	}
	if deleteData {
		return os.RemoveAll(m.DataPath(name))
	}
	return nil
}

// escapeEnvValue renders a value for compose's env_file parser so that it
// reads back byte for byte, whatever the value holds.
//
// This is not shell quoting and not Go quoting, and the difference is not
// cosmetic. Compose expands ${VAR} inside DOUBLE-quoted values, so writing a
// secret with strconv.Quote silently destroyed any value containing a dollar
// sign followed by a name: a bcrypt hash ("$2b$12$KIXQ…") reached the
// container as "$2b$12".
//
// Nor is single quoting literal, though it looks it: compose's parser
// treats \' as an escaped quote even inside single quotes, so a value ending
// in a backslash ran on into the next line, and every line after it became a
// variable of its own — later duplicates win. A provider app's minted
// integration value could override FOG_API_URL in a consumer's .env that
// way, and collect its FOG_API_TOKEN.
//
// So: always double quotes, with every character the parser acts on escaped.
// Inside double quotes compose (compose-go/dotenv) unescapes \\ \" \$ \n
// \r, turns \$ into $$ and $$ into $, and leaves nothing else live. Pinned
// by a round-trip fuzz test against compose-go's own parser.
func escapeEnvValue(v string) string {
	return `"` + envDoubleQuoteEscaper.Replace(v) + `"`
}

var envDoubleQuoteEscaper = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	`$`, `\$`,
	"\n", `\n`,
	"\r", `\r`,
)

// EnvFileLine is envFileLine, for fog's own .env files.
func EnvFileLine(k, v string) (string, error) { return envFileLine(k, v) }

// envFileLine renders one KEY="value" line, refusing what no quoting can
// carry: a key compose would read as something else, or a NUL, which no
// environment variable can hold.
func envFileLine(k, v string) (string, error) {
	if !envKeyRe.MatchString(k) {
		return "", fmt.Errorf("environment variable name %q is not a valid name", k)
	}
	if strings.IndexByte(v, 0) >= 0 {
		return "", fmt.Errorf("environment variable %s holds a NUL byte", k)
	}
	return k + "=" + escapeEnvValue(v) + "\n", nil
}

// ensureHostDir prepares a host-path bind source so that fog, not Docker,
// decides how it comes into existence. Docker creates a missing bind source
// as root:root 0755 — which is exactly how an app pointed at a NAS directory
// that doesn't exist yet ends up unable to write to it.
//
// Only the final component is created, and only when its parent is already a
// directory. A host path whose parent is missing is the "the share isn't
// mounted" case, and materializing a tree under a dead mountpoint would hide
// that failure behind a directory quietly filling the root disk.
func ensureHostDir(path string) (created bool, err error) {
	switch _, err := os.Stat(path); {
	case err == nil:
		return false, nil
	case !os.IsNotExist(err):
		return false, err
	}
	parent := filepath.Dir(path)
	if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return false, fmt.Errorf("%s does not exist, and neither does %s — is the share mounted?", path, parent)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// mkdirInRoot creates rel and its parents inside root, never outside it.
// Drive is written by apps: a folder one of them replaced with a symlink to
// /etc must not have fogd, as root, create directories there.
func mkdirInRoot(root, rel string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.MkdirAll(rel, 0o755)
}

// ensureVolumeOwner applies "uid:gid" ownership to a volume's directory.
//
// recurse separates the two kinds of tree fog owns this decision for:
//
//   - A managed volume is fog's own, so the whole tree is fog's to enforce.
//     The recursive walk is the expensive part — chown -R over a large media
//     archive takes minutes, and this used to run on EVERY deploy. It now
//     runs only when it can change anything: on a freshly created dir
//     (trivially cheap), or when the existing root's ownership disagrees
//     with the spec — the rare "Owner was edited" case, where the tree
//     underneath genuinely needs re-owning. A root that already matches
//     means a previous deploy enforced this Owner and the walk is skipped.
//
//   - A host path is someone else's directory — a NAS share, a media library
//     with terabytes fog never put there. Only the mount root is touched:
//     enough to let the container's uid write into it, without a walk fog
//     has no business doing over a share it doesn't own.
//
// fogd runs as root, and everything under a volume is the app's to change
// while it runs, so nothing here follows a symlink out of the volume. The
// volume's own directory is opened O_NOFOLLOW — one that is a symlink (a
// host path another app swapped for one to /etc) is refused, never chowned
// through — and owned through that descriptor. The walk under it goes
// through an os.Root on the same directory: each entry is lchowned, so a
// link is re-owned itself and never what it names, and a directory swapped
// for a link mid-walk can't lead the walk outside.
func ensureVolumeOwner(path, owner string, created, recurse bool) error {
	parts := strings.SplitN(owner, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("owner %q is not uid:gid", owner)
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("owner %q: uid %q is not numeric", owner, parts[0])
	}
	gid, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("owner %q: gid %q is not numeric", owner, parts[1])
	}
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return fmt.Errorf("%s is a symbolic link or not a directory; not changing its owner", path)
		}
		return err
	}
	defer dir.Close()
	st, err := dir.Stat()
	if err != nil {
		return err
	}
	if !created {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == uid && int(sys.Gid) == gid {
			return nil
		}
	}
	return volumeChown(dir, path, uid, gid, recurse)
}

// volumeChown re-owns an opened volume directory (and, with recurse, the tree
// under it) — the half of ensureVolumeOwner that changes anything, a seam so
// tests can watch the decision without root.
var volumeChown = chownVolume

func chownVolume(dir *os.File, path string, uid, gid int, recurse bool) error {
	owner := fmt.Sprintf("%d:%d", uid, gid)
	st, err := dir.Stat()
	if err != nil {
		return err
	}
	if err := dir.Chown(uid, gid); err != nil {
		return fmt.Errorf("chown %s %s: %w", owner, path, err)
	}
	if !recurse {
		return nil
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	// The root must be the directory just checked, not one swapped in since.
	if rst, err := root.Stat("."); err != nil || !os.SameFile(rst, st) {
		return fmt.Errorf("%s changed while its owner was being set", path)
	}
	var failed error
	walkErr := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if failed == nil {
				failed = err
			}
			return nil
		}
		if p == "." {
			return nil
		}
		if err := root.Lchown(p, uid, gid); err != nil && failed == nil {
			failed = fmt.Errorf("chown %s %s: %w", owner, filepath.Join(path, p), err)
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	return failed
}

// ComposeCmd builds a docker compose invocation in the app's directory.
//
// The context is load-bearing, not decorative: compose is where every deploy
// spends its time, so a call that ignored cancellation would make the caller's
// timeout a fiction and pin the app's lock to work nobody is waiting for.
// execx kills the whole process group, since compose's real work (pulls,
// buildkit) happens in grandchildren.
//
// Every invocation names its file, its project and its settings file
// outright. Left to find them, compose read the app's .env — written from
// the spec's env, which another app's integration offer can set — as its
// own settings: COMPOSE_FILE="compose.yaml:files/evil.yaml" ran a file of
// the app's choosing (a privileged container with / bound), and
// COMPOSE_PROJECT_NAME=<another app> with --remove-orphans tore that app's
// containers down. So: -f the file fog wrote, the project named for the
// app, an --env-file fog owns and keeps empty (nothing fog writes needs
// interpolating: composeLiteral, expandEnvRefs), and an environment with
// no COMPOSE_* setting in it.
func (m *Materializer) ComposeCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	dir := m.AppDir(name)
	full := append([]string{"compose",
		"--project-name", name,
		"--project-directory", dir,
		"-f", filepath.Join(dir, "compose.yaml"),
		"--env-file", m.emptyEnvFile(),
	}, args...)
	cmd := execx.Command(ctx, "docker", full...)
	cmd.Dir = dir
	cmd.Env = composeEnviron(os.Environ())
	return cmd
}

// emptyEnvFile is the settings file every compose invocation reads: fog's,
// outside every app's directory, and empty.
func (m *Materializer) emptyEnvFile() string {
	p := filepath.Join(m.DataDir, "compose-settings.env")
	if st, err := os.Stat(p); err != nil || st.Size() != 0 {
		fsx.WriteFileAtomic(p, nil, 0o644)
	}
	return p
}

// composeEnviron is fogd's environment without anything compose would read
// as a setting (COMPOSE_*) or the CLI as a behaviour (DOCKER_* beyond where
// the daemon is and how to reach it).
func composeEnviron(env []string) []string {
	keep := map[string]bool{"DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_CONFIG": true,
		"DOCKER_CERT_PATH": true, "DOCKER_TLS_VERIFY": true}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if strings.HasPrefix(u, "COMPOSE_") || (strings.HasPrefix(u, "DOCKER_") && !keep[u]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// refusedOrNotReady sorts a bind that couldn't be made: a source that
// isn't there (a NAS share not mounted yet, a disk Drive was placed on) is
// storage not ready, and the app waits for it; anything else — a link in
// the path, a folder that can't be made safely — is refused by the rules.
// Waiting is no way around the rules: the app is stopped either way.
func refusedOrNotReady(err error) error {
	if errors.As(err, new(notReadyError)) {
		return err
	}
	if errors.Is(err, fs.ErrNotExist) {
		return notReadyError{err}
	}
	return rulesError{err}
}

// zoneLinks are the only host paths whose symbolic link fog follows, and
// only into zoneinfoDir, read-only: every compose file binds the host's
// time zone, and /etc/localtime is a link on nearly every system.
var zoneLinks = []string{"/etc/localtime", "/etc/timezone"}

const zoneinfoDir = "/usr/share/zoneinfo"

// zoneinfoTarget is the path a host volume is pinned from: v.Host as
// written — the pin follows no link in it — or, for one of zoneLinks bound
// read-only, the zone file it links to, checked again as a bind of its
// own.
func zoneinfoTarget(v Volume) (string, error) {
	p := cleanHostPath(v.Host)
	if !slices.Contains(zoneLinks, p) {
		return v.Host, nil
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return v.Host, nil
	}
	if !v.ReadOnly {
		return "", fmt.Errorf("%s is a link; fog follows it only for a read-only bind", p)
	}
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	target = cleanHostPath(target)
	if target == zoneinfoDir || !hostWithin(target, zoneinfoDir) {
		return "", fmt.Errorf("%s links to %s, outside %s; give the real path", p, target, zoneinfoDir)
	}
	if why := (HostStorage{}).RefusedHostPath(target); why != "" || !readOnlySafe(target) {
		return "", fmt.Errorf("%s links to %s, which fog doesn't bind", p, target)
	}
	return target, nil
}

// disarmComposeFile replaces app's compose file with one that runs
// nothing, for an app the rules refused: whatever reads the file on disk
// afterwards — a restore, a hand-run compose — finds no service in it.
func (m *Materializer) disarmComposeFile(name string) error {
	p := filepath.Join(m.AppDir(name), "compose.yaml")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil
	}
	return fsx.WriteFileAtomic(p, []byte("# Refused by fog's rules: deploy the app again to run it.\nservices: {}\n"), 0o644)
}
