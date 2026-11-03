# Security

Fog runs a household's apps, files, backups, sign-in and chat on one
machine, and talks to other households' Fogs. This page says what Fog is
designed to protect, how much of that has been checked, and where the limits
are. Each area's own document has the detail; this is the map.

## Reporting a vulnerability

Please report security problems privately, through GitHub's
**Report a vulnerability** button on this repository's Security tab, not
in a public issue. Include what you found, how to reproduce it, and which
version or commit you tested. You'll get an acknowledgement, and a fix or an
honest answer about why something won't change.

## How much this has been checked

Every guarantee below has been reviewed adversarially and repeatedly by the
people who wrote the code, and each fix carries a test that performs the
attack it closes. **None of it has had an independent external audit.** In
particular:

- **Chat's end-to-end encryption** (MLS, RFC 9420) has not been externally
  audited. Treat its guarantees as *designed and tested so the server can't
  read your rooms*, not as audited, and don't rely on it against a
  well-resourced adversary until it has been.
- **Sign-in** follows OAuth 2.0 security best practice (RFC 9700) but has not
  been run against the OpenID Foundation conformance suite.

If you depend on any of this for something that matters, an external review
of that area is the right next step.

## What Fog protects, and where to read more

| Area | What it is meant to guarantee | Details and known limits |
| --- | --- | --- |
| **Apps** | An app is untrusted: it can't reach the host, fog's admin, the operator's session or another app's data or identity, beyond what the operator approved. Host folders, raw sockets and app-to-app reach need the operator's approval, recorded by fog, never taken from an app's own spec. | [docs/sealed-network.md](docs/sealed-network.md), [docs/files-contract.md](docs/files-contract.md) |
| **Sign-in** | Fog is the household's OpenID provider. No app holds fog's session; sessions are pinned and bound to the browser; every credential check is rate-limited on the attacker's budget; credential changes need a fresh proof. | [docs/signin-contract.md](docs/signin-contract.md) — *Guarantees and known gaps* |
| **Chat** | Rooms are end-to-end encrypted. The server orders messages and checks every commit, but can't read rooms or silently add a device; clients check every device against the person's identity key. | [docs/chat-contract.md](docs/chat-contract.md) — *Guarantees and known gaps* |
| **Federation** | Paired households talk over one encrypted, signed, replay-protected channel; a peer is trusted only for what it was granted, and only for its own people. | [docs/federation-contract.md](docs/federation-contract.md) — *Security*, *What the wire still shows*, *Known gaps* |
| **Files** | Drive never follows a link; share links are scoped to one item and rate-limited; peers can't write outside a share or overwrite your files. | [docs/files-contract.md](docs/files-contract.md) — *Guarantees fog makes* |
| **Backups and offsite** | An app's backup can't run code as the database superuser or write outside its own folders; offsite lockers are sealed with a passphrase, and junk in the bucket can't block or roll back a recovery. | [docs/backup-contract.md](docs/backup-contract.md), [docs/locker-contract.md](docs/locker-contract.md), [docs/disaster-recovery.md](docs/disaster-recovery.md) |
| **Compute nodes** | A machine lending its hardware runs fog's work confined (Linux namespaces, Landlock and seccomp; a macOS sandbox profile) and fails closed if it can't confine. Only signed, newer releases install. | [docs/compute-contract.md](docs/compute-contract.md) |
| **Updates** | Fog installs and updates only releases signed with its pinned key, newer than what's running and recent enough, from any mirror, and rolls a failed update back. | [docs/hosting-the-source.md](docs/hosting-the-source.md), [docs/installer.md](docs/installer.md) |

## Assumptions

Fog's guarantees rest on a few things it can't enforce itself:

- **The machine.** Anyone with root on the host, or with the disk, has
  everything: fog's keys sit beside its data by design, so a stolen disk is a
  full compromise. Use full-disk encryption if that matters to you.
- **The operator.** The operator is trusted. Approvals they give (a host
  folder, raw sockets, reach between apps) do what they say.
- **Plain http.** Fog works without a domain or TLS, and federation and chat
  stay encrypted end to end over it. But a browser's session cookie on a plain
  http address can be seen by anything on that network path and by every
  port of that address. Prefer the domain and HTTPS setup.
- **Your Docker.** Fog relies on Docker's container isolation and drops
  privileges it doesn't need. A kernel or Docker escape is outside what fog
  can prevent.
