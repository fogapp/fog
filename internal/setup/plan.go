package setup

import (
	"fmt"
	"path/filepath"
	"strings"
)

// The installed disk's shape. Sizes in MiB. The recovery partition carries a
// complete copy of the installer (live environment + OS payload), which is
// what makes the machine repairable and reinstallable with no USB stick and
// no network — the resilience ladder's rung 3.
type PartitionPlan struct {
	DiskMB     int64
	ESPMB      int64
	RecoveryMB int64
	RootMB     int64
	// DataMB is the remainder: fog state, app volumes, backups staging.
	DataMB int64
}

// MinDiskBytes: below this the machine can't hold the OS, the recovery
// copy, and meaningful app data. Validate() enforces it.
const MinDiskBytes = 24 << 30

func PlanPartitions(diskBytes int64) (PartitionPlan, error) {
	if diskBytes < MinDiskBytes {
		return PartitionPlan{}, fmt.Errorf("disk is %d GB — Fog needs at least %d GB", diskBytes>>30, MinDiskBytes>>30)
	}
	p := PartitionPlan{
		DiskMB:     diskBytes >> 20,
		ESPMB:      1024,
		RecoveryMB: 6144,
		RootMB:     32768,
	}
	// The OS partition only grows by logs, apt caches and whatever lands
	// outside /var/lib/fog, but running out of it stops self-updates cold —
	// so it gets headroom wherever the disk can spare it, stepping down only
	// where 32 GB would be a real bite out of app data.
	switch {
	case diskBytes < 48<<30:
		p.RootMB = 10240
	case diskBytes < 128<<30:
		p.RootMB = 16384
	}
	// 1 MiB alignment at the front, 1 MiB slack at the tail for GPT backup.
	p.DataMB = p.DiskMB - p.ESPMB - p.RecoveryMB - p.RootMB - 2
	// The remainder is the whole point of the machine — app data. This floor
	// is what MinDiskBytes exists to guarantee; if the fixed partitions ever
	// grow past it, fail here instead of shipping a data-starved server.
	if p.DataMB < 4096 {
		return PartitionPlan{}, fmt.Errorf("only %d MiB would remain for app data after the OS partitions", p.DataMB)
	}
	return p, nil
}

// SfdiskScript renders the plan for `sfdisk <disk>`. Labels, not device
// paths, are how everything downstream finds these partitions — device
// names change between boots, labels don't.
func (p PartitionPlan) SfdiskScript() string {
	var b strings.Builder
	b.WriteString("label: gpt\n")
	fmt.Fprintf(&b, "start=1MiB, size=%dMiB, type=uefi, name=\"fog-esp\"\n", p.ESPMB)
	fmt.Fprintf(&b, "size=%dMiB, type=linux, name=\"fog-recovery\"\n", p.RecoveryMB)
	fmt.Fprintf(&b, "size=%dMiB, type=linux, name=\"fog-root\"\n", p.RootMB)
	b.WriteString("type=linux, name=\"fog-data\"\n")
	return b.String()
}

// PartitionPath names partition n of a disk, handling the nvme/mmc "p"
// convention (/dev/nvme0n1p3) vs plain (/dev/sda3).
func PartitionPath(disk string, n int) string {
	base := filepath.Base(disk)
	if len(base) > 0 && base[len(base)-1] >= '0' && base[len(base)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

// Fstab for the installed system. The ESP mounts at /boot so Debian kernel
// updates land where systemd-boot reads them; the data partition mounts at
// /var/lib/fog so everything precious lives on its own filesystem and the
// OS root stays disposable.
func Fstab() string {
	return `# Written by the Fog installer. Labels, not devices — see fog-recovery docs.
LABEL=fog-root  /            ext4  defaults,errors=remount-ro  0 1
LABEL=FOG-ESP   /boot        vfat  umask=0077                  0 2
LABEL=fog-data  /var/lib/fog ext4  defaults                    0 2
`
}

// RecoveryLoaderEntry is the boot menu's last resort: when every OS entry
// has exhausted its boot-counting tries, systemd-boot lands here and the
// machine comes up as its own repair tool. sort-key keeps it below every
// kernel entry in normal times.
func RecoveryLoaderEntry() string {
	return `title Fog Recovery
sort-key zzz-recovery
linux /fog-recovery/vmlinuz
initrd /fog-recovery/initrd.img
options boot=live fog.repair=1 quiet loglevel=3
`
}

// LoaderConf: no default line — systemd-boot picks the best (newest, still
// counted) entry, which is exactly the boot-counting behavior we want.
func LoaderConf() string {
	return "timeout 0\neditor no\nconsole-mode max\n"
}
