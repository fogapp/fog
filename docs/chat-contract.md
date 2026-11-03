# fog-chat/1 — sealed rooms

*Sibling of fog-federation/1 ([federation-contract.md](federation-contract.md));
the client API is [chat-openapi.yaml](chat-openapi.yaml).*

Every fog can host rooms for the people of its household. A room lives on one
fog, its **hub**, which orders, stores and delivers ciphertext it cannot read,
and enforces who is in the room from the public half of the MLS group. Paired
fogs join as **followers** over the signed federation channel. Rooms are
designed and tested so the server cannot read them: nobody reads a conversation
they don't hold a device seat in. That has not been externally audited; see
[Guarantees and known gaps](#guarantees-and-known-gaps).

The model follows the IETF MIMI drafts (hub-and-spoke rooms, roles, a content
profile, key material fetches) without claiming compliance. Group encryption is
MLS, RFC 9420.

## People, devices, keys

A **person** is a row in `people`, with one sign-in and one session cookie:
`role` is `operator`, `member`, `guest` or `bot`; `status` is `active` or
`suspended`. `role` alone decides what a session may reach, and nothing a role
grants reaches past the ciphertext. Members join by invite link (`/join/<secret>`)
and sign in with a passkey or password. Native clients hold a bearer token, which
the stream also accepts as `?token=`.

**Addresses.** On the chat wire a person is `fog:<instance>/<handle>`; the second
half may be the person's ULID. The directory also carries the rename-proof
`person:<instance>/<ULID>`. Handles are unique per fog, never globally.

**Keys** live on devices, never on fog:

| Key | Holder | Purpose |
|---|---|---|
| Identity key (Ed25519) | The first device; sealed into the backup | Attests devices. Write-once on the server: a second write is `409 identity_set` |
| Device key (Ed25519) | One device | MLS leaf signature key and registered `device_key`. The BasicCredential identity is the person's address |

**Attestation** is `sign(identity, "fog-device-attest/1\n" + deviceKey)`. Fog
verifies and stores it and cannot forge one. A device without one is
**unverified**. The first device attests itself; a new device registers unverified,
and a device holding the identity seed attests it. Sibling sync then adds verified
devices to every room and removes leaves of revoked ones. An unverified device is
what someone holding only the person's sign-in can make, so nobody puts one in a
room: the hub refuses an Add or a self-join of any device whose attestation doesn't
verify against its person's identity key (it checks the signature itself, for a
remote person against the key their fog advertises), and clients check it again
before adding. Device kinds are `web`, `ios`, `android`, `desktop` and `bot`.

**Device-bound sessions.** MLS routes need a session bound to a device, or they
answer `428 need_device`. Registering a device binds one. A device fog already knows
gets one by signing `fog-device-session/1\n<nonce>` (`GET …/devices/{id}/challenge`,
`POST …/devices/{id}/session`); nonces are single-use and live 5 minutes.

**Verification marks.** Two people compare the fingerprint of an identity key in
person and mark each other verified. The mark lives on their devices and is bound to
the key compared, so a changed key drops it: a client that sees a person's key differ
from the one it pinned — when adding them, or in the directory — drops its mark and
says so. Device changes are MLS commits, so each is a timeline line on every member's
client.

**Safety codes** are computed from what the device holds, never from what fog serves now:
this device's from its own identity seed (or, without it, the key it pinned for its own
person), the other person's from the key this device pinned for them — only where it pinned
none, the key fog serves, marked as such, which a match in person then pins
(`ChatClient.safetyCodes`). When fog serves either person a key other than the one held, the
device says so loudly beside the codes. A fog that swaps keys therefore can't make two
people's codes agree.

**After an identity reset** (below) a client that acts on the reset holds no key for the
person and takes none on fog's word: until a member re-adds them in a commit the client
verifies itself, this person adds them, or compares safety codes, none of their devices
is trusted, and the room's info pane and `identityWarnings` say so. The same holds for
anyone whose key fog serves differs from the one pinned. A re-add counts when the commit
comes from a current member's attested leaf (not an external join, not the person
themselves, and never fog's bot, whose key fog's operator holds) and every device of theirs
it adds is attested by the key fog now serves; the key is pinned only once the whole commit
is accepted.
A client acts on a reset line only when the room's hub is the person's home fog or this
fog, and the person held a leaf in that room; any other reset line is shown in that room
and changes nothing.

