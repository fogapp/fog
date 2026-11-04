// Package catalog is Fog's built-in app store: a curated set of well-behaved
// apps with sensible specs, so installing Sonarr is one click and a name.
// Entries are templates over the same AppSpec used everywhere — installing
// from the store and hand-crafting an app produce identical objects, and
// everything remains editable afterwards. Apps that need siblings (their own
// database, a broker) declare them as aux sidecars in the same spec.
package catalog

import (
	"fmt"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/hostcpu"
	"gitea.lan/matthew/fog/internal/signin"
)

// Option is a small, operator-facing input surfaced at install time.
// Target selects what it fills: "env:<KEY>", "volume:<name>" (where the
// volume lives — the spec's own placement is the default, so a library
// folder in Files needs no answer at all), "secret:<KEY>" (vault-stored,
// masked in the UI), or "repo" (git clone URL).
type Option struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Hint     string `json:"hint,omitempty"`
	Target   string `json:"target"`
	Required bool   `json:"required"`
	Default  string `json:"default,omitempty"`
	// Generate makes the UI prefill a random value ("alnum32" | "hex32" |
	// "hex64" | "hex128") — used for fresh-install secrets like Rails
	// encryption keys.
	// Options with a Default or Generate are tucked into the install pane's
	// Advanced section; only un-defaultable inputs stay primary.
	Generate string `json:"generate,omitempty"`
	// Advanced tucks an input away even though nothing can prefill it — for
	// knobs that are genuinely optional and would only crowd the first
	// screen. Blank means the app decides for itself, so the primary pane
	// stays the handful of answers Fog actually needs.
	Advanced bool `json:"advanced,omitempty"`
}

// Companion is an optional sibling app offered at install time — "also
// install X" checkboxes for entries that work better together (Hub →
// Immich, Paperless, Whisper). Companions must be one-click installable:
// every option needs a default or generator.
type Companion struct {
	ID    string `json:"id"`    // catalog entry ID
	Blurb string `json:"blurb"` // why this app pairs with the parent
	// Default ticks the companion in the install pane: the parent does
	// little without it (Hook without a downloader).
	Default bool `json:"default,omitempty"`
}

// Requires states what the entry's image assumes of the machine — things
// Fog can check before an install rather than learn from a crash loop.
type Requires struct {
	// CPU is the x86-64 microarchitecture level the image's prebuilt
	// binaries were compiled for: "x86-64-v2" (SSE4.2, POPCNT), "x86-64-v3"
	// (AVX2), or "x86-64-v4" (AVX-512). Only amd64 hosts are held to it —
	// arm64 images are built separately and have no such level.
	CPU string `json:"cpu,omitempty"`
}

type Entry struct {
	ID          string       `json:"id"` // default app name (slug)
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Category    string       `json:"category"`
	Icon        string       `json:"icon"` // emoji — the UI stays self-contained
	Spec        apps.AppSpec `json:"spec"`
	Options     []Option     `json:"options,omitempty"`
	Companions  []Companion  `json:"companions,omitempty"`
	Requires    *Requires    `json:"requires,omitempty"`
	// Formerly is every name this software's backups were made under before
	// it was renamed (apps.FormerNames): an artifact declaring one restores
	// into this entry as readily as one declaring its ID.
	Formerly []string `json:"formerly,omitempty"`
	// FirstParty names the household's own repository an entry builds from
	// ("fit"); its clone URL comes from this fog's settings (source.go).
	FirstParty string `json:"firstParty,omitempty"`
	// PublicRepo is a first-party app's published clone URL, used when the
	// operator hasn't overridden it, ahead of their base.
	PublicRepo string `json:"publicRepo,omitempty"`
	// ProjectURL is the upstream project's repository, for image apps: the
	// image name can't say where lscr.io or Docker Hub images come from, so
	// the catalog does.
	ProjectURL string `json:"projectUrl,omitempty"`
	// Blocker is why this machine can't run the entry, in the operator's
	// terms, or empty. It is not part of the catalog — the API fills it in
	// per host from Preflight, so the store, the install pane, and the
	// server's refusal all say the same thing.
	Blocker string `json:"blocker,omitempty"`

	// The store front (listing.go): what the listing page shows beyond
	// the one-line Description. Filled from listings by Entries(); its
	// fields serialize inline, as the entry's own.
	Listing
	// Footprint (footprint.go) is what the app is expected to cost the
	// machine; the fit in /api/capacity holds it against the host.
	Footprint *Footprint `json:"footprint,omitempty"`
}

// cpuLevel parses a Requires.CPU tag; 0 means none.
func cpuLevel(tag string) int {
	switch tag {
	case "x86-64-v2":
		return 2
	case "x86-64-v3":
		return 3
	case "x86-64-v4":
		return 4
	}
	return 0
}

// Preflight checks the entry's requirements against the host and returns
// the reason it can't be installed here, or "" when it can. Silence when
// the host is unreadable: a check that can't run doesn't get to refuse.
func (e Entry) Preflight(host hostcpu.Info) string {
	if e.Requires == nil {
		return ""
	}
	need := cpuLevel(e.Requires.CPU)
	if need == 0 || host.Arch != "amd64" || !host.Known || host.Level >= need {
		return ""
	}
	model := host.Model
	if model == "" {
		model = "unnamed"
	}
	why := fmt.Sprintf("%s's image is built for %s processors (%s) and this machine's doesn't provide those instructions — it reports as “%s”.",
		e.Title, e.Requires.CPU, hostcpu.Describe(need), model)
	if host.Virtual {
		// Almost always a hypervisor presenting its lowest-common-denominator
		// model; the physical host underneath is fine.
		return why + " That's a virtual machine's CPU type hiding what the real processor can do: in the " +
			"hypervisor (Proxmox: Hardware → Processors → Type) set it to “host”, reboot this VM, and install again."
	}
	return why + " That's the physical processor, so this app can't run on this machine."
}

// PreflightSpec applies Preflight to a spec that came from outside the
// store — the create API, where the request carries no entry id. Same
// image, same binaries: a spec on a catalog entry's image inherits its
// requirements.
func PreflightSpec(spec apps.AppSpec, host hostcpu.Info) string {
	if spec.Source.Type != "image" || spec.Source.Image == "" {
		return ""
	}
	if e := EntryForSpec(Entries(), "", &spec); e != nil {
		return e.Preflight(host)
	}
	return ""
}

// sameImage compares repositories, ignoring the tag: a pinned BookOrbit is
// still BookOrbit.
func sameImage(a, b string) bool {
	return dockerx.ImageName(a) == dockerx.ImageName(b)
}

// tzOption is shared by every entry; the UI prefills it from the browser.
var tzOption = Option{Key: "tz", Label: "Timezone", Target: "env:TZ", Default: "Etc/UTC"}

// contentFolder is a volume that lives in Drive under Content: a library
// of the household's own — Movies, Music, Books — that the app reads and
// writes but does not own. The operator browses it, drops files into it,
// and several apps share it: the arr stack and Jellyfin all read one
// Content tree, so hardlinks work across them. An empty library names
// Content itself, which is what one shared root for a whole stack is.
//
// What an app owns rather than reads needs no helper: a volume that names
// nowhere is given Apps/<App>/<volume> when the app is created.
func contentFolder(volume, mount, library string) apps.Volume {
	rel := apps.FilesContentDir
	if library != "" {
		rel += "/" + library
	}
	return apps.Volume{Name: volume, Mount: mount, Files: rel}
}

// contentFolderOwned is contentFolder for an image that runs as a
// particular uid and needs the folder to belong to it.
func contentFolderOwned(volume, mount, library, owner string) apps.Volume {
	v := contentFolder(volume, mount, library)
	v.Owner = owner
	return v
}

func mediaOption(hint string) Option {
	return Option{Key: "media", Label: "Media folder", Hint: hint, Target: "volume:media"}
}

func entry(id, title, desc, category, icon, image string, port int, health string, extra func(*apps.AppSpec)) Entry {
	spec := apps.AppSpec{
		Source:  apps.Source{Type: "image", Image: image},
		Env:     map[string]string{"TZ": "Etc/UTC"},
		Volumes: []apps.Volume{{Name: "config", Mount: "/config", Owner: "1000:1000"}},
		Routes:  []apps.Route{{Port: port}},
		Backup:  &apps.BackupSpec{Enabled: true},
	}
	if health != "" {
		spec.Health = &apps.Health{Path: health}
	}
	if extra != nil {
		extra(&spec)
	}
	return Entry{ID: id, Title: title, Description: desc, Category: category, Icon: icon, Spec: spec}
}

func lsioEntry(id, title, desc, category, icon, image string, port int, health string, withMedia string) Entry {
	e := entry(id, title, desc, category, icon, image, port, health, func(s *apps.AppSpec) {
		s.Env["PUID"] = "1000"
		s.Env["PGID"] = "1000"
	})
	e.Options = []Option{tzOption}
	if withMedia != "" {
		e.Spec.Volumes = append(e.Spec.Volumes, contentFolder("media", "/data", ""))
		e.Options = append(e.Options, mediaOption(withMedia))
	}
	return e
}

// DefaultEnabled is the store's out-of-the-box visible set.
var DefaultEnabled = []string{"fin", "hub", "office", "feed", "hook", "stream", "once", "hud", "lucid", "newtab", "signet", "fit", "immich", "paperless", "whisper", "gotenberg"}

// generatedSecret is a required secret with a safe auto-generated default —
// installs are fresh instances, so every key can simply be minted.
func generatedSecret(key, hint, kind string) Option {
	return Option{Key: key, Label: key, Hint: hint, Target: "secret:" + key, Required: true, Generate: kind}
}

// railsSecrets: the full set a credentials-free Rails app needs. Hub and
// Fin removed config/credentials.yml.enc — SECRET_KEY_BASE comes from the
// environment, so everything is generatable and installs need no input.
func railsSecrets() []Option {
	return []Option{
		generatedSecret("SECRET_KEY_BASE", "Signs cookies and ActiveStorage URLs; unique per instance.", "hex128"),
		generatedSecret("AR_ENCRYPTION_PRIMARY_KEY", "Encrypts sensitive database columns from day one.", "alnum32"),
		generatedSecret("AR_ENCRYPTION_DETERMINISTIC_KEY", "", "alnum32"),
		generatedSecret("AR_ENCRYPTION_KEY_DERIVATION_SALT", "", "alnum32"),
	}
}

// firstPartySource builds a household app from its repo's main branch,
// stamping the commit into the image as GIT_REV. The repository itself is
// this fog's to say (source.go): the entry names it in FirstParty, and
// Resolve fills the URL in.
func firstPartySource(dockerfile string) apps.Source {
	return apps.Source{
		Type: "git", Branch: "main", Dockerfile: dockerfile,
		BuildArgs: map[string]string{"GIT_REV": "{{commit}}"},
	}
}

// repoOption lets the operator point a household app at another clone. Its
// default is the resolved repository (Resolve).
func repoOption() Option {
	return Option{Key: "repo", Label: "Git repository", Target: "repo", Required: true}
}

// railsRelease is the household Rails apps' release command: restore a
// staged fog-backup/1 artifact if one is armed, then db:prepare.
var railsRelease = []string{"./bin/rails", "fog:release"}

// railsSecretKeys is the secret list of a spec whose options are
// railsSecrets() plus extra — derived, so the two can't drift apart.
func railsSecretKeys(extra ...string) []string {
	var keys []string
	for _, o := range railsSecrets() {
		keys = append(keys, strings.TrimPrefix(o.Target, "secret:"))
	}
	return append(keys, extra...)
}

