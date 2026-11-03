# fog-federation/1 — fog to fog

*Sibling of fog-connect/1 ([connect-contract.md](connect-contract.md)),
fog-files/1 ([files-contract.md](files-contract.md)), fog-chat/1
([chat-contract.md](chat-contract.md)) and fog-signin/1
([signin-contract.md](signin-contract.md)).*

Federation is the mutual, keyed, capability-scoped relationship between two
fog instances. An operator pastes one invite code; from then on the two speak
signed HTTPS to each other's `/fed/v1` endpoints, and every shared thing —
errors, app directory, Connect, folders, chat, sign-in — is a toggle on that
pairing, never a new credential exchange.

This document is the **federation plane**, fogd to fogd. The **app plane** is
app to app, directly, in the app's own protocol, using addresses and
credentials this plane handed over; fog never proxies app traffic.
`fog-federation/1` is not something an app declares in `fog.yaml`.

- **The owner enforces.** What a peer may do here is decided, stored and
  checked here on every request. What a peer claims is display data.
- **The receiver assigns identity.** Origin comes from the verified key, never
  from a payload.
- **Invite-only and pairwise.** No discovery, no gossip. Trust does not cross
  two hops, with one bounded exception:
  [third households](chat-contract.md#third-households) in chat.
- **Fog is a convenience.** An app outside fog keeps everything it has.

## Instance identity

Minted lazily on first use, then permanent:

- **Instance ID**: the stable name in every peer's database, in every person
  address and every room — so it is the identity key's own: 26 characters of
  Crockford base32 over the marker bits `10` and the first 128 bits of
  `sha256("fogfed1-instance\n" || public key)`. A derived ID starts with one of
  `G H J K M N P Q`. Wherever a key is taken in for an ID (a pairing either way,
  an instance document), a derived ID its key doesn't derive is refused, so no
  household can claim another's. Instances minted before this have a random ULID
  (starting `0`–`7`), which can't be checked against a key; see
  [Key-bound pairing](#key-bound-pairing).
- **Ed25519 keypair**: the seed is vault-encrypted as `_fed_identity_key`; the
  public key sits in the singleton `fed_identity` row. Fog refuses to load an
  identity whose seed and row disagree.
- **Encryption keypair**: X25519, the key every body sent to this instance is
  sealed to (see [Sealed bodies](#sealed-bodies)). Its seed is vault-encrypted
  as `_fed_encryption_key`, the pair is HPKE's `DeriveKeyPair` of it, and the
  public half sits in `fed_identity.enc_public_key`. An identity minted before
  sealed bodies gets one the first time it loads. The identity key binds it:
  `encryptionKeySig` is the transcript
  `fogfed1-enckey\n<instanceId>\n<sha256hex(key)>`, signed.
- **Display name**: the `server_name` setting, default `fog`.
- **Advertised URL**: `fed_advertised_url` when set, else
  `https://fog.<base_domain>`, else empty (dial-out only).

The identity document exchanged at pairing is
`{instanceId, name, url, publicKey, fogVersion, encryptionKey, encryptionKeySig}`;
one without a bound encryption key is refused. A key's **fingerprint** is
the SHA-256 of the raw key as colon-separated hex (short form: four bytes), for
comparing over the phone.

## Key-bound pairing

**Every new pairing is between key-bound IDs.** A pairing that would make a new peer
row — either side, inviting or joining — needs this fog's own instance ID and the
other side's to be derived from their keys, the other's checked against the key in
its document. A household already held here (a row with its keys) re-pairs under the
rules it was paired under, so pairings made before key-bound IDs keep working with
their pinned keys.

- **A legacy fog pairs with no one new.** Minting an invite is refused (`409`), and so
  is joining one unless its encryption key is a held peer's — before anything is
  dialed. The Connect page says: this fog's identity predates key-bound IDs; renew it
  to pair. An inviter refuses a legacy joiner, and a legacy inviter any new joiner,
  before the invite is spent, saying which side must renew.
- **Renewing** (`POST /api/federation/identity/renew {breakPairings}`) mints new
  identity and encryption keys and the ID they derive. Everything that knows the fog
  by its old ID stops recognising it: paired households, and chat, whose addresses
  and room memberships carry the instance ID. So when any household is paired or any
  chat room exists, a request without `breakPairings: true` is a `409`
  `{error, households, chatRooms}` saying exactly that; with it, the identity is
  renewed, an Inbox notice names the households to pair again, and the ledger gets
  `federation.identity.renew`. Peer rows are kept, so pairing again refreshes each in
  place, grants and all; this fog's own contributions in peers' shares are carried
  to the new ID. The new seeds are staged in the vault before the identity row is
  rewritten, and a renewal interrupted between the two is finished on the next load.
- **A row is checked wherever it stands for a household.** A peer row whose ID is in
  the derived form but isn't its key's — accepted by a fog from before the check —
  stands for nobody: its requests are refused (`key_unusable`), nothing is sent to it,
  and chat doesn't count it as pairing that household (a person address's home, a
  room's hub, the third-household rule).

## Pairing: one paste

An invite is `FOG-` plus the base64url of
`{"u": url, "s": secret, "n": name, "k": encryptionKey}`: where to dial, a random
single-use secret (only its SHA-256 is stored), the inviter's name, advisory and
optional, and the inviter's encryption key, which the pair request is sealed to.
A code without `k` is from a fog that doesn't seal bodies and is refused. Invites live 24 hours. With no advertised
URL the inviter uses the address the operator reached the UI at.

1. `POST /api/federation/invites {grants: [{capability, config?}]}` stores the
   pre-selected grants and returns the code once.
2. `POST /api/federation/join/preview {code}` decodes it without dialing or
   spending anything.
3. `POST /api/federation/join {code}` has the joiner's fogd call
   `POST /fed/v1/pair` with `{secret, instance}`, sealed to the invite's `k` (the
   recipient named `pair:<k>`) and signed with its own key.
4. The inviter opens the request (one that isn't sealed to its key is refused)
   and rate-limits it, 10 per 15 minutes: a request naming a live invite against
   that invite's own allowance, any other against the caller's address — the
   outer request's, as fogd's proxy vouches for it — so guessing at secrets from
   behind the same proxy never uses up the invited household's attempts. It then
   requires `Fog-Instance` to match the
   document, verifies against the key inside the request and the document's
   encryption-key binding, atomically claims the invite, stores and pins the peer's
   keys, applies the grants, and answers `{instance, grants, sig}`, sealed to the
   joiner's encryption key. Wrong, spent and expired secrets get the same `403`.
5. The joiner opens the answer, verifies `sig`, an Ed25519 signature over
   `fogfed1-pair\n<secret>\n<sha256hex(instance bytes)>`, with the key in the
   answer, checks the document's encryption key is the invite's `k` and bound by
   that key, stores and pins the peer, and heartbeats at once. `grants` is outside the
   signature, so the joiner records nothing from it; the peer's grants arrive in
   that first, signed heartbeat.

Both sides grant `people.directory` by default (`{"people": "members"}`) unless
the invite set it.

**A handshake completes on both sides or neither.** The inviter finishes before
the joiner sees the answer, so the joiner never refuses committed work. Either
side refreshes in place when it already holds that instance **under the same
keys**: re-pairing works from either end, moves the URL (an empty one never
clears it), and keeps the name, label, grants and history. **An invite's grants
go to a new peer only.** A household already paired here that pairs again with
some invite — leaked, or minted for someone else — keeps exactly the grants it
has, and none of the invite's are applied; if it carried any, the operator gets a
`fed-pair-invite-unused` notice. Whether a household is already paired is this
side's own record — its row — never `unpaired_at`, which the peer's own answers
stamp; only the operator removing the row here makes it a new peer again.

**A paired instance's keys never change.** Fog has no key rotation: an
instance's ID and keypairs are permanent. A pairing that names an instance already
held here under any other identity key, or any other encryption key, is refused on
either side — `409` from `POST /fed/v1/pair` (after the claim, so the invite stays
spent) and from `POST /api/federation/join` — and changes nothing: the row keeps
its key, address and grants, and the invite's grants land nowhere. The operator
gets a `fed-pair-key-conflict` notice with both fingerprints, and the ledger a
`federation.pair.refused` row. Migrations 0070 and 0072 make `public_key`,
`instance_id` and a known `enc_public_key` immutable in `fed_peers` beneath the
code. A pairing that claims
this fog's own instance ID is refused the same way. A household that reinstalled
from scratch has a new identity: the operator removes its old entry and pairs
again. Names are slugs of the advertised name, at most 32 characters, with `-2`,
`-3` on collision.

## The wire

`/fed/v1` sits outside `/api`, so session auth and the Origin guard don't apply.

### The envelope

**On the wire every exchange looks the same:** a `POST /fed/v1/sealed?to=<recipient>` whose body
is the real request, sealed, answered by a `200` whose body is the real answer,
sealed. Method, path and query (a file's name, a handle, a room), the headers that
say what is asked (`If-None-Match`, `Upload-Offset`, a relay ticket), the answer's
status and headers (`ETag`, a 304, a 404) all travel inside. Any other `/fed/v1`
path, or a body that isn't sealed, is refused. The inner request must be a
`/fed/v1/` route, already clean (no `..`, `.` or doubled slash), and not the
envelope itself: anything else — the API, the UI, a path that climbs out — is a
sealed `404` (`no such fed route`) and reaches no handler.

**Every request names its recipient, under its signature.** The outer target is
`/fed/v1/sealed?to=<recipient>` — the receiving fog's instance ID, or for the pair
hop its pair recipient `pair:<k>`, escaped as a query value — and the signature
covers the target. A request naming any other recipient, or none, is refused
(`wrong_recipient`) before anything is read or held: headers signed for one fog are
worth nothing at another, however they got there.

**A peer is known by its headers; strangers wait in their own line.** A request
declares its sealed body's digest in `Fog-Content-SHA256`, so its signature verifies
from the headers before a byte of the body is read. Until the body is in and hashes
to that digest, though, the headers are all there is, and headers can be copied by
whoever saw them. So they are reserved in the replay record as they arrive — a copy
of them is refused (`replayed`) while this one is pending — and the reservation is
given back if the body doesn't come or isn't the one signed. The request takes one
of that peer's 8 slots; when all are held, a holder whose body hasn't verified and
is arriving slower than 16 KiB/s after its first 2 seconds gives way to a later
request of the peer's, its read cut off. A holder making real progress keeps its
slot, and otherwise the newcomer is turned away (`503`). Nothing is answered under
this fog's signature until the body verifies.
Everything else — a pairing, a caller with no row, headers that don't verify — is
read unauthenticated: one of 32 slots in all and 4 per client network (an IPv4
address, an IPv6 /64; through fog's own Cloudflare tunnel, the visitor's as
`Cf-Connecting-Ip` names it), turned away with a `503` before anything is read when
none is free; no more than a sealed pair request's worth (64 KiB plaintext) from a
caller with no row, or 1 MiB from a known one. Strangers never touch a peer's
slots. In either line the body must be in by an absolute deadline: 10 seconds plus
its declared length at 16 KiB/s, so a trickle holds nothing longer, however it
trickles.

**An envelope is held to its route.** The first record names the inner method and
target, so while the rest is still arriving the read is held to what that route
takes — 64 KiB for a `GET` or `DELETE`, 1 MiB for most `POST`s, 8 MiB for flare,
feedback, chat notices and contribution chunks, chat's own sizes for its blobs,
submissions and group info; a refusal's worth (64 KiB) for a route whose grant the
peer doesn't hold — and refused with a `413` past it. Nothing is acted on from that
peek: the whole body is verified first. Bodies past 1 MiB are spooled to disk, not
held in memory — to `fed-spool` under fog's data directory, never `/tmp` (memory on
the systems fog runs on), within 1 GiB across every request at once (a `503` past
it), and swept at start — and once verified the inner body is opened
record by record as its handler reads it, so the handler's own limit applies as the
bytes come. fogd's listener has a 10-second header timeout and a 2-minute idle
timeout. Caddy, in front of fogd by default, streams request bodies through
without buffering them, so these limits are fogd's own and hold with or without it.

```
POST /fed/v1/sealed?to=<the recipient's instance ID>
Fog-Instance:           01J5XW…
Fog-Timestamp:          2026-08-13T17:04:05.123456789Z
Fog-Signature:          <base64url Ed25519 over the sealed body>
Fog-Content-SHA256:     <sha256hex of the sealed body, which the signature covers>
Fog-Encryption-Key:     <the caller's X25519 key>
Fog-Encryption-Key-Sig: <its identity key's binding>

body   = Seal(request info, u32 len(R) || R || inner body)
R      = {"method", "target", "headers"}

200, application/octet-stream
body   = Seal(answer info, u32 len(H) || H || u16 len(S) || S || inner body)
H      = {"status", "headers", "digest", "instance", "ts", "encryptionKey", "encryptionKeySig"}
S      = the responder's ANSWER signature over H (below)
```

The receiver authenticates the outer request, opens it, and hands the inner
request — `POST /fed/v1/heartbeat`, `GET /fed/v1/files/shares/7/blob/…` — to its
handler, exactly as if it had arrived on its own; whatever the handler answers is
sealed back in one place (`fedPlane`). An empty body is the connection proof.

### Signed requests

```
signed: fogfed1\nPOST\n/fed/v1/sealed?to=<recipient>\n<timestamp>\n<sha256hex(sealed body)>
```

The receiver finds the peer by `Fog-Instance` (which selects a key and asserts
nothing), rejects timestamps outside ±5 minutes, verifies, and only then opens and
dispatches. The signature covers the sealed bytes, and through them the inner
method, target, headers and body. No secret is stored about a peer; deleting the
row is total revocation.

**Every encoding has one spelling.** Signatures are unpadded base64url of exactly
64 bytes, keys padded standard base64 of exactly 32, and both are decoded
strictly: a final character with its spare bits set, padding where there is none
(or none where there is), or whitespace is refused, not read as the same bytes.
The conformance vectors list such respellings, refused on both sides.

**A signed request is accepted once.** The timestamp carries nanoseconds and
never repeats within a signer, so no two genuine requests are the same bytes.
The receiver writes every request it accepts into `fed_seen_requests`, keyed by
the sha256 of its instance and its signature's decoded bytes, and refuses one
already there with `replayed`. A request is entered when it is taken — when its
headers verify, as a reservation given back only if its body fails, or when a body
read before verifying does — and kept until long after any copy of it could
complete: its timestamp plus the five-minute window, plus the longest any body may
take to arrive (10 seconds plus the largest envelope at 16 KiB/s), plus a minute.
At completion the timestamp is checked again: within the window, widened only by
the time that request's own body was allowed. The record is in the database, so a
restart doesn't re-open the window.

### Signed answers

Every answer — a `2xx`, a `304`, a refusal — carries a head signed by the
responder under a pseudo-method no request can carry, bound to the request it
answers by that request's `Fog-Instance` and `Fog-Signature`:

```
ANSWER: fogfed1\nANSWER\n/fed/v1/sealed\n<head ts>\n<sha256hex(status "\n" instance "\n" sig "\n" H)>
```

H carries the status, the headers and the sha256 of the inner body, which the
requester checks as the body is read to its end: a file streams with bounded
memory, and one cut short, changed or swapped fails at the end rather than
landing as something shorter. A captured answer verifies for no other exchange —
not a later request, not another caller's request carrying a copied signature —
and the status is part of what is signed, so a refusal can't pass for a `200`, nor
a `304` confirm a version it didn't name (its `ETag` is in H). The conformance
vectors in `clients/fogfed` carry known answers for the signature and whole
envelopes both ways, and the Ruby gem makes and reads them identically. The older
`RESPONSE` and `ANSWER-DIGEST` forms are neither sent nor accepted.

### Sealed bodies

**Every body is encrypted to the fog it goes to**, on top of being signed: request
and answer, a file's bytes, the proof, the pair hop. It works the same over
`https`, plain `http` or a bare LAN address, with no setup. The construction is
HPKE (RFC 9180) base mode from Go's standard library — DHKEM(X25519, HKDF-SHA256),
HKDF-SHA256, ChaCha20-Poly1305 — one context per body, framed as records so a file
of any size streams with bounded memory:

```
body   = 0x01 || enc (32 bytes) || record*
record = len (uint32 BE) || Seal(aad = final ? 0x01 : 0x00, ≤ 64 KiB of plaintext)

request info: fogfed1-request\nPOST\n/fed/v1/sealed\n<requester>\n<recipient>\n<timestamp>
answer info:  fogfed1-answer\nPOST\n/fed/v1/sealed\n<requester>\n<request sig>\n<responder>
```

HPKE's sequence numbers make the records' order part of the authentication; the
final flag means a body cut short is an error rather than a shorter body, and
nothing may follow it. The info binds a body to its exchange: a request body to
its sender, recipient and signed timestamp (the signature covers the sealed bytes,
so the timestamp — unique per signer — stands for it); an answer to the request it
answers. A sealed body moved into any other exchange, sent under another peer's
signature, or sealed to another key doesn't open. A request is sealed then signed;
the receiver verifies, records it against replay, then opens. An answer's head is
signed, then the answer sealed to the requester's pinned key; the requester opens,
then verifies the head.

**Keys are learned signed and then pinned.** Every request carries
`Fog-Encryption-Key` and `Fog-Encryption-Key-Sig` — the caller's key and its
identity key's binding — and every answer's head the responder's. A pairing made
since sealed bodies pins both sides' keys from the instance documents. A pairing
older than that has none pinned: each side takes the peer's key the first time a
request, or a proof's answer, verified under the peer's pinned identity key,
presents it with a binding that verifies, and pins it. A key without that binding
is never taken, and is no evidence of anything: the headers are outside the
signature, so a key swapped in on the way is refused (`encryption_required`) with
no pin and no notice. A different key, bound by the peer's identity, from the
pinned one is refused
(`encryption_key_changed`), changes nothing, and raises a `fed-enc-key-conflict`
notice — the no-silent-rekey rule identity keys follow.

**There is no unsealed fallback.** A request without a key, with one its identity
doesn't bind, to any path but the envelope's, or with a body that doesn't open, is
refused (`encryption_required`, `unsealed`). A peer whose proof isn't sealed, or
whose head presents no key, is sent nothing (`fed.ErrNoEncryption`); an invite
without `k` is refused before anything is dialed.

**A refusal is signed only for a request that verified.** A request whose body
hasn't verified — no instance, another recipient, a bad signature, a body that isn't
the one its headers were signed for, a full pool, too large, too slow, headers that
verified but a body not yet in — is refused in the clear, unsigned and unsealed, and
nothing is sealed to an encryption key that hasn't been verified, and the requester counts that as nobody's word: bound only to headers anyone
can copy, a signed refusal could be relayed as this fog's answer to another request.
The one exception is `unknown_peer`, signed and sealed to the key the request
presents: it names an instance this fog holds no row for, so it can't stand in for an
answer to a request this fog would take.

### Proven connections

**Nothing is sent to an address until it proves it is the peer.** A peer's stored
address can outlive it — stale DNS, a recycled lease, a lapsed domain. So the
first thing written to every new connection is the proof — an envelope with an
empty body, signed — and the connection carries a request only if the answer opens
with this fog's key and its head is signed by the peer's key for that proof. The
head carries the peer's bound encryption key, pinned then if it wasn't. One that
can't prove it is closed with nothing else sent (`fed.ErrUnproven`); a proof the
peer refuses with a signed `unknown_peer` is read like any refusal. Connections are
pooled per peer, never shared between two peers at one address, go through no
proxy, and the fed plane follows no redirects: a `3xx` isn't the envelope, and the
request is never sent on. The only exception is the pair hop, which has no key yet
to prove: what it sends is sealed to the key the invite carries.

### Rejections

Every auth refusal is a `403` with `{"error": "…", "code": "…"}`, additive-only —
sealed in the envelope when the request verified (and for `unknown_peer`), in the
clear otherwise:

| `code` | Meaning |
|---|---|
| `unknown_peer` | No peer row for that instance: the pairing is gone from this side |
| `missing_instance` | No `Fog-Instance` header |
| `key_unusable` | The stored key won't parse |
| `bad_signature` | Row and key are fine; the signature failed (skew, tampering, a new key) |
| `replayed` | The signature verified and was already accepted once: the same signed request again |
| `encryption_required` | No encryption key presented, one the caller's identity doesn't bind, or a pair request not sealed to the invite's key |
| `encryption_key_changed` | An encryption key other than the one pinned for that instance |
| `unsealed` | The body doesn't open for this exchange |
| `wrong_recipient` | The target names another recipient (`?to=`), or none |

`unknown_peer` is the whole disconnect protocol. **Nothing is pushed on unpair.**
The dialing side counts consecutive `unknown_peer` answers to its heartbeats and at
three (about six minutes) stamps `unpaired_at`. Only a refusal the peer's own key
signed for that heartbeat counts, so neither a proxy's `403` nor a stranger at the
peer's old address can read as an unpairing; any verified exchange clears it. The
stamp is a claim about the peer: it removes no row, grant or history. A peer that
reinstalled answers with a new key, so its refusals don't verify against the old
row: that row reads as unreachable, never as unpaired.

### Endpoints

The one route on the wire is `POST /fed/v1/sealed`. These are the inner requests it
carries:

| Route | Auth | Purpose |
|---|---|---|
| `POST /fed/v1/pair` | invite secret | The handshake |
| (empty body) | signed | A new connection's proof; records nothing |
| `POST /fed/v1/heartbeat` | signed | State exchange (1 MiB) |
| `POST /fed/v1/flare/envelope` | `flare.ingest` | Error batches (8 MiB) |
| `POST /fed/v1/feedback` | `feedback.ingest` | Feedback batches (8 MiB) |
| `POST /fed/v1/connect` | signed, then Connect's checks | [connect-contract.md](connect-contract.md) |
| `POST /fed/v1/signin/par`, `/token`, `/logout`; `GET /fed/v1/signin/keys` | signed; `par` needs `people.visit` | [Visitors](signin-contract.md#other-households) |
| `/fed/v1/files/*` | `files.share` | [Files across peers](#files-across-peers) |
| `/fed/v1/chat/*` | `chat` | [chat-contract.md](chat-contract.md#the-wire) |

### Heartbeat

`fed.Loop` dials each addressed peer every 2 minutes, four at a time, with a
leading exchange at start. One dial refreshes both sides.

```json
{
  "fogVersion": "0.14.23",
  "url": "https://fog.example.com",
  "grants": ["apps.directory", "people.directory"],
  "directory": [{"app": "feed", "url": "https://feed.example.com",
                 "contracts": ["fog-connect/1"]}],
  "people": [{"address": "person:01J5XW…/01J6…", "handle": "sam",
              "displayName": "Sam", "role": "member", "devices": []}],
  "feedback": []
}
```

- Every field is recorded only from a verified payload: a signed request, or an
  answer signed by the peer's key. An unsigned answer fails the exchange.
- A payload signed earlier than one already applied from that peer — two
  exchanges crossing, one each way — is not applied, and the exchange still
  succeeds. (A replay can't arrive at all: requests are accepted once, answers are
  bound to their request.)
- `url` moves the stored address. Empty never clears a known one. A domainless
  box advertises its live LAN address.
- `grants` are the names we grant them. Display only.
- `directory` needs an `apps.directory` grant; `people` a `people.directory`
  grant. Identity keys and devices ride along only with `chat` granted too. Guests
  and bots are never advertised, and the type has no field for email, sessions,
  app roles or presence.
- `feedback` is the status of reports this peer relayed here: state, not events. It
  applies only to reports delivered to that peer, only while this side still sends it
  reports (the `feedback.ingest` use row), and only with a stamp no later than five
  minutes ahead of this clock, taken as now if it is ahead at all.

**Online** means a verified exchange in either direction within 6 minutes. Each
side also measures its own dials in memory, since a peer can reach us while we
can't reach it. Five failed dials across at least 8 minutes raise one
`fed-peer-unreachable` notice. A refusal the peer signed counts as an answer; an
answer the peer's key didn't sign — a proxy's error page, a stranger at its old
address, a connection that fails its proof — counts as a failed dial. Streamed
transfers count only their transport failures; the heartbeat proves reachability.

A peer's `url` is nullable. One reachable side is enough, because a heartbeat
response carries the responder's whole payload. `computeReachability` states the
verdict (`reachable`, `url`, `via`: `advertised` | `domain` | `none`, tunnel state,
advice). Unreachable is a soft state, never an error.

## The peer model

| Table | Holds |
|---|---|
| `fed_identity` | This instance |
| `fed_peers` | `instance_id`, `name`, `label`, `url`, `public_key`, `enc_public_key`, `fog_version`, `remote_grants`, `directory`, `people`, `last_seen_at`, `unpaired_at`, `reject_streak` |
| `fed_capabilities` | `(peer_id, capability, direction)` with a JSON `config` |
| `fed_invites` | `secret_digest`, pre-selected `grants`, `expires_at`, `claimed_by` |
| `fed_outbox` | Queued rows per peer and `kind` (`flare`, `chat`) |

`name` is the unique slug everything machine-facing uses. `label` is the operator's
free-form display name and addresses nothing. An empty `url` means dial-in only.
`remote_grants`, `directory` and `people` come from the last verified heartbeat and
are not enforcement inputs, except that a hub checks a remote device against
`people` before admitting it.

**Unpair** (`DELETE /api/federation/peers/{id}`) runs while the channel still
exists: files say `revoked`, chat cuts the household, visits end, the row goes
(cascading its capabilities, outbox rows and relayed error groups), and contracted
apps are asked best-effort to revoke what Connect minted.

## Capabilities

The registry is `fedCapabilities` in `internal/server/fed_handlers.go`; unknown names
are refused. **Grant** is what the peer may do here; **use** is what this instance
does toward the peer. A row's presence is the toggle, and enforcement reads grant
rows on the receiving side.

| Capability | Grant | Use | Config |
|---|---|---|---|
| `flare.ingest` | Peer may post error envelopes here | Forward our errors there | use: `{mode: redacted\|full, apps?: […]}`, `apps` non-empty when present |
| `feedback.ingest` | Peer may send reports here | Send ours there | use: `{mode: note\|full}`, required |
| `apps.directory` | Publish our apps in heartbeats | Show this peer to our apps | grant: `{}` every app, `{apps: […]}` exactly those (`[]` is none) |
| `apps.introduce` | Peer's apps may connect to ours | Unused | grant: `{}` every contracted app, `{apps: […]}` exactly those (`[]` is none) |
| `files.share` | Peer may read what we share with it | We subscribe to it | use: `{bandwidthBps?}` |
| `chat` | Peer's people may share rooms with ours | Unused | grant: `{acceptThirdHouseholds?}` |
| `people.directory` | Peer sees who is here | Unused | grant: `{people: "members" \| […]}` |
| `people.visit` | Our people may sign in to the peer's apps | Unused | grant: `{people: "members" \| […]}` |

Configs are checked when written — by `PUT /api/federation/peers/{id}` and when an
invite is minted — against exactly the shapes above, and anything else is a `400`.
An app scope is read strictly where it is enforced, too: one that doesn't parse
covers no app, never every app.

`files.share`'s grant is written when a share is created, its use row on subscribing.
Withdrawing the grant stops every share to that peer as if each were stopped by hand
(the peer is told `revoked`, the rows go), so a later share can't bring the old ones
back. `chat` must be granted on both sides for a room to span them; revoking it cuts
the household from every shared room. Withdrawing `apps.introduce`, or narrowing its
allow-list, asks each contracted app it no longer covers to revoke what it minted for
the peer (`DELETE /fog/connect/grants/{instance}`), as unpairing does. **A grant
names apps, not names:** revocations at an app are owed durably — recorded per peer
and app, and retried every minute until the app holding the name answers (see
[connect-contract.md](connect-contract.md#delete-fogconnectgrantsinstance)) — so a
peer unpaired while the app was down is still revoked when it comes back. Deleting an
app freezes minting for it, then owes a revocation to every peer it ever minted for
and every paired one, and pays what it can while the app still runs. Those
credentials live in the app's own data, so a delete that would keep the data stops
with a `409` (`connect_revoke_failed`) while anything is owed at that app, for any
peer — the operator may delete the data too, or keep it anyway by saying so
(`keepUnrevoked=1`), and then the debt is paid by whatever app later holds the name.
The delete then takes the app's name out of every
`apps.directory` and `apps.introduce` grant that lists it (a grant for every app is
left as it is; it never named the app). A new app that takes the name is covered by
no grant made for the old one. A use row
reaches what is already queued under it: removing or changing `flare.ingest`'s drops
the queued events, removing `feedback.ingest`'s drops queued reports, and narrowing it
to `note` sends queued reports without their dossier.

`PUT /api/federation/peers/{id}` applies name, label, URL and capabilities whole or
not at all, and kicks a heartbeat. `GET …/mirror` renders what the peer sees of us
in the [consent grammar](consent-grammar.md). `POST …/recheck` dials now: a peer that
really unpaired refuses and keeps the stamp, one mid-restore clears it.
`GET /api/federation/ledger` is every boundary crossing, audited, on three axes
(`trust`, `discovery`, `sharing`), with the apps' own ledgers merged in.

## Flare forwarding

Forwarding is the use side of `flare.ingest`, per peer. **Scrubbing happens at
enqueue**, per destination: in `redacted` mode an unscrubbed payload never sits in a
row. Redaction is an allowlist (`event_id`, `timestamp`, `platform`, `level`,
`transaction`, `release`, `environment`, `fingerprint`, `message`, and the exception
chain rebuilt through the typed parser, dropping frame-local `vars`). The `fog.app`
tag is stamped after, and the receiver trusts it for the app's name only.

Consent is read on every event and again just before a batch is sent, so turning
forwarding off (or changing its mode or apps) stops it with the next event, and drops
what was queued under the old consent. A batch is one Sentry envelope
(`application/x-sentry-envelope`), at most 50 rows and 2 MiB, every 30 seconds; a row
over 4 MiB is dropped. The receiver checks the grant,
limits to 120 envelopes a minute per peer, dedupes on `event_id` within a group, and
answers a signed `{"accepted": n}`. A relayed event is never forwarded on. The local
copy is the durable record; this stream is a courtesy.

`POST /fed/v1/feedback` takes `{items: […]}` and answers
`{accepted: […], refused: {uid: reason}}`.

**The outbox** (`fed.Outbox`) drains flare, chat and feedback, each keeping its own
queue and settlement. Peers drain independently, four at a time. `fed_outbox` keeps
1,000 rows per peer and kind, trimming the oldest. A failed batch waits 30 seconds
doubling to an hour. An answer must be signed before rows settle, since the address
may have outlived the peer.

## Files across peers

The rules an app sees are in [files-contract.md](files-contract.md); this is the wire
and lens beneath them. A **share** is one folder, one peer, one lens, and the only
thing naming what crosses. A peer sees a title, never a path.

| Axis | Values | Default |
|---|---|---|
| `scope` | `all` · `picked` · `filtered` (`kind:image\|video\|audio\|document\|archive\|text`, `glob:<pattern>`) | `all` |
| `detail` | `full` · `previews` · `listing` | `full` |
| `since` | empty, or an RFC 3339 time | empty |
| `mode` | `read` · `contribute` (scope `all` only) | `read` |
| `apps` | `allowed` · `denied` | `allowed` |

**The floor** has no configuration path: `.fog/`, symlinks and special files, replicas
of other peers' shares, and anything marked *keep on this box*. A symlink at any
component of a path — not only its last — ends it: nothing is listed, served,
previewed, relayed or written through one. A file is served only under the name the
disk spells it with, every component exact, so on a case-insensitive disk another
spelling can't open it past the checks made by name (a contribution's author, a
picked folder, a filter); *keep on this box* and the replica folders are compared
without regard to case. Any lens change bumps
`revision`. Widening `detail`, `read` to `contribute`, or `apps` to `allowed` sets the
subscription to `offer_changed` until the operator acknowledges; widening `scope` or
`since` is not gated. Narrowing scrubs on the next pull.

| Route (all signed) | Body and answer |
|---|---|
| `GET /fed/v1/files/catalog` | `{shares: [{id, title, mode, detail, apps, since, revision, impact, files, bytes, updatedAt}], contract}` |
| `GET …/shares/{id}/manifest` | `{revision, mode, detail, apps, impact, entries, etag}`, conditional |
| `GET …/shares/{id}/blob/{path…}` | The file, streamed whole, its digest in the signed head; refused on `listing` |
| `GET …/shares/{id}/thumb/{path…}?w=` | JPEG, default width 320; refused on `listing` |
| `POST /fed/v1/files/events` | `{shareIds, event}`: `changed` \| `revoked` \| `withdrawn`; accepts a grant or a use row |
| `POST …/shares/{id}/contributions` | `{path, size, sha256, mtime}` → `{id, path, size, offset}` |
| `PATCH …/contributions/{uid}` | A chunk, `Upload-Offset`, at most 8 MiB; wrong offset is `409 {offset}` |
| `POST …/contributions/{uid}/complete` | The entry as it landed |
| `DELETE …/contributions/{uid}` | Abort |
| `DELETE …/contributions?path=` | The author withdraws a file |
| `GET …/shares/{id}/relay/{path…}` | `{ticket, for}`: the origin's relay ticket for a contribution in that share |
| `GET /fed/v1/files/contributions/{origin}/{share}/blob/{path…}` | A contribution, served by its author to a third fog, against `Fog-Relay-Ticket` |

A manifest entry is `{path, kind, size, mtime, sha256, mime}`, plus `origin`,
`originShare` and `stub` for contributions. The manifest is the whole share, so there
is no cursor to corrupt and deletes converge. `sha256` may be absent while the origin
hashes; the receiver then takes the digest the blob is signed with.

**Pull is the truth; push is a nudge.** A subscriber pulls every 15 minutes and within
30 seconds of `changed`; origins nudge at most every 30 seconds. Downloads land
atomically, verified against both digests, with an optional bandwidth
cap, two at a time. Failures back off from 15 minutes to a day and never remove
anything; what a verified manifest no longer lists is pruned before anything is
fetched, so a failed download can't keep it. **Pruning takes back only what the sync
placed:** entry by entry, a file only while it is still what was placed — the file
itself, its device and inode, and its size and time as the disk had them once placed,
all four recorded then (migration 0090) and never taken from the origin's claim — and
not kept on this box, to the trash, never deleted outright; a
folder only once it is empty, so whatever the subscriber put in it stays, and the
folder with it. A folder already here with no replica row is never claimed by a
manifest that names it. A replica is ordinary files at
`Peers/<peer>/<title>`, read-only, out of snapshots until the operator opts in, and
never re-shared. A receiver may ask for less detail than offered, never more;
`previews` entries stay dehydrated until opened or pinned, and a share narrowed to
`listing` dehydrates every copy on the next pull, pinned or not. A quota overrun
pauses a `full` subscription with a notice. **A size is a promise:** the quota is
checked against the sizes the manifest declares — every size between 0 and 16 TiB
or the whole manifest is refused, summed without wrapping, counting only what the
pull would fetch (not entries that are the subscriber's own, nor stubs) — and every
download — a pull, an
open, a contribution fetched from its author — stops with an error at the first
byte past its entry's size, so what lands is never more than was counted, and the
bytes counted as fetched are the bytes that arrived. An open of a file the origin
has since grown waits for the next pull.

**An origin names files, not places.** Every manifest path must be canonical and
inside the share — no `..`, `.`, empty or leading segments, no reserved name — or the
whole manifest is refused before anything is placed, fetched or pruned. So is one
that names any entry twice — or two that differ only in case, where the disk can't
tell them apart — or gives a file a time or a digest that doesn't read. Names the disk
folds together in other ways (Unicode NFC and NFD on APFS, ZFS `formD`, casefold
ext4, CIFS) are caught by asking the disk: before an entry is placed, if its name
already leads to a file another entry holds or this pull placed — the same device
and inode — the pull is refused, and nothing is placed over it or reported as the
subscriber's. A rename an
origin answers a contribution with is held to the same rule and must stay in its
folder. A subscription ends as `revoked` only on the origin's own word: a manifest
`404` or `unknown_peer` it signed. A peer's thumbnails are cached under a key scoped
to that origin, so the digest it claims can't stand in for a local file's, and only
a JPEG of at most 4 MiB and 4096 pixels a side is taken as one. An origin's catalog
makes at most three offer notices; past that, one notice counts them.

**A subscriber's own files are its own records' to judge.** In a contribute-mode
folder, a path the subscriber recorded as its own contribution, and a file there it
holds no replica row for (put there by the operator, not yet pushed, or kept on this
box), is never overwritten, dehydrated or replaced by a stub, whatever the manifest
says of its author.

**What the lens hides stays hidden.** Under a `filtered` scope or a `since` date the
manifest lists only the folders leading to entries that cross, and a withdrawn
contribution's stub is shown only when the lens would show the file.

**`apps: denied` holds wherever the copy is.** A deploy whose volume would reach such a
copy — mounting it, the drive, `Peers` or the peer's folder — is refused. And the copy
is held to it when it lands, against the volumes in place then: subscribing is refused
while an app's volume reaches the folder, and a live copy whose lens turns to `denied`
under such a volume is removed from the disk and its subscription paused with a
`files-fed-apps-mount` notice, until the operator changes that app's volume and
resumes it. An open never fetches bytes there meanwhile.

**Stop sharing** leaves the copy until the receiving operator removes or keeps it, and
turns what that peer contributed through the share into stubs, as unpair does.
**Unsubscribe** removes the copy. **Unpair** removes every replica from that peer and
every share to it. Removing a copy — unsubscribing, unpairing, an apps-mount hold —
takes away what the sync placed, under the pruning rules: only what is still as
placed, never the subscriber's own files nor what they keep, to the trash (off the
disk outright for an apps-mount hold, so no app can reach it there), and the folders
once empty. Nothing a sync writes lands over a file of the subscriber's: the bytes
are staged, and placed by a rename that refuses to replace anything, or — replacing
the sync's own earlier copy — that moves what is there aside, checks it is that
copy, and puts it back if it isn't; a file saved under the name meanwhile is kept, the
sync lets the name go, and one `files-fed-conflict` notice per peer says which file
was kept and that the peer's version was not written. A file moved aside carries a
receipt naming where it came from: if fog stops before it goes back, the next start
puts it back, or beside its name under a free one when the name is taken, and it is
never swept. A copy being placed is marked first with the size and time it will
have, so one that lands just before a crash is still known as the sync's. A file the
sync placed becomes the subscriber's only by the subscriber's own write: a save
through a new file (as editors save) is another inode, and so the subscriber's even
at the same size and time; a write in place changes the time. (A row from before
0090 has its device and inode recorded at the next pull, while its file still has
the size and time it was placed with.) Every check of a
replica's name — the write guard, placing, listing, opening — goes by the name the
disk gives what it reaches, so on a case-insensitive disk another spelling is the
same file.

**Contribute mode.** Every file has one origin and is edited only there. A peer writes
into its replica folder and each file reaches the origin through a session, recorded in
`file_fed_origins` under the name the origin's disk gives it (on a case-insensitive
disk `PHOTOS/x.txt` lands in, and is recorded under, `Photos/x.txt`), and every lookup
of a file's author finds it by that name or by the file itself, so no spelling strips
a contribution of its author; a collision lands as `name (from <peer>).ext`. No peer
may hold more than 10,000 contributions here, nor any share more than 25,000
(`413`) — counted with the sessions still open, each a contribution on its way, and
again as each lands — and no peer may have more than 16 sessions open at once,
replaces of its own files included (`429`); a session 30 minutes without a chunk is ended and its upload discarded. A
manifest build reads each folder once however many contributions there are; a
share may also cap
contributed bytes (`413`), checked when a session begins and again when it lands. A
peer re-sending its own contribution replaces it, and only while the file there is
still that contribution — recorded as theirs, not withdrawn, holding the bytes they
sent — checked again when the session lands; once the operator has rejected,
replaced or edited it, the session ends instead. A rename the origin answers with is
followed locally only onto a free name or the same file (a case-only rename on a
case-insensitive disk), never over another file. A
session belongs to the share as it is at each chunk: once the share stops taking
contributions or the file's path is kept on this box, the session ends and its upload is
discarded. The origin may read, copy or reject a contribution, not edit it. Other
subscribers see a stub: bytes cross only over the author's own peerings, and only on the
origin's word. The subscriber asks the origin for a **relay ticket**
(`GET …/shares/{id}/relay/{path…}`), which the origin issues only when the caller's own
share carries the file with more than a listing, not on the floor, and the contribution
not withdrawn: `{origin, author, holder, share, path, exp}`, five minutes long, signed as
the transcript `fogfed1-relay\n<exp>\n<sha256hex(ticket)>`. The author serves the bytes
only against a ticket that verifies with the origin's key and names exactly that holder,
file and author (`Fog-Relay-Ticket`), and only while it still contributes to that share,
for a file it pushed, not kept on this box. A file kept on this box is never contributed. On unpair a fog sends
`withdrawn`, and its contributions turn to stubs. An author withdrawing a file takes
back only its own bytes: while the file is still the contribution as it landed, it
goes to the origin's trash; once the operator has edited it, it stays, as the
operator's, and only its provenance goes — as it does when a contribution would turn
to a stub.

## Down to the apps

Every app deploy gets `FOG_API_TOKEN`, and `FOG_API_URL` when fogd's address is known.
`/app/v1` sits outside `/api`, is bearer-authenticated by token digest, limited to 120
calls a minute per app, and answers `401` for an unknown token.

```
GET /app/v1/connect/peers
→ {"peers": [{"name": "riverside", "displayName": "The Riverside house",
              "instanceId": "01J5XW…", "online": true, "lastSeenAt": "…",
              "apps": [{"app": "feed", "url": "https://feed.example.com",
                        "contracts": ["fog-connect/1"]}]}]}
```

A peer appears only with the `use` row of `apps.directory`. `name` addresses;
`displayName` is what a person reads. The view has no keys or capabilities, and a fog
with no peers answers an empty list, so an app degrades to standalone with no special
case. A heartbeat lists an app with a route and either a resolvable host or a declared
contract, so a peer can tell "no public URL" (no `url`) from "no such app". `title` is
in the wire shape and not populated. A directory entry grants nothing.

| Endpoint | Purpose |
|---|---|
| `POST /app/v1/connect/grants` | A credential from a sibling app ([connect-contract.md](connect-contract.md)) |
| `GET /app/v1/connect/reachability` | The operator's reachability verdict |
| `GET /app/v1/files/mounts` | This app's mounts ([files-contract.md](files-contract.md)) |
| `GET /app/v1/chat/rooms`, `POST /app/v1/chat/rooms/{id}/messages` | Rooms the fog bot is in, for an app with `chat` in its `uses` ([chat-contract.md](chat-contract.md#bots)) |

**`clients/fog-client`** is the vendored Ruby wrapper: `FogClient.available?`,
`FogClient.connect.peers`, `.request`, `.reachability`. Inert without the variables, and
it never raises into the host app. **`clients/fogfed`** is the Ruby mirror of
`internal/fed`: identity, `fogfed1` signing, the pairing transcript apps use for their
own federation, `Lens`, `Impact`. `conformance/vectors.json` holds known-answer vectors
both languages test against.

## Security

- **A compromised peer** reaches its grants, within the limits listed below.
- **A stolen `fog.db`** holds public keys and digests. The identity key is
  vault-encrypted, so `master.key` stays the crown jewel.
- **The app token** reads the app's federation view and asks outward. It cannot mint
  peers, change grants or read another app's anything.
- **Keys are continuous.** A paired instance's key never changes: a bad signature is
  refused, and a pairing that names an instance already held under another key is
  refused on either side with a notice, leaving the row, its key and its grants as
  they were. The store refuses the change too (migration 0070). Fog has no key
  rotation; a household that lost its identity pairs as a new peer.
- **Only proven answers count.** Every answer is signed bound to the request it
  answers and must verify against the peer's stored key; anything else records
  nothing, is not "seen", stamps no unpairing and revokes no subscription.
- **Only proven addresses are written to.** Every connection proves the peer's key
  before a request goes over it, and no redirect is followed.
- **Exchanges are sealed end to end.** Every request — method, target, headers,
  body — and every answer — status, headers, body — is encrypted to the receiving
  fog's pinned, identity-bound key inside one envelope, and bound to its exchange,
  so neither a squatter at a stale address nor anyone on the path of a plain-`http`
  one can read, move or replay one. There is no unsealed fallback.
- **A signed request is accepted once.** The replay record refuses a signature seen
  before inside its window, across restarts, however it is spelled: encodings are
  strict and the record is keyed on the decoded bytes. A request is reserved as its
  headers arrive and its record kept past the latest moment any copy of it could
  complete, so a body held back can't outlast it.
- **An instance ID is its key's.** A derived ID is checked against the key it comes
  with wherever one is taken in, so no household can claim another's name (legacy
  IDs excepted, below).
- **Strangers cost little.** A peer verifies from its headers and has slots of its
  own; anything unverified holds one of a few slots, a few per address, under an
  absolute deadline, and may be no bigger than a pairing (1 MiB for a known
  instance). Every envelope is held to its inner route's size while it arrives,
  large bodies are spooled to disk, and an inner body is opened only as its handler
  reads it. The envelope dispatches `/fed/v1` routes only.
- **Refusals can't be relayed.** Only a request that verified gets a signed answer.
- **A peer names files, never decides over ours.** Paths stay inside the share; a
  symlink anywhere along one ends it; a name is served only as the disk spells it;
  downloads stop at their declared size and sizes can't talk the quota down; a
  subscriber's own files and an origin's operator's files are never overwritten or
  removed on a peer's say, and what a peer's withdrawal removes goes to the trash;
  a contribution's author is recorded under the name the disk gives the file and
  found by it, or by the file itself, under any spelling.
- **Configs mean exactly what they say.** Capability configs are checked on write; an
  app scope or people scope that can't be read covers nothing; flare forwarding reads
  its consent on every event; a peer's feedback statuses apply only while this side
  still sends it reports, and only with a stamp no later than now.

### What the wire still shows

Over plain `http`, or to anyone who can read the transport, a fed exchange shows:

- **That two fogs talk, and when and how much:** the addresses, the sizes of
  sealed bodies and the timing. Padding sizes and timing is not attempted.
- **The caller's instance ID, its public keys, the request's timestamp and
  signature.** The receiver needs them before it can open anything: the instance
  ID to find the identity key that verifies the request, the encryption key (and
  its binding) for a pairing that hasn't pinned it yet, and the signature to
  verify and to record against replay. The instance ID and keys are what a fog is
  known by to its peers, not anything it holds.
- **Whom it is for:** the recipient's instance ID (or pair recipient) in the target,
  which the signature has to cover. The address says as much already.
- **For the pair hop, that it is one.** Its body is sealed to the key the invite
  carries, so it reads like any envelope, but it comes from an instance the
  inviter doesn't know.

The answer shows nothing of its own outside the seal: always `200`, always the
same content type. Over `https` the TLS session covers all of the above but the
addresses.

### Known gaps

- **Pairings made before key-bound IDs keep their legacy IDs.** A peer row with a
  random ULID was taken as given when it was paired, and still is: its key is pinned,
  so nobody else can be that row, but nothing proves the household that paired under
  that ID was the one that minted it. No new pairing can name a legacy ID.
- **Chat can't check a third household it holds no row for.** A person address names
  its household's instance ID, and where that ID is key-derived it is checked against
  the household's key wherever the key is known — a peer row. A household this fog
  isn't paired with has no key here, so its ID is taken as the hub says, under the
  `acceptThirdHouseholds` rule.

## Not built

`backup.replicate`, `updates.relay`, shared notices, and a remote fleet view.

## Versioning

`fog-federation/1` is `fogfed1` signatures on `/fed/v1`. Rejection codes and payload
fields are additive; receivers ignore what they don't know, and an empty field from an
older fog is never read as a statement. Anything that changes the signing string bumps
the version. The request signing string is unchanged since `fogfed1`; answers moved from
`RESPONSE` to the bound `ANSWER` and `ANSWER-DIGEST` pseudo-methods, which no earlier
verifier accepts, and every body is now sealed (an invite carries `k`, documents carry
a bound `encryptionKey`), so fogs from before those changes and after them don't
federate: the older side is refused, never served unsealed. Every exchange now
travels in the envelope (`POST /fed/v1/sealed`), so fogs from before the envelope
don't federate with fogs after it either. The Ruby gem (`clients/fogfed` 0.4.0)
signs answers, seals and opens bodies, and makes and reads the envelope byte for
byte as fogd does.
