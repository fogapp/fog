# Installing Fog from USB

The bootable installer turns a blank (or sacrificial) x86-64 UEFI machine
into a Fog server in a few minutes, offline. For the design and internals
see [imaging/README.md](../imaging/README.md).

## Install

1. **Get the image** — `fog-installer-x86_64.img` (+ `.sha256`) from the
   release, or `make installer-image` from source. `site/download/` is the
   shareable how-to page published beside each release.
2. **Flash** — balenaEtcher, Rufus (DD mode), `dd bs=4M`, or `imaging/flash.sh`,
   which downloads it, checks it against the signed release manifest
   (signature against fog's release key, an issue date within 180 days,
   then the image's sha256 — once after download and again on the bytes as
   they are written), and writes it for you. Given a release directory's manifest, it finds the
   image beside it.
3. **Boot the server from the stick** — wired Ethernet recommended. With a
   display attached the wizard appears on it; either way it's at
   `http://fog.local` from any browser on the LAN.
4. **Follow the wizard** — new server or kit restore; disk with explicit
   erase consent; name + account; a few minutes of progress; restart. The
   browser follows the reboot and signs you in (fresh installs) or lands on
   the restored server's login (restores).
5. **Unplug the stick during that restart.** Left in, the machine boots the
   installer a second time — it reads the marker a finished install writes
   to the ESP, opens on "this machine is already a Fog server", and asks
   for the stick to come out and the machine to restart. It never offers to
   erase the server it just built without being asked twice.

The stick stays useful: it is also the disaster-recovery tool for the
machine (or its replacement).

## What lands on the disk

| Partition | Label | Holds |
|---|---|---|
| 1 (1 GB) | `FOG-ESP` | systemd-boot + kernels + `fog-install.json`, mounted at `/boot` |
| 2 (6 GB) | `fog-recovery` | read-only copy of the installer: live env + OS payload |
| 3 (10–32 GB) | `fog-root` | Debian 13 + Docker + fogd — disposable, rebuildable from p2 |
| 4 (rest) | `fog-data` | `/var/lib/fog`: Fog state, app volumes, Docker data-root — the precious part |

The OS is stock apt-managed Debian; Fog manages its own updates as before.

The image is preconfigured so an app can't take the machine down with it:
journald and Docker logs are capped, earlyoom kills an app container before
fogd, Docker, or sshd, and the data partition keeps apps from filling the OS.
Wired interfaces fall back to IPv4 link-local, so `<name>.local` answers even
on a direct cable to a laptop with no router.

## The machine's own console

A monitor plugged into the installed server shows its name, the URLs that
reach it, and whether Fog is healthy — printed above an ordinary Debian
login prompt on tty1. The account created in the wizard logs in there for a
bash shell with `sudo` and `docker`, exactly as over SSH. The live installer
(and the recovery environment it becomes) skips the prompt: its console is a
root shell, since whoever holds the stick can already erase the disk.

Moving the installed server to a different network later is supported —
[moving-house.md](moving-house.md) covers what heals itself and the little
that doesn't.

## When something goes wrong

The design rule: **hard-restart the machine.** Each restart descends an
escalation ladder that needs nothing from you:

1. Power loss → filesystems repair on boot; fogd verifies its database and
   posts an honest "recovered" notice.
2. A hung or crashing fogd → systemd watchdog restarts it (fogd heartbeats
   only while its database answers, so a hung one counts); a hung kernel →
   the hardware watchdog reboots the machine after 30 seconds. A
   self-update that doesn't come up healthy is undone by itself: see
   below.
3. A kernel/OS update that can't boot → systemd-boot counts three tries
   (a try only succeeds when Fog itself answers) and falls back to the
   previous kernel by itself.
4. Still broken → the machine boots **Fog Recovery** from its own disk:
   the repair menu on the attached display and at `http://fog.local` —
   filesystem check, app-runtime rebuild (keeps all volumes), OS reinstall
   (keeps all data), diagnostics download. Every screen says what is safe.
   Its last option, **Drop to the command line**, is the safe mode: the
   screen hands over to a root shell on the machine's console with nothing
   from the disk mounted, for whatever the menu doesn't cover
   (`systemctl start fog-kiosk` brings the menu back).
5. Dead disk → new disk + the USB stick + your recovery kit and app
   backups (see [disaster-recovery.md](disaster-recovery.md)).