// finEntry: Matthew's finances app — Rails 8 + React, built from git. It
// was Manor until 2026-11; a Manor install moves to it by restoring a
// backup into a fresh Fin, which takes Manor's artifacts as its own.
func finEntry() Entry {
	return Entry{
		ID: "fin", FirstParty: "fin", Title: "Fin", Category: "Personal cloud", Icon: "🏦",
		Description: "Personal finance hub — portfolios, budgets, net worth, and automations.",
		Spec: apps.AppSpec{
			Source: firstPartySource(""),
			Env: map[string]string{
				"GIT_SHA":              "{{commit}}",
				"JOB_CONCURRENCY":      "1",
				"FIN_WEBAUTHN_RP_ID":   "{{fqdn}}",
				"FIN_WEBAUTHN_ORIGINS": "https://{{fqdn}}",
			},
			Secrets:   railsSecretKeys(),
			Processes: []apps.Process{{Name: "worker", Command: []string{"bundle", "exec", "ruby", "bin/jobs"}}},
			Release:   railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases:  []string{"fin_production"},
				Extensions: []string{"citext", "pg_trgm", "pgcrypto"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"FIN_DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			// First managed volume doubles as fog-backup/1 restore staging.
			Volumes: []apps.Volume{{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"}},
			Routes:  []apps.Route{{Port: 80}},
			Health:  &apps.Health{Path: "/up"},
			Backup:  &apps.BackupSpec{Enabled: true, Mode: "delegated", Timeout: "45m"},
			Notes: "First visit: register your passkey (or restore a fog-backup/1 artifact from a previous " +
				"instance), then set FIN_OPEN_REGISTRATION=false in Config → Environment. " +
				"Passkeys are pinned to FIN_WEBAUTHN_RP_ID — pick the hostname once and keep it; a restore " +
				"under a different RP ID invalidates every passkey (preflight makes that explicit). " +
				"Add the deploy key and webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: append([]Option{
			repoOption(),
		}, railsSecrets()...),
	}
}

// hubEntry: Matthew's household hub — Rails 8 + React, built from git.
func hubEntry() Entry {
	return Entry{
		ID: "hub", FirstParty: "hub", Title: "Hub", Category: "Personal cloud", Icon: "🏡",
		Description: "Family operating system — people, chronicle, documents, todos, podcasts.",
		Spec: apps.AppSpec{
			Source: firstPartySource(""),
			Env: map[string]string{
				"GIT_REV":             "{{commit}}",
				"SOLID_QUEUE_IN_PUMA": "1", // without this, background jobs silently never run
				// Absolute links: invitation replies, and the link on each
				// notification Hub publishes to the event stream.
				"HUB_BASE_URL": "https://{{fqdn}}",
			},
			Secrets: railsSecretKeys(),
			Release: railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases:  []string{"hub_production", "hub_production_cache", "hub_production_queue"},
				Extensions: []string{"pg_trgm"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port",
					"DATABASE_USER": "user", "DATABASE_PASSWORD": "password",
				},
			}},
			Volumes: []apps.Volume{
				// First managed volume doubles as fog-backup/1 restore staging.
				{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"},
				{Name: "immich-cache", Mount: "/rails/immich_cache", Owner: "1000:1000"},
			},
			// Podcast audio needs Range passthrough — never buffer this route.
			Routes: []apps.Route{{Port: 80, NoBuffer: true}},
			Health: &apps.Health{Path: "/up"},
			Backup: &apps.BackupSpec{Enabled: true, Mode: "delegated", Timeout: "45m"},
			Notes: "First run: open the app and the setup wizard creates your household and admin account — " +
				"or restores a fog-backup/1 artifact from a previous instance. Blobs default to the managed " +
				"volume at /rails/storage; only change Settings → Storage if you know where you're pointing it.",
		},
		Options: append([]Option{
			repoOption(),
		}, railsSecrets()...),
		Companions: []Companion{
			{ID: "immich", Blurb: "Photo & video library — Hub pulls photos from Immich into its chronicle."},
			{ID: "paperless", Blurb: "Searchable document archive behind Hub's documents features."},
			{ID: "whisper", Blurb: "Speech-to-text server Hub uses to transcribe podcast audio."},
		},
	}
}

// officeEntry: Matthew's document editor — Rails 8 + React, built from git.
// Documents live in Postgres; the Files-backed volume is the bridge out
// (Save to Files, mirroring, library export) and in (open from Files).
func officeEntry() Entry {
	return Entry{
		ID: "office", FirstParty: "office", Title: "Office", Category: "Personal cloud", Icon: "📝",
		Description: "Document editor — a Docs/Word stand-in with .docx, Markdown and PDF support, collaboration and history.",
		Spec: apps.AppSpec{
			Source: firstPartySource(""),
			Env: map[string]string{
				"GIT_REV":             "{{commit}}",
				"SOLID_QUEUE_IN_PUMA": "1", // without this, background jobs silently never run
				// Optional: exact PDF export and legacy formats. Filled by
				// the Gotenberg companion's route; Office works without it.
				"GOTENBERG_URL": "http://gotenberg:3000",
				// The hostname a backup artifact records as its identity, so a
				// restore onto a differently-named instance warns instead of
				// passing silently (fog-backup/1 preflight).
				"OFFICE_HOST": "{{fqdn}}",
			},
			// The companion its env names, approved for it to reach (netwire.go).
			Reach:   []string{"gotenberg"},
			Secrets: railsSecretKeys(),
			Release: railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases:  []string{"office_production", "office_production_cache", "office_production_queue", "office_production_cable"},
				Extensions: []string{"pg_trgm"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port",
					"DATABASE_USER": "user", "DATABASE_PASSWORD": "password",
				},
			}},
			Volumes: []apps.Volume{
				// First managed volume doubles as fog-backup/1 restore staging.
				{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"},
				// The household's document tree in Drive: Office opens files
				// from here and saves exports back (fog-files/1).
				contentFolderOwned("files", "/rails/files", "Documents", "1000:1000"),
			},
			Routes: []apps.Route{{Port: 80}},
			Health: &apps.Health{Path: "/up"},
			Backup: &apps.BackupSpec{Enabled: true, Mode: "delegated", Timeout: "45m"},
			Notes: "First run: open the app and the setup wizard creates your household and admin account — " +
				"or restores a fog-backup/1 artifact from a previous instance. Install Gotenberg alongside " +
				"for exact PDF export and .doc/.odt/.rtf import; Office detects it through GOTENBERG_URL.",
		},
		Options: append([]Option{
			repoOption(),
		}, railsSecrets()...),
		Companions: []Companion{
			{ID: "gotenberg", Blurb: "Chromium + LibreOffice conversion API — exact PDF export and legacy document formats for Office."},
		},
	}
}

