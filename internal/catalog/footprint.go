package catalog

import (
	"sort"

	"gitea.lan/matthew/fog/internal/apps"
)

// What an app costs the machine, and whether the machine can afford it.
//
// A household server fills up one install at a time, and the moment it
// stops being fast is never announced: the tenth app is as easy to add as
// the first, and only later does everything feel slow. The footprint is
// the store's estimate of what an entry takes — memory at rest and when
// busy, how hard it works the processor, what its images weigh on disk —
// and the fit is that estimate held against what the machine has and what
// the apps already on it are measured to be using. The figures are
// expectations, not promises: they come from what these images do in
// practice on a home server, and the fit says "plenty of room", "getting
// tight" or "over" rather than pretending to a decimal place.

// Footprint is an entry's expected cost, every container of the app
// counted together — Immich's machine-learning sidecar is part of Immich.
type Footprint struct {
	// Memory is what the app holds at rest, once it has settled after
	// start; MemoryPeak is what it can climb to when busy. Bytes.
	Memory     uint64 `json:"memory"`
	MemoryPeak uint64 `json:"memoryPeak"`
	// CPU is how hard the app works the processor: "light" (a service
	// that mostly waits), "moderate" (busy in bursts an operator will
	// notice), or "heavy" (can hold a core or more for minutes at a time).
	// CPUWhen names the heavy moment — "transcoding", "OCR on new
	// documents" — so the listing can say when, not just that.
	CPU     string `json:"cpu" ts:"\"light\" | \"moderate\" | \"heavy\""`
	CPUWhen string `json:"cpuWhen,omitempty"`
	// Disk is what the app's images take to download and unpack. DiskGrows
	// says what its data grows with, when that is worth knowing before
	// the install — a photo library is a different commitment from a
	// config folder.
	Disk      uint64 `json:"disk"`
	DiskGrows string `json:"diskGrows,omitempty"`
}

const (
	CPULight    = "light"
	CPUModerate = "moderate"
	CPUHeavy    = "heavy"
)

// cpuLoad is how many cores' worth of demand a class stands for when the
// app is busy, for adding up what a machine has taken on. A heavy app can
// hold a core; a moderate one half of one for a while; a light one is
// noise. The sum is the load if everything were busy at once — the worst
// afternoon, not the average one.
func cpuLoad(class string) float64 {
	switch class {
	case CPUHeavy:
		return 1
	case CPUModerate:
		return 0.5
	}
	return 0.1
}

const (
	mb = uint64(1) << 20
	gb = uint64(1) << 30
)

// EntryForSpec finds the catalog entry an installed app came from, by the
// name it was given at install (the entry's id is the default), then by
// the image it runs, then by the repository it builds. Nil for an app
// that came from nowhere in the catalog — a hand-built one is its own
// thing and gets no footprint.
func EntryForSpec(entries []Entry, name string, spec *apps.AppSpec) *Entry {
	for i := range entries {
		if entries[i].ID == name {
			return &entries[i]
		}
	}
	if spec == nil {
		return nil
	}
	for i := range entries {
		e := &entries[i]
		switch spec.Source.Type {
		case "image":
			if e.Spec.Source.Type == "image" && spec.Source.Image != "" && sameImage(e.Spec.Source.Image, spec.Source.Image) {
				return e
			}
		case "git":
			if e.Spec.Source.Type == "git" && spec.Source.Repo != "" && e.Spec.Source.Repo == spec.Source.Repo {
				return e
			}
		}
	}
	return nil
}

// Host is what the machine has, and what it is using right now — memory
// as the kernel reports it in use (so page cache, which is reclaimable,
// does not count against a new app), the disk that holds Fog's data.
type Host struct {
	MemTotal  uint64 `json:"memTotal"`
	MemUsed   uint64 `json:"memUsed"`
	Cores     int    `json:"cores"`
	DiskTotal uint64 `json:"diskTotal"`
	DiskFree  uint64 `json:"diskFree"`
}

// Installed is an app already on the machine as the fit sees it: which
// entry it is (nil for a hand-built app), whether it is running, and what
// its containers are measured to be using, together.
type Installed struct {
	Name     string `json:"name"`
	Entry    string `json:"entry,omitempty"`
	Running  bool   `json:"running"`
	MemUsage uint64 `json:"memUsage"`
	entry    *Entry
}

// NewInstalled binds an installed app to its catalog entry for the fit.
func NewInstalled(name string, entry *Entry, running bool, memUsage uint64) Installed {
	in := Installed{Name: name, Running: running, MemUsage: memUsage, entry: entry}
	if entry != nil {
		in.Entry = entry.ID
	}
	return in
}

// Tone is the fit's answer in one word: "ok", "tight", or "over". Tight is
// the honest middle — it will work, and it is where a machine starts to
// feel full.
type Tone string

const (
	ToneOK    Tone = "ok"
	ToneTight Tone = "tight"
	ToneOver  Tone = "over"
)

