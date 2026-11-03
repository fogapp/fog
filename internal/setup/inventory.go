// Package setup is the brain of the bootable installer: hardware inventory,
// the staged install engine, and the HTTP surface the wizard UI talks to —
// the same API whether the wizard renders on the machine's own display or in
// a browser at http://fog.local.
package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/execx"
)

type Disk struct {
	Path      string `json:"path"`  // /dev/nvme0n1
	Model     string `json:"model"` // "Samsung SSD 970 EVO 500GB"
	SizeBytes int64  `json:"sizeBytes"`
	Bus       string `json:"bus"` // nvme | sata | usb | …
	Removable bool   `json:"removable"`
	// BootMedium marks the drive the installer itself booted from — never a
	// valid install target.
	BootMedium bool `json:"bootMedium"`
	// FogDisk marks a drive already carrying an installed Fog system (all
	// four of its partition labels). Erasing it is still allowed — this is
	// what lets the wizard name what it would destroy.
	FogDisk bool `json:"fogDisk"`
	// Contents is the honest one-line description shown before the erase
	// confirmation: "empty", "an existing Linux system (ext4)", …
	Contents string `json:"contents"`

	// espDev is the FOG-ESP partition of a FogDisk, where the completed-
	// install marker lives. Internal: the wizard reads the marker's
	// contents (Inventory.Existing), never a device path to mount.
	espDev string
}

type Network struct {
	Online    bool   `json:"online"`
	Interface string `json:"interface,omitempty"`
	IP        string `json:"ip,omitempty"`
}

// ExistingInstall is a finished Fog installation found on one of this
// machine's own disks, read from the marker the install wrote. Its whole
// job is the "you left the stick in" case: the machine is already a server,
// and the wizard must say so rather than open with an offer to erase it.
type ExistingInstall struct {
	Disk        string `json:"disk"`
	ServerName  string `json:"serverName,omitempty"`
	Mode        string `json:"mode,omitempty"` // how it was installed: fresh | restore
	Version     string `json:"version,omitempty"`
	InstalledAt string `json:"installedAt,omitempty"`
}

type Inventory struct {
	Disks []Disk `json:"disks"`
	// Candidate is the auto-picked install target: set only when the choice
	// is unambiguous (exactly one internal disk). Otherwise the wizard shows
	// a picker.
	Candidate string  `json:"candidate"`
	Network   Network `json:"network"`
	Machine   string  `json:"machine,omitempty"` // DMI product name, for the "installing on" line
	// Existing is set when this machine's own disk already holds a finished
	// Fog install. nil is the ordinary case, and the only one the wizard
	// treats as "nothing here yet".
	Existing *ExistingInstall `json:"existing,omitempty"`
}

// bootMediumKnown reports whether any disk was identified as the installer's
// own boot medium — when none was, removable targets are refused (Validate).
func (inv Inventory) bootMediumKnown() bool {
	for _, d := range inv.Disks {
		if d.BootMedium {
			return true
		}
	}
	return false
}

// pickCandidate applies the disk policy: auto-pick only when exactly one
// internal (non-removable, non-boot-medium) disk exists.
func pickCandidate(disks []Disk) string {
	var internal []Disk
	for _, d := range disks {
		if !d.Removable && !d.BootMedium {
			internal = append(internal, d)
		}
	}
	if len(internal) == 1 {
		return internal[0].Path
	}
	return ""
}

// ---- live inventory (Linux, via lsblk) ----

type lsblkDevice struct {
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	Size     int64         `json:"size"`
	Model    string        `json:"model"`
	Tran     string        `json:"tran"`
	Rm       bool          `json:"rm"`
	Fstype   string        `json:"fstype"`
	Label    string        `json:"label"`
	Children []lsblkDevice `json:"children"`
}

// hasLabel reports whether the device or any of its partitions carries the
// filesystem label.
func hasLabel(dev lsblkDevice, label string) bool {
	if dev.Label == label {
		return true
	}
	for _, c := range dev.Children {
		if hasLabel(c, label) {
			return true
		}
	}
	return false
}

type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

// parseLsblk turns `lsblk -Jb -o NAME,TYPE,SIZE,MODEL,TRAN,RM,FSTYPE` output
// into disks. bootDisk names the device the live environment mounted its own
// medium from ("" when unknown).
func parseLsblk(raw []byte, bootDisk string) ([]Disk, error) {
	var out lsblkOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lsblk output: %w", err)
	}
	var disks []Disk
	for _, dev := range out.Blockdevices {
		if dev.Type != "disk" {
			continue
		}
		// Read-only loop/zram style devices don't show as type=disk; CD-ROMs
		// do on some machines — skip anything without a size.
		if dev.Size <= 0 {
			continue
		}
		d := Disk{
			Path:      "/dev/" + dev.Name,
			Model:     strings.TrimSpace(dev.Model),
			SizeBytes: dev.Size,
			Bus:       dev.Tran,
			Removable: dev.Rm || dev.Tran == "usb",
			// The boot medium is recognized two ways: by the disk name the
			// live environment recorded, and — independent of that plumbing —
			// by the stick's own FOG-BOOT label, so a failed recording can
			// never turn the installer's stick into an offerable target.
			BootMedium: (bootDisk != "" && dev.Name == bootDisk) || hasLabel(dev, "FOG-BOOT"),
			FogDisk:    isFogDisk(dev),
		}
		if d.FogDisk {
			d.espDev = partitionByLabel(dev, "FOG-ESP")
		}
		d.Contents = describeContents(dev)
		disks = append(disks, d)
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].SizeBytes > disks[j].SizeBytes })
	return disks, nil
}