**Key packages** are one-use. A device keeps at most 100 unused, 16 B to 64 KiB each,
and one **last-resort** package (RFC 9420's pattern) that is never used up; the device
keeps its private keys. Each fetch of a person's key material consumes one per device,
within a budget: an asker (a paired household, or a person here) consumes at most 12 of
one person's an hour, a household at most 120 in all, and everyone together 120 of one
person's; a person fetching their own isn't bounded. Past a budget, or once a device has
none left, the fetch gets the device's last-resort package (`lastResort: true`), so
draining can't keep anyone out of a room. A device with neither reports `null`.

## Rooms

| Kind | Membership | Defaults |
|---|---|---|
| `channel` | Listed to the household (`workspaceVisible`); needs a label | `open-household`, `shared` history |
| `group` | Invite-only, unlisted | `invite`, `since-join` |
| `dm` | 2–9 people, fixed; created idempotently by participant set | `invite`, `since-join` |

All three can span households and all three are sealed. There is no unencrypted room
kind. The client creates the MLS group, then `POST /api/chat/rooms` with a `groupId` of
8–64 bytes and the group's GroupInfo at epoch 0 with its ratchet tree; the hub checks it
names this group with only the creator's device in it, and the creator sits at leaf 0 as
owner. The creator's device must be one their identity key attests. The room's id is minted by the hub and bound to its household: the first 16
characters of a ULID and 10 derived from them and the hub's instance id. A follower that
holds a replica takes invites under its id only from its current hub — the one that made
it, or the one a hand-over moved it to, as the follower heard from the hub before — and
never deletes a replica for an invite; anyone else's is refused `room_id_taken`. A room
id it doesn't hold yet is taken from the household it is bound to, refused from anyone
else when it is bound to this fog or to another household paired with it, and otherwise
taken as offered.

The **log** is `chat_events`: per room, a strictly increasing `seq`, a client-minted
ULID `id` (a replay returns the original with `replayed: true`), `epoch`, `kind`,
sender, `payload`, `hints`. Kinds are `mls_private`, `mls_commit` and `system`.
`mls_proposal` is refused; commits inline their proposals. System events are the hub's
own JSON: the departures below `{kind, left, reason}`, and `hub-moved {from, to, by}`.

**Hub.** Assigns `seq`, stamps `hubAt`, parses commits with `mlsview` and checks them
against its public leaf list and the policy, stores the log, and fans out. Of a private
message it checks the sender, the epoch and the content type, which travels in the
clear: only application data may be encrypted, so no membership change passes it
sealed. **Follower.** Relays its people's submissions and room changes, keeps a replica
of the ciphertext log, serves its own people from it, and never accepts into the
replica what the hub hasn't sequenced. A gap triggers a pull.

A person is a participant from the commit that adds them until the hub counts them
out: when a commit removes their last leaf, or at a departure (below).
`chat_participants` records `joined_seq`, `left_seq` and the identity key they joined
with. **Access ends at departure.** A former participant reads the log up to the event
that took them out and fetches the attachments stored before it — what they were sent
and hold keys for — and nothing after: not on a stream opened earlier, not in the
room's details, not by a Welcome still pending (departure drops it). A household whose
people have all left gets the same from the hub, and a departed snapshot that archives
its replica. Coming back takes a member's Add, as a member.

**Deleting a person** is a departure from every room, and leaves nothing for whoever is
given the handle next. Rooms hubbed here count them out with an `identity-reset` line; rooms
hubbed elsewhere are told through their hub, which does the same. Their DMs here are
archived, their webhooks revoked, their pending Welcomes dropped, and every chat row naming
them — participant rows, bans, DM sets, invites — moves to their retired address,
`fog:<instance>/<ULID>`, which no living person answers to. A hub also binds each
participant to the identity key they joined with: someone acting under a participant's
address with another key (from a household whose word of a deletion or reset never
arrived) is counted out with an `identity-reset` line and refused, and a follower's replica
keeps a row for one of its own people only for the key they hold now; any other row gives
the address's holder nothing. A tombstone of their rows, giving nothing, stays under the
address with the identity key they held, so the address's history of keys survives them.

A household's word of its people's resets and deletions reaches rooms hubbed elsewhere
through a durable queue (`fed_outbox` kind `chat-departure`, `POST /fed/v1/chat/departures`),
retried until the hub acknowledges it, across restarts and absences; past 100,000 owed to
one hub a new one is refused, loudly, never an old one dropped. The hub takes it on the
household's signature alone — a deleted person is in no directory — for that household's own
people only. It acknowledges only what it applied: a room that moved on is named with its
new hub — the household tells that hub once it has itself followed the move there (by the
hub's own moved notice), and until then keeps the departure owed and asks again — and one
frozen mid-move is retried. None is ever dropped.

**Departures** are hub system events `{"kind", "left": [addrs], "reason"}`:
`household-cut`, `banned`, `left` and `identity-reset`. MLS forbids a commit that
removes its own committer, so leaving is `PATCH {"leave": true}`. The hub holds no key,
so the first member device to see a departure commits the removal of those people's
leaves; any member may remove the leaves of someone the hub counts out (a revoked device's
leaf, see Recovery, by rank or household). Until that removal lands, no member device
encrypts anything to the group: a device that has seen the
departure removes the leaves itself before it next sends.

**Moving a hub.** `POST /api/chat/rooms/{id}/migrate {to}` needs `canChangePolicy` on the
hub, and `to` must be a paired household with someone in the room. A hub can't push a
room on anyone: `to` adopts only if one of its own people in the room, holding
`canChangePolicy`, agreed in the last hour (`PATCH {"acceptHub": true}` on their fog; kept
in memory) to a move from the hub that hosted the room when they agreed. The agreement is
used up only by an adoption that completes; one that fails leaves it standing. The old hub freezes the room (`503 moving`) — under its commit lock, after which nothing is
sequenced there by any path (submits, departures, bans, room changes), so the seq it hands
over is the final one; a reset, deletion or unpairing meanwhile is applied once the move
settles — here if the room stayed, or, for this household's own people, by word to the new
hub (another household's standing at the new hub is that hub's to judge) — the target syncs to
the frozen seq, vets what it was handed — the policy parses and holds every participant's
role, the creator is a participant, a DM's set is 2–9 people covering every participant
and only a DM has one, the verified public state names this group, epoch and tree —
adopts it, appends `hub-moved`, and copies attachments in the background, each digest
checked, at most 10,000 blobs and 20 GiB. The target must be granted chat here. An `open-household` channel adopted by a household other than the one that made it
becomes invite-only: it was open to its makers' household, not the new one. The old hub
becomes a follower and tells the others. A follower not paired with the new hub archives its replica. The
MLS group never notices.

## Policy

A room's policy is hub state, JSON in `chat_rooms.policy`, shaped after the MIMI room
policy draft. It is set at creation, default per kind.

```json
{
  "roles": [
    {"index": 1, "name": "banned", "capabilities": []},
    {"index": 2, "name": "member", "capabilities": ["canSendMessage", "…"]},
    {"index": 3, "name": "admin", "capabilities": ["…"]},
    {"index": 4, "name": "owner", "capabilities": ["*"]}
  ],
  "join": "invite", "history": "since-join", "households": "hub-peers",
  "hints": ["mentions"], "retention": {"days": 0}
}
```

- **member**: `canSendMessage`, `canReact`, `canEditOwn`, `canDeleteOwn`,
  `canAddParticipant`, `canShareHistory`. In a DM, neither member nor admin holds
  `canAddParticipant`.
- **admin** adds `canRemoveParticipant`, `canDeleteOtherMessage`, `canChangeRoomName`,
  `canChangeRoomTopic`, `canChangeRoomAvatar`, `canPin`, `canChangePolicy`,
  `canChangeRoles`.
- `join` is `open-household` or `invite`; `history` is `shared` or `since-join`;
  `households` is `hub-peers`; `hints` may be `["mentions"]` or `[]`; `retention.days` is
  0 (forever) to 3650.

**The hub enforces** on each commit RFC 9420 §12.2's rules on which leaves it touches:
inline proposals only; no Update (inline, it is always the committer's own, which is
forbidden), PreSharedKey or ReInit; no removing the committer or one leaf twice; unique
signature keys; a path keeps its leaf's name and signature key, the device key; an
external commit carries one external init and at most one Remove, of the joiner's own
leaf. So no commit replaces anyone's leaf but the committer's. The committer's device must be one their identity key attests. Then the policy: every
Add needs `canAddParticipant` unless it adds the actor's own device, or a device of someone
already in the room under the identity key they joined with; an Add must name a
live device of an active person, attested by their identity key (for a remote person,
one their home fog advertises), and never a banned person; every Remove of
another needs `canRemoveParticipant`, and reaches only people ranked below the actor (no
admin removes an owner), unless the hub already counts them out, the leaf's device left the
room by itself, or the leaf's device was revoked — for a remote person, only when their household's current directory (it heartbeated
recently) lists them without that device; a household away, a narrowed grant or a person no
longer listed says nothing — and a revoked device of someone ranked at or above the remover
goes only by their own household; a DM refuses anyone
outside its set, by an Add or a self-join, on whichever household hubs it (a replica
carries the set for when the hub moves); policy extensions (type `0xF0F0`, the only extension a commit may
carry) need `canChangePolicy`, and any change to the roles in one — compared exactly, each
role's index, name and set of capabilities — needs `*` (no client sends one). Sending needs `canSendMessage`.

Roles, labels, visibility, archive and leaving are hub writes through
`PATCH /api/chat/rooms/{id}`, every field checked against the actor's role before any is
written. A follower relays them to the hub (`POST /fed/v1/chat/rooms/{id}/update`) and
shows what the hub decided; only `notify`, the person's own setting, stays at home.
Roles move only below one's own rank — a role holding `*` ranks above all, the rest by
index, banned lowest: the target's role now and the new one must both rank below the
actor's, except that a `*` holder may make a peer. Nobody changes their own role, so
nobody demotes an owner. A ban is a departure: the person is out at once and stays out
— no Add, no self-join — until someone who outranks banned gives them another role. A ban
reaches anyone who was ever in the room and anyone of the hub's household, by canonical
address; a ban on someone never in the room gives them nothing of it (their row is out from
before the start). Outsiders, former participants and banned people alike get a
workspace-visible channel's summary at most — its label, topic and size — never who is in
it, their roles or devices. A
webhook goes with its maker when they leave, or lose `canChangePolicy`. New
participants, and returning ones, start as member.

**Clients enforce** the content rules the hub can't see: only an author's edit applies;
a delete applies from its author or from a role holding `canDeleteOtherMessage`, a pin
only from one holding `canPin`; reactions need `canReact`, and edits and deletes of
one's own `canEditOwn` and `canDeleteOwn`; history is taken only from a role holding
`canShareHistory`, and a device re-sends only what it received itself, never history
another member shared with it. A client judges each item by the role the hub recorded for its
sender when it sequenced the event (`senderRole`), and for items without one by the roles
the hub reports (`RoomView.roles`); items stored before clients checked fold as they
always did. Shared history is the sharer's account: kept as shared by them, it never
changes a message the device received itself. A message's sender is the leaf MLS proves
signed it, not the sender the hub names. A message's id is a ULID (failing that, `event:<seq>`) and the client's own
lines (membership, identity resets) live in another namespace, so no message can hide
one. Those lines are nobody's message: no edit, delete, pin or reaction applies to them,
from anyone. Member content can't take their type — a content type no member may send,
`membership` above all, is refused at decode, by the SDK and by fog's bot — or claim to be a
system line: only lines the client makes from verified commits and hub events, and fog's
bot, render as one. Who is in a room is drawn from the group's leaves, set against the
participants the hub lists: anyone holding a leaf the hub doesn't list shows as an
unexplained member, with a warning (`ChatClient.members`). `canChangeRoomAvatar` has nothing to
govern yet: rooms have no avatar.

## What the server can and can't see

| The hub, followers and their operators can see | They cannot see |
|---|---|
| Who is in each room, on which devices, and when they joined or left | Message text, edits, deletes, reactions, replies, threads, pins, receipts, history |
| Who sent each event, when, how large, and any expiry set | Which message a reaction, edit or pin is for |
| Labels and topics typed into room settings | Attachment contents, names or types, or which message carries one |
| That an attachment exists, its size and room | Who was mentioned, unless hinted |
| Mention **hints** a sender declared | Anything in a room it holds no seat in |
| Read marks (`read_seq`), typing and presence as they pass | |

A label or topic is plaintext wherever it is typed. A group or DM may have none.

**Hints** are the one deliberate leak: a locked phone can't decrypt a message to learn
it was named. A sender may attach `{"mentions": [addrs]}`, at most 50 valid addresses,
used to wake those people. The room's `hints` policy filters them; `[]` refuses them.
They are unverifiable and buy a liar one notification.

A link preview is fetched by fog when the person opted in, so fog sees that URL; the
card is then encrypted into the message by the sender. The fetcher refuses non-HTTP
schemes, credentials in URLs, `localhost`, `.local`, `.lan`, private and link-local
addresses, checked on the address actually dialed.

## How MLS is used

Ciphersuite 1, `MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519`. Clients run ts-mls; the
hub uses mls-go and holds no key material. Commits are
PublicMessages, application data PrivateMessages, and commits, Welcome and GroupInfo
travel MLSMessage-framed; key packages travel raw. ts-mls omits RFC 9420's length prefix
on `external_pub`, so the SDK assembles RFC-correct GroupInfo and shims the read side.

**The hub verifies every commit** against the group's public state before ordering it —
what an external joiner would hold: the full public ratchet tree, the GroupContext and
the interim transcript hash, seeded from the creator's GroupInfo and advanced only by
commits it verified. It checks the commit's signature by its sender's leaf over the
content and this epoch's GroupContext; each added KeyPackage's signature and its leaf's;
the committer's new leaf's signature; that the UpdatePath names a key for exactly the
filtered direct path and that the new tree's parent hashes all hold. From the new tree
hash and confirmed transcript hash it derives the next GroupContext, and a GroupInfo sent
with the commit, or published later for that epoch, must match it exactly and be signed
by a member. A room from before the hub kept this state seeds it from a GroupInfo for its
current epoch whose tree holds exactly the members the hub knows; until then it takes no
commit (`409 need_group_info`), and clients publish one and retry.

**Joining.** A client adding someone fetches their key material (through its own fog if
they live elsewhere), commits Adds for every device with a key package, and submits with
the Welcome and the post-commit GroupInfo. The hub files the Welcome per added device.
The invitee's device processes it and catches up.

**Joining by Welcome** is bound to the room's own group: the joined epoch must carry the
confirmation tag of the hub-ordered commit that added this device — a MAC from secrets only
the real group holds, which every member's client checks when it applies that commit — so a
Welcome its sender built on a joiner secret of their own is refused, however well its public
state matches; and the joined group must have the group id of the GroupInfo the hub verified
for the room, and at that GroupInfo's epoch its tree hash and confirmed transcript hash; one for an earlier epoch than the hub's GroupInfo
is provisional — nothing is sent into the room until catching up has brought the device to
that epoch and the group matches it exactly; a log that ends short (a follower's copy a
commit behind the hub) is read again, five seconds doubling, a dozen times, and the match,
when it comes, clears any stale mark; only a group that differs at that epoch, or a log
that never gets there, refuses. A follower serving its people the hub's GroupInfo first
pulls its copy of the log past it. A Welcome into anything else leaves the room `stale` with a
reason, so no member can cut a newcomer off into a look-alike group. Those checks come first,
then the leaves' attestations with at most one directory fetch for the whole Welcome. Only
checked data that disagrees refuses an invite for good; what can't be checked yet — fog
unreachable, a follower that hasn't synced the adding commit, a GroupInfo not yet at the
Welcome's epoch — is tried again, five seconds doubling to ten minutes, and never refuses.
Invites are taken after the rooms already held are caught up, all at once, each within a
minute.

**One message, never the room.** An event fog labels as MLS that is neither a public commit
nor a private application message is fog's fault and one bad event — shown as unreadable,
never a reason for a room to go stale; a follower keeps only the
MLS events whose bytes are what the hub says (a commit of the room's group at the epoch
given, or an application message of it), and puts a `hub-fault` system note in any other's
place. A private message a device can't decrypt — garbage from a
member, a sending generation already used, a gap in one sender's chain past what the
ratchet follows — is shown as one message that couldn't be decrypted; the room goes on.
Room-wide stale is only for refused commits and refused Welcomes. A device follows a
sender's chain up to 1,000 messages ahead of the last it read from them, and to 100,000 in
one epoch, checked from the sender data before any key is derived — so messages pruned by
retention or never delivered are stepped over, and a crafted far-ahead generation costs
only its own message; past either bound, only that sender's later messages in that epoch
are lost to that device, until the next commit. **Epochs roll before chains get long**: a
device moves a room to a fresh epoch (an empty commit with an update path) once it has sent
10,000 messages in one, once one is a week old, or when it can't follow someone else's
chain; fog's bot does the same, after 10,000 of its messages or a week. **One state, one
writer**: a browser's tabs
share one device's keys, so the SDK takes a Web Lock for them before anything else; a second
tab says chat is open in another and waits, taking over when that tab goes. Within a client,
every read-modify-write of a room's MLS state — a send, a receipt, history shared, a commit,
a join, an event applied — runs in that room's one queue, and a send keeps its advanced
state before it reaches the network (a message the hub then refuses is lost, never its
generation reused). No sending generation is ever used twice, and no epoch is ever written
back over a newer one.

**External commits** let a device commit itself in, against GroupInfo a member published
for this group's current epoch. The device must be one its person's identity key
attests, for local and remote people alike. Then the hub allows: a current
participant's device, if the person's identity key is still the one they joined with (a
different one is someone else under their name, and the hub takes them out with an
`identity-reset` departure); a device with a Welcome pending for it; and a home member of
an `open-household` channel hubbed here. Banned people are refused, and so is anyone who
was ever in the room under another identity key, in every kind of room: after a reset only
a member's Add brings them back. An open channel's door takes an address only under the
identity key it has always had here: one that has held another in any room — a reset, a
deleted person's handle given again — gets in only by a member's Add, which members' clients
take as vouching for the new key. The same rules decide who may fetch the GroupInfo.

**Clients verify every leaf.** A leaf is its place, its name and its key together: one
whose name or key differs from what that place held, or at a place the commit removed, is
new, and every add and every remove is a membership line. A device key never changes
names in a group: a commit that removes a leaf and gives its key to anyone else is
refused, and the device remembers each key's name across every epoch it sees and every way
it joined (Welcome, a channel's door, a resync), so a key
can't come back later under another. Every leaf a commit brings into the group — an Add, an
external joiner, whoever committed it — and every leaf of a group a device joins by
Welcome or by itself must be a device whose attestation verifies against that person's
identity key *as this device holds it*: its own identity seed's key, or the key it pinned
when it first met the person (or took deliberately, adding them). Fog's directory supplies
the attestations, but a key fog serves that differs from the held one is a warning, not a
new truth. A commit with a leaf that fails is refused; the room goes stale with a visible
reason. Badges come from the group's leaves, history is re-sent only to leaves the sharing
device verified this way, and a device adds siblings only when the person's identity key
fog serves is the one it holds — otherwise it warns loudly and adds nothing.

**Clients check commits** before applying them. What an MLS event is comes from its own
bytes — a public commit or a private application message — never from the kind or sender
fog labelled it with: a commit labelled as a message, or as this device's own, gets every
check a commit gets (a commit for an epoch this device is already past is its own, applied
when made), and anything that is neither is refused and the room marked stale. A commit or
proposal that arrives encrypted is refused, as is any standalone proposal. A public commit must keep the
hub's structural rules above; a DM's may add only its set, by an Add or an external
join (a follower's replica carries the set, so this holds on every household); and after it every leaf it
didn't remove must keep its name and signature key. A refused commit leaves the room
`stale` on that device rather than let it diverge (a commit or Welcome refusal; never a
message it can't read), and stale stays stale until what caused
it is resolved — a message that decrypts meanwhile doesn't end it, a restart doesn't (every
stale reason is kept on the device), and nothing is sent into a stale room. **A device
never joins a group by itself.** A stale room shows its reason and one action, "Leave this
room on this device" (`PATCH {"leaveDevice": true}`): the hub marks that device's leaf as its
own departure, with a `device-left` line, and any member's device may then remove it (once a
commit has taken that leaf out, no device removes it again, so the device added back under
the same key stays) — the person stays in the room, with their role and their other devices; losing only leaves their
devices left by themselves doesn't count them out. The device drops everything it held for
the room and comes back like any new device of the person's: their other devices add it by
sibling sync, or any member adds it — adding a device of someone already in the room, under
the identity key they joined with, takes no `canAddParticipant`, in a DM too (within its
set); adding a person still does. That is a fresh join with every check a join gets, and it
changes no one's role. A device takes no Welcome for a room it still holds keys for, in step
or stale, and rejoins nothing on connect; an invitation it can't open (sealed to a package it
holds no keys for) is said where the room is. After any
join, the leaves of anyone fog no longer lists are marked for removal, and nothing is sent
until they are gone. A Welcome is used once (by digest) and never at or below the highest
epoch the device has held for the room (kept past leaving), so a replayed one can't rewind
it; Welcomes are judged in the room's queue. A commit this device made whose answer was lost, that landed anyway, is taken as its
own state when it comes back in the log. A room refused over a device nobody vouches for is
resolved by its removal: a member whose role may remove people — shown whose device it
claims to be, and asked to confirm; a device since vouched for isn't offered — takes the
refused commit and at once commits that device's removal (`removeRefusedDevices`), so
nothing is encrypted to it, and every member's device that refused the same accepts the pair — and only a next
commit, from an attested member, that removes every device it refused and brings in
nothing unattested.

**Sending.** A message is a CBOR content map (`src/content.ts`; `internal/chat/content.go`;
vectors in `test/vectors`), encrypted for the current epoch. A commit must be for exactly
the current epoch, and carry the GroupInfo of the epoch it makes (`400` otherwise), so the
hub always holds a current one; a private message may be up to 2 behind. Otherwise the answer is
`409 epoch_stale {epoch, seq}`: sync and retry. Clients send `message`, `edit`, `delete`,
`reaction`, `pin`, `receipt` and `history`, with `topicId` for threads, `inReplyTo`,
`replaces` and `expires`. Unknown keys are ignored; an unknown type is refused. Profile
version is 1.

**History.** In `since-join` rooms a joiner starts at their join, and the client API
floors reads there. In `shared` rooms, each eligible online device re-sends what it holds
inside the group as `history` messages naming the new devices (the last 400, in chunks of
100, with jitter); receivers skip what they hold. Every member can read those messages,
and all were already entitled to them. A `shared` room's past is as private as its
members' devices.

**Search and export** are client-side, over the plaintext the device holds. **Saved
messages** stay on the device.

**Retention.** The hub prunes events past `retention.days`, and attachments stored before
the same cutoff, hourly; followers apply the room's policy to their replica. A message's
`expiresAt` prunes that event from every fog holding it. Neither reaches a client's plaintext.

## Recovery

- **A recovery phrase** is twelve BIP-39 words, shown once, opted into per person. The
  backup key is the BIP-39 seed through HKDF, labelled `fog-chat-backup/1`; the blob is
  AES-256-GCM. It holds the identity and device seeds, decrypted messages, cursors, pins
  and verification marks, at most 64 MiB, written 15 seconds after a change. Fog stores
  bytes and a version; an older version is `409`.
- **Restore** needs a sign-in and the phrase. The device then proves its key for a
  device-bound session, and replaces every key package it published before, its
  last-resort one included (`replace: true`): it holds none of their private keys. It
  keeps the stored history and verification marks, leaves its old place in each of its
  rooms (only this device), and joins nothing by itself: the person's other devices, or
  any member, add it back. It cannot decrypt what was sent while it was gone.
- **No device and no phrase**: the operator's `POST /api/people/{id}/reset-identity` clears
  the identity key and revokes every device, so the next device starts a new identity.
  The old history is gone, and so are the rooms: the reset takes the person out of every
  room with an `identity-reset` departure (relayed to the hub of rooms hubbed elsewhere),
  which every member's client shows as a warning and drops its pin and verification mark
  for (trusting no new key until a member re-adds them or safety codes are compared), and
  their pending Welcomes go. The new identity gets back only by a member adding it.
  A device whose identity seed is lost can't be verified, so it can't rejoin by itself
  either: that is the case for a reset.
- **Device revoked**: `DELETE /api/chat/me/devices/{id}` ends its sessions, its open
  streams and its key packages, and posts a `device-revoked {person, devices}` line in each
  room hubbed here where it holds a leaf. Any member ranked above the person, or of their
  household, may remove that leaf (a revoked device of someone ranked at or above a member
  goes only by their own household or a higher rank), and the first such device to see the
  line does. A member who may not keeps the leaf out of what it trusts and sends nothing
  into the room — saying why — until someone who may removes it; never "they're back". The
  person's other devices remove its leaves everywhere.
  A signed-out session or a suspended person's stream closes too.
- **A lost phone** when the person can't get in: the operator's
  `POST /api/people/{id}/signout` ends every session; devices and keys stay.

## Attachments

A file is encrypted on the sender's device with a fresh AES-256-GCM key and nonce, as a
single blob, and uploaded to the hub (`POST /api/chat/blobs?room=`), at most 200 MiB. A
follower relays a large one to the hub in pieces of just under 16 MiB, checked whole at the end, and
keeps no copy of one the hub refuses. A
blob is content-addressed by the SHA-256 of its ciphertext and belongs to one room.
Stored attachments are held to quotas — 10 GiB of one person's uploads, 20 GiB of one
room's, 200 GiB in all — checked and stored as one step. A chunked upload reserves its
declared total against them at its first piece, beside at most 2 GiB reserved by uploads
under way — 512 MiB of it by one person, 1 GiB by one household — and every piece must
declare that same total; only a piece that adds bytes keeps a reservation from lapsing (an
hour), and a follower whose relay fails tells the hub, which releases it at once. A relayed
upload's declared length is held to on both sides; a plain upload's declared length
is checked before its body is read, and held to. Past a quota, `507 quota`. Copies of other
hubs' attachments are a cache of at most 50 GiB, the oldest evicted to make room; a fog that
hands a room on keeps its copies until the new hub has fetched each one.
`{blob, key, nonce, name, mime, size}` travels inside the sealed message; a client drops an
attachment whose `blob` isn't 64 lowercase hex characters, and path-escapes every id it puts
in a URL. Followers fetch
on first read and check the digest, so a hub can withhold a blob but not substitute one.
Saving to Files is a deliberate client action.

The bytes and the stated type are the sender's, so the web client never lets them act in
fog's origin: an attachment shows inline only when its decrypted bytes are a PNG, JPEG,
GIF or WebP (SVG never is), and only as an `<img>`; anything else is saved as
`application/octet-stream` behind the download attribute, and no blob URL is ever opened
as a page. Message links are links only when http(s) or mailto. A reader's browser fetches
nothing a sender names: images in markdown and link previews are not loaded, and a link
card names the link's own host, never the `site` its sender wrote. The chat UI
needs no inline script and only blob images, so it runs under a strict Content-Security-Policy.

## Realtime and notifications

`GET /api/chat/stream` is one WebSocket per client. The first frame is
`{"since": {roomId: seq}}`, within 15 seconds. The server replays in pages of 500, sends
`synced` with cursors and who is online, then relays `event`, `rooms`, `invite` and
`ephemeral` frames. A slow client is disconnected and reconnects with cursors. The log is
the truth; the stream is a nudge.

**Typing** crosses households, at most once per person and room every 3 seconds, best
effort. **Presence** is local to a fog. Neither is stored.

A push subscription's endpoint must be a public https address: fog checks it when the
subscription is made and again on the address actually dialed when it sends.

**Notifications carry no content.** Each person's home fog notifies them, for hubbed and
replica rooms alike: `{kind, roomId, label, sender, senderName, seq}` by Web Push (VAPID
keys in the vault; the service worker holds no keys and shows that line) or a personal
ntfy topic. A per-room `notify` of `all`, `mentions` or `none` and a DND window apply;
mentions pierce DND and reach people reading live. Preferences are `dnd`, `receipts`,
`previews`, `ntfyTopic` and `pushEnabled`. Read receipts are encrypted content, sent only
if the person turned them on.

## Bots

A bot is a member with its own keys. Fog has one: the person `fog`, role `bot`, a real MLS
client whose seed is the vault secret `_chat_bot_key` and whose group state is sealed by
the vault under `<data>/chat/bot`. It is in no room by default, beyond the `#fog` channel
it creates, which anyone in the household may join. A member with `canAddParticipant`
adds it like anyone else; it joins by Welcome and can be removed the same way. It reads
and writes exactly those rooms, keeping a plaintext record of their message text in
`chat_bot_messages`; an author's edit or delete (or a delete by a role holding
`canDeleteOtherMessage`) updates it, and once the bot is out of a room it forgets the room
and everything it read there. It is never advertised to peers, so it can only be in rooms hubbed
at home.

| Surface | Reach |
|---|---|
| `#fog` | Inbox notices mirrored in |
| `POST /hooks/chat/<token>` | Posts through the bot. JSON `{text}` or plain text, 64 KiB, 8,000 characters, 60 a minute per hook. Created by `canChangePolicy` once the bot is in the room; the token is shown once. The post carries `system: "webhook:<name>"` |
| `GET /app/v1/chat/rooms`, `POST /app/v1/chat/rooms/{id}/messages {text}` | An app whose spec has `chat` in its `uses` (the operator's grant; any other app gets `403`), by `FOG_API_TOKEN`; bot rooms only; 8,000 characters, 60 a minute; audited. The post carries `system: "app:<name>"` and reads as that app's, never as fog's |
| MCP `fog_chat_rooms`, `_read`, `_search`, `_send` | Bot rooms only. `_send` needs a token minted with *may post to chat*. The post carries `system: "mcp:<name>"` |

The operator's MCP token can't reach the operator's own rooms unless fog is in them.
Clients keep a `system` stamp only from fog's bot (this fog's, or the room's hub's), and
only `fog` or an integration's `app:`, `webhook:` or `mcp:` name, which the line shows as
its speaker; shared history can't carry one, or name the bot as an item's sender.

## Across households

One capability, `chat`, in the [registry](federation-contract.md#capabilities), granted on
both sides. A peer sees people through `people.directory` — the people its grant names, and nobody
when the grant's config is missing or unreadable; identity keys and devices ride along
only with `chat` granted. Key material is served only for people the directory shows.
A cross-household room states itself in the info pane's `impact` sentence.

### The wire

All on the signed channel, behind the `chat` grant, with signed answers except where noted.

| Route | Body and answer |
|---|---|
| `GET /fed/v1/chat/people/{handle}/keys` | `{handle, displayName, identityKey, ciphersuite, devices: [{id, name, kind, deviceKey, attestation, verified, keyPackage}]}`; consumes a package per device |
| `POST /fed/v1/chat/rooms/{id}/submit` | `{actor, deviceKey, input}` → `{event, replayed}` (3 MiB) |
| `GET /fed/v1/chat/rooms/{id}` | Snapshot `{room, groupId, policy, participants, tree, groupInfo, groupInfoEpoch}`, ETag and `304` |
| `GET …/events?after=&limit=` | `{events, seq, epoch}`, limit 500 |
| `GET …/groupinfo?actor=&deviceKey=`, `POST …/groupinfo` | `{groupInfo, epoch}`; post takes `{actor, epoch, groupInfo}` |
| `POST …/update` | `{actor, patch}` (a room change) or `{actor, identityReset: true}` → `{ok}` |
| `GET …/blob/{sha}` | Raw bytes, unsigned, with the room seq it was stored at; the fetcher checks the digest |
| `POST …/blobs?actor=` | Raw bytes → `{sha256, size}` |
| `GET …/blobs`, `POST …/adopt` | Hub hand-over: `{blobs}`; `{seq, epoch, by}` → `{seq}` |
| `POST /fed/v1/chat/notify` | `{items}` → `{ok, refused: [{roomId, person, code}]}` (8 MiB) |
| `POST /fed/v1/chat/ephemeral` | `{roomId, person, signal}` → `204`, never retried |
| `POST /fed/v1/chat/departures` | `{items: [{roomId, person}]}` → `{ok}`: a household's own people's resets and deletions, retried until acknowledged |

Notify items are `{type, roomId, hub, …}`: `event {event}`, `invite {invite}` with a room
snapshot and the Welcome, `moved {newHub}`. Invites are filed behind the answer by at most
two workers per hub, a room's invites as one job with one call back, at most 256 rooms,
200 invites per room and 8 MiB waiting from one hub, and 32 MiB from all; past that they are
dropped. A cut drops a hub's queued invites, and none is filed for a hub no longer granted
chat. An item is dropped unless `hub` is the signing
peer. A follower mirrors a room only when an invite for one of its people brings it, and
shows one of its people in a room only if they already are or were invited; a participant's
household is the one their address names, whatever a snapshot says. A follower's answer may refuse invites it was sent in that batch, for its own people
only; anything else in it is ignored. An invite for someone who doesn't live here, or whom the
hub's `people.directory` grant doesn't show, is answered as one filed would be and sets up
what a filed one would: a replica of the room, held by a decoy invite that names nobody,
synced after the answer and polled after, exactly as a filed one is. Whether a handle is
shown is read from a snapshot of the grant taken once per job, so someone, someone hidden and
no one cost the same. A replica held only by invites nobody has accepted — real or decoy — is
polled for seven days from the newest, then not; decoys are then dropped (never a replica any
of this fog's people were in), and keep no tree or verified state. At most 64 such rooms per
hub, 8 per inviting person and 512 in all are watched — counting only rooms in their watch
window that none of this fog's people has accepted on any device (each person's latest
acceptance is kept past the prune of old invites); past that every new invite that would
add one is answered `invites_full`, whoever it names. That is not a refusal: the hub keeps
the invitee in the room and sends the invite again an hour later. A room any of this fog's people is a member of — in without
an invite, or having accepted one on any device — is polled whatever else is pending. So neither the answer, nor the calls back, nor the
polling that follows tell the hub who lives here. Once someone accepts, their replica is
polled from then on, which says they did. A
`moved` is followed only to a household this fog has granted chat; otherwise the replica
is archived. Only a refusal the hub signed is read as the hub's (its code steers the
client); an unsigned one counts as the hub being unreachable. Room routes need a stake: a current participant or a pending invite of that
household. A household whose people have all left gets only the events up to its last
departure, the blobs stored before it, and a departed snapshot; the hub pushes it the
departure itself. **A household may only act for its own people**: an `actor` from any
other instance is refused.

**Delivery.** **Pull is the truth; push is a nudge.** The hub queues to `fed_outbox` kind
`chat`, one row per event per follower, flushed after 250 ms, retried every 15 seconds,
batched to 200 rows and 2 MiB; an oversize row is dropped, and the hub's log is the record.
A follower polls every 5 minutes and pulls gaps, an unreachable hub backing off to an hour.
A pull steps over seqs the hub's log no longer holds (expired or pruned): the hub's answer
declares them by leaving them out, and its `seq` says where its log ends. Each page must
move the replica forward or the pull stops. The poll pulls from a few hubs at once, each
hub's rooms in turn, each room within two minutes, each hub within four and the pass within
ten; a pass waits on its hubs at most 30 seconds, and a hub still being pulled from is skipped by the next. A
hub's push of an event, a move, and the catch-up after a relayed submit run within one
room's budget too. An archived replica stays archived: no move revives it.
While a hub is down its rooms are read-only everywhere: a relayed submit fails with a `500`
and no code. A household's own channels are hubbed at home.

### Third households

A room hubbed by one household may hold people from a household another member isn't paired
with. The hub vouches: it admits only people its own peers advertise, and every event names
its sender. An unpaired household's fog carries bytes, not words, and gets nothing else: no
directory, no DMs, no follow. `{"acceptThirdHouseholds": false}` on my grant to a hub refuses
its invites to rooms containing a household I'm not paired with: the hub records the invitee
out (`refused:third_household`), and a re-sync refuses to update such a replica.

### Cuts

Unpairing or revoking `chat` calls `PeerCut`. Replicas of their rooms are archived, read-only,
copy kept. Their people are marked left of every room hubbed here, with a `household-cut`
system event; the hub holds no key, so **the first member device to see it commits the MLS
removal**, and that is when keys rotate. Queued rows are dropped. The far side learns by being
refused.

## Clients

- **`clients/chat-sdk`** (`@fog/chat-sdk`, TypeScript) holds everything that touches a key:
  identity, MLS, attachments, the plaintext store, backup. Every call it makes has a deadline
  (30 seconds; 10 minutes for attachments), and it reads fog's directory before catching up,
  so changed keys are known before anything is judged. Leaving a room drops what the device
  kept for it (refusals, provisional joins, stale reasons). `src/index.ts` is the stable 1.x
  surface; its README lists it. It needs a secure context.
- **`frontend/src/pages/chat`** is the reference client.
- **[chat-openapi.yaml](chat-openapi.yaml)** is the HTTP contract. Errors are
  `{"error", "code"}`: `not_found` 404, `epoch_stale` 409, `forbidden` 403, `bad_request`
  400, `need_device` 428, `archived` 410, `moving` 503.
- **`clients/chat-sdk/test/conformance.mjs`** runs against any scratch fogd, including the
  check that the server holds ciphertext only, and inside Go as `TestChatConformanceSuite`.

Budgets: 600 chat calls a minute per person, 120 per app. Payloads: 1 MiB per MLS message,
500 events a page.

## Guarantees and known gaps

Describe the guarantee to people as **designed and tested so the server cannot read your
rooms, not audited**. No external cryptographic review has happened.

**What holds.** Fog's operator, and anyone who takes its disk, can learn the left-hand column
[above](#what-the-server-can-and-cant-see). They cannot read text, attachments, reactions,
receipts or history in a room where they hold no device seat, and the bot's rooms are the
deliberate exception. An admin who isn't a participant can't read a room. Conformance
proves no event payload, Welcome or blob on the server contains the plaintext.

The hub orders only commits it has verified from public data — signature, leaves, tree,
parent hashes, the next GroupContext — and only GroupInfo that matches it, and the same
structural and policy rules hold at the hub and in every client. No membership change
travels encrypted past it, and no commit replaces a leaf other than its committer's. Only
devices their person's identity key attests are added or let themselves in, for local and
remote people alike. An identity reset takes the person out of every room with a warning
there, and the new identity needs a member to add it. Roles move only below one's own
rank, and nobody demotes or removes an owner. A ban removes and holds until lifted. A DM's
people are fixed at whichever household hubs it. Room changes made on a follower are
decided by the hub. Whoever leaves — removed, banned, cut, reset or by choice — and any
household left with no one in a room, gets nothing sequenced after: not the log, not the
stream, not attachments, not the room's details, and members' devices encrypt nothing
more to their leaves. A paired household speaks only for its own people and invites,
follows a room only to a household granted chat, places no room in anyone's list uninvited,
learns nothing of who lives here from an invite, can't take over another household's room
id or invite under one a hand-over took from it, can't hand a room to a household none of whose members agreed to host it, and can't
drain anyone's key packages. After an identity reset no client trusts the person's new key
until a member re-adds them or safety codes are compared, and the reset person can't let
themselves back into any room. Lines a client makes itself can't be edited, deleted or
imitated by members, and a leaf the hub can't account for shows as such. No device key
speaks under another name, and every membership change is a line. A Welcome can't cut a
newcomer off into a look-alike group, or leave one sending before it matches the group fog
verified. What a client does with an MLS event is decided by its bytes, never fog's label.
Safety codes come from the keys a device holds, so a fog swapping keys can't make them
agree. A deleted person's rooms, roles and bans pass to nobody who is given their handle,
an address with another identity key behind it can't walk into an open channel, and word of
a reset or deletion reaches every hub however long it is away. A room refused over a device
nobody vouches for can be recovered by removing it, and stays refused until then. One hub, room or notify request can't stall a
follower's sync or make it do unbounded work, and attachments are held to quotas. What fog's bot
posts for an app, webhook or MCP client says so, and no member or shared history can speak
as fog. No hub, follower or member can put a device into a
group that every member's client doesn't verify against the identity keys it holds, and an
attachment or link a sender chose can't run in fog's origin or make a reader's browser
fetch anything.
Each of these has a test that performs the attack.

**Known gaps.** What follows is inherent to a server that holds no keys, or to the design,
and each says why it can't be used to read a room.

1. **Metadata.** Membership, timing, sizes, labels and mention hints are visible to the hub,
   by design: it orders and routes the room. Web Push also hands room label, sender and kind to
   the browser vendor's relay.
2. **A member can still break a room for everyone.** What only the group's secrets can check
   — the confirmation and membership tags, that the encrypted path secrets decrypt, an external
   join's KEM output, a GroupInfo's `external_pub` — the hub can't check without holding those
   secrets, which is exactly what it must not do. So a member can sign a commit that is right
   in every public respect and wrong inside; members' clients refuse it and mark the room stale.
   That is denial of service by someone already in the room, attributed to them by their
   signature; it reveals nothing and changes no membership, and the remedy is a new room
   without them. A difference between how mls-go and ts-mls read the same bytes would show the
   same way, as a stale room, never as a membership the hub didn't check.
3. **Roles and policy are the hub's.** A person's address is resolved to one canonical form
   (`fog:<instance>/<handle>`) wherever participants are compared, so a ban or a role can't be
   walked round with a ULID alias. Who may delete others' messages, pin or share history
   is hub state, not agreed inside the MLS group, so clients judge content acts by the role the
   hub recorded for the sender at sequencing time. A lying hub could make a member's delete or
   pin count as a moderator's; it gains nothing it doesn't already have, since it can withhold
   any event outright. Clients don't re-check who may add or remove whom; the hub does.
4. **Verification rests on the keys fog serves.** A person's identity key comes from their
   home fog, which can substitute it for its own people, as can an operator with write access
   to fog's database. Clients warn when a key differs from the one they pinned and drop the
   verification mark, and a changed key can't rejoin a room without a member adding it; an
   unpinned first contact trusts the key fog serves until safety codes are compared in person.
   After a reset, or when fog serves a key other than the pinned one, a member's re-add is
   taken as vouching for the new key: a member who adds whatever fog serves passes that trust
   on to everyone in the room (never fog's bot).
   For people who joined before fog kept identity keys per participant, the hub takes the key it
   first sees on a self-join as theirs.
5. **Removal waits for a member device.** The hub can't rotate keys. After a departure the
   person's leaves stay in the group until a member device commits their removal, which the
   first to see the departure does within seconds, and no member device sends anything in the
   meantime. The departed person is sent nothing either. A reset or deletion of someone in a
   room hubbed elsewhere reaches that hub when it can be reached (the queue keeps it until
   then); its identity checks hold regardless.
   Fog's bot waits the same way.
6. **Shared history is the sharer's word.** A device can't check who said what before it
   joined; it shows shared history as shared by its sender and never lets it change messages it
   received itself. The room's members were all entitled to it.
7. **Plaintext outlives the hub.** Retention and `expires` prune the server; clients keep what
   they decrypted. Any member can always keep a copy.
8. **The phrase trades forward secrecy for recovery.** A stolen phrase plus a sign-in recovers
   history and keys. The UI says so.
9. **The web client ships from the fog.** A compromised fog can ship a compromised client;
   subresource integrity can't help when the same server serves the page. A standalone app
   pinned to a release avoids this.
10. **`since-join` is a floor fog applies to its own people.** A follower holds the whole
    ciphertext log of rooms its people are in, from before they joined, for epochs none of its
    people hold keys for.
11. **Hub hand-over asks one member, not all.** The old hub can hand a room to a paired
    household with someone in it who holds `canChangePolicy` and agreed; the other members
    aren't asked. That household gets what any follower holds —
    ciphertext and the verified public state — and gains no way in: it can add no device,
    since every member's client verifies every leaf against the identity keys it holds, and
    followers follow only to a household they granted chat.
12. **A moved room can't invite households that know its first hub.** A room id is bound
    to the household that made it. A fog that already follows a room follows it through a
    hand-over; one that doesn't, and is paired with the room's first household, refuses an
    invite into it from anyone else, since it can't tell a hand-over from a squat. Likewise
    a paired household that learns a room id bound to a household this fog doesn't know can
    set up a replica under it first, and the real hub's invite is then refused
    `room_id_taken` (reported to that hub). Neither reveals anything or admits anyone.
13. **Not done:** external review, threat model sign-off, load tests of large groups, and
    fuzzing of `mlsview` and the CBOR decoder beyond unit tests. Hub CPU is bounded by the
    payload cap only.

## Not built

MIMI transport (`mimi://`, mTLS), a Matrix bridge, custom emoji, native push endpoints, QR device
transfer, encrypted room names, room avatars, attachment chunking and thumbnails, abuse reports,
knocking, a hub-offline banner, an operator choice to remove an archived replica, turning off
typing or presence, inline actions from `#fog`.

## Versioning

`fog-chat/1` is the wire above and content profile `v: 1`. Payload fields are additive; an unknown
content version is refused. Anything that changes framing, the ciphersuite or how a client must
treat a commit bumps the version.
