# fog-connect/1 — the app-side Connect contract

*Companion to docs/federation-contract.md; sibling of fog-backup/1
(docs/backup-contract.md) and fog-files/1 (docs/files-contract.md).*

Connect is the one way an app in one household gets an authenticated
credential from its sibling app in a paired household, with zero human
configuration. Fog carries discovery and trust; the *credential* is always
the app's own. An app opts into being connected to by declaring this
contract.

## How a connection happens

1. The requesting app asks its own fogd (`POST /app/v1/connect/grants`) for
   a peer household and a target app, carrying its own fogfed public key and,
   optionally, an opaque handshake `payload`. Only an app that declares
   `fog-connect/1` itself may ask (`403` otherwise), as only one that does is
   asked.
2. That fogd mints a **vouch** — `{from, to, app, fromApp, fromKey, nonce,
   iat, exp}` signed with its instance key, five minutes long — and relays it
   over the signed federation channel to the peer's fogd
   (`POST /fed/v1/connect {vouch, payload}`).
3. The peer's fogd runs every check in one place: the vouch is the signing
   peer's own, minted for this instance, verified against the key pinned at
   pairing, unexpired and nonced; the operator granted the peer
   `apps.introduce` and its allow-list covers the app; the app exists and
   declares `fog-connect/1`. A replayed nonce is answered from the cache.
4. Only then is the app asked, through `POST /fog/connect/grants`, to mint
   the credential. The credential and any answering payload flow back along
   the same hops, signed by the granting fogd. Fog never stores either.

There is no other door. An app never receives a vouch from the internet and
never verifies one itself; its only caller is its own fogd, bearer-
authenticated by `FOG_API_TOKEN`.

## Declaring

In the app repo's `fog.yaml`:

```yaml
connect:
  contract: fog-connect/1
  hostChange: >-            # optional: what a change of host costs
    Peers must re-pair; follows break.
```

Fog records the declaration at build time, advertises it in the peer
directory (`contracts` on each directory entry), and refuses to connect
anything to apps that never declared it.

## The endpoints

Two an app serves to mint and revoke, plus two optional ones. All inside
the app's existing `scope "fog"` routes — bearer-authenticated by
`FOG_API_TOKEN`, outside session auth, exactly like `fog-backup/1`:

### `POST /fog/connect/grants`

Called by the app's *own* fogd once a peer's app has been authorized.

```json
{
  "peer":    {"instance": "01J5XW…", "name": "dave", "publicKey": "…"},
  "appUrl":  "https://feed.dave.house",
  "vouch":   "…",
  "payload": {"instance_json": "…"}
}
```

`peer.instance` is the peer household's fog instance ID, verified over the
signed channel; `peer.name` is what this household calls it; `peer.publicKey`
is the requesting app's fogfed key, bound into the vouch by the requesting
fogd, so the app may pin it without any key fetch. `appUrl` is the
requesting app's public address as its fogd knows it. `vouch` is the signed
attestation itself, for the app to keep as evidence; `payload` is whatever
the requesting app sent, opaque to fog and absent when it sent nothing.

Respond `200` with whatever "access for that remote counterpart" means for
this app, and optionally a payload of the app's own:

```json
{"credential": {"apiKey": "…"}, "payload": {"instance_json": "…"}}
```

`credential` is opaque to fog — string, object, whatever the app's manual
credential form would have produced — and required: an answer without one
is a refusal, and the requesting app never receives it. `payload` is relayed
back verbatim; an app with its own identity handshake (Hub exchanges
instance documents) carries it here instead of making a second trip.

**The wrapper rule:** this endpoint must be a
thin adapter over credential machinery the app already exposes in its own
settings UI. The minted grant appears in the app's UI next to hand-made
ones, is revocable there, and creating the same grant manually — no fog
involved — yields an equivalent working connection. If implementing this
endpoint requires building credential machinery the app doesn't otherwise
have, build the in-app feature first, then let fog drive it. A connection
is a pre-filled form, not a private side door.

Fog attested `appUrl`, so an app that guards against private addresses may
treat that host as reachable on the LAN from then on.

### `DELETE /fog/connect/grants/{instance}`

