package catalog

import (
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

// Every entry says what it costs, and says it in the vocabulary the fit
// understands.
func TestFootprintsComplete(t *testing.T) {
	ids := map[string]bool{}
	for _, e := range Entries() {
		ids[e.ID] = true
		f := e.Footprint
		if f == nil {
			t.Errorf("%s: no footprint", e.ID)
			continue
		}
		if f.Memory == 0 || f.MemoryPeak < f.Memory || f.Disk == 0 {
			t.Errorf("%s: footprint needs memory, a peak at least as large, and a disk size (got %+v)", e.ID, *f)
		}
		switch f.CPU {
		case CPULight, CPUModerate, CPUHeavy:
		default:
			t.Errorf("%s: cpu class %q is not light, moderate, or heavy", e.ID, f.CPU)
		}
		if f.CPU == CPUHeavy && f.CPUWhen == "" {
			t.Errorf("%s: a heavy app says when it is heavy", e.ID)
		}
	}
	for id := range footprints {
		if !ids[id] {
			t.Errorf("footprints names %q, which is not a catalog entry", id)
		}
	}
}

func TestEntryForSpec(t *testing.T) {
	entries := Resolve(Entries(), exampleSources)
	if e := EntryForSpec(entries, "jellyfin", nil); e == nil || e.ID != "jellyfin" {
		t.Errorf("by name: got %v", e)
	}
	// Renamed at install, same image, a pinned tag: still Jellyfin.
	spec := &apps.AppSpec{Source: apps.Source{Type: "image", Image: "lscr.io/linuxserver/jellyfin:10.10.3"}}
	if e := EntryForSpec(entries, "tv", spec); e == nil || e.ID != "jellyfin" {
		t.Errorf("by image: got %v", e)
	}
	git := &apps.AppSpec{Source: apps.Source{Type: "git", Repo: "ssh://git@git.example.com/you/hub.git"}}
	if e := EntryForSpec(entries, "home", git); e == nil || e.ID != "hub" {
		t.Errorf("by repo: got %v", e)
	}
	other := &apps.AppSpec{Source: apps.Source{Type: "image", Image: "nginx:latest"}}
	if e := EntryForSpec(entries, "web", other); e != nil {
		t.Errorf("a hand-built app matched %s", e.ID)
	}
}

func byID(id string) Entry {
	for _, e := range Entries() {
		if e.ID == id {
			return e
		}
	}
	panic("no entry " + id)
}

func TestAssessFitMemory(t *testing.T) {
	jellyfin := byID("jellyfin") // 400 MB at rest, 2 GB busy
	host := Host{MemTotal: 8 * gb, MemUsed: 2 * gb, Cores: 8, DiskTotal: 500 * gb, DiskFree: 400 * gb}

	fit := AssessFit(host, nil, jellyfin)
	if fit.Memory.Tone != ToneOK || fit.Tone != ToneOK {
		t.Errorf("2 GB used of 8: expected ok, got %+v", fit)
	}
	if fit.Memory.After != 2*gb+400*mb || fit.Memory.AfterPeak != 2*gb+2000*mb {
		t.Errorf("after figures wrong: %+v", fit.Memory)
	}

	host.MemUsed = 5 * gb // 5.4 GB after, peak 7 GB: still room
	if fit := AssessFit(host, nil, jellyfin); fit.Memory.Tone != ToneOK {
		t.Errorf("5.4 GB of 8 with the peak inside RAM: expected ok, got %s", fit.Memory.Tone)
	}
	host.MemUsed = 6*gb + 200*mb // peak 8.2 GB leans on swap: tight, not over
	if fit := AssessFit(host, nil, jellyfin); fit.Memory.Tone != ToneTight {
		t.Errorf("peak past the machine: expected tight, got %s", fit.Memory.Tone)
	}
	host.MemUsed = 7*gb + 700*mb // 8.1 GB at rest doesn't fit in RAM: over
	if fit := AssessFit(host, nil, jellyfin); fit.Memory.Tone != ToneOver {
		t.Errorf("at rest past the machine: expected over, got %s", fit.Memory.Tone)
	}
	// The reported case: 4.9 GB of 7.8 in use, a 50 MB app.
	small := Host{MemTotal: 7800 * mb, MemUsed: 4900 * mb, Cores: 4, DiskTotal: 60 * gb, DiskFree: 17 * gb}
	if fit := AssessFit(small, nil, byID("vaultwarden")); fit.Tone != ToneOK {
		t.Errorf("vaultwarden on a 63%%-used box: expected ok, got %+v", fit)
	}
	// Unknown host: no verdict, not a wrong one.
	if fit := AssessFit(Host{}, nil, jellyfin); fit.Memory.Tone != ToneOK || fit.Memory.After != 0 {
		t.Errorf("unknown host should stay ok with no figures, got %+v", fit.Memory)
	}
}

func TestAssessFitCPU(t *testing.T) {
	host := Host{MemTotal: 64 * gb, MemUsed: 4 * gb, Cores: 4, DiskTotal: 1000 * gb, DiskFree: 900 * gb}
	immich, paperless, sab := byID("immich"), byID("paperless"), byID("sabnzbd")
	installed := []Installed{
		NewInstalled("immich", &immich, true, gb),
		NewInstalled("paperless", &paperless, false, 0), // stopped still counts: one start from busy
		NewInstalled("web", nil, true, 100*mb),          // hand-built: unknown, uncounted
	}
	fit := AssessFit(host, installed, sab) // three heavy apps on four cores
	if fit.CPU.Tone != ToneOK || fit.CPU.Load != 3 {
		t.Errorf("3 heavy on 4 cores: expected ok, load 3; got %+v", fit.CPU)
	}
	if len(fit.CPU.Heavy) != 2 || fit.CPU.Heavy[0] != "Immich" || fit.CPU.Heavy[1] != "Paperless-ngx" {
		t.Errorf("heavy list: %v", fit.CPU.Heavy)
	}
	host.Cores = 2
	if fit := AssessFit(host, installed, sab); fit.CPU.Tone != ToneTight {
		t.Errorf("3 heavy on 2 cores: expected tight (never over), got %s", fit.CPU.Tone)
	}
	// A light app never trips the processor, however busy the box is.
	if fit := AssessFit(host, installed, byID("vaultwarden")); fit.CPU.Tone != ToneOK {
		t.Errorf("light app on a crowded box: expected ok, got %s", fit.CPU.Tone)
	}
	if fit := AssessFit(host, nil, byID("ntfy")); fit.CPU.Tone != ToneOK {
		t.Errorf("one light app on an empty box: expected ok, got %s", fit.CPU.Tone)
	}
}

func TestAssessFitDisk(t *testing.T) {
	whisper := byID("whisper") // 6 GB of images
	host := Host{MemTotal: 64 * gb, MemUsed: 4 * gb, Cores: 16, DiskTotal: 100 * gb, DiskFree: 50 * gb}
	if fit := AssessFit(host, nil, whisper); fit.Disk.Tone != ToneOK || fit.Disk.FreeAfter != 50*gb-6000*mb {
		t.Errorf("50 GB free: expected ok with 44 GB after, got %+v", fit.Disk)
	}
	host.DiskFree = 9 * gb // 3 GB left after: under the 5 GB floor
	if fit := AssessFit(host, nil, whisper); fit.Disk.Tone != ToneTight {
		t.Errorf("3 GB left: expected tight, got %s", fit.Disk.Tone)
	}
	host.DiskFree = 4 * gb
	if fit := AssessFit(host, nil, whisper); fit.Disk.Tone != ToneOver || fit.Tone != ToneOver {
		t.Errorf("images don't fit: expected over, got %+v", fit)
	}
}