func worse(a, b Tone) Tone {
	rank := map[Tone]int{ToneOK: 0, ToneTight: 1, ToneOver: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Fit is one entry held against the machine: what it would add, where
// that leaves the machine, and the tone of each answer.
type Fit struct {
	Memory FitMemory `json:"memory"`
	CPU    FitCPU    `json:"cpu"`
	Disk   FitDisk   `json:"disk"`
	// Tone is the worst of the three — what a card can show in one glance.
	Tone Tone `json:"tone"`
}

type FitMemory struct {
	// Add is the entry's memory at rest, AddPeak when busy; After and
	// AfterPeak are the machine's in-use memory with each added. Bytes.
	Add       uint64 `json:"add"`
	AddPeak   uint64 `json:"addPeak"`
	After     uint64 `json:"after"`
	AfterPeak uint64 `json:"afterPeak"`
	Tone      Tone   `json:"tone"`
}

type FitCPU struct {
	// Load is the cores' worth of demand if every installed app and this
	// one were busy at once; Heavy names the installed apps that can hold
	// a core, so the listing can say who this one would be sharing with.
	Load  float64  `json:"load"`
	Heavy []string `json:"heavy"`
	Tone  Tone     `json:"tone"`
}

type FitDisk struct {
	// Need is the entry's images; FreeAfter is what the data disk would
	// have left once they are pulled. Bytes.
	Need      uint64 `json:"need"`
	FreeAfter uint64 `json:"freeAfter"`
	Tone      Tone   `json:"tone"`
}

// Thresholds, set to warn about what an operator would actually feel.
// Memory is the one resource that can really run out: an app that doesn't
// fit in RAM even at rest is over, and one that leaves the machine past
// seven eighths at rest — or whose busy peak would outgrow the RAM and
// lean on swap — is tight. The processor is time-shared, so it is never
// "over": busy apps take turns and get slower, they don't fail. A light
// app adds nothing worth a warning; a moderate or heavy one is tight when
// everything installed, busy at once, would ask for more than every core.
// Disk keeps a tenth free — a full data disk breaks every app at once,
// not just the new one.
const (
	memTightFrac  = 0.875
	memOverFrac   = 1.0
	diskKeepFrac  = 0.1
	diskKeepFloor = 5 * gb
)

// AssessFit answers whether the machine can take this entry, given what
// it is already carrying. Unknown host figures (a zero total) give no
// verdict for that resource rather than a wrong one: the tone stays ok
// and the numbers stay zero, and the UI says nothing it cannot back.
func AssessFit(h Host, installed []Installed, e Entry) Fit {
	fit := Fit{Tone: ToneOK}
	f := e.Footprint
	if f == nil {
		f = &Footprint{}
	}

	// Memory: what is in use now, as measured, plus this app.
	fit.Memory = FitMemory{Add: f.Memory, AddPeak: f.MemoryPeak, Tone: ToneOK}
	if h.MemTotal > 0 {
		fit.Memory.After = h.MemUsed + f.Memory
		fit.Memory.AfterPeak = h.MemUsed + f.MemoryPeak
		total := float64(h.MemTotal)
		switch {
		case float64(fit.Memory.After) > total*memOverFrac:
			fit.Memory.Tone = ToneOver
		case float64(fit.Memory.After) > total*memTightFrac || fit.Memory.AfterPeak > h.MemTotal:
			fit.Memory.Tone = ToneTight
		}
	}

	// Processor: every catalog app on the machine, running or not — a
	// stopped app is one start away from busy — plus this one.
	fit.CPU = FitCPU{Tone: ToneOK, Heavy: []string{}}
	load := cpuLoad(f.CPU)
	for _, in := range installed {
		if in.entry == nil || in.entry.Footprint == nil {
			continue
		}
		load += cpuLoad(in.entry.Footprint.CPU)
		if in.entry.Footprint.CPU == CPUHeavy {
			fit.CPU.Heavy = append(fit.CPU.Heavy, in.entry.Title)
		}
	}
	sort.Strings(fit.CPU.Heavy)
	fit.CPU.Load = load
	if h.Cores > 0 && f.CPU != "" && f.CPU != CPULight && load > float64(h.Cores) {
		fit.CPU.Tone = ToneTight
	}

	// Disk: the images have to land, and the disk has to stay usable.
	fit.Disk = FitDisk{Need: f.Disk, Tone: ToneOK}
	if h.DiskTotal > 0 {
		keep := uint64(float64(h.DiskTotal) * diskKeepFrac)
		if keep > diskKeepFloor {
			keep = diskKeepFloor
		}
		switch {
		case f.Disk > h.DiskFree:
			fit.Disk.Tone = ToneOver
		case h.DiskFree-f.Disk < keep:
			fit.Disk.FreeAfter = h.DiskFree - f.Disk
			fit.Disk.Tone = ToneTight
		default:
			fit.Disk.FreeAfter = h.DiskFree - f.Disk
		}
	}

	fit.Tone = worse(worse(fit.Memory.Tone, fit.CPU.Tone), fit.Disk.Tone)
	return fit
}
