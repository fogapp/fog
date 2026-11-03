# Testing the installer on Proxmox

(To *run* Fog on Proxmox rather than test the image, one command builds it
with no image to copy: as a VM, [proxmox-vm.md](proxmox-vm.md), or as an LXC
container, [proxmox-lxc.md](proxmox-lxc.md).)

Boot `fog-installer-x86_64.img` in a Proxmox VM: the image becomes a small
boot disk attached beside a blank target disk, and the wizard installs from
one to the other exactly as it would from a USB stick on metal. For the
real-hardware flow see [installer.md](installer.md).

The VM shape mirrors what `make installer-e2e` boots in QEMU: q35 machine,
OVMF (UEFI) firmware with Secure Boot off, virtio disk and NIC, 32 GB
target disk, 2 GB RAM minimum (4 GB is comfortable).

## 1. Copy the image to the Proxmox host

    scp imaging/out/fog-installer-x86_64.img root@<proxmox-host>:/var/lib/vz/images/

Optionally verify against `imaging/out/fog-installer-x86_64.img.sha256`
with `sha256sum` on the host.

## 2. Create the VM and import the installer

On the Proxmox host (substitute your storage for `local-lvm`, pick a free
VMID):

    VMID=200
    qm create $VMID --name fog-test --ostype l26 \
      --machine q35 --bios ovmf \
      --efidisk0 local-lvm:1,efitype=4m,pre-enrolled-keys=0 \
      --cpu host --cores 2 --memory 4096 \
      --scsihw virtio-scsi-single --scsi0 local-lvm:32 \
      --net0 virtio,bridge=vmbr0 --vga virtio

    # attach the installer image as a second disk — this is the "USB stick"
    qm set $VMID --scsi1 local-lvm:0,import-from=/var/lib/vz/images/fog-installer-x86_64.img

    # boot from the stick first
    qm set $VMID --boot order=scsi1

**The one setting that matters: `pre-enrolled-keys=0`.** Proxmox's default
EFI vars ship with Secure Boot keys pre-enrolled, and the image boots
systemd-boot directly (no shim), so Secure Boot would refuse it.

`scsi0` is the blank disk Fog installs onto; `scsi1` is the installer.
The web UI can create the VM (same firmware/machine settings) but image
import is CLI-only — the two `qm set` lines are needed either way.

## 3. Boot into the wizard

    qm start $VMID

The Proxmox console plays the role of the attached display: the kiosk
wizard appears there once the live environment is up. It is also at
`http://fog.local` from any LAN browser — the bridged NIC puts the VM on
the real LAN. If mDNS doesn't resolve, or another Fog machine already owns
the name (avahi then renames this one `fog-2.local`), use the VM's IP;
the wizard listens on port 80.

## 4. Run the install

Walk the wizard as a fresh install. It offers exactly one candidate disk —
the 32 GB `scsi0` — because the inventory excludes the medium it booted
from. No network is needed for the install itself.

## 5. "Pull the stick" and boot the installed system

When the wizard finishes, shut down and point the boot order at the
installed disk:

    qm set $VMID --boot order=scsi0
    qm start $VMID

Once satisfied, drop the installer disk entirely:

    qm set $VMID --delete scsi1
    qm disk unlink $VMID --idlist unused0

Verify the way the release gate does: the dashboard answers at
`http://fog.local:8080` (or the VM's IP, same port). From there it's a normal
Fog server.

## If something doesn't come up

- **Drops to a UEFI shell / "no bootable device"** — boot order isn't
  `scsi1`, or the EFI disk has pre-enrolled keys; check `qm config $VMID`.
  A wrong EFI disk is easiest to delete and recreate with
  `pre-enrolled-keys=0`.
- **Console stays blank after the boot menu** — the kiosk needs a
  KMS-capable display, hence `--vga virtio`. The wizard is fully usable
  from a LAN browser either way, so a blank console never blocks install.
- **`fog.local` doesn't resolve** — mDNS only crosses the local broadcast
  domain; a VPN, VLAN, or non-bridged network mode hides it. The VM's IP
  always works.
- **Rebuilt the image, want to retest** — copy the new `.img` over, delete
  `scsi1`, rerun the `import-from` line, and put the boot order back on
  `scsi1`. The VM shell and target disk are reusable: the installer recognizes
  the previous install, and **Install Fog again** on that screen overwrites it.