// feedEntry: Matthew's curation engine — Rails + React, built from git. The
// spec mirrors feed.fog.yaml in the repo (the Phase 1 shape: no ml/whisper
// sidecars, no streamer process yet).
func feedEntry() Entry {
	return Entry{
		ID: "feed", FirstParty: "feed", Title: "Feed", Category: "Media automation", Icon: "📡",
		Description: "Curation engine — follow-only ingestion into a permanent archive, ranked reading, multi-format egress.",
		Spec: apps.AppSpec{
			Source: firstPartySource(""),
			Env: map[string]string{
				"RAILS_ENV":         "production",
				"GIT_SHA":           "{{commit}}",
				"FEED_HOST":         "{{fqdn}}",
				"TIME_ZONE":         "America/Los_Angeles",
				"MEDIA_ROOT":        "/data/media",
				"NTFY_URL":          "http://ntfy",
				"NTFY_TOPIC":        "feed",
				"RAILS_MAX_THREADS": "8",
				"JOB_CONCURRENCY":   "1",
			},
			// The companion its env names, approved for it to reach (netwire.go).
			Reach:     []string{"ntfy"},
			Secrets:   railsSecretKeys(),
			Processes: []apps.Process{{Name: "worker", Command: []string{"./bin/jobs"}}},
			// Migrations run here and nowhere else — fog overlaps two web
			// replicas during a swap.
			Release: railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases: []string{"feed_production", "feed_production_cache", "feed_production_queue"},
				// vector needs fog's pgvector-capable postgres image; fog
				// creates extensions as superuser, so the app's migration
				// (CREATE EXTENSION IF NOT EXISTS) then no-ops cleanly.
				// pgcrypto: every table's UUID default needs it.
				Extensions: []string{"pg_trgm", "citext", "pgcrypto", "vector"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			Volumes: []apps.Volume{
				// Small managed state — included in fog's nightly backups.
				{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"},
				// TB-scale archive on a host path: skipped by fog backups by
				// design, and no owner so deploys don't chown a huge tree.
				{Name: "media", Mount: "/data/media", Host: "/mnt/media/feed"},
			},
			// noBuffer for Range requests (archived media) and SSE.
			Routes:         []apps.Route{{Port: 80, NoBuffer: true}},
			Health:         &apps.Health{Path: "/up"},
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			Backup:         &apps.BackupSpec{Enabled: true, Mode: "delegated", Timeout: "45m"},
			MemoryLimit:    "4g",
			Notes: "First visit: complete the setup wizard to create the household and admin account — " +
				"or restore a fog-backup/1 artifact from a previous instance. " +
				"Add the deploy key and webhook (Overview tab) to the Gitea repo for push-to-deploy. " +
				"The media archive lives on a host path and is deliberately outside the backup artifact — " +
				"back it up separately (restic/rclone); after a restore the media integrity sweep " +
				"re-fetches anything the archive tree is missing.",
		},
		Options: append([]Option{
			repoOption(),
			{Key: "media", Label: "Media archive", Hint: "TB-scale archive storage; excluded from fog backups.",
				Target: "volume:media", Default: "/mnt/media/feed"},
		}, railsSecrets()...),
		Companions: []Companion{
			{ID: "ntfy", Blurb: "Push notifications — Feed publishes ingest and health alerts to ntfy."},
		},
	}
}

// hookEntry: Matthew's ebook/audiobook acquisition engine — Rails + React,
// built from git. The spec mirrors hook.fog.yaml in the repo.
func hookEntry() Entry {
	return Entry{
		ID: "hook", FirstParty: "hook", Title: "Hook", Category: "Media automation", Icon: "🪝",
		Description: "Acquisition engine for ebooks and audiobooks — say what you want, Hook grabs the right edition and hands it to BookOrbit.",
		Spec: apps.AppSpec{
			Source: firstPartySource(""),
			Env: map[string]string{
				"RAILS_ENV":         "production",
				"GIT_SHA":           "{{commit}}",
				"HOOK_HOST":         "{{fqdn}}",
				"TIME_ZONE":         "America/Los_Angeles",
				"NTFY_URL":          "http://ntfy",
				"NTFY_TOPIC":        "hook",
				"RAILS_MAX_THREADS": "8",
				"JOB_CONCURRENCY":   "1",
			},
			// The companion its env names, approved for it to reach (netwire.go).
			Reach:     []string{"ntfy"},
			Secrets:   railsSecretKeys(),
			Processes: []apps.Process{{Name: "worker", Command: []string{"./bin/jobs"}}},
			// Migrations run here and nowhere else — fog overlaps two web
			// replicas during a swap.
			Release: railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases: []string{"hook_production", "hook_production_cache", "hook_production_queue"},
				// pgcrypto: every table's UUID default is gen_random_uuid(),
				// so a fresh instance can't even load its schema without it.
				Extensions: []string{"pg_trgm", "citext", "pgcrypto"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			Volumes: []apps.Volume{
				// Small managed state (NZB staging, review holds) — in fog's backups.
				{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"},
				// All of Drive's Content, once: SABnzbd's downloads, Stream's
				// and BookOrbit's libraries are folders inside it, so Hook
				// sees each of them and files a download into a library with
				// a rename rather than a copy. Fog hands Hook the path
				// translations to each app it uses (/app/v1/connections);
				// nothing here or in Hook's Settings names a path.
				contentFolder("content", "/data", ""),
			},
			Routes:         []apps.Route{{Port: 80}},
			Health:         &apps.Health{Path: "/up"},
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			// fog-backup/1: Hook produces one verified artifact via /fog/backup.
			// The media mount is another app's folder, covered by fog's Files
			// snapshots rather than this artifact.
			Backup:      &apps.BackupSpec{Enabled: true, Mode: "delegated", Timeout: "20m"},
			MemoryLimit: "1g",
			// The apps Hook works with (SABnzbd, Stream, BookOrbit) are the
			// wants in its repo's fog.yaml: each is connected on Hook's
			// Integrations tab, which hands Hook its address, key and path
			// translations and mounts the folders it shares.
			Notes: "First visit: complete the setup wizard to create the household and admin account — " +
				"or restore a fog-backup/1 artifact from a previous instance. Then Settings → Indexers " +
				"(your Newznab indexers, or point Hook at Prowlarr). SABnzbd, Stream and BookOrbit " +
				"installed on this server are connected from Hook's Integrations tab: Hook adds SABnzbd as a " +
				"download client and finds each library's folders on its own, with no paths to enter. " +
				"BookOrbit's login is still entered in Settings → Libraries. Add the deploy key and " +
				"webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: append([]Option{
			repoOption(),
			{Key: "content", Label: "Content folder", Hint: "Drive's Content — downloads and libraries. Hook reads finished downloads and files them into the libraries here.", Target: "volume:content"},
		}, railsSecrets()...),
		Companions: []Companion{
			{ID: "sabnzbd", Blurb: "Usenet downloader — Hook hands NZBs to it and reads the finished files back.", Default: true},
			{ID: "prowlarr", Blurb: "Indexer manager — Hook can pull its book-capable indexers straight from Prowlarr."},
			{ID: "bookorbit", Blurb: "Library and reading platform — where Hook delivers finished books."},
			{ID: "ntfy", Blurb: "Push notifications — Hook publishes grab and review alerts to ntfy."},
		},
	}
}

// streamEntry: Matthew's video player — Rails + React, built from git. The
// spec mirrors stream.fog.yaml in the repo.
func streamEntry() Entry {
	return Entry{
		ID: "stream", FirstParty: "stream", Title: "Stream", Category: "Media servers", Icon: "▶️",
		Description: "The household's video player — movies, shows, and everything else, played in the browser at original quality.",
		Spec: apps.AppSpec{
			Source: firstPartySource("Dockerfile"),
			Env: map[string]string{
				"RAILS_ENV":   "production",
				"GIT_SHA":     "{{commit}}",
				"STREAM_HOST": "{{fqdn}}",
				"TIME_ZONE":   "America/Los_Angeles",
				"NTFY_URL":    "http://ntfy",
				"NTFY_TOPIC":  "stream",
				// Two Puma workers of twelve threads: a segment request waits
				// on the transcoder, and API calls mustn't queue behind them.
				"WEB_CONCURRENCY":   "2",
				"RAILS_MAX_THREADS": "12",
				"JOB_CONCURRENCY":   "1",
				"STREAM_CACHE_DIR":  "/cache",
				// Thruster cuts responses at 30s by default; a direct-play
				// stream is hours long.
				"HTTP_WRITE_TIMEOUT": "0",
				// Lucid, inert until connecting it on the Integrations tab
				// sets LUCID_API_KEY too.
				"LUCID_URL": "http://lucid",
			},
			// STREAM_TOKEN_KEY signs media URLs — its own secret so rotating
			// it ends every playback session without signing anyone out.
			// The companion its env names, approved for it to reach (netwire.go).
			Reach:   []string{"ntfy"},
			Secrets: railsSecretKeys("STREAM_TOKEN_KEY"),
			Processes: []apps.Process{
				{Name: "worker", Command: []string{"./bin/jobs"}},
				// Owns live ffmpeg sessions; idles until Stream's Phase 2.
				{Name: "transcoder", Command: []string{"./bin/transcoder"}},
			},
			// Migrations run here and nowhere else — fog overlaps two web
			// replicas during a swap.
			Release: railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases: []string{"stream_production", "stream_production_cache", "stream_production_queue"},
				// pgcrypto: every table's UUID default needs it. vector:
				// "More like this" (pgvector). fog creates these as superuser;
				// the app's role can't, so its schema load fails without them.
				Extensions: []string{"pg_trgm", "citext", "pgcrypto", "unaccent", "vector"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			Volumes: []apps.Volume{
				// Artwork, trickplay sprites, extracted subtitles — in fog's backups.
				{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"},
				// Live HLS segments and scratch; regenerable.
				{Name: "cache", Mount: "/cache", Owner: "1000:1000"},
				// The household's libraries, which Stream only ever reads.
				// Which folder is which library is Settings › Libraries, not
				// a spec fact.
				contentFolder("movies", "/media/movies", "Movies"),
				contentFolder("tv", "/media/tv", "TV"),
				contentFolder("videos", "/media/videos", "Videos"),
				// Where Stream downloads the YouTube channels it follows — the
				// one media folder it writes (Settings › YouTube).
				contentFolder("youtube", "/media/youtube", "YouTube"),
			},
			Routes:         []apps.Route{{Port: 80}},
			Health:         &apps.Health{Path: "/up"},
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			// fog-backup/1: Stream produces one verified artifact via
			// /fog/backup. Media is covered by Drive's own snapshots.
			Backup:      &apps.BackupSpec{Enabled: true, BeforeUpdate: beforeUpdate(true), Mode: "delegated", Timeout: "30m"},
			MemoryLimit: "3g",
			// Hook delivers into the libraries and tells Stream about each
			// file; it must see the same folders, and Fog translates paths.
			Connection: &apps.Connection{Port: 80, Shares: []string{"movies", "tv", "videos", "youtube"}},
			Notes: "First visit: complete the setup wizard to create the household and admin account — " +
				"or restore a fog-backup/1 artifact from a previous instance. Then Settings › Libraries " +
				"(point a library at each folder under /media) and Settings › Integrations (a TMDB key for " +
				"metadata; optionally Sonarr/Radarr webhooks and a Plex server to migrate watch history from). " +
				"Stream only reads your media folders. Add the deploy key and webhook (Overview tab) to the " +
				"Gitea repo for push-to-deploy.",
		},
		Options: append([]Option{
			repoOption(),
			{Key: "movies", Label: "Movies folder", Hint: "Mounted read at /media/movies.", Target: "volume:movies"},
			{Key: "tv", Label: "TV folder", Hint: "Mounted read at /media/tv.", Target: "volume:tv"},
			{Key: "videos", Label: "Videos folder", Hint: "Everything that isn't a film or a series — YouTube archives, home videos. Mounted at /media/videos.", Target: "volume:videos"},
			{Key: "youtube", Label: "YouTube downloads folder", Hint: "Where Stream saves the YouTube channels it follows (it writes here). Mounted at /media/youtube.", Target: "volume:youtube"},
		}, append(railsSecrets(),
			generatedSecret("STREAM_TOKEN_KEY", "Signs media URLs; rotating it ends every playback session.", "hex64"))...),
		Companions: []Companion{
			{ID: "sonarr", Blurb: "TV series management — Stream picks up new episodes from Sonarr's webhooks."},
			{ID: "radarr", Blurb: "Movie management — Stream picks up new films from Radarr's webhooks."},
			{ID: "ntfy", Blurb: "Push notifications — Stream publishes library and health alerts to ntfy."},
		},
	}
}

// onceEntry: Matthew's One Second Everyday maker — Rails + React, built
// from git. The spec mirrors fog.yaml in the once repo.
func onceEntry() Entry {
	return Entry{
		ID: "once", FirstParty: "once", Title: "Once", Category: "Personal cloud", Icon: "🎞️",
		Description: "One second a day from Immich — pick the people and the dates, arrange the days, and get one film with music.",
		Spec: apps.AppSpec{
			Source: firstPartySource("Dockerfile"),
			Env: map[string]string{
				"RAILS_ENV":         "production",
				"GIT_SHA":           "{{commit}}",
				"APP_PUBLIC_URL":    "https://{{fqdn}}",
				"TIME_ZONE":         "America/Los_Angeles",
				"RAILS_MAX_THREADS": "8",
				"LUCID_URL":         "http://lucid",
				// One-second segments and previews in the cache volume;
				// finished films in a folder Once makes inside Videos.
				"ONCE_CACHE_DIR":       "/data/cache",
				"ONCE_FILMS_DIR":       "/data/videos/Once",
				"ONCE_SOUNDTRACKS_DIR": "/data/soundtracks",
				// A render cuts RENDER_PARALLEL seconds at once, each ffmpeg
				// on RENDER_THREADS threads.
				"RENDER_PARALLEL":   "2",
				"RENDER_THREADS":    "2",
				"ONCE_CACHE_MAX_GB": "20",
			},
			Secrets: railsSecretKeys(),
			// Immich hands Once its address and key (its Connection).
			Uses: []string{"immich"},
			Processes: []apps.Process{
				{Name: "worker", Command: []string{"./bin/jobs"}},
				// ffmpeg renders, one at a time, apart from syncs and downloads.
				{Name: "renderer", Command: []string{"./bin/jobs", "-c", "config/queue.render.yml", "--skip-recurring"}},
			},
			// Migrations run here and nowhere else — fog overlaps two web
			// replicas during a swap.
			Release: railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases: []string{"once_production", "once_production_cache", "once_production_queue"},
				// pgcrypto: every table's UUID default is gen_random_uuid().
				Extensions: []string{"pgcrypto", "citext"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			Volumes: []apps.Volume{
				// First managed volume doubles as fog-backup/1 restore staging.
				{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"},
				// Uploaded and fetched soundtracks — in Once's backups.
				{Name: "soundtracks", Mount: "/data/soundtracks", Owner: "1000:1000"},
				// Rendered seconds: rebuildable, size-capped, never backed up.
				{Name: "cache", Mount: "/data/cache", Owner: "1000:1000"},
				// The household's video library; finished films go into its
				// Once folder, covered by Drive's snapshots.
				contentFolder("videos", "/data/videos", "Videos"),
			},
			// Previews and proxied Immich playback are Range-served video.
			Routes:         []apps.Route{{Port: 80, NoBuffer: true}},
			Health:         &apps.Health{Path: "/up"},
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			// fog-backup/1: Once produces one verified artifact via
			// /fog/backup — the database, storage and soundtracks.
			Backup:      &apps.BackupSpec{Enabled: true, Mode: "delegated", Timeout: "20m"},
			MemoryLimit: "2g",
			Notes: "First visit: sign in with fog — the first person to do so is Once's admin — or, off fog, " +
				"complete the setup wizard or restore a fog-backup/1 artifact. Then connect Immich: make an API " +
				"key in Immich (person.read, asset.read, asset.view, asset.download; asset.upload and the album " +
				"permissions to save films back) and add it as Immich's IMMICH_API_KEY secret here — Once picks " +
				"it up within five minutes — or paste Immich's address and the key into Once's Settings › Immich. " +
				"Finished films land in Content/Videos/Once. Add the deploy key and webhook (Overview tab) to " +
				"the Gitea repo for push-to-deploy.",
		},
		Options: append([]Option{
			repoOption(),
			{Key: "videos", Label: "Videos folder", Hint: "Drive's video library. Once saves finished films in a Once folder inside it, mounted at /data/videos.", Target: "volume:videos"},
		}, railsSecrets()...),
		Companions: []Companion{
			{ID: "immich", Blurb: "Photo & video library — Once makes its films from the people and videos in Immich.", Default: true},
		},
	}
}

// hudEntry: Matthew's attention router — Go + React, built from git. The
// spec mirrors hud.fog.yaml in the repo.
func hudEntry() Entry {
	return Entry{
		ID: "hud", FirstParty: "hud", Title: "HUD", Category: "Personal cloud", Icon: "🔔",
		Description: "Brings calendars, webhooks, and agents to the edge of attention — a BusyBar on the desk, a phone, the room's lights — without interrupting work.",
		Spec: apps.AppSpec{
			Source: firstPartySource("Dockerfile"),
			Env:    map[string]string{"HUD_PUBLIC_URL": "https://{{fqdn}}"},
			// HUD_MASTER_KEY encrypts the credentials HUD stores (device keys,
			// OAuth tokens, CalDAV passwords). The Google client is optional
			// until a Google Calendar source is added.
			Secrets: []string{"HUD_MASTER_KEY", "GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET"},
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases: []string{"hud"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			Routes: []apps.Route{
				// hud.<domain>: the console and its live SSE stream, behind fog's login.
				{Port: 8080, Auth: true, NoBuffer: true},
				// hud-api.<domain>: CloudEvents ingest, MCP, ntfy action
				// callbacks. Not gated by fog; every request needs a HUD token.
				{Host: "hud-api", Port: 8081, NoBuffer: true},
			},
			Health: &apps.Health{Path: "/healthz"},
			// Safe with swap: a Postgres advisory lock lets only one instance
			// drive devices and run the engine.
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			// HUD routes the household's events: it reads the event stream.
			// The apps it watches (SABnzbd, Home Assistant) are wants in its
			// repo's fog.yaml, connected on its Integrations tab.
			Uses: []string{"events"},
			Notes: "The console is at this app's address behind Fog's login; machine endpoints (webhooks, MCP, ntfy " +
				"actions) are at hud-api.<domain> and need a token from Settings → API tokens. HUD_MASTER_KEY " +
				"encrypts every credential HUD stores — changing it makes them unreadable, so keep it with your " +
				"backups. For Google Calendar, set GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET in " +
				"Config → Secrets and redeploy; CalDAV and ICS feeds need neither. Apps run on bridge networking, " +
				"so add the BusyBar by IP (give it a DHCP reservation) or use Scan on the Surfaces page. Add the " +
				"deploy key and webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: []Option{
			repoOption(),
			generatedSecret("HUD_MASTER_KEY", "Encrypts stored credentials; changing it makes them unreadable.", "hex64"),
			{Key: "google_client_id", Label: "Google OAuth client ID", Hint: "Only for Google Calendar sources. Blank until you have a client.",
				Target: "secret:GOOGLE_OAUTH_CLIENT_ID", Advanced: true},
			{Key: "google_client_secret", Label: "Google OAuth client secret", Target: "secret:GOOGLE_OAUTH_CLIENT_SECRET", Advanced: true},
		},
		Companions: []Companion{
			{ID: "ntfy", Blurb: "Push notifications — HUD's phone surface, with action buttons that call back into HUD."},
			{ID: "homeassistant", Blurb: "Home automation — HUD drives the room's lights and reacts to sensors changing."},
			{ID: "sabnzbd", Blurb: "Usenet downloader — HUD hears when a download finishes or fails."},
		},
	}
}

// lucidEntry: Matthew's observability app — Rails 8 + React, built from
// git. The spec mirrors lucid.fog.yaml in the repo.
func lucidEntry() Entry {
	return Entry{
		ID: "lucid", FirstParty: "lucid", Title: "Lucid", Category: "Utilities", Icon: "🔭",
		Description: "Observability for every app on this server — metrics, traces, logs, profiles, and visitor analytics behind one query bar.",
		Spec: apps.AppSpec{
			Source: firstPartySource("Dockerfile"),
			Env: map[string]string{
				"RAILS_ENV":         "production",
				"GIT_SHA":           "{{commit}}",
				"LUCID_HOST":        "{{fqdn}}",
				"TIME_ZONE":         "America/Los_Angeles",
				"RAILS_MAX_THREADS": "8",
				"JOB_CONCURRENCY":   "1",
				// Lucid observes itself. The key is minted in its own
				// Settings › Apps once it is up, or by connecting Lucid on
				// its own Integrations tab; until then the agent is inert.
				"LUCID_URL": "http://lucid",
			},
			Secrets: railsSecretKeys("LUCID_API_KEY"),
			// Its integration offers (telemetry, read access) are declared in
			// the repo's fog.yaml, which fog reads at every deploy — not here,
			// where they would reach only a fresh install.
			Processes: []apps.Process{{Name: "worker", Command: []string{"./bin/jobs"}}},
			// db:prepare also creates the day partitions ahead. Migrations
			// run here and nowhere else — fog overlaps two web replicas
			// during a swap.
			Release: railsRelease,
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				// lucid_production is configuration: small and in the backup
				// artifact. _telemetry is every signal, partitioned by day:
				// large, disposable, and left out of it.
				Databases:  []string{"lucid_production", "lucid_production_telemetry", "lucid_production_cache", "lucid_production_queue"},
				Extensions: []string{"pg_trgm", "citext", "pgcrypto"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			// Backup staging — in fog's backups.
			Volumes:        []apps.Volume{{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"}},
			Routes:         []apps.Route{{Port: 80}},
			Health:         &apps.Health{Path: "/up"},
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			// fog-backup/1: Lucid produces one verified artifact via
			// /fog/backup of its configuration database only. Telemetry is
			// history and is not in it.
			Backup: &apps.BackupSpec{Enabled: true, BeforeUpdate: beforeUpdate(true), Mode: "delegated", Timeout: "20m"},
			// Per container: at 768m web and worker both sat at the ceiling
			// and swapped.
			MemoryLimit: "1536m",
			Notes: "First visit: complete the setup wizard to create the household and admin account — " +
				"or restore a fog-backup/1 artifact from a previous instance. Then open each app to watch, and on its " +
				"Integrations tab connect Lucid: fog has Lucid mint that app its own ingest key and a read-only token, sets " +
				"LUCID_URL, LUCID_API_KEY and LUCID_READ_TOKEN, and redeploys it (a Rails app also needs the lucid gem). Connect it on " +
				"Lucid's own Integrations tab to have Lucid report on itself. Telemetry is kept within the disk budget " +
				"in Settings › Retention and is not backed up. Add the deploy key and webhook (Overview tab) to " +
				"the Gitea repo for push-to-deploy.",
		},
		Options: append([]Option{
			repoOption(),
			{Key: "lucid_api_key", Label: "Lucid's own ingest key", Hint: "So Lucid reports on itself. Blank until you connect Lucid on its own Integrations tab.",
				Target: "secret:LUCID_API_KEY", Advanced: true},
		}, railsSecrets()...),
		Companions: []Companion{
			{ID: "ntfy", Blurb: "Push notifications — Lucid sends its alerts to an ntfy topic (Settings › Notifications)."},
		},
	}
}

// newtabEntry: Matthew's start page — Go, built from git. The spec mirrors
// newtab.fog.yaml in the repo. It reads the app directory through
// FOG_API_URL/FOG_API_TOKEN, which every app gets, so it needs no setup.
func newtabEntry() Entry {
	return Entry{
		ID: "newtab", FirstParty: "newtab", Title: "Newtab", Category: "Utilities", Icon: "🧭",
		Description: "The household's start page — every app one keystroke away, with live content from them, painted before the tab finishes opening.",
		Spec: apps.AppSpec{
			Source: firstPartySource("Dockerfile"),
			Env: map[string]string{
				"NEWTAB_HOST": "{{fqdn}}",
			},
			// What it wants of Lucid (telemetry, and read access under the
			// names its widgets read) is in the repo's fog.yaml, read at
			// every deploy.
			Volumes: []apps.Volume{
				// The snapshot mirror and image cache: a redeploy serves a warm page.
				{Name: "state", Mount: "/state", Owner: "1000:1000"},
				// newtab.yaml and icons/, at Apps/Newtab/config in Files.
				{Name: "config", Mount: "/config", Owner: "1000:1000"},
			},
			// The page holds one event stream open for live updates. Fog's
			// gate decides who may open it (the Sign-in tab) and tells newtab
			// who they are with a signed Fog-Assertion.
			Routes:         []apps.Route{{Port: 8080, NoBuffer: true, Auth: true}},
			Health:         &apps.Health{Path: "/up"},
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			// fog-backup/1: newtab produces one verified artifact via
			// /fog/backup of both volumes, less state's caches.
			Backup:      &apps.BackupSpec{Enabled: true, BeforeUpdate: beforeUpdate(true), Mode: "delegated"},
			MemoryLimit: "128m",
			Notes: "Needs no setup: open it and every app fog runs is already there. Bookmarks, pinned apps and " +
				"the palette are in newtab.yaml at Apps/Newtab/config in Files, or use the page's own editor. " +
				"With Lucid installed, connect it on the Integrations tab: one connection sends page timings to Lucid and feeds its widgets. " +
				"Add the deploy key and webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: []Option{repoOption()},
	}
}

// signetEntry: Matthew's capture-and-recall hub — Rust + Svelte, built
// from git. The spec mirrors fog.yaml in the repo. SIGNET_TOKEN is only the
// bootstrap admin credential; every client mints its own scoped token.
func signetEntry() Entry {
	return Entry{
		ID: "signet", FirstParty: "signet", Title: "Signet", Category: "Personal cloud", Icon: "🔖",
		Description: "Save anything from any device in one gesture, with why you saved it — and get it back when it matters, including inside your AI agent sessions.",
		Spec: apps.AppSpec{
			Source:  firstPartySource("Dockerfile"),
			Env:     map[string]string{"SIGNET_PUBLIC_URL": "https://{{fqdn}}"},
			Secrets: []string{"SIGNET_TOKEN"},
			Volumes: []apps.Volume{
				// signet.db, blobs/ (snapshots) and vault/, the plain-file
				// mirror of every item.
				{Name: "data", Mount: "/data", Owner: "1000:1000"},
			},
			// The hub checks its own bearer tokens; MCP streams responses.
			Routes: []apps.Route{{Port: 7717, NoBuffer: true}},
			Health: &apps.Health{Path: "/up"},
			// One SQLite writer: restart, don't overlap two.
			DeployStrategy: "restart",
			UpdatePolicy:   "auto",
			Backup:         &apps.BackupSpec{Enabled: true},
			MemoryLimit:    "768m",
			Notes: "SIGNET_TOKEN (Config → Secrets) is the bootstrap admin token — use it once to mint a scoped " +
				"token per client (`signet token create laptop --scopes read,capture,write`), and don't hand it " +
				"to clients. The browser extension, Mac and iOS apps and the Claude Code plugin all take this " +
				"app's address and a token. Embeddings auto-detect an Ollama; set SIGNET_EMBED_PROVIDER and " +
				"SIGNET_EMBED_URL in Config → Environment to point elsewhere, and ANTHROPIC_API_KEY as a secret " +
				"for summaries. Add the deploy key and webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: []Option{
			repoOption(),
			generatedSecret("SIGNET_TOKEN", "The bootstrap admin token; mint per-client tokens with it.", "hex64"),
		},
	}
}

// beforeUpdate overrides the fleet-wide backup_before_update setting for one
// app.
func beforeUpdate(on bool) *bool { return &on }

// fitEntry: Matthew's household health record — Rails 8 + React, built from
// git. The spec mirrors fit.fog.yaml in the repo. network and lanOnly are the
// boundary, not tuning: they are what keep Fit's data in the house.
func fitEntry() Entry {
	return Entry{
		ID: "fit", FirstParty: "fit", Title: "Fit", Category: "Personal cloud", Icon: "🩺",
		Description: "A household's health record — Whoop, Withings, Apple Health, medications and supplements, on a sealed network that never leaves the house.",
		Spec: apps.AppSpec{
			Source: firstPartySource("Dockerfile"),
			Env: map[string]string{
				"RAILS_ENV":         "production",
				"GIT_SHA":           "{{commit}}",
				"FIT_HOST":          "{{fqdn}}",
				"TIME_ZONE":         "America/Los_Angeles",
				"RAILS_MAX_THREADS": "8",
				"JOB_CONCURRENCY":   "1",
				"NTFY_URL":          "http://ntfy",
				"NTFY_TOPIC":        "fit",
				"LUCID_URL":         "http://lucid",
			},
			// The companion its env names, approved for it to reach (netwire.go).
			Reach:   []string{"ntfy"},
			Secrets: railsSecretKeys(),
			// No route out of the machine. Vendor data comes through
			// fit-collector, which Fit pulls from.
			Network: apps.NetworkSealed,
			Wants:   []apps.Want{{Offer: "fit-collector/pull"}},
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases:  []string{"fit_production", "fit_production_cache", "fit_production_queue"},
				Extensions: []string{"pg_trgm", "citext", "pgcrypto"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			// Migrations run here and nowhere else — fog overlaps two web
			// replicas during a swap.
			Release:   railsRelease,
			Processes: []apps.Process{{Name: "worker", Command: []string{"./bin/jobs"}}},
			// First managed volume doubles as fog-backup/1 restore staging.
			Volumes:        []apps.Volume{{Name: "storage", Mount: "/rails/storage", Owner: "1000:1000"}},
			Routes:         []apps.Route{{Port: 80, LanOnly: true}},
			Health:         &apps.Health{Path: "/up"},
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			Backup:         &apps.BackupSpec{Enabled: true, BeforeUpdate: beforeUpdate(true), Mode: "delegated", Timeout: "30m"},
			MemoryLimit:    "1g",
			Notes: "Don't add real data until the seal is proven: run the steps in fog's docs/sealed-network.md, " +
				"\"Verifying on a real server\", then check that Fit's Sources page says Sealed. Set sealedPeer on Lucid " +
				"and ntfy (Config) if you want Fit to reach them; without it their addresses don't resolve from the sealed " +
				"network. Sign in with your passkey: the first person in is Fit's admin. Connect fit-collector and Lucid on " +
				"the Integrations tab, and pin model names in Settings › Household models to turn on briefs and lab reading. " +
				"Add the deploy key and webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: append([]Option{
			repoOption(),
		}, railsSecrets()...),
		Companions: []Companion{
			{ID: "fit-collector", Blurb: "Fetches Whoop and Withings data outside the seal — Fit pulls from it, and it can't read Fit.", Default: true},
		},
	}
}

// thinkFleetEntry: the household hub for think — Go, built from the think
// repo, whose fog.yaml carries its device sign-in. Macs keep their own
// records and models; the hub is where those records meet and where model
// files are kept once for the house. Both routes are LAN-only: the console
// behind fog's gate, the API for devices on a host of its own, each request
// with a device token.
func thinkFleetEntry() Entry {
	return Entry{
		ID: "think-fleet", FirstParty: "think", Title: "think fleet", Category: "Personal cloud", Icon: "🧠",
		Description: "One record of every model each Mac in the house has tried, run and benchmarked, a library of model files downloaded once, and the house's local AI on one endpoint.",
		Spec: apps.AppSpec{
			Source: firstPartySource("cmd/think-fleet/Dockerfile"),
			Env: map[string]string{
				"THINK_FLEET_FILES": "/library",
				"THINK_FLEET_API":   ":8081",
				"THINK_FLEET_QUIET": "01:00-06:00",
				"TZ":                "America/Los_Angeles",
			},
			Secrets: []string{"THINK_FLEET_KEY"},
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases: []string{"think_fleet"},
				Inject: map[string]string{
					"DATABASE_HOST": "host", "DATABASE_PORT": "port", "DATABASE_USER": "user",
					"DATABASE_PASSWORD": "password", "DATABASE_NAME": "dbname",
				},
			}},
			// Model files are tens of GiB each and every one can be fetched
			// again by its hash, so the library lives in Content, where a NAS
			// can hold it, and stays out of the app's backups.
			Volumes: []apps.Volume{{Name: "library", Mount: "/library", Files: "Content/AI models", Owner: "1000:1000"}},
			Routes: []apps.Route{
				// think-fleet.<domain>: the console, behind fog's login.
				{Port: 8080, Auth: true, LanOnly: true},
				// think-fleet-api.<domain>: what each Mac calls, with its own token.
				{Host: "think-fleet-api", Port: 8081, LanOnly: true, NoBuffer: true},
			},
			Health: &apps.Health{Path: "/healthz"},
			// Safe with swap: migrations and the hub's own work take Postgres
			// advisory locks, so two replicas never both do either.
			DeployStrategy: "swap",
			UpdatePolicy:   "auto",
			Backup:         &apps.BackupSpec{Enabled: true},
			// No uses: publishing notices and serving its start-page tile
			// need no grant, and it reads nothing of fog's.
			MemoryLimit: "512m",
			Notes: "On each Mac with think, run `think fleet join https://think-fleet-api.<domain>` and approve the code " +
				"with your passkey. The library keeps model files in Content/AI models: move Content to a NAS " +
				"(Settings › Storage › Drive) for room, or set THINK_FLEET_BUDGET_GB to cap it. Add the deploy key and " +
				"webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: []Option{
			repoOption(),
			generatedSecret("THINK_FLEET_KEY", "Signs the tickets Macs copy files with; changing it makes Macs join again.", "hex64"),
		},
	}
}

// fitCollectorEntry: the one Fit process with internet access, from the same
// repo. The spec mirrors fit-collector.fog.yaml, less the vendor credentials:
// those can only be registered once this app has an address for their
// redirect URIs, so they are added from Config → Secrets. Declaring them here
// would fail every deploy until then.
func fitCollectorEntry() Entry {
	return Entry{
		ID: "fit-collector", FirstParty: "fit", Title: "Fit Collector", Category: "Personal cloud", Icon: "📥",
		Description: "Fetches Whoop and Withings data for Fit — it holds the vendor tokens, and has no way to read Fit.",
		Spec: apps.AppSpec{
			Source: firstPartySource("Dockerfile"),
			Env: map[string]string{
				"RACK_ENV":       "production",
				"COLLECTOR_DATA": "/data",
				"COLLECTOR_HOST": "{{fqdn}}",
			},
			Secrets: []string{"COLLECTOR_KEY"},
			Command: []string{"./bin/collector"},
			// Fit, on the sealed network, reaches this by name. It keeps its way out.
			SealedPeer: true,
			// In the spec, because the repo's fog.yaml is also Fit's, which
			// offers nothing. The token fog mints here is only ever for Fit.
			Connection: &apps.Connection{Port: 80, Offers: []apps.Offer{{
				ID:      "pull",
				Title:   "Vendor data",
				Blurb:   "Fit can collect what this has fetched from Whoop and Withings. Nothing else can.",
				Env:     map[string]string{"FIT_COLLECTOR_URL": "{{url}}"},
				Secrets: []string{"FIT_COLLECTOR_TOKEN"},
			}}},
			Volumes:      []apps.Volume{{Name: "data", Mount: "/data", Owner: "1000:1000"}},
			Routes:       []apps.Route{{Port: 80, LanOnly: true}},
			Health:       &apps.Health{Path: "/up"},
			UpdatePolicy: "auto",
			Backup:       &apps.BackupSpec{Enabled: true, BeforeUpdate: beforeUpdate(false)},
			MemoryLimit:  "256m",
			Notes: "Register apps with Whoop and Withings using the redirect URIs https://<this app's address>/oauth/whoop/callback " +
				"and /oauth/withings/callback, checking first that each vendor accepts a hostname on a LAN address. Then add " +
				"WHOOP_CLIENT_ID, WHOOP_CLIENT_SECRET, WITHINGS_CLIENT_ID and WITHINGS_CLIENT_SECRET in Config → Secrets and " +
				"redeploy. Keep this app named fit-collector: Fit asks for it by name. COLLECTOR_KEY encrypts the vendor tokens " +
				"stored here. Add the deploy key and webhook (Overview tab) to the Gitea repo for push-to-deploy.",
		},
		Options: []Option{
			repoOption(),
			generatedSecret("COLLECTOR_KEY", "Encrypts the vendor tokens stored here; changing it makes them unreadable.", "hex64"),
		},
	}
}

// immichEntry: photo library. Ships with its own sidecars — Immich needs
// its own Postgres build (VectorChord on a major it pins itself; fog's
// pgvector cluster is not what it wants) plus Valkey, so the managed
// datastore is not an option here.
func immichEntry() Entry {
	return Entry{
		ID: "immich", Title: "Immich", Category: "Personal cloud", Icon: "📸",
		Description: "Photo & video library — phone backup, faces, and smart search.",
		Spec: apps.AppSpec{
			Source: apps.Source{Type: "image", Image: "ghcr.io/immich-app/immich-server:release"},
			Env: map[string]string{
				"TZ":                          "Etc/UTC",
				"DB_HOSTNAME":                 "database",
				"DB_USERNAME":                 "postgres",
				"DB_DATABASE_NAME":            "immich",
				"REDIS_HOSTNAME":              "redis",
				"IMMICH_MACHINE_LEARNING_URL": "http://machine-learning:3003",
			},
			Secrets: []string{"DB_PASSWORD"},
			Aux: []apps.AuxService{
				{
					Name: "database", Image: "ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0",
					Env: map[string]string{
						"POSTGRES_PASSWORD":    "${DB_PASSWORD}",
						"POSTGRES_USER":        "postgres",
						"POSTGRES_DB":          "immich",
						"POSTGRES_INITDB_ARGS": "--data-checksums",
					},
					Volumes: []apps.Volume{{Name: "pgdata", Mount: "/var/lib/postgresql/data"}},
					ShmSize: "128mb",
				},
				{Name: "redis", Image: "docker.io/valkey/valkey:9"},
				{
					Name: "machine-learning", Image: "ghcr.io/immich-app/immich-machine-learning:release",
					Volumes: []apps.Volume{{Name: "model-cache", Mount: "/cache"}},
				},
			},
			Volumes: []apps.Volume{{Name: "upload", Mount: "/data"}},
			// Video playback needs Range passthrough.
			Routes: []apps.Route{{Port: 2283, NoBuffer: true}},
			Health: &apps.Health{Path: "/api/server/ping"},
			// Immich migrates its database on startup — never overlap versions.
			DeployStrategy: "restart",
			Backup:         &apps.BackupSpec{Enabled: true},
			// Apps that use Immich (Once, Hub) reach it here. Immich mints its
			// own API keys, so the credential is pasted into the vault after
			// setup — deliberately not in Secrets, which would fail every
			// deploy until then.
			Connection: &apps.Connection{Port: 2283, Secret: "IMMICH_API_KEY"},
			Notes: "Your first visit creates the admin account. Phone apps connect to this app's URL. " +
				"The library lives in Drive under Apps → Immich, and snapshots include all of it — if it " +
				"outgrows nightly backups, move the upload volume out of the app's own folder (its Drive " +
				"tab → Move) and back that up separately. " +
				"To let apps that use Immich (Once, Hub) in: create an API key in Immich (Account Settings → API " +
				"Keys) and add it as the IMMICH_API_KEY secret in Config → Secrets; an app that declares uses: " +
				"[immich] picks it up from fog. An app without that (or off fog) takes http://<this app's name>:2283 " +
				"and the key in its own settings.",
		},
		Options: []Option{
			tzOption,
			generatedSecret("DB_PASSWORD", "Immich's private Postgres password.", "alnum32"),
		},
	}
}

// paperlessEntry: document archive. SQLite for the database (fine at
// household scale), Valkey sidecar as the required task broker.
// noPAR is a signin manifest's "this client sends its request on the front
// channel": for third-party OIDC libraries that cannot push one.
var noPAR = false

func paperlessEntry() Entry {
	return Entry{
		ID: "paperless", Title: "Paperless-ngx", Category: "Personal cloud", Icon: "📄",
		Description: "Document archive — scan, OCR, tag, and find anything again.",
		Spec: apps.AppSpec{
			Source: apps.Source{Type: "image", Image: "ghcr.io/paperless-ngx/paperless-ngx:latest"},
			Env: map[string]string{
				"PAPERLESS_REDIS":        "redis://broker:6379",
				"PAPERLESS_URL":          "https://{{fqdn}}",
				"PAPERLESS_TIME_ZONE":    "Etc/UTC",
				"PAPERLESS_OCR_LANGUAGE": "eng",
				"PAPERLESS_ADMIN_USER":   "admin",
				// Sign in with fog (docs/signin-proposal.md §13): django-
				// allauth's OpenID Connect provider, configured entirely
				// from the environment. The {{oidc.*}} values are filled in
				// at deploy, once fog has registered Paperless as a client;
				// without a domain they are left out and Paperless keeps
				// its own accounts.
				"PAPERLESS_APPS":               "allauth.socialaccount.providers.openid_connect",
				"PAPERLESS_SOCIAL_AUTO_SIGNUP": "true",
				"PAPERLESS_SOCIALACCOUNT_PROVIDERS": `{"openid_connect":{"OAUTH_PKCE_ENABLED":true,"APPS":[{"provider_id":"fog","name":"fog",` +
					`"client_id":"{{oidc.client_id}}","secret":"{{oidc.client_secret}}",` +
					`"settings":{"server_url":"{{oidc.issuer}}","token_auth_method":"client_secret_basic"}}]}}`,
			},
			Secrets: []string{"PAPERLESS_SECRET_KEY"},
			Aux: []apps.AuxService{
				{
					Name: "broker", Image: "docker.io/valkey/valkey:9-alpine",
					Volumes: []apps.Volume{{Name: "redisdata", Mount: "/data"}},
				},
			},
			Volumes: []apps.Volume{
				{Name: "data", Mount: "/usr/src/paperless/data", Owner: "1000:1000"},
				{Name: "media", Mount: "/usr/src/paperless/media", Owner: "1000:1000"},
				{Name: "export", Mount: "/usr/src/paperless/export", Owner: "1000:1000"},
				// The drop box: a scan placed here — from the Drive page, a
				// phone, or a scanner writing to the folder — is ingested
				// and removed. Drive's Content/Scans, so it needs no setup.
				contentFolder("consume", "/usr/src/paperless/consume", "Scans"),
			},
			Routes: []apps.Route{{Port: 8000}},
			Health: &apps.Health{Path: "/"},
			// allauth cannot push its authorization request, and fog's
			// people may have an email to offer it.
			Signin: &signin.Manifest{Contract: signin.Contract, Redirect: "/accounts/oidc/fog/login/callback/",
				PAR: &noPAR, Email: true},
			// Paperless migrates its database on startup — never overlap versions.
			DeployStrategy: "restart",
			Backup:         &apps.BackupSpec{Enabled: true},
			Notes: "Sign in with fog once fog has a domain: the household's people get Paperless accounts on " +
				"first sign-in (choose who on the Sign-in tab). " +
				"First login: admin / your install password (default changeme — change it right away in " +
				"the top-right menu → My Profile). Anything dropped into Drive → Content → Scans is ingested " +
				"automatically and removed once filed — upload from the Drive page, save from a phone " +
				"over WebDAV, or point a scanner at that folder. " +
				"To connect Hub: create an auth token in Paperless (Django admin → Tokens) and point Hub's " +
				"documents settings at http://<this app's name>:8000.",
		},
		Options: []Option{
			{Key: "consume", Label: "Scans folder", Hint: "Drop documents here and Paperless files them.", Target: "volume:consume"},
			{Key: "tz", Label: "Timezone", Target: "env:PAPERLESS_TIME_ZONE", Default: "Etc/UTC"},
			{Key: "admin_password", Label: "Admin password", Hint: "First-login password for the admin account; change it after signing in.",
				Target: "env:PAPERLESS_ADMIN_PASSWORD", Default: "changeme"},
			{Key: "ocr_language", Label: "OCR language", Hint: "Tesseract code: eng, deu, fra, spa…",
				Target: "env:PAPERLESS_OCR_LANGUAGE", Default: "eng"},
			generatedSecret("PAPERLESS_SECRET_KEY", "Django session-signing key; unique per instance.", "hex128"),
		},
	}
}

// whisperEntry: speech-to-text API (CPU image). No backup — the only state
// is a re-downloadable model cache.
func whisperEntry() Entry {
	return Entry{
		ID: "whisper", Title: "Whisper", Category: "Personal cloud", Icon: "🎙️",
		Description: "Speech-to-text server — a transcription API for Hub and anything else.",
		Spec: apps.AppSpec{
			Source: apps.Source{Type: "image", Image: "onerahmet/openai-whisper-asr-webservice:latest"},
			Env: map[string]string{
				"ASR_ENGINE": "faster_whisper",
				"ASR_MODEL":  "base",
			},
			Volumes: []apps.Volume{{Name: "cache", Mount: "/root/.cache"}},
			Routes:  []apps.Route{{Port: 9000}},
			Health:  &apps.Health{Path: "/docs"},
			Notes: "The model downloads on first start — the first transcription takes a while. " +
				"API docs live at /docs. This is the CPU image; bigger models are more accurate but slower. " +
				"Point Hub's transcription settings at http://<this app's name>:9000.",
		},
		Options: []Option{
			{Key: "model", Label: "Whisper model", Hint: "tiny, base, small, medium, or large-v3 — bigger is more accurate but slower on CPU.",
				Target: "env:ASR_MODEL", Default: "base"},
		},
	}
}

// openclawBootstrap prepares the state volume so the gateway can start.
//
// Three things the image will not do for itself:
//
//  1. A gateway with no openclaw.json refuses to start ("Missing config. Run
//     `openclaw setup`…") and the container crash-loops. Upstream's compose
//     flow onboards before the first `up`; this is that step, run once, from
//     the same image and volumes. Which brain it onboards with depends on
//     what the operator gave at install: a model server on their own network,
//     an Anthropic key, or neither (the gateway still boots, and the Control
//     UI can add a provider later).
//  2. A fresh workspace ships BOOTSTRAP.md, which makes the agent open its
//     first conversation by interviewing you about who it should be. When the
//     install pane already asked those questions, the answers are written
//     into the workspace and the ritual is retired.
//  3. On a non-loopback bind the gateway only seeds localhost Control UI
//     origins, so a browser arriving through Fog's proxy is turned away with
//     "origin not allowed". The app's own hostname has to be allowlisted —
//     over HTTPS, since the Control UI needs a secure context to hold a
//     device identity at all.
//
// Idempotent by design — it is the release slot, so it reruns on every deploy
// and must leave a configured install (its identity, its provider, and any
// origins the operator added) alone.
const openclawBootstrap = `set -e
STATE="${OPENCLAW_STATE_DIR:-$HOME/.openclaw}"
CLI="/app/dist/index.js"

# Locate the model server behind FOG_LOCAL_LLM_URL and choose a model on it.
# Prints "<kind>	<baseUrl>	<modelId>"; explains itself and fails the deploy
# when nothing usable answers, rather than installing a mute assistant.
cat > /tmp/fog-probe.mjs <<'PROBE'
const raw = (process.env.FOG_LOCAL_LLM_URL || "").trim();
const want = (process.env.FOG_LOCAL_LLM_MODEL || "").trim();
const url = /^[a-z][a-z0-9+.-]*:\/\//i.test(raw) ? raw : "http://" + raw;
let u;
try { u = new URL(url) } catch { u = null }
if (!u || (u.protocol !== "http:" && u.protocol !== "https:")) {
  console.error("openclaw: \"" + raw + "\" is not a usable address — give an IP, host, or URL such as 192.0.2.10 or http://gpu-box.lan:11434");
  process.exit(1);
}
const path = u.pathname.replace(/\/+$/, "");
// An explicit port or path means the operator knows where the server is; a
// bare host gets the default ports of the servers people actually run.
const bases = path || u.port
  ? [u.protocol + "//" + u.host + path]
  : [11434, 1234, 8000, 8080].map((p) => u.protocol + "//" + u.hostname + ":" + p);

async function json(target) {
  const res = await fetch(target, { signal: AbortSignal.timeout(3000) });
  if (!res.ok) throw new Error("HTTP " + res.status);
  return await res.json();
}

// Model servers also list embedders, rerankers, and speech models; none of
// them can hold a conversation, so they never win the automatic pick.
const CHATTY = (id) => !/embed|rerank|whisper|clip|tts|stt|moderation|guard/i.test(id);

// Ollama's native API. Tool support and context window come from /api/tags on
// current Ollama; older daemons omit them and every model stays a candidate.
async function ollama(base) {
  const body = await json(base + "/api/tags");
  const models = Array.isArray(body?.models) ? body.models : [];
  if (models.length === 0) throw new Error("no models");
  return models.map((m) => ({
    id: m.name,
    tools: Array.isArray(m.capabilities) ? m.capabilities.includes("tools") : null,
    ctx: Number(m?.details?.context_length) || 0,
  }));
}

// LM Studio, vLLM, llama.cpp, LiteLLM — anything with an OpenAI-style /v1.
async function openai(base) {
  const root = base.endsWith("/v1") ? base : base + "/v1";
  const body = await json(root + "/models");
  const models = Array.isArray(body?.data) ? body.data : [];
  if (models.length === 0) throw new Error("no models");
  return { root, list: models.map((m) => ({ id: String(m.id), tools: null, ctx: 0 })) };
}

function pick(models, kind) {
  if (want) {
    const hit = models.find((m) => m.id === want)
      || models.find((m) => m.id === want + ":latest")
      || models.find((m) => m.id.replace(/:latest$/, "") === want);
    if (!hit) {
      console.error("openclaw: the server has no model named \"" + want + "\". It offers: " + models.map((m) => m.id).join(", "));
      process.exit(1);
    }
    return hit.id;
  }
  // Agents need tool calling; among models that have it, the roomiest context
  // wins, and the name breaks ties so redeploys keep choosing the same model.
  const chat = models.filter((m) => CHATTY(m.id));
  const usable = chat.some((m) => m.tools) ? chat.filter((m) => m.tools) : chat;
  const best = [...usable].sort((a, b) => b.ctx - a.ctx || a.id.localeCompare(b.id))[0];
  if (!best) {
    console.error("openclaw: " + kind + " answered but offers no chat model — pull or load one first.");
    process.exit(1);
  }
  const others = usable.filter((m) => m.id !== best.id).map((m) => m.id);
  if (others.length > 0) console.error("openclaw: also available: " + others.join(", "));
  return best.id;
}

const tried = [];
for (const base of bases) {
  let why = "no answer";
  if (!base.endsWith("/v1")) {
    try {
      const models = await ollama(base);
      console.log(["ollama", base, pick(models, "Ollama")].join("\t"));
      process.exit(0);
    } catch (err) { why = err.message }
  }
  try {
    const { root, list } = await openai(base);
    console.log(["openai", root, pick(list, "The server")].join("\t"));
    process.exit(0);
  } catch (err) { why = err.message }
  tried.push(base + " (" + why + ")");
}
console.error("openclaw: no LLM server answered at " + raw + ". Tried " + tried.join(", ") + ".");
console.error("openclaw: check the address, that the server is running, and that it listens on the network rather than localhost only (Ollama needs OLLAMA_HOST=0.0.0.0).");
process.exit(1);
PROBE

# Answer the first-run "who am I" ritual from what the install pane collected.
# Only ever runs while BOOTSTRAP.md is there: its presence is OpenClaw's own
# marker for an agent that has not been introduced yet, and the agent deletes
# it once it has. So a later deploy never overwrites an identity that the
# operator and the agent have since made their own.
cat > /tmp/fog-identity.js <<'IDENT'
const fs = require("node:fs");
const path = require("node:path");
const ws = process.argv[2];
const bootstrap = path.join(ws, "BOOTSTRAP.md");
if (!fs.existsSync(bootstrap)) process.exit(0);
const val = (k) => (process.env[k] || "").trim();
const name = val("FOG_AGENT_NAME");
const user = val("FOG_USER_NAME");
if (!name && !user) process.exit(0);

if (name) {
  const line = (label, v) => "- **" + label + ":** " + v;
  fs.writeFileSync(path.join(ws, "IDENTITY.md"), [
    "# IDENTITY.md - Who Am I?",
    "",
    line("Name", name),
    line("Creature", val("FOG_AGENT_CREATURE") || "AI assistant"),
    line("Vibe", val("FOG_AGENT_VIBE") || "warm, direct, and a little playful"),
    line("Emoji", val("FOG_AGENT_EMOJI") || "🦞"),
    "",
    "---",
    "",
    "Set up during install. A starting point, not a cage — rewrite it as you become yourself.",
    "",
  ].join("\n"));
  console.log("openclaw: seeded IDENTITY.md as " + name);
}

if (user) {
  const rows = ["- **Name:** " + user, "- **What to call them:** " + user];
  const pronouns = val("FOG_USER_PRONOUNS");
  if (pronouns) rows.push("- **Pronouns:** " + pronouns);
  const tz = val("TZ");
  if (tz) rows.push("- **Timezone:** " + tz);
  fs.writeFileSync(path.join(ws, "USER.md"), [
    "# USER.md - About Your Human",
    "",
    ...rows,
    "",
    "## Context",
    "",
    "_(Learn what they care about as you go, and write it down here.)_",
    "",
  ].join("\n"));
  console.log("openclaw: seeded USER.md for " + user);
}

// The naming conversation is the point of the ritual; keep it when the agent
// still has no name of its own.
if (name) {
  fs.rmSync(bootstrap, { force: true });
  console.log("openclaw: introductions are done — removed BOOTSTRAP.md");
}
IDENT

if [ ! -f "$STATE/openclaw.json" ]; then
  echo "openclaw: no config yet — running first-run setup"
  set -- --non-interactive --accept-risk --mode local --flow quickstart \
    --gateway-auth token --gateway-token-ref-env OPENCLAW_GATEWAY_TOKEN \
    --gateway-bind lan --gateway-port 18789 \
    --no-install-daemon --skip-daemon --skip-health --skip-channels \
    --skip-skills --skip-search --skip-hooks --skip-ui \
    --suppress-gateway-token-output
  KIND=""
  if [ -n "$FOG_LOCAL_LLM_URL" ]; then
    FOUND=$(node /tmp/fog-probe.mjs)
    KIND=$(printf '%s' "$FOUND" | cut -f1)
    BASE=$(printf '%s' "$FOUND" | cut -f2)
    MODEL=$(printf '%s' "$FOUND" | cut -f3)
    echo "openclaw: answering with $MODEL on $BASE"
    if [ "$KIND" = "ollama" ]; then
      set -- "$@" --auth-choice ollama --custom-base-url "$BASE" --custom-model-id "$MODEL"
    else
      set -- "$@" --auth-choice custom-api-key --custom-compatibility openai \
        --custom-provider-id local --custom-base-url "$BASE" --custom-model-id "$MODEL" \
        --custom-api-key "${FOG_LOCAL_LLM_API_KEY:-local}"
    fi
  elif [ -n "$ANTHROPIC_API_KEY" ]; then
    # --secret-input-mode ref stores the credential as a reference to
    # ANTHROPIC_API_KEY rather than a copy, so the vault stays the one place
    # the key lives and backups carry no key.
    set -- "$@" --auth-choice apiKey --anthropic-api-key "$ANTHROPIC_API_KEY" --secret-input-mode ref
  else
    # No brain chosen yet. The gateway and Control UI still come up, which
    # beats a crash-looping app the operator can't get into to fix it.
    echo "openclaw: no model provider given — pick one in the Control UI"
    set -- "$@" --auth-choice skip
  fi
  node "$CLI" onboard "$@"
  if [ "$KIND" = "ollama" ]; then
    # A LAN Ollama needs no real credential; the marker keeps the provider
    # self-contained instead of resolving an env var at runtime. Local models
    # can be slow to wake, so a cold first turn gets room.
    node "$CLI" config set models.providers.ollama.apiKey ollama-local >/dev/null
    node "$CLI" config set models.providers.ollama.timeoutSeconds 300 --strict-json >/dev/null
  fi
fi

node /tmp/fog-identity.js "$STATE/workspace" || true

[ -n "$FOG_FQDN" ] || exit 0
ORIGIN="https://$FOG_FQDN"
cur=$(node "$CLI" config get gateway.controlUi.allowedOrigins 2>/dev/null) || cur=""
next=$(CUR="$cur" ORIGIN="$ORIGIN" node -e '
  let cur = [];
  try { cur = JSON.parse(process.env.CUR) } catch {}
  if (!Array.isArray(cur)) cur = [];
  if (cur.includes(process.env.ORIGIN)) { console.log(""); }
  else { console.log(JSON.stringify([...cur, process.env.ORIGIN])); }
')
[ -n "$next" ] || exit 0
echo "openclaw: allowing Control UI origin $ORIGIN"
node "$CLI" config set gateway.controlUi.allowedOrigins "$next" --strict-json
`

// openclawEntry: personal AI assistant gateway. Single-instance state
// (sessions, memory, config) lives in the state volume — restart, not swap.
func openclawEntry() Entry {
	return Entry{
		ID: "openclaw", Title: "OpenClaw", Category: "Personal cloud", Icon: "🦞",
		Description: "Personal AI assistant — chat with your agent over WhatsApp, Telegram, and the web, with memory and tools.",
		Spec: apps.AppSpec{
			Source: apps.Source{Type: "image", Image: "ghcr.io/openclaw/openclaw:extended-stable"},
			// Mirrors the upstream docker-compose command; "lan" binds the
			// container interface so Fog's proxy can reach the gateway.
			Command: []string{"node", "dist/index.js", "gateway", "--bind", "lan", "--port", "18789"},
			Env: map[string]string{
				"TZ":                 "Etc/UTC",
				"HOME":               "/home/node",
				"OPENCLAW_STATE_DIR": "/home/node/.openclaw",
				// Docker's bridge drops mDNS multicast, and the gateway
				// restarts itself when advertising keeps failing.
				"OPENCLAW_DISABLE_BONJOUR": "1",
				// The hostname Fog serves this app on, for the Control UI
				// origin allowlist the bootstrap writes.
				"FOG_FQDN": "{{fqdn}}",
			},
			Secrets: []string{"OPENCLAW_GATEWAY_TOKEN", "ANTHROPIC_API_KEY", "FOG_LOCAL_LLM_API_KEY"},
			Volumes: []apps.Volume{
				// Config, memory, sessions, and the agent workspace.
				{Name: "state", Mount: "/home/node/.openclaw", Owner: "1000:1000"},
				// Channel auth profiles (WhatsApp pairing etc.).
				{Name: "auth", Mount: "/home/node/.config/openclaw", Owner: "1000:1000"},
			},
			// Agent replies stream — never buffer this route.
			Routes: []apps.Route{{Port: 18789, NoBuffer: true}},
			// The documented unauthenticated liveness probe.
			Health:  &apps.Health{Path: "/healthz"},
			Release: []string{"sh", "-c", openclawBootstrap},
			// One gateway owns the state directory — never overlap instances.
			DeployStrategy: "restart",
			Backup:         &apps.BackupSpec{Enabled: true},
			// Agents spawn subprocesses; cap them before they eat the box.
			MemoryLimit: "4g",
			Notes: "Installing runs OpenClaw's setup for you, so the gateway comes up configured. " +
				"Give it the address of a model server on your own network — Ollama, LM Studio, vLLM, or " +
				"anything with an OpenAI-compatible API — and Fog finds it, picks a model that can use " +
				"tools, and points the assistant at it; nothing leaves the house. An Anthropic key instead " +
				"has it answer with Claude, and with neither it still boots so you can choose a provider in " +
				"the Control UI. Name the assistant at install and it skips the first-run “who am I” " +
				"interview; leave the name blank and it will ask you in your first conversation. " +
				"First visit: the Control UI asks for the gateway token — copy it from Config → Secrets " +
				"(OPENCLAW_GATEWAY_TOKEN). From there, pair chat channels (WhatsApp, Telegram, …) in the " +
				"dashboard. The Control UI needs a secure context, so reach it over HTTPS — set up a domain " +
				"certificate in Fog if you haven't. " +
				"Memory, sessions, and config live in the state volume and are included in backups.",
		},
		Options: []Option{
			{Key: "local_llm_url", Label: "Local LLM address",
				Hint: "IP, host, or URL of a model server on your network — Ollama, LM Studio, vLLM, or any " +
					"OpenAI-compatible endpoint (e.g. 192.0.2.10). Fog discovers its models and picks one. " +
					"It has to listen on the network, not just localhost (Ollama needs OLLAMA_HOST=0.0.0.0).",
				Target: "env:FOG_LOCAL_LLM_URL"},
			{Key: "anthropic_api_key", Label: "Anthropic API key",
				Hint:   "Used when no local address is given. Leave both blank to choose a provider later in the Control UI.",
				Target: "secret:ANTHROPIC_API_KEY"},
			{Key: "agent_name", Label: "Assistant's name",
				Hint:   "What you'll call it. Left blank, it interviews you about who it should be in your first conversation.",
				Target: "env:FOG_AGENT_NAME"},
			{Key: "user_name", Label: "Your name",
				Hint: "What the assistant calls you.", Target: "env:FOG_USER_NAME"},
			{Key: "local_llm_model", Label: "Local LLM model",
				Hint:   "Model ID on that server, e.g. qwen3:8b. Blank picks the roomiest one that supports tool calling.",
				Target: "env:FOG_LOCAL_LLM_MODEL", Advanced: true},
			{Key: "local_llm_api_key", Label: "Local LLM API key",
				Hint:   "Only for local gateways that ask for one (LiteLLM and friends) — Ollama and LM Studio don't.",
				Target: "secret:FOG_LOCAL_LLM_API_KEY", Advanced: true},
			{Key: "user_pronouns", Label: "Your pronouns",
				Hint: "So the assistant refers to you correctly.", Target: "env:FOG_USER_PRONOUNS", Advanced: true},
			{Key: "agent_creature", Label: "Assistant's nature",
				Hint:   "AI? robot? familiar? ghost in the machine?",
				Target: "env:FOG_AGENT_CREATURE", Default: "AI assistant"},
			{Key: "agent_vibe", Label: "Assistant's vibe",
				Hint:   "How it comes across — sharp, warm, chaotic, calm.",
				Target: "env:FOG_AGENT_VIBE", Default: "warm, direct, and a little playful"},
			{Key: "agent_emoji", Label: "Assistant's emoji", Hint: "Its signature.",
				Target: "env:FOG_AGENT_EMOJI", Default: "🦞"},
			generatedSecret("OPENCLAW_GATEWAY_TOKEN", "Protects the gateway's Control UI and API; unique per instance.", "hex32"),
			tzOption,
		},
	}
}

// bookorbitEntry: self-hosted library and reading platform (ebooks, PDFs,
// audiobooks, comics) with progress sync to Kobo and KOReader. Image-based,
// on fog's managed Postgres — its migrator creates the extensions it needs
// on boot, but fog declares them too so they exist before the first run.
func bookorbitEntry() Entry {
	return Entry{
		ID: "bookorbit", Title: "BookOrbit", Category: "Media servers", Icon: "📖",
		Description: "Library and reading platform — ebooks, PDFs, audiobooks, and comics, with progress sync to Kobo and KOReader.",
		Spec: apps.AppSpec{
			Source: apps.Source{Type: "image", Image: "ghcr.io/bookorbit/bookorbit:latest"},
			Env: map[string]string{
				"TZ":   "Etc/UTC",
				"PUID": "1000",
				"PGID": "1000",
				// Emails, Kobo endpoints, and the KOReader plugin download
				// all point devices at this URL.
				"APP_URL": "https://{{fqdn}}",
			},
			Secrets: []string{"JWT_SECRET", "SETUP_BOOTSTRAP_TOKEN", "EMAIL_ENCRYPTION_KEY", "MIGRATION_ENCRYPTION_KEY"},
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases:  []string{"bookorbit"},
				Extensions: []string{"uuid-ossp", "pg_trgm", "unaccent", "vector"},
				// The entrypoint assembles DATABASE_URL from these parts.
				Inject: map[string]string{
					"POSTGRES_HOST": "host", "POSTGRES_PORT": "port", "POSTGRES_USER": "user",
					"POSTGRES_PASSWORD": "password", "POSTGRES_DB": "dbname",
				},
			}},
			Volumes: []apps.Volume{
				// Covers, the upload bucket, and everything else the app writes.
				{Name: "data", Mount: "/data", Owner: "1000:1000"},
				// The library itself: Drive's Content/Books, where the
				// operator drops ebooks and finds what BookOrbit has.
				contentFolder("books", "/books", "Books"),
			},
			// Readers stream audiobooks with Range requests — never buffer.
			Routes: []apps.Route{{Port: 3000, NoBuffer: true}},
			// The image's own HEALTHCHECK endpoint.
			Health: &apps.Health{Path: "/api/v1/health"},
			// Migrations run at container start, and one instance owns the
			// data volume's scan and upload state — never overlap two.
			DeployStrategy: "restart",
			Backup:         &apps.BackupSpec{Enabled: true},
			// Hook delivers through the Book Dock API and reads the shelf
			// from the library folder, which it must see too.
			Connection: &apps.Connection{Port: 3000, Shares: []string{"books"}},
			// The entrypoint sizes Node's heap from the cgroup limit; without
			// one it assumes 2 GB of heap regardless of what the box has.
			MemoryLimit: "2g",
			Notes: "First visit: the setup wizard asks for the setup token — copy it from Config → Secrets " +
				"(SETUP_BOOTSTRAP_TOKEN); it is only used once, to create your admin account. Your books " +
				"live in Drive → Content → Books, which BookOrbit sees as /books — create a library pointing " +
				"there and drop ebooks into the folder from the Drive page. Kobo sync and the KOReader " +
				"plugin use this app's address, so reach it over HTTPS at the hostname Fog serves it on. " +
				"If you point the books folder at your own host path instead and it is owned by a user " +
				"other than UID 1000, set PUID/PGID to match in Config → Environment.",
		},
		// sharp (image processing) ships prebuilt binaries that refuse to
		// load below x86-64-v2; the container dies on boot with
		// "Unsupported CPU" — seen on a Proxmox VM left on the kvm64 model.
		Requires: &Requires{CPU: "x86-64-v2"},
		Options: []Option{
			{Key: "books", Label: "Books folder", Hint: "Your ebook library — BookOrbit sees it as /books.", Target: "volume:books"},
			generatedSecret("JWT_SECRET", "Signs login tokens; unique per instance.", "hex128"),
			generatedSecret("SETUP_BOOTSTRAP_TOKEN", "One-time token the setup wizard asks for on first visit.", "hex32"),
			generatedSecret("EMAIL_ENCRYPTION_KEY", "Encrypts stored SMTP credentials at rest.", "hex64"),
			generatedSecret("MIGRATION_ENCRYPTION_KEY", "Encrypts stored migration-source credentials at rest.", "hex64"),
			tzOption,
		},
	}
}

// n8nEntry: workflow automation on Fog's Postgres. Everything n8n stores
// that matters — workflows, credentials, execution history — is in the
// database; the data volume holds its settings file and binary data.
func n8nEntry() Entry {
	return Entry{
		ID: "n8n", Title: "n8n", Category: "Utilities", Icon: "🔀",
		Description: "Workflow automation — connect apps and services with visual flows, webhooks, schedules, and AI agents.",
		Spec: apps.AppSpec{
			Source: apps.Source{Type: "image", Image: "n8nio/n8n:latest"},
			Env: map[string]string{
				// Webhook URLs n8n shows in the editor and registers with
				// outside services are built from these; left alone they
				// say http://localhost:5678.
				"N8N_HOST":            "{{fqdn}}",
				"N8N_PROTOCOL":        "https",
				"WEBHOOK_URL":         "https://{{fqdn}}/",
				"N8N_EDITOR_BASE_URL": "https://{{fqdn}}/",
				// Fog's proxy sits in front, so the client's address is in
				// X-Forwarded-For — n8n's rate limits key on it.
				"N8N_PROXY_HOPS":                        "1",
				"GENERIC_TIMEZONE":                      "Etc/UTC",
				"DB_TYPE":                               "postgresdb",
				"N8N_ENFORCE_SETTINGS_FILE_PERMISSIONS": "true",
				"N8N_DIAGNOSTICS_ENABLED":               "false",
			},
			// Encrypts every credential stored in the database. Without it
			// n8n mints one into the data volume, and a restore that loses
			// the volume but keeps the database can't read a credential.
			Secrets: []string{"N8N_ENCRYPTION_KEY"},
			Needs: &apps.Needs{Postgres: &apps.PostgresNeed{
				Databases: []string{"n8n"},
				Inject: map[string]string{
					"DB_POSTGRESDB_HOST": "host", "DB_POSTGRESDB_PORT": "port", "DB_POSTGRESDB_USER": "user",
					"DB_POSTGRESDB_PASSWORD": "password", "DB_POSTGRESDB_DATABASE": "dbname",
				},
			}},
			// The image runs as node (1000).
			Volumes: []apps.Volume{{Name: "data", Mount: "/home/node/.n8n", Owner: "1000:1000"}},
			// Webhook responses and AI agents stream — never buffer.
			Routes: []apps.Route{{Port: 5678, NoBuffer: true}},
			Health: &apps.Health{Path: "/healthz"},
			// Migrations run at start, and one instance owns the schedules
			// and webhooks — two overlapping would fire each one twice.
			DeployStrategy: "restart",
			Backup:         &apps.BackupSpec{Enabled: true},
			Notes: "First visit: n8n asks you to create the owner account — do it right away, since whoever gets there " +
				"first owns the instance. Webhook URLs use this app's HTTPS address, so services outside the house can " +
				"reach them only if Fog serves this app publicly. Reach the editor over HTTPS at the hostname Fog serves " +
				"it on: n8n's session cookie is secure-only, so plain http:// will not keep you signed in. " +
				"Workflows, credentials, and execution history live in Fog's Postgres and are in the app's backups; " +
				"N8N_ENCRYPTION_KEY in Config → Secrets encrypts the stored credentials, so never change it after setup.",
		},
		Options: []Option{
			{Key: "tz", Label: "Timezone", Hint: "Schedule triggers fire in this zone.", Target: "env:GENERIC_TIMEZONE", Default: "Etc/UTC"},
			generatedSecret("N8N_ENCRYPTION_KEY", "Encrypts stored credentials; never change it after setup.", "alnum32"),
		},
	}
}

// sabHostWhitelistInit runs in SABnzbd's container before SABnzbd itself
// (LinuxServer images execute what they find in /custom-cont-init.d). It is
// additive and idempotent: the operator's own whitelist entries survive, and
// a changed base domain simply adds the new hostname on the next deploy.
const sabHostWhitelistInit = `#!/usr/bin/with-contenv bash
# Fog: SABnzbd refuses any request whose Host header it does not recognise —
# its DNS-rebind guard. Two names reach it: this app's own hostname behind
# Fog's proxy, and its name on the fog network ("sabnzbd"), which is how the
# apps that use it call its API. SABnzbd has no way to learn either, so fold
# both into host_whitelist before it starts; entries already there (the
# container's hostname, anything the operator added) are kept.
ini=/config/sabnzbd.ini
hosts=$(printf '%s,%s' "${FOG_FQDN:-}" "${FOG_NETWORK_NAME:-sabnzbd}" | tr '[:upper:]' '[:lower:]')

if [[ ! -f $ini ]]; then
    # First start: SABnzbd fills in every other default around this one.
    printf '[misc]\nhost_whitelist = %s\n' "$(printf '%s' "$hosts" | sed 's/^,//; s/,/, /g')" > "$ini"
    lsiown abc:abc "$ini"
    exit 0
fi

tmp=$(mktemp)
awk -v hosts="$hosts" '
{
    line[NR] = $0
    if ($0 ~ /^host_whitelist[ \t]*=/) wl = NR
    else if ($0 ~ /^\[misc\][ \t]*$/) misc = NR
}
END {
    if (wl) { cur = line[wl]; sub(/^host_whitelist[ \t]*=[ \t]*/, "", cur) }
    n = split(cur "," hosts, part, ",")
    for (i = 1; i <= n; i++) {
        gsub(/^[ \t]+|[ \t]+$/, "", part[i])
        if (part[i] != "" && !seen[part[i]]++) list = list (list == "" ? "" : ", ") part[i]
    }
    entry = "host_whitelist = " list
    last = NR
    if (wl) line[wl] = entry
    else if (misc) line[misc] = line[misc] "\n" entry
    else { last = NR + 1; line[last] = "[misc]\n" entry }
    for (i = 1; i <= last; i++) print line[i]
}' "$ini" > "$tmp"

# Rewrite only on a real change, and in place, so the file keeps the
# ownership SABnzbd needs to save its own config over it later.
if [[ -s $tmp ]] && ! cmp -s "$tmp" "$ini"; then
    cat "$tmp" > "$ini"
fi
rm -f "$tmp"
`

// sabFoldersInit keeps SABnzbd's download folders inside /data, the folder
// it shares with the apps that use it (connection.shares). Left alone,
// SABnzbd puts them under /config — the image's own default is a
// /downloads that nothing mounts — and finished files land where no other
// app can see them. A folder the operator chose inside /data is kept.
const sabFoldersInit = `#!/usr/bin/with-contenv bash
# Fog: downloads must land in /data, the folder Fog shares with the apps
# that use SABnzbd (Hook reads finished files there). Point download_dir
# and complete_dir into it unless they already are.
ini=/config/sabnzbd.ini
shared=/data
if [[ ! -f $ini ]]; then
    printf '[misc]\n' > "$ini"
    lsiown abc:abc "$ini"
fi

tmp=$(mktemp)
awk -v shared="$shared" '
function inside(v) { return v == shared || index(v, shared "/") == 1 }
function value(l) { sub(/^[^=]*=[ \t]*/, "", l); return l }
{
    line[NR] = $0
    if ($0 ~ /^download_dir[ \t]*=/) dd = NR
    else if ($0 ~ /^complete_dir[ \t]*=/) cd = NR
    else if ($0 ~ /^\[misc\][ \t]*$/ && !misc) misc = NR
}
END {
    last = NR
    if (!misc) { last = NR + 1; line[last] = "[misc]"; misc = last }
    add = ""
    if (!dd) add = add "\ndownload_dir = " shared "/incomplete"
    else if (!inside(value(line[dd]))) line[dd] = "download_dir = " shared "/incomplete"
    if (!cd) add = add "\ncomplete_dir = " shared "/complete"
    else if (!inside(value(line[cd]))) line[cd] = "complete_dir = " shared "/complete"
    line[misc] = line[misc] add
    for (i = 1; i <= last; i++) print line[i]
}' "$ini" > "$tmp"

if [[ -s $tmp ]] && ! cmp -s "$tmp" "$ini"; then
    cat "$tmp" > "$ini"
fi
rm -f "$tmp"

for key in download_dir complete_dir; do
    dir=$(sed -n "s/^$key[[:space:]]*=[[:space:]]*//p" "$ini" | head -n1)
    if [[ $dir == "$shared" || $dir == "$shared"/* ]]; then
        mkdir -p "$dir" && lsiown abc:abc "$dir"
    fi
done
exit 0
`

// sabAPIKeyInit holds SABnzbd's API key to the one in Fog's vault, so the
// connection Fog hands a consumer (docs/events-contract.md) carries a key
// that works. It runs after the whitelist hook, which has already made sure
// the file exists. Without the secret it does nothing and SABnzbd keeps the
// key it minted — an install from before this hook loses no integrations
// until the operator copies that key into the vault.
const sabAPIKeyInit = `#!/usr/bin/with-contenv bash
# Fog: SABnzbd's API key is SABNZBD_API_KEY from Fog's vault, the one Fog
# hands to apps it connects to SABnzbd. Unset, SABnzbd keeps its own.
ini=/config/sabnzbd.ini
key=${SABNZBD_API_KEY:-}
[[ -n $key ]] || exit 0

if [[ ! -f $ini ]]; then
    printf '[misc]\napi_key = %s\n' "$key" > "$ini"
    lsiown abc:abc "$ini"
    exit 0
fi

tmp=$(mktemp)
awk -v key="$key" '
{
    line[NR] = $0
    if ($0 ~ /^api_key[ \t]*=/ && !ak) ak = NR
    else if ($0 ~ /^\[misc\][ \t]*$/) misc = NR
}
END {
    entry = "api_key = " key
    last = NR
    if (ak) line[ak] = entry
    else if (misc) line[misc] = line[misc] "\n" entry
    else { last = NR + 1; line[last] = "[misc]\n" entry }
    for (i = 1; i <= last; i++) print line[i]
}' "$ini" > "$tmp"

if [[ -s $tmp ]] && ! cmp -s "$tmp" "$ini"; then
    cat "$tmp" > "$ini"
fi
rm -f "$tmp"
`

// Entries returns the catalog. Order within a category is display order.
func Entries() []Entry {
	sonarr := lsioEntry("sonarr", "Sonarr", "TV series management — finds, grabs, and organizes episodes.", "Media automation", "📺", "lscr.io/linuxserver/sonarr:latest", 8989, "/ping", "Mounted at /data — use one shared folder across the stack so hardlinks work.")
	radarr := lsioEntry("radarr", "Radarr", "Movie management — the Sonarr of movies.", "Media automation", "🎬", "lscr.io/linuxserver/radarr:latest", 7878, "/ping", "Mounted at /data — use one shared folder across the stack so hardlinks work.")
	lidarr := lsioEntry("lidarr", "Lidarr", "Music collection management.", "Media automation", "🎵", "lscr.io/linuxserver/lidarr:latest", 8686, "/ping", "Mounted at /data.")
	prowlarr := lsioEntry("prowlarr", "Prowlarr", "Indexer manager that feeds Sonarr, Radarr, and Lidarr.", "Media automation", "🔍", "lscr.io/linuxserver/prowlarr:latest", 9696, "/ping", "")
	bazarr := lsioEntry("bazarr", "Bazarr", "Subtitle management for Sonarr and Radarr libraries.", "Media automation", "💬", "lscr.io/linuxserver/bazarr:latest", 6767, "", "Mounted at /data.")

	qbit := lsioEntry("qbittorrent", "qBittorrent", "Torrent client with a web UI.", "Downloads", "⬇️", "lscr.io/linuxserver/qbittorrent:latest", 8080, "", "Mounted at /data — same shared folder as the arr apps.")
	qbit.Spec.Env["WEBUI_PORT"] = "8080"
	sab := lsioEntry("sabnzbd", "SABnzbd", "Usenet downloader.", "Downloads", "📦", "lscr.io/linuxserver/sabnzbd:latest", 8080, "", "Drive's Downloads folder, mounted at /data — the apps that use SABnzbd see it too.")
	// SABnzbd refuses any request whose Host header it doesn't recognise, so
	// a fresh install answered "Access denied - Hostname verification failed"
	// at the very address Fog had just handed the operator. It has no setting
	// for this outside its own config file, so the hostname is folded into
	// host_whitelist by an init hook the image runs before SABnzbd starts.
	sab.Spec.Env["FOG_FQDN"] = "{{fqdn}}"
	sab.Spec.Files = []apps.File{{
		Name:    "host-whitelist-init",
		Mount:   "/custom-cont-init.d/10-fog-host-whitelist",
		Exec:    true,
		Content: sabHostWhitelistInit,
	}, {
		Name:    "api-key-init",
		Mount:   "/custom-cont-init.d/11-fog-api-key",
		Exec:    true,
		Content: sabAPIKeyInit,
	}, {
		Name:    "folders-init",
		Mount:   "/custom-cont-init.d/12-fog-folders",
		Exec:    true,
		Content: sabFoldersInit,
	}}
	// SABnzbd's folder is the downloads folder in Drive, not all of
	// Content: it is what it shares with the apps that use it, and nothing
	// it does needs the libraries.
	sab.Spec.Volumes[1] = contentFolder("media", "/data", "Downloads")
	// The API key lives in Fog's vault (minted at install) and is held in
	// SABnzbd's config by the hook above, so apps that use sabnzbd — HUD
	// watching for finished downloads — are handed a key that works.
	sab.Spec.Secrets = append(sab.Spec.Secrets, "SABNZBD_API_KEY")
	sab.Spec.Connection = &apps.Connection{Port: 8080, Secret: "SABNZBD_API_KEY", Shares: []string{"media"}}
	sab.Options = append(sab.Options, generatedSecret("SABNZBD_API_KEY",
		"SABnzbd's API key, held in its config at every start. Apps connected to SABnzbd are handed it.", "hex32"))

	jellyfin := lsioEntry("jellyfin", "Jellyfin", "Media server — stream your library to any device.", "Media servers", "🍿", "lscr.io/linuxserver/jellyfin:latest", 8096, "/health", "Your media library, mounted read-write at /data.")
	navidrome := entry("navidrome", "Navidrome", "Music streaming server with Subsonic-compatible apps.", "Media servers", "🎧", "deluan/navidrome:latest", 4533, "/ping", func(s *apps.AppSpec) {
		s.Volumes = []apps.Volume{{Name: "data", Mount: "/data"}, contentFolder("music", "/music", "Music")}
	})
	navidrome.Options = []Option{tzOption, {Key: "music", Label: "Music folder", Hint: "Navidrome streams what it finds here.", Target: "volume:music"}}
	abs := entry("audiobookshelf", "Audiobookshelf", "Audiobook and podcast server.", "Media servers", "📚", "ghcr.io/advplyr/audiobookshelf:latest", 80, "/healthcheck", func(s *apps.AppSpec) {
		s.Volumes = []apps.Volume{{Name: "config", Mount: "/config"}, {Name: "metadata", Mount: "/metadata"}, contentFolder("library", "/library", "Audiobooks")}
	})
	abs.Options = []Option{tzOption, {Key: "library", Label: "Audiobook folder", Hint: "Point a library at /library in the app.", Target: "volume:library"}}

	jellyseerr := entry("jellyseerr", "Jellyseerr", "Request management for Jellyfin — family-friendly front door for the arr stack.", "Media automation", "🙋", "fallenbagel/jellyseerr:latest", 5055, "/api/v1/status", func(s *apps.AppSpec) {
		s.Volumes = []apps.Volume{{Name: "config", Mount: "/app/config"}}
	})
	jellyseerr.Options = []Option{tzOption}

	kuma := entry("uptime-kuma", "Uptime Kuma", "Uptime monitoring with status pages and alerts.", "Utilities", "🟢", "louislam/uptime-kuma:1", 3001, "/", func(s *apps.AppSpec) {
		s.Volumes = []apps.Volume{{Name: "data", Mount: "/app/data"}}
	})
	vaultwarden := entry("vaultwarden", "Vaultwarden", "Bitwarden-compatible password manager server.", "Utilities", "🔐", "vaultwarden/server:latest", 80, "/alive", func(s *apps.AppSpec) {
		s.Volumes = []apps.Volume{{Name: "data", Mount: "/data"}}
	})
	ntfy := entry("ntfy", "ntfy", "Push notifications you can point Fog and your other apps at.", "Utilities", "🔔", "binwiederhier/ntfy:latest", 80, "/v1/health", func(s *apps.AppSpec) {
		s.Command = []string{"serve", "--cache-file", "/var/cache/ntfy/cache.db"}
		s.Volumes = []apps.Volume{{Name: "cache", Mount: "/var/cache/ntfy"}, {Name: "config", Mount: "/etc/ntfy"}}
	})
	adguard := entry("adguard", "AdGuard Home", "Network-wide ad and tracker blocking — a DNS server for your whole LAN.", "Utilities", "🛡️", "adguard/adguardhome:latest", 3000, "/", func(s *apps.AppSpec) {
		delete(s.Env, "TZ") // AdGuard keeps its own config; no env needed
		s.Volumes = []apps.Volume{
			{Name: "work", Mount: "/opt/adguardhome/work"},
			{Name: "conf", Mount: "/opt/adguardhome/conf"},
		}
		// DNS itself can't go through the HTTP proxy — publish 53 on the host.
		s.Ports = []apps.Port{
			{Port: 53, Protocol: "tcp"},
			{Port: 53, Protocol: "udp"},
		}
		s.DeployStrategy = "restart"
		s.Notes = "In the setup wizard, set the Admin Web Interface port to **3000** (it defaults to 80) — " +
			"that's the port Fog's route points at; the DNS listen interface and port 53 are fine as offered. " +
			"Then point clients at this server: either set your router's DHCP DNS to this server's IP, " +
			"or change DNS per device. If port 53 is already taken on the host (e.g. systemd-resolved's " +
			"stub listener or another DNS server), free it first or the app won't start."
	})
	adguard.Options = nil // configured entirely through its own setup wizard
	homeassistant := lsioEntry("homeassistant", "Home Assistant", "Home automation hub. (Bridge networking — some discovery integrations may need extra setup.)", "Utilities", "🏠", "lscr.io/linuxserver/homeassistant:latest", 8123, "", "")
	// Home Assistant mints its own tokens, so the connection's credential
	// is pasted into the vault after setup — deliberately not in Secrets,
	// which would fail every deploy until then.
	homeassistant.Spec.Connection = &apps.Connection{Port: 8123, Secret: "HASS_TOKEN"}
	homeassistant.Spec.Notes = "To let apps such as HUD use Home Assistant, create a long-lived access token " +
		"(your profile → Security) and add it as the HASS_TOKEN secret in Config → Secrets."
	homepage := entry("homepage", "Homepage", "A tidy dashboard for everything running on this server.", "Utilities", "🗂️", "ghcr.io/gethomepage/homepage:latest", 3000, "", func(s *apps.AppSpec) {
		s.Volumes = []apps.Volume{{Name: "config", Mount: "/app/config"}}
		s.Env["HOMEPAGE_ALLOWED_HOSTS"] = "{{fqdn}}"
	})

	// Gitea is a git host with the trimmings — pull requests, issues, a
	// package registry, actions. SQLite is plenty at household scale (its
	// own docs say so), so the whole instance is one volume, and the docker
	// image rewrites app.ini from GITEA__ env on every boot, so a domain
	// change on Fog's side reaches the clone URLs Gitea prints. The install
	// wizard is skipped: it would only re-ask what the env already answers.
	// SSH can't go through the HTTP proxy, and 22 on the host belongs to the
	// server's own sshd, so the container's sshd is published on 2222 and
	// SSH_PORT tells Gitea to advertise that in clone URLs.
	gitea := entry("gitea", "Gitea", "Git hosting with pull requests, issues, and a package registry.", "Development", "🍵", "gitea/gitea:1", 3000, "/api/healthz", func(s *apps.AppSpec) {
		s.Env["USER_UID"] = "1000"
		s.Env["USER_GID"] = "1000"
		s.Env["GITEA__database__DB_TYPE"] = "sqlite3"
		s.Env["GITEA__security__INSTALL_LOCK"] = "true"
		s.Env["GITEA__server__DOMAIN"] = "{{fqdn}}"
		s.Env["GITEA__server__SSH_DOMAIN"] = "{{fqdn}}"
		s.Env["GITEA__server__ROOT_URL"] = "https://{{fqdn}}/"
		s.Env["GITEA__server__SSH_PORT"] = "2222"
		s.Env["GITEA__server__LFS_START_SERVER"] = "true"
		s.Volumes = []apps.Volume{{Name: "data", Mount: "/data", Owner: "1000:1000"}}
		s.Ports = []apps.Port{{Port: 22, HostPort: 2222}}
		s.DeployStrategy = "restart"
		// Clones and pushes stream for as long as the repository is large.
		s.Routes = []apps.Route{{Port: 3000, NoBuffer: true}}
		s.Notes = "The first account to register becomes the administrator, so register yours right away, " +
			"then close registration if this server is reachable beyond the household " +
			"(Site Administration → Configuration, or set `GITEA__service__DISABLE_REGISTRATION` to `true` in the app's env). " +
			"Clone over HTTPS at this app's URL, or over SSH on port **2222** — add your public key under Settings → SSH / GPG Keys. " +
			"Repositories, the database, and the config all live in the data volume, so snapshots capture the whole instance."
	})
	gitea.Options = []Option{tzOption}

	// VS Code in the browser, via code-server (the open-source build of VS
	// Code Coder ships as a web app). The integrated terminal is a shell in
	// the container, so the editor always sits behind its own password.
	// Settings, extensions, and the default workspace all live under
	// /config, so the one volume — and its backups — carry the whole setup.
	vscode := lsioEntry("vscode", "VS Code", "VS Code in the browser — edit code, run a terminal, and install extensions from any machine.", "Development", "🧑‍💻", "lscr.io/linuxserver/code-server:latest", 8443, "/healthz", "")
	vscode.Spec.Env["DEFAULT_WORKSPACE"] = "/config/workspace"
	vscode.Spec.Secrets = []string{"PASSWORD", "SUDO_PASSWORD"}
	// One editor owns the settings and extensions under /config — two
	// overlapping during a swap would fight over them.
	vscode.Spec.DeployStrategy = "restart"
	vscode.Spec.Notes = "Sign in with the password in Config → Secrets (PASSWORD). The terminal is a shell inside this app's " +
		"container, so anyone with the password can run commands there — keep it strong, and don't share it beyond the people " +
		"you'd give a login on this server. Extensions come from Open VSX, the open marketplace, rather than Microsoft's, so a " +
		"few Microsoft-only ones (Copilot, Pylance, Remote-SSH) aren't available. Previews and extension pages need a secure " +
		"context, so reach it over HTTPS at the hostname Fog serves it on. Your projects live in /config/workspace, and " +
		"settings, extensions, and workspace are all included in backups. To use sudo in the terminal, set SUDO_PASSWORD in " +
		"Config → Secrets and redeploy."
	vscode.Options = []Option{
		generatedSecret("PASSWORD", "The editor's sign-in password; copy it from Config → Secrets after install.", "alnum32"),
		{Key: "sudo_password", Label: "Sudo password", Hint: "Lets the terminal run sudo. Blank leaves sudo off.",
			Target: "secret:SUDO_PASSWORD", Advanced: true},
		tzOption,
	}

	gotenberg := entry("gotenberg", "Gotenberg", "Document conversion API: HTML and Office files to PDF via Chromium and LibreOffice.", "Utilities", "🖨️", "gotenberg/gotenberg:8", 3000, "/health", func(s *apps.AppSpec) {
		// Stateless: nothing to persist, nothing to back up.
		s.Volumes = nil
		s.Backup = &apps.BackupSpec{Enabled: false}
	})

	entries := []Entry{
		// The household's own apps lead the categories they belong to.
		hubEntry(), finEntry(), officeEntry(),
		immichEntry(), paperlessEntry(), whisperEntry(), openclawEntry(),
		feedEntry(), hookEntry(), streamEntry(), onceEntry(), hudEntry(), lucidEntry(), newtabEntry(), signetEntry(), fitEntry(), fitCollectorEntry(), thinkFleetEntry(),
		sonarr, radarr, lidarr, prowlarr, bazarr, jellyseerr,
		qbit, sab,
		jellyfin, navidrome, abs, bookorbitEntry(),
		kuma, vaultwarden, ntfy, adguard, homeassistant, homepage, gotenberg, n8nEntry(),
		gitea, vscode,
	}
	for i := range entries {
		entries[i].ProjectURL = projectURLs[entries[i].ID]
		entries[i].PublicRepo = publicRepos[entries[i].FirstParty]
		entries[i].Listing = listings[entries[i].ID]
		entries[i].Footprint = footprints[entries[i].ID]
		entries[i].Formerly = apps.FormerNames[entries[i].ID]
	}
	return entries
}

// publicRepos maps a first-party repository to its published clone URL,
// for the apps that are published. One listed here installs on any fog;
// the rest need the operator's source setting.
var publicRepos = map[string]string{}

// projectURLs maps each image entry to its upstream repository — the app's
// own project, not the image packager's (linuxserver's docker-* repos).
var projectURLs = map[string]string{
	"immich":         "https://github.com/immich-app/immich",
	"paperless":      "https://github.com/paperless-ngx/paperless-ngx",
	"whisper":        "https://github.com/ahmetoner/whisper-asr-webservice",
	"openclaw":       "https://github.com/openclaw/openclaw",
	"bookorbit":      "https://github.com/bookorbit/bookorbit",
	"sonarr":         "https://github.com/Sonarr/Sonarr",
	"radarr":         "https://github.com/Radarr/Radarr",
	"lidarr":         "https://github.com/Lidarr/Lidarr",
	"prowlarr":       "https://github.com/Prowlarr/Prowlarr",
	"bazarr":         "https://github.com/morpheus65535/bazarr",
	"jellyseerr":     "https://github.com/Fallenbagel/jellyseerr",
	"qbittorrent":    "https://github.com/qbittorrent/qBittorrent",
	"sabnzbd":        "https://github.com/sabnzbd/sabnzbd",
	"jellyfin":       "https://github.com/jellyfin/jellyfin",
	"navidrome":      "https://github.com/navidrome/navidrome",
	"audiobookshelf": "https://github.com/advplyr/audiobookshelf",
	"uptime-kuma":    "https://github.com/louislam/uptime-kuma",
	"vaultwarden":    "https://github.com/dani-garcia/vaultwarden",
	"ntfy":           "https://github.com/binwiederhier/ntfy",
	"adguard":        "https://github.com/AdguardTeam/AdGuardHome",
	"homeassistant":  "https://github.com/home-assistant/core",
	"homepage":       "https://github.com/gethomepage/homepage",
	"gitea":          "https://github.com/go-gitea/gitea",
	"gotenberg":      "https://github.com/gotenberg/gotenberg",
	"vscode":         "https://github.com/coder/code-server",
	"n8n":            "https://github.com/n8n-io/n8n",
}