Recovery can also be entered on purpose from the boot menu (hold the
boot-menu key, pick "Fog Recovery").

## When a self-update doesn't start

Before fogd swaps in a new version it keeps the running binary as
`/usr/local/bin/fogd.prev` and writes `/usr/local/bin/fogd.update-pending`:
both versions, both binaries' sha256, and a deadline five minutes out. The
new fogd deletes that marker once it has answered its own health check (its
database, and `/api/setup/status` on its own listener) continuously for 30
seconds. If the marker is still there at the deadline, the new version
never got healthy — it couldn't start, crashed in a loop, or hung — and
`/usr/local/bin/fogd-rollback`, a small shell script that needs nothing from
either binary, puts `fogd.prev` back. It runs from two places, so either
alone is enough:

- **before every start of fogd** (`ExecStartPre=-` in `fogd.service`): a
  crash-looping fogd is restarted every few seconds, and the first start
  after the deadline starts the previous binary instead;
- **a one-shot systemd timer** the updater arms (`systemd-run`) for the
  deadline, which also catches a fogd that hangs without being restarted,
  and a server whose unit predates the `ExecStartPre` line. If the timer
  can't be armed, the update doesn't go ahead.

The restored fogd posts the failure to the Inbox — which version failed,
and its last log lines — and marks that version ignored, so the update
prompt stops offering it; Settings › Server › Updates can still apply it on
purpose. It can't loop: the marker is claimed before anything is restored,
so an update is rolled back at most once, nothing re-applies an update by
itself, and the guard only ever swaps the exact pair of binaries the marker
names (a fogd installed by hand since, or a `fogd.prev` that isn't the
replaced binary, is left alone). Only the binary is rolled back: if the
failed version changed the database before it stopped, those changes stay.

`install.sh` does the same check itself when it replaces a fogd: if the new
one doesn't start, it puts the previous binary back and says so.

### The unit files follow the binary

A self-update brings fog's own unit files along: `fogd.service` (with its
hardening and the `ExecStartPre` guard) and `fogd.socket`, the same files
the signed manifest lists, built into the verified binary. They change only
after the new binary has proven itself on the old ones, as a trial of their
own: fogd stages them, arms the guard, and hands the change to
`fogd-rollback --install-units` outside its sandbox (fogd can't write
`/etc`). That backs up the installed files, installs the new ones, keeps
every drop-in as it was — the install source, the branch, a Proxmox port
(the socket is given the same port when `FOG_LISTEN` isn't 8080, whether
it is set in an `Environment=` drop-in or an `EnvironmentFile=`; `install.sh`
does the same on a re-run) —
reloads systemd, enables the socket, then stops fogd, starts the socket on
the freed port, and starts fogd on it. If fogd isn't healthy on them, with
the socket active, by the deadline, the previous unit files, socket and
port drop-in are put back, fogd runs on them, the Inbox says so, and those
unit files aren't tried again. So a server installed before any of this —
the owner's included — gets the socket, the hardening and the guard on its
first self-update to a release that has them, without re-running
`install.sh`. A unit at a custom path, or one that doesn't run
`/usr/local/bin/fogd`, is left alone.

### fog's port is never free

`fogd.socket` holds fog's port (8080) for fogd: systemd binds it and hands
it to each fogd as it starts, so between one fogd and the next — a
self-update's restart, a crash, the watchdog — the port stays bound, and
nothing else on the machine can take it, block the new version, or answer
in its place. fogd binds `FOG_LISTEN` itself only when started without the
socket (a server installed before it, until its next self-update installs
it, or `install.sh` is re-run). With
the socket in place, `systemctl stop fogd` leaves the port open and the
next request starts fogd again; `systemctl stop fogd.socket fogd` stops
both. If a rollback was caused by another process taking the port (only
possible without the socket), the Inbox says so and the release is not
marked ignored.

## Testing the image

`make installer-e2e` boots the image in QEMU, installs onto a virtual disk
through the real wizard API, reboots into the installed disk, asserts the
dashboard answers with the preseeded account, then boots the stick again
beside that disk and asserts the installer recognizes the server it built.
`FOG_RELEASE_IMAGE=1 ./scripts/release.sh vX.Y.Z` runs this gate before any
image is published.