Revoke every credential minted for the peer identified by `{instance}` (the
peer's instance ID). `200` on success and for unknown instances alike.

Fog owes this call when the operator unpairs that peer, when the peer's
`apps.introduce` grant stops covering this app, and — for every peer this app
ever minted for, paired now or not, and every paired peer — when this app is
deleted. An owed call is **durable**: fog records which peers each app has
minted for, keeps each owed revocation until the app holding the name answers
it `2xx`, and retries it every minute, so an app that was down when a peer was
unpaired still gets the call when it comes back, and an app later installed
under a deleted app's name gets it before that peer can connect again: a new
credential for a peer is minted only once anything owed to it at that app is
paid. A deleted app's data is kept only when nothing is owed at it, for any
peer, current or former — or when the operator explicitly keeps it anyway, in
which case the debt stays and is paid by whatever app later holds the name.
While an app is being deleted, fog mints nothing for it: mints under way finish
first and are among what is revoked.

### `POST /fog/connect/peers/changed` (optional)

Called when the peer directory this app can see (`GET
/app/v1/connect/peers`) changes: a peer paired or unpaired here, was
renamed or relabelled, had its `apps.directory` exposure toggled, or
advertised a different set of apps in its heartbeat.

```json
{"reason": "peer.paired", "changedAt": "2026-08-22T18:04:11Z"}
```

Answer `200` (or `204`) and re-read the directory. Fog sends **no peer data
in the notice** on purpose: the GET stays the single authority, so two
notices arriving out of order cannot leave an app holding a stale list, and
an app that missed one — it was redeploying, it 404s this path, it had not
been written yet — is corrected by its own next poll. `reason` is
diagnostic; the correct response to any value is the same re-read.

Fog pushes only when the *app-visible* view actually moved: online flags and
`lastSeenAt` are excluded from that comparison, so a heartbeat every two
minutes, each way, does not become a call every two minutes. The first change after
a fogd restart always pushes — fogd cannot know what an app saw before it
went down, so it re-nudges once rather than staying quiet about something
missed.

This endpoint is optional in the strict sense: an app that never implements
it loses latency, not function, and should keep polling the directory on a
schedule regardless. The push makes the list current in seconds; the poll is
what makes it *eventually* correct.

### `GET /fog/connect/ledger` (optional)

Called by the app's own fogd when the operator opens the Connect page, whose
Activity tab is the ledger of what crossed a boundary, so the app's own
crossings — deliveries, follows, grants, whatever a boundary means to it — read
beside fog's in one place. `?limit=N` caps the rows; `?peer=<instance>` narrows
them to one paired household by its instance ID.

Answer `200` with rows in fog's own audit shape plus an axis and a reading:

```json
{"entries": [{
  "at": "2026-09-01T18:04:11Z",
  "actor": "app:feed",
  "action": "federation.delivery.delivered",
  "subject": "01J5XW…",
  "detail": "{\"direction\":\"out\"}",
  "axis": "sharing",
  "summary": "delivered Create to feed.dave.house"
}]}
```

`axis` is `trust` | `discovery` | `sharing` (the same three fog uses);
`summary` is the plain reading fog shows. Fog reads every contracted app in
parallel with a three-second bound and treats a missing endpoint, a slow app,
or a malformed answer as "no rows from that app" — never as an error.

## The requesting side

The counterpart app never implements anything: it calls its own fogd —
via the vendored `fog-client` gem —

```ruby
FogClient.connect.peers                                  # discover
FogClient.connect.request(peer: "dave", app: "feed",
                          public_key: my_key, payload: { ... })
# => Grant(app: "feed", url: "https://feed.dave.house", credential: {...},
#          payload: {...}, peer_instance: "01J5XW…", peer_name: "dave")
```

— and feeds the result into the same remote-connection code path a human
would have configured by hand. `app` defaults to the requesting app's own
name on the peer, and may only name that app: Connect joins an app to its
sibling, and the requesting fogd refuses a Hub that asks for a peer's Feed
before anything crosses to the peer. Every connectable app declares this
contract, so a requester never finds its sibling in the directory by
contract alone — it looks for its own name. `nil` means refused, unreachable, or outside fog; a peer
whose fog predates Connect is reported as needing to update.

## Guarantees fog makes

- The requesting fogd relays only over the mutually-keyed, signed federation
  channel; the vouch names the verified peer instance, never a claimed one,
  and binds the requesting app's key.
- The granting fogd checks the operator's `apps.introduce` grant and
  allow-list, and the app's contract, before the app is ever contacted, and
  audits both outcomes on both instances (`connect.request`,
  `connect.serve`), each refusal with its reason. The same outcome is set on
  the request's Glass trace (`connect.*` attributes, a refusal marking it
  errored) and logged on it, with the names — never the values — of the
  credential's fields.
- A replayed connect request (same nonce, inside the vouch's five minutes)
  receives the cached response — an app is never asked to mint twice for
  one request.
- Fog never persists a credential or a payload; they exist in fog only in
  transit.
- A directory notice is best-effort and unordered. Fog does not retry it,
  and never treats an app's failure to answer as a federation error.
- An app may ask its own fogd `GET /app/v1/connect/reachability`
  (`FogClient.connect.reachability`) whether this household is dialable
  from the internet — the same verdict the Connect page shows the operator —
  so it can say so honestly in its own UI and pause public delivery rather
  than burn attempts.
