# Changelog

## Unreleased

- `leaveDevice(roomId)`: a stale device leaves the room on this device only;
  the person stays with their role, and the device comes back like any new
  device (sibling sync, or any member's Add). `device-left` lines are acted
  on like revocations; a doomed leaf a commit has taken out is never removed
  again, so a device added back under the same key stays. `addMembers` skips
  devices already in the group.
- A device restored from a backup replaces its published key packages
  (`publishKeyPackages(…, replace)`) and leaves its old places, so it can be
  added back. An invitation it can't open shows as the room's reason.

- Removed: `resync`, `recover`, `resyncAll`, `ContinuityError`, and taking
  a Welcome over a room's held keys. A device never joins a group by
  itself: a stale room is left (`leave`), and a member adds the device back
  with an ordinary Welcome. Welcomes are used once (by digest), never at or
  below the highest epoch held, and judged in the room's queue.
- A hub-labelled event that isn't MLS is one unreadable event, never stale.
- A commit whose answer was lost is adopted when it lands.
- Rooms roll to a fresh epoch after 10,000 own messages, a week, or a
  sender this device can't follow. After any join, leaves of people fog no
  longer lists are removed before anything is sent.
- `externalJoin` no longer takes a leaf to replace.

- One queue per room for every read-modify-write of its MLS state (sends,
  receipts, commits, joins, history, incoming events); a send keeps its
  advanced state before the network.
- `recover(roomId)` (also run on connect for stale rooms): leave the own
  leaf and rejoin from the hub's GroupInfo. A Welcome for a room held stale
  replaces the stale keys.
- A provisional join whose log ends short is re-read, not refused; the match
  clears the stale mark.
- Far-ahead generations: at most 1,000 ahead per message and 100,000 per
  sender per epoch, checked before ratcheting.

- A message that can't be decrypted is one `undecryptable` line, never a
  stale room (the round-7 rule is reverted; the join's confirmation-tag
  check is the look-alike defence). Senders' chains are followed 10,000
  messages ahead.
- One client per device's keys: `DeviceLock`, `webDeviceLock`, and
  `DeviceInUseError` for a second tab; IndexedDB storage names the lock.
- A Welcome that can't be checked yet is retried with backoff, never
  refused; refused or long-retried invites don't block `resyncAll`.
- A revoked device's leaf this device may not remove stays out of what it
  trusts, and sending into the room waits, with a reason.

- A Welcome must carry the confirmation tag of the hub-ordered commit that
  added this device (`Mls.welcomedBy`); the GroupInfo and that tag are
  checked before the leaves, with one directory fetch per Welcome; refused
  invites are never judged again. A message for the current epoch that
  can't be decrypted makes the room stale.
- Every stale reason is kept across restarts; leaving a room clears what was
  kept for it.
- `connect()` reads the directory first, catches up, then takes invites
  concurrently, each within a minute. Every API call has a deadline.
- `refusedDevices()` names whose device each is and leaves out ones since
  vouched for. Key names are recorded on every join path.

- What an MLS event is comes from its bytes (`Mls.kindOf`, `process(…,
  expect)`), never the hub's `kind` or sender label: a commit labelled as a
  message or as this device's own gets every commit check.
- A room stale over a device nobody vouches for can be recovered:
  `refusedDevices`, `removeRefusedDevices`, and members' devices accept the
  refused commit together with a member's removal of the device. Stale
  stays stale (across restarts) until resolved; nothing is sent into it.
- Device keys keep their name across every epoch seen. A member's re-add
  vouches for a reset or changed key only once the commit is accepted, and
  fog's bot never vouches. Cached attestations drop on revocation.
- `safetyCodes(addr)`: codes from this device's seed and pins, with warnings
  when fog serves other keys.
- A Welcome behind the hub's GroupInfo is provisional until the catch-up
  matches it exactly; nothing is sent until then.

- A leaf is its place, name and key together: one at a place a commit
  removed, or whose name or key changed, is new and must be attested, and
  every add and remove is a membership line. A commit that hands a device
  key to another name is refused (`mls.process` also returns `removed`).
- Joining by Welcome checks the group against the hub-verified GroupInfo
  (`Mls.sameGroupAs`); a look-alike group leaves the room stale.
- History shared by another member is never re-shared.

- Lines the client makes (membership, identity resets) take no edit,
  delete, pin or reaction from anyone. `decodeContent` refuses a type no
  member may send (`membership` included). A message with no ULID id is
  filed as `event:<seq>`.
- `members(roomId, listed)`: who the group's leaves say is in a room, with
  anyone fog doesn't list marked `unexplained`.
- After an identity reset, no key is pinned for the person on fog's word:
  `awaitingKey(addr)` until a member's re-add commit (verified here),
  `addMembers`, or `markVerified`; `identityWarnings()` lists them. A reset
  line acts only from the person's home hub or this fog, about someone who
  held a leaf in the room.
- Attachments whose `blob` isn't hex SHA-256 are dropped at decode; every
  id in an API path is escaped (`isBlobRef`).
- The bot's integration stamps (`app:`, `webhook:`, `mcp:`) are kept and
  `systemLabel` names the speaker; shared history can't carry a stamp or
  name the bot. `linkHost` gives a link card's real host.
- `updateRoom(..., { acceptHub: true })` agrees to this fog hosting a room.

- Every leaf a commit brings in (an Add, an external join), and every leaf of
  a group joined by Welcome or by itself, must be attested by its person's
  identity key as this device holds it (its seed's, or the key pinned at
  first sight); otherwise the commit is refused and the room goes stale
  with `Room.staleReason`. A key fog serves that differs is reported, not
  adopted (`identityWarnings()`); `addMembers` still takes a changed key,
  deliberately. New: `knownIdentity`, `leafAttested`, `leafTrust` (badges
  from the group's actual leaves).
- History is re-sent only to leaves this device verified. Sibling sync adds
  devices only when fog's identity key for this person is the one this
  device holds, and warns otherwise.
- Message ids must be ULIDs; the client's own lines use other ids
  (`membership:<seq>`), so no message can hide one. Member content can't
  carry `system` (except fog's bot's `"fog"`), including in shared history.
- `inlineImageType`, `safeHref`, `DOWNLOAD_TYPE`: what a UI may do with a
  sender's bytes and links.

- `createRoom` sends the new group's GroupInfo (with its ratchet tree): the
  hub now verifies every commit against the group's public state. A commit
  refused with `need_group_info` (a room from before that) gets this
  device's GroupInfo published and is retried.
- A message's moderation rights are judged by the role the hub recorded
  for its sender when it was sequenced (`EventView.senderRole`).
- Nothing is encrypted to a room while it still holds leaves the hub said
  must go (a departure, a revoked device): the device removes them first.

- Incoming commits are held to fog's rules before they apply: a commit or
  proposal that arrives encrypted, any standalone proposal, an Update inside a
  commit, a path that renames its committer or changes its key, an external
  commit removing anyone but the joiner, or an add outside a DM's set throws
  `RefusedMessage` (`code: "refused"`) and leaves the room `stale`.
  `Mls.process` takes the room's `CommitRules`.
- `addMembers` adds only devices whose attestation verifies against the
  person's identity key and whose key package is that device's, under that
  address; the rest come back in `skipped` with the reason. Sibling sync
  checks attestations itself rather than trust `verified`.
- Content rules apply from the roles the hub reports (`RoomView.roles`, new):
  a delete of someone else's message needs `canDeleteOtherMessage`, a pin
  `canPin`, a reaction `canReact`, own edits and deletes `canEditOwn` and
  `canDeleteOwn`; history is taken only from `canShareHistory`, and is kept
  as `sharedBy` its sender and never changes a message received directly.
  `can(roomId, capability)` says what this person may do.
- `leave` takes the person (every device) out through the hub
  (`updateRoom({leave: true})`): MLS forbids a commit removing its own
  committer, so the old self-removal never worked.
- Departure system events (`household-cut`, `banned`, `left`,
  `identity-reset`) make any member device, not only an admin's, commit the
  removal of the departed people's leaves. `identity-reset` also drops the
  pinned key and verification mark, emits `identity` with `reset: true`, and
  adds an `identity-reset` line to the room. A device that sees its own
  person depart drops its keys for the room.
- `directory()` compares each person's identity key with the one pinned and
  reports a change as an `identity` event, dropping the mark.
- An unverified device no longer tries to rejoin rooms by itself: the hub
  refuses any self-join by a device its person's identity key hasn't attested.

- `messages()` caches each list and returns it frozen, typed
  `readonly MessageView[]`; the same list comes back until something in it
  changes, and a message whose view did not change keeps its object, so a
  UI can memoise on identity. `readMarks()` is cached the same way and typed
  `ReadonlyMap`. Code that sorted or spliced the returned array in place
  must copy it first.
- The response types fogd encodes from Go structs (`RoomView`,
  `ParticipantView`, `EventView`, `DeviceView`, `DirectoryPerson`,
  `KeyMaterial`, `SessionView`, `Webhook`, `Prefs`, `PreviewCardResponse`,
  `Policy`) are generated from those structs into `src/wire.gen.ts` and
  re-exported under the same names; their shapes are unchanged. The device
  and policy shapes they nest are now named types too (`DirectoryDevice`,
  `KeyMaterialDevice`, `PolicyRole`, `PolicyRetention`) in `wire.gen.ts`.

## 1.0.0 — 2026-09-04

First stable surface; see README "1.0: what is stable".

- Sealed rooms over MLS (RFC 9420, ciphersuite 1) with fog's people directory,
  Welcome and external joins, history bundles, membership lines.
- Multi-device: attestation by the person's identity key, sibling sync,
  in-person verification marks, phrase-sealed backup and restore, resync.
- Federation-aware: rooms across households, follower replicas, relays.
- Slack parity: threads, reactions, pins, edits, deletes, attachments, typing,
  presence, read receipts (opt-in, encrypted), saved messages, link previews,
  preferences, export.
- `api.bearer()` exposes the device session token; `api.sessions()` and
  `api.revokeSession()` manage sessions.
- `api.migrateRoom()` moves a room's hub to another household.
- Conformance suite in `test/conformance.mjs`.
