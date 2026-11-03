# Fog

A metalayer for a home server: install Fog on a fresh box, then install, run,
update, back up, and manage every other application through its web UI — your
own git-repo apps and third-party container apps alike.

## Principles

1. **Everything through the UI** — Fog is the system of record for all app config.
2. **No hidden state** — Fog materializes plain compose files and a Caddyfile; stop
   Fog and everything keeps running.
3. **One app model, many sources** — git-built and image-pulled apps are the same object.
4. **Boring where it matters** — self-update, migrations, and backups are conservative.
5. **One box, one household** — Fog runs on one server. Other machines can lend it
   their hardware, and other households can pair with it, but each Fog is its own.

## What it does

- **Apps** — one declarative spec for image and git sources. Materialized
  compose files, config revisions with rollback, live logs, a web terminal,
  health checks, push-to-deploy, blue-green swaps, worker processes, and
  update polling with manual, notify, or auto policies. Add an app from the
  app store, a repo, or a compose file.
- **Network** — Caddy with wildcard TLS from one domain, a sign-in gate for any
  route, remote access by port forward, Cloudflare Tunnel, or Tailscale, and
  [sealed apps and LAN-only routes](docs/sealed-network.md).
- **Storage** — NAS shares, a managed Postgres shared across apps, and
  **Files**: a plain directory tree with a Finder-grade browser, share links,
  a WebDAV mount, and hardlink snapshots. Apps keep their libraries in
  `Files/Apps/<Name>`.
- **Backups** — nightly backups with retention and restore, a recovery kit for
  Fog itself, and encrypted offsite copies in object storage.
- **People** — household accounts with passkeys, and Fog as the
  [OpenID provider](docs/signin-contract.md) for the apps, so one person signs
  in everywhere.
- **Chat** — [end-to-end encrypted rooms](docs/chat-contract.md) that the server
  can't read, within a household and across paired ones.
- **Connect** — [federation](docs/federation-contract.md) between paired Fogs:
  shared folders, chat, visitors, and error forwarding, plus
  [credentials between sibling apps](docs/connect-contract.md).
- **Compute** — [other machines lend their hardware](docs/compute-contract.md) to
  apps, as transcoders and inference endpoints that Fog provides.
- **Operations** — one Inbox for errors, issues, and notices, Sentry-compatible
  [error tracking](clients/flare/README.md), notifications, an activity log,
  checksum-verified self-update, and an [MCP server](internal/mcpserver) so an AI
  assistant can read and manage the box. ⌘K opens the command palette.

## Install

The commands below fetch from this project's own Gitea, which is only reachable
on its home network. To install from anywhere else, host the release files
yourself and set `FOG_SOURCE`, the one value every install path and self-update
takes: a repo URL, a directory of release files, or a `latest.json`. See
[docs/hosting-the-source.md](docs/hosting-the-source.md).

**Bootable USB (recommended).** Flash `fog-installer-x86_64.img` to a USB stick,
boot the machine from it, and follow the wizard, on the attached display or from
any browser at `http://fog.local`. See [docs/installer.md](docs/installer.md), or
[docs/from-repo-to-server.md](docs/from-repo-to-server.md) for the complete
build-flash-boot walkthrough from this repo.

**Proxmox VE host.** Paste this into a node's shell:

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/fogapp/fog/main/proxmox-vm.sh)"
```

It creates a Debian 13 VM with Docker and Fog inside and prints the dashboard URL
— see [docs/proxmox-vm.md](docs/proxmox-vm.md). For a lighter LXC container with
no NAS mounts of its own, use `proxmox-lxc.sh` the same way:
[docs/proxmox-lxc.md](docs/proxmox-lxc.md). Every value is a flag or an
environment variable, so either also runs unattended.

**Existing Linux server.**

```sh
curl -fsSL https://raw.githubusercontent.com/fogapp/fog/main/install.sh | sh
```

This installs `/usr/local/bin/fogd` and a systemd unit, creates `/var/lib/fog`,
and starts the first-run wizard on port 8080.

## Development

Requirements: Go 1.26+, Node 22.18+, and Docker for running apps.

```sh
make dev-backend    # run fogd against ./.devdata on :8080
make dev-frontend   # vite dev server on :5173, proxies /api → :8080
make build          # full production build (frontend + static binary)
make test           # go tests
make check          # everything CI runs; releases gate on this
make gen-types      # regenerate TypeScript types from the Go structs
```

| Binary | Purpose |
| --- | --- |
| `fogd` | The server: API, embedded React UI, and everything it manages |
| `fog-setup` | The bootable installer's agent and wizard |
| `fog-console` | Keeps the machine's own display useful |
| `fog-node` | Lends another machine's hardware to Fog |
| `fog-locker` | Reads an offsite locker from any machine, without Fog |

The `clients/` directory holds what apps use to integrate:
[fog-client](clients/fog-client/README.md) (Ruby),
[flare](clients/flare/README.md) (error reporting),
[fogfed](clients/fogfed/README.md) (the consent lens), and
[chat-sdk](clients/chat-sdk/README.md) (TypeScript).

## Documentation

**Security:** what Fog protects, how much has been checked, and how to report
a problem — [SECURITY.md](SECURITY.md).

**Running a server**

- [Disaster recovery](docs/disaster-recovery.md)
- [Remote access](docs/remote-access.md) and [away access](docs/vpn.md)
- [Moving to another network](docs/moving-house.md)
- [Upgrading the shared Postgres](docs/postgres-upgrade.md)
- [Hosting the source elsewhere](docs/hosting-the-source.md)
- [Testing the installer on Proxmox](docs/proxmox-testing.md)

**Building an app for Fog** — the contracts an app can implement:

- [fog-backup/1](docs/backup-contract.md): delegated backups
- [fog-signin/1](docs/signin-contract.md): household sign-in
- [fog-files/1](docs/files-contract.md): folders from Files
- [fog-connect/1](docs/connect-contract.md): sibling apps across households
- [fog-integration/1](docs/integrations-contract.md): apps on the same box
- [fog-compute/1](docs/compute-contract.md): borrowed hardware
- [fog-widgets/1](docs/widgets-contract.md): start page widgets and search
- [The event stream](docs/events-contract.md) and
  [the home view](docs/home-view.md)
- [Sealed apps and LAN-only routes](docs/sealed-network.md)

**Protocols and formats**

- [fog-federation/1](docs/federation-contract.md): Fog to Fog
- [fog-chat/1](docs/chat-contract.md): rooms across households
- [fog-locker/1](docs/locker-contract.md): the offsite layout
- [The consent grammar](docs/consent-grammar.md): what a share promises
- [Fog identity](docs/design/fog-identity.md): the visual design spec

Earlier design proposals are in the git history.
