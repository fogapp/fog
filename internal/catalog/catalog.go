// Package catalog is Fog's app store: well-behaved apps with sensible specs,
// so installing Sonarr is one click and a name. The apps themselves are not
// here. They live in catalogs — git repositories in the fog-catalog/1 format
// (format.go, docs/catalog-contract.md) — and a fog builds its store from
// the list of catalogs it is given (registry.go). This package is what an
// entry is, how a catalog is read, and what fog does with an entry: resolve
// its source, preflight it against the host, weigh what it costs.
//
// Entries are templates over the same AppSpec used everywhere — installing
// from the store and hand-crafting an app produce identical objects, and
// everything remains editable afterwards. Apps that need siblings (their own
// database, a broker) declare them as aux sidecars in the same spec.
package catalog

import (
	"fmt"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/hostcpu"
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
	// Catalog is the name of the catalog the entry came from, and Trusted
	// whether that is one of fog's own (DefaultRemotes). An entry from any
	// other catalog installs held for review. Neither is the catalog's to
	// say: fog fills them in as it loads it.
	Catalog string `json:"catalog,omitempty"`
	Trusted bool   `json:"trusted,omitempty"`

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
func PreflightSpec(entries []Entry, spec apps.AppSpec, host hostcpu.Info) string {
	if spec.Source.Type != "image" || spec.Source.Image == "" {
		return ""
	}
	if e := EntryForSpec(entries, "", &spec); e != nil {
		return e.Preflight(host)
	}
	return ""
}

// sameImage compares repositories, ignoring the tag: a pinned BookOrbit is
// still BookOrbit.
func sameImage(a, b string) bool {
	return dockerx.ImageName(a) == dockerx.ImageName(b)
}