// isFogDisk reports whether a disk carries the full set of an installed Fog
// system's labels. A partial set is left out on purpose: it is what an
// install that died between mkfs and the first file copy leaves behind, and
// that is not a Fog server.
func isFogDisk(dev lsblkDevice) bool {
	for _, label := range fogLabels {
		if !hasLabel(dev, label) {
			return false
		}
	}
	return true
}

// partitionByLabel names the device holding one of a disk's labelled
// partitions ("" when the disk doesn't carry it).
func partitionByLabel(dev lsblkDevice, label string) string {
	for _, c := range dev.Children {
		if c.Label == label {
			return "/dev/" + c.Name
		}
		if p := partitionByLabel(c, label); p != "" {
			return p
		}
	}
	return ""
}

// findExisting returns the finished Fog install on one of this machine's own
// disks. readMarker reads the completed-install marker off a disk's ESP; a
// disk carrying the labels but no readable marker is deliberately not
// reported — the wizard must not call a half-written disk a server.
func findExisting(disks []Disk, readMarker func(espDev string) *InstallMarker) *ExistingInstall {
	for _, d := range disks {
		if !d.FogDisk || d.BootMedium || d.espDev == "" {
			continue
		}
		m := readMarker(d.espDev)
		if m == nil {
			continue
		}
		return &ExistingInstall{
			Disk:        d.Path,
			ServerName:  m.ServerName,
			Mode:        m.Mode,
			Version:     m.Version,
			InstalledAt: m.InstalledAt,
		}
	}
	return nil
}

// describeContents summarizes what an erase would destroy, in the operator's
// terms, from partition filesystems.
func describeContents(dev lsblkDevice) string {
	if len(dev.Children) == 0 {
		if dev.Fstype != "" {
			return "a " + dev.Fstype + " filesystem"
		}
		return "empty"
	}
	if isFogDisk(dev) {
		return "an existing Fog server"
	}
	seen := map[string]bool{}
	var kinds []string
	for _, part := range dev.Children {
		fs := part.Fstype
		if fs == "" || seen[fs] {
			continue
		}
		seen[fs] = true
		kinds = append(kinds, fs)
	}
	switch {
	case seen["ntfs"]:
		return "an existing Windows system"
	case seen["ext4"] || seen["btrfs"] || seen["xfs"]:
		return "an existing Linux system (" + strings.Join(kinds, ", ") + ")"
	case len(kinds) == 0:
		return fmt.Sprintf("%d unformatted partition(s)", len(dev.Children))
	default:
		return "data (" + strings.Join(kinds, ", ") + ")"
	}
}

// probeTimeout bounds every command the inventory shells out to. The
// installer's audience is flaky hardware: a drive hanging in D-state can
// wedge lsblk (or a probe mount) forever, and without a deadline one bad
// disk freezes the whole inventory — and the UI polling it — with no
// diagnosis.
const probeTimeout = 10 * time.Second

// runProbe runs one inventory probe with a hard deadline. WaitDelay makes
// the call return even when the child is unkillable (D-state ignores
// SIGKILL), so a stuck probe costs one timeout instead of a goroutine — and
// its caller's lock — held forever. A timeout comes back as an error that
// says what hung.
func runProbe(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := execx.Command(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s did not answer within %s — a drive may be failing to respond; try unplugging recently added disks", name, probeTimeout)
	}
	return out, err
}

// LiveInventory shells out to lsblk on the running (Linux) live environment.
func LiveInventory() (Inventory, error) {
	raw, err := runProbe("lsblk", "-Jb", "-o", "NAME,TYPE,SIZE,MODEL,TRAN,RM,FSTYPE,LABEL")
	if err != nil {
		return Inventory{}, fmt.Errorf("lsblk: %w", err)
	}
	disks, err := parseLsblk(raw, bootDiskName())
	if err != nil {
		return Inventory{}, err
	}
	inv := Inventory{
		Disks:     disks,
		Candidate: pickCandidate(disks),
		Network:   liveNetwork(),
		Machine:   dmiProductName(),
		Existing:  findExisting(disks, readInstallMarker),
	}
	return inv, nil
}
