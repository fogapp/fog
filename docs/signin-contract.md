# fog-signin/1 — household sign-in

*Sibling of fog-federation/1 ([federation-contract.md](federation-contract.md)),
fog-connect/1 ([connect-contract.md](connect-contract.md)) and fog-backup/1
([backup-contract.md](backup-contract.md)).*

fog is the household's OpenID provider. An app implements ordinary OIDC, so
standalone it signs in against any provider. On fog, the client, its secret,
who may use it and in what role all come from what the app declares, and fog
keeps the app honest afterwards with back-channel logout and SCIM. fog proves
who a person is; the app keeps its own users, sessions and roles, and fog's
tokens are never its API credential.

Three paths: **fog-signin/1 apps** (reference implementation
`FogClient::Signin`; adopters include Stream, Manor, Lucid, Office, Fit);
**third-party OIDC images**, declared in the spec (Paperless); and **apps that
speak nothing**, behind [the gate](#the-gate).

## Declaring

In the repo's `fog.yaml`, or the spec for an image app. The spec's block wins.

```yaml
signin:
  contract: fog-signin/1
  redirect: /auth/oidc/callback     # a path on the app's host
  redirects: [app.example:///cb]    # optional: more paths, or a native scheme
  logout: /auth/oidc/backchannel    # optional: back-channel logout
  scim: /scim/v2                    # optional: accept provisioning
  roles: [admin, member, viewer]    # the app's vocabulary, most privileged first
  default: {operator: admin, member: member}   # fog role -> app role
  acr: phr                          # optional: require a passkey
  email: true                       # optional: register for the email scope
  par: false                        # optional: allow a front-channel request
  device: true                      # optional: device sign-in
  visitors: {role: viewer}          # optional: the most a visitor is given
```

An invalid declaration fails the deploy, naming the field. `redirects` may be
a path or a custom scheme with a dot in it, never another web origin.
`default` maps only `operator` and `member`, into `roles` when set. `visitors`
needs `roles`, and its role can't be the first of several. `par: false` is for
libraries that cannot push a request; PKCE stays mandatory. An image app's env
values may name `{{oidc.issuer}}`, `{{oidc.client_id}}` and
`{{oidc.client_secret}}`, filled in at deploy, or dropped when there is no client.

## What fog provisions

Each deploy upserts one `oidc_clients` row. `client_id` is the app's name, and
URIs resolve against `https://<first route's host>`, so a changed host
re-registers. The only post-logout URI is that origin plus `/`. An app that
stops declaring is unregistered and its secrets deleted. Without a domain
nothing registers, and the Sign-in tab says why.

| Variable | Value |
|---|---|
| `OIDC_ISSUER` | `https://fog.<domain>` |
| `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | the app's name; the secret |
| `FOG_OIDC_INTERNAL_URL` | fogd's address on the host, for back-channel calls; optional |
| `SCIM_TOKEN` | the bearer fog presents to the app; only with `signin.scim` |

**Secrets** are minted once into the vault (`_oidc_client_secret`,
`_scim_token`); the row keeps digests only: the secret the running app holds,
a rotated one waiting for the next deploy, and the one a deploy just replaced.
*Rotate secret* mints a waiting secret and leaves the running one alone, so
rotating twice before a deploy never locks the app out. The next deploy carries
the new secret; the app's first use of it makes it current, and the old one
works for 10 more minutes, for the old containers of a blue-green swap, then
never. *Revoke now* is for a leak: a new secret is the only one that works from
that moment, the requests, codes, access tokens and device sign-ins minted for
the client are deleted, and fog redeploys the app, which cannot sign anyone in
until that deploy is done.

**Signing keys** are P-256 in `signing_keys`, private halves in the vault
(`_signing_key_<kid>`), published 7 days before signing, signing 90, published
a day after. `typ` says what a token is: `JWT` (ID tokens, gate assertions),
`logout+jwt`, `fog-signin-proof+jwt`, `fog-visit+jwt`. *Rotate now* under
Settings › People (`POST /api/signin/keys/retire` with `{"confirm": "retire
every key"}`, audited as `signin.keys.retire`) is the emergency rotation: every
key is unpublished and deleted at once, a new one signs immediately, and nothing
an old key signed verifies at fog. A leaked key could have signed anyone in to
any app, so every app taking back-channel logout is owed: a logout by `sub` for
each of the household's people, seen by it or not; one by `sid` for each visitor
session it hosts; and one *sessions-revoked* logout ending every session it
started from a sign-in before the rotation (below). fog-client refetches the key
set every 5 minutes, so apps on it stop accepting the old keys within that.

## The protocol profile

A request outside the profile fails with the standard error, never a fallback.

Public: `/.well-known/openid-configuration`, `/oauth2/jwks`. Client-authenticated:
`POST /oauth2/par`, `/token`, `/revoke`, `/device_authorization`. By fog session:
`/oauth2/authorize`, `/oauth2/end_session`. By access token: `/oauth2/userinfo`.

| Area | fog |
|---|---|
| Flow | `response_type=code`, `response_mode=query` only |
| PKCE | Required for every client, `S256` only |
| PAR | For every client; required per client unless `par: false` (so discovery says `false`). `request_uri` lives 5 minutes |
| `iss` (RFC 9207) | On every authorization response, errors included |
| Redirect URIs | Exact match. An unknown client or URI is an error on fog's page, never a redirect |
| Client auth | `client_secret_basic` or `_post`, never both; digest compared in constant time to the running, waiting and (for its 10 minutes) replaced secret; 120 a minute per client and caller address |
| Code | 60 seconds, single use by the database. A replay deletes the access tokens it minted, a person's or a visitor's, and is audited. Wrong client, redirect or verifier are one `invalid_grant`. Redeeming re-asks the policy: refused now, or another role, is `invalid_grant` |
| ID token | ES256, 5 minutes, `aud` and `azp` the client, `at_hash`, `nonce` when sent, `fog_session_exp` |
| Access token | Opaque, digest at rest, 10 minutes, good only at userinfo, which re-asks the policy as the token endpoint does |
| Limits | Authorize: 120 a minute per signed-in person, else per address. Userinfo: 120 a minute per address |
| Scopes | `openid` required; unknown or unregistered is `invalid_scope` |
| `prompt` | `none`, `login`; anything else is refused |
| `max_age`, `acr_values`, `login_hint` | Supported; unknown `acr_values` are ignored |
| Revocation | Access tokens; `200` whether or not the token existed |
| Refused | Repeated parameters, implicit, hybrid, password and client-credentials grants, public clients, dynamic registration, request objects, front-channel logout. No refresh tokens |

Nobody signed in goes to `/login` (`prompt=none`: `login_required`); a policy
refusal to fog's refusal page (`access_denied`); a need for a passkey or fresher
proof to `/confirm` (`interaction_required`). An interactive request is held
server-side with a server-set `Began`: `prompt=login` is met only by a proof made
after it, `max_age` by that or one no older. The code carries the decision, and
the token endpoint signs it once the person is checked to be still signed in.

## Claims and assurance

**The person address** is one name for a person anywhere:
`person:<instance ULID>/<person ULID>`, the home fog's federation identity and
`people.uid`. `sub` is the address, the same for every client, because
correlation across the household's own apps is the point. It survives a
rename; `preferred_username` follows one.

| Scope | Claims |
|---|---|
| `openid` | `sub`, `sid`, `auth_time`, `acr`, `amr`; `fog_home` (the home instance) and `fog_session_exp` in the ID token; `fog_visitor: true` and `fog_home` for a visitor, in userinfo too |
| `profile` | `preferred_username`, `name` |
| `email` | `email`, `email_verified`, only when the person has an address |
| `roles` | `roles`: the role in this app, a one-element array |

Clients are registered for `profile` and `roles`, plus `email` with
`signin.email`. Userinfo serves the grant, so `roles` is what sign-in decided.
`fog_session_exp` (unix seconds) is when the fog session behind the sign-in
ends: a person's, or a visitor's (which never outlasts their home session).
`claims_supported` lists exactly the claims above and the registered ones fog
sets, and nothing else.
`email_verified` is always `false`: fog cannot send mail. An email is never for
linking an account and never goes to a peer.

Sessions record `amr`: `["pwd"]` for a password, `["pop", "user"]` for a passkey
with the user verified, `["pop"]` for an unverified passkey or a chat device's
key, `[]` for joining by invite without a password. `acr` is `urn:fog:acr:phr` for
`pop` with `user`, otherwise `urn:fog:acr:pwd`.

`acr: phr`, or asking for it in `acr_values`, sends a lesser session to
`/confirm?with=passkey`. `/confirm` calls `/api/people/reauth/password` or
`/api/people/reauth/passkey/{begin,finish}`; the proof replaces `amr` and
`auth_time` on the same session, so the `sid` apps hold stays good.

## Policy

Each app has one policy (`signin_policies`, `signin_policy_people`), gated or
not. No row means operator only, so nothing opens by surprise. Audiences:
`operator` (*Only me*), `household` (operator and members, *Household*),
`people` (the operator and chosen members, *Choose people*).

`identity.Decide` is the whole of it, pure and total, in a fixed order, so a
refusal for who someone is can never be overridden. **Who**: not `active`, a
`guest` or a bot is refused. **Audience**: anything unrecognised fails closed to
operator only. **Role**: a per-person override, else `default`, else the fog role;
outside `roles` is refused, and an override never changes who is admitted.
**Assurance**: short of `acr` or `max_age`, step up.

A refusal reads "Manor isn't shared with you yet", with **Ask for access**:
`POST /oauth2/ask` files "Name asked to use Manor" in the Inbox. The operator's
API is `/api/apps/{name}/signin`, with `/rotate` (`{"now": true}` to revoke at
once), `/signout` and `/visitors`; and `/api/signin/keys`, with `/retire`.

## Sessions and logout

fog's session cookie follows the setup's scheme, the same rule the gate signs
by. Under the household's domain on an https setup (Caddy serves TLS: a DNS
token is set) it is `__Host-fog_session` (Secure, HttpOnly, `SameSite=Lax`,
host-only), always; no request can talk it down to the plain cookie, whatever
it says of its transport. Off the domain it is `__Host-` over https, too. On
an http setup, and on a plain-http bare address, no browser stores a Secure
cookie, so it is a host-only `fog_session` without Secure.
- **Where fog sets `__Host-`,** it reads nothing else. A `fog_session` there —
  the domain-wide cookie from before `__Host-`, or one a sibling host planted —
  is never adopted, and is expired.
- **On an http setup or a bare address,** whatever shares the host can plant a
  `fog_session` beside fog's: a sibling host, domain-wide; an app on another
  port of a bare address (cookies ignore ports), host-only at a longer `Path`,
  which the browser sends first. Every same-named cookie is a candidate, and
  only a session that passes the address pin below counts. Beside every
  session cookie fog sets `fog_browser` (`__Host-fog_browser` where the session
  is `__Host-`), a fresh random value per sign-in whose digest is recorded on
  the session. When more than one session would count, only the one bound to a
  `fog_browser` the browser carries does; if that leaves more than one, none
  does (`401`). Whenever a request carries `fog_session` or `fog_browser` more
  than once, every copy that could be a plant is expired: host-only at each
  path the request's path lies under (to 12 segments) except `/`, where fog
  sets its own, and domain-wide at all of them.

It lasts 30 days from sign-in and does not slide. Its public `sid` rides in tokens.
Signing in always mints a new token, never one the browser brought, and ends the
session the browser held before. A password the person changes ends their other
sessions; one the operator sets ends all of them. Only the sign-in endpoints
themselves — password, passkey, joining, setup — mint a browser session, at
most 30 per person per 15 minutes (beyond that `429`); nothing else spends that
budget. A person holds at most 50 browser sessions: minting another ends the
least recently used, with the logout its apps are owed — but never one seen in
the last 15 minutes (a session in use is never ended to make room), and never
a device's token. Signing
out ends only a session the request could use — the same checks as any other
request — and a token that is no one there gets `401` and ends nothing.

No app may use a person's session. A plain `fog_session` is sent by browsers to
every port of a bare address, a direct-port app's included. So how a session
may be used is recorded when fog hands it out (`people_sessions.issued_as`),
never read from the name or header a request presents it under:
- **`plain`**: a plain cookie on an http setup or a bare address. It works only from the address it was signed in from
  (an IPv6 /64, IPv4 mapping aside; a link-local IPv6 address, whose /64 is
  the whole LAN's, is its full address and interface), however it is
  presented. A
  replay from an app's container, from anyone it sent the token to, or the
  same value renamed `__Host-fog_session`, is refused.
- **`host`**: `__Host-fog_session` on an https setup. Only fog's own host holds
  it, so presented as `__Host-fog_session` it is not pinned. Presented any other
  way — a plain cookie on a bare address, a bearer — it has left that cookie,
  and is pinned like `plain`.
- **`device`**: a native chat client's bearer token, proven by its device key.
  It is never accepted as a cookie, and not pinned, so the client keeps
  working as it moves. Because it is unpinned, only a browser session proven
  within 10 minutes may mint one (registering a native device, or a
  device-key sign-in asking for a native token).
- **`chat`** and **`chat-host`**: a browser's chat device's credential: a
  cookie of its own, `fog_chat` (`chat`), or `__Host-fog_chat` where the
  session is `__Host-` (`chat-host`), read only on chat's routes, where it
  comes first. Which one fog set is recorded: `chat` is pinned like `plain`
  however it is presented, and only a `chat-host` credential presented as
  `__Host-fog_chat` is not. It counts only when it is bound to a
  `fog_browser` cookie the request carries (when it carries any, as a
  browser always does) and is the same person as the browser's own session
  when that is present; a `fog_chat` that isn't this browser's is expired
  wherever it could have been set. Opening chat never touches the browser's
  session, which stays the person's full session.

Only the sign-in endpoints mint a browser session. A device's credential —
`chat` or `device` — is minted only from the browser's own session: a fog
session cookie not tied to any device, the same person, at most 10 per
minting session per 15 minutes; a browser with chat open mints from its own
session, never its chat credential. A bearer, a chat credential or a device's
token mints and binds nothing. On chat's routes, a chat credential or a
device's token alone lists and ends only devices' sessions, never a
browser's; where the person's alerts go — moving their ntfy topic, turning
push off — changes only with the browser's own session proven within 10
minutes (else `403`, or `409 reauth_required` for a stale one); and a chat
credential or a device's token adds or removes only its own device's push
target, never another device's. Every other chat route stays inside chat. A device holds one credential at a time;
revoking it ends it; signing out ends this browser's chat credential too, and
signing someone else in on the browser ends one that isn't theirs. Any
session tied to a chat device — `chat`, `device`, or a browser chat session
from before `chat` — reaches chat alone, by its record, however it is
presented: outside `/api/chat/` it is no one, so no operator route ever
accepts it. Before a device is written, the request's budgets are checked;
a person holds at most 20 live chat devices, and one registration carries at
most 100 key packages.

No session of any kind is accepted from an app container's network (this
host's Docker bridges), and no one is signed in from there: password, passkey
and invite sign-ins (passkey begin and finish alike) answer `403`. Sessions from before `issued_as` count as
`plain`; sessions from before `fog_browser` never win a tie.
`X-Forwarded-*` (scheme, host, client address) is believed only from Caddy, by
the proxy secret it sends on every request, the gate's sub-request included;
from anyone else it is removed before anything reads it.

A browser's state-changing request to `/api`,
`/oauth2` or `/visit` is refused when `Sec-Fetch-Site` says it came from
anywhere but fog's own page (cross-site, or same-site: a sibling app's page,
another port), or when it carries `Sec-Fetch-Site` and no `Origin`; `protect`
already refuses an `Origin` naming another host. Setup and its restores and
claim, sign-in and joining are refused from a browser whose `Host` isn't this
box's (its addresses, its hostname and `.local`, `localhost`, the household's
domain and the address it advertises), so a page whose DNS name was rebound to
fog's address can't run setup; the USB installer does the same.

Endings reach apps through `app_outbox`, whose rows database triggers write in the
statement that ends the session, so no path can forget to say so.
`oidc_session_clients` and `oidc_links` say who saw a session and a person.

| Event | Token carries | Delivered to |
|---|---|---|
| Sign-out of fog; a session revoked or expired | `sid` | Apps that session signed in to |
| Person suspended or removed | `sub`, and `sid` per session | Every app that has seen them |
| Person demoted (member to guest), or given a lesser role in an app — by the operator, the app's policy, or a deploy that changes `roles` or `default` | `sub` | Each app where they may now do less (`identity.Reduced`) |
| Removed from an app's audience; *Sign everyone out* | `sub`, per person | That app |
| Emergency key rotation | `sub` per household person, `sid` per hosted visitor, and *sessions-revoked* | Every app |
| A visitor's session ends or expires | `sid` | Apps it signed in to |
| A host changes or withdraws a visitor grant, or a deploy stops hosting visitors or caps them below it | `sid` | That app |

Only apps declaring `signin.logout` are told: a form `POST` of `logout_token`,
`typ: logout+jwt`, with `iss`, `aud`, `iat`, `exp` (2 minutes), `jti`, `events`,
`sid` and/or `sub`, never a `nonce`. `200` or `204` is success. A
*sessions-revoked* logout also carries the event `urn:fog:event:sessions-revoked`
with `{"before": <unix seconds>}` and `sid` `urn:fog:sid:all`, which no session
holds, so a relying party that doesn't know the event ends nothing and still
answers `200`; fog-client ends every session from before then
(`end_all_sessions!`), and answers `501` when the adopter can't, so fog keeps
trying and the Inbox says so. It is signed
fresh per attempt with a stable `jti`, on a 15-second ticker and at once after a
sign-out. Failures back off from 15 seconds to an hour. After a day the Inbox
says "Stream hasn't heard that someone signed out"; after 30 days it is dropped,
since no app session outlives the `fog_session_exp` it was given.

**Signing out from an app.** `end_session_endpoint` with an `id_token_hint`
that is an ID token fog issued (`typ: JWT`, `aud` a registered client, expired
or not) naming the current `sid` signs out at once. Anything else — another
token fog signed included — gets a *Sign out of fog?* page posting back with an
Origin check.
`post_logout_redirect_uri` must be registered for the hinted client. This ends
the fog session, and so every app's; an app may also end only its own.

An app binds each session to `sid`, verifies the logout token, ends sessions by
`sid` else `sub`, and ends each session by the `fog_session_exp` its ID token
stated (at most 30 days, 12 hours for a visitor). fog also sends the logout when
a session expires, for an app that keeps sessions longer.

## Provisioning: SCIM

For apps declaring `signin.scim`, fog is a SCIM 2.0 client presenting
`SCIM_TOKEN`. It computes who the policy admits, compares with what it last
said (`scim_state`) and sends the difference, every five minutes and when a
person or policy is edited, so it converges from any state.

- `GET /Users?filter=externalId eq "…"` when fog holds no id for the person.
- `POST /Users` (`201`) when they are newly admitted and unknown to the app.
- `PUT /Users/{id}` (`200`) on any change; no longer admitted is `active: false`.
  A `404` looks them up again, once.

It sends `externalId` (the address), `userName`, `displayName`, `active`, `roles`,
and `emails` with the scope, and never `DELETE`. Without SCIM, accounts appear at
first sign-in. `FogClient::Signin::SCIM` also takes `PATCH` of `active`.

Deleting a person writes, in the same statement, a `scim_departures` row for
every SCIM app that was told they were active or saw them sign in. Each reconcile
sends those `active: false` first and deletes the row only once the app has
heard, so a removal survives an unreachable app and a restart. A client whose
reconcile has failed for an hour is in the Inbox ("Manor hasn't heard who may use
it", `scim_health`), and the notice goes when it next succeeds.

## The gate

For a route marked `auth: true`, in front of an app that cannot sign people in.
fog's cookie never leaves `fog.<domain>`, so a gated host gets its own.

1. Caddy's `forward_auth` asks `/api/authgate` about every request. With no gate
   cookie, `GET` redirects to `/gate/authorize?…&bind=<digest>` on fog and sets
   (or keeps, for 10 more minutes) a random value in `__Host-fog_gate_bind`:
   host-only, `Path=/`, and by its prefix set by this host and never by a sibling.
   The app on the host sees it; it is a binding, not a credential. On plain http
   it is `fog_gate_bind`, which a sibling could plant. Other methods get `401`.
2. fog runs the policy and redirects to `/.fog/gate/callback?code=…` on the host,
   which Caddy routes to fogd. The code is 60 seconds, single use, host-bound, and
   bound to the digest; an authorize without one goes back to the host's root.
3. fogd spends the code, and only with the binding cookie whose value hashes to
   the code's digest sets `__Host-fog_gate`, host-only (`fog_gate` on plain
   http). A callback link opened in another browser signs nobody in. `next` must
   be the same scheme and host. Gate sessions cascade from the fog session.
4. Each request re-runs `Decide`, with no cache, and answers `204` with
   `Remote-User` (handle), `Remote-Name`, `Remote-Groups` (the override, else
   the fog role) and `Fog-Assertion`. A manifest's `roles`, `default` and `acr`
   play no part.

`Fog-Assertion` is a 60-second ES256 JWT with `iss` (`<scheme>://fog.<domain>`),
`aud` (`<scheme>://<host>`),
`sub`, `preferred_username`, `name`, `roles`, `fog_home`, `sid`, `amr`, `acr`,
`iat`, `exp`, verifiable against `/oauth2/jwks`. On **every** route Caddy
deletes the client-sent gate headers — `Remote-User`, `Remote-Name`,
`Remote-Groups`, `Remote-Email` and `Fog-Assertion` — in every spelling a
server folds into the same name (`-`, `_`, `.`, any case), and strips fog's
cookies. Other `Remote-*` and `Fog-*` names pass Caddy; fog-client's gate
deletes those too. Every app sits on a network of its own
([isolation](sealed-network.md#isolation-every-app-on-a-network-of-its-own)):
only Caddy and fog-wire reach a gated app's container at all. An app wired
to it reaches it through fog-wire, which deletes the same headers in every
spelling, so nothing but the gate ever sets them. An app that wants a second
check verifies `Fog-Assertion` (its `aud` is the host).

One scheme names everything the gate signs: `https` when Caddy serves the
household's hosts over TLS (a DNS token is set), `http` when it serves plain
http. It is a fact about the setup, not about one request's transport. The
assertion's `iss` and `aud`, what `/app/v1/gate` advertises, and what fog
(`/app/v1/files/search`) and fog-client accept are the same strings. Each side
checks the issuer exactly and the audience against the app's own list, scheme
included, so an assertion naming the other scheme is refused. On plain http the
assertion is still signed, and only the transport is unprotected, so it
verifies the same way: no supported setup needs the opt-out.

`GET /app/v1/gate` with `FOG_API_TOKEN` tells an app what to verify against:
`{issuer, audiences, jwksPath}`, where `audiences` are `<scheme>://<host>` for its
own gated hosts and no other app's. fog-client does this by default
(`FogClient::Signin::Gate::Middleware`, and `Gate::Rails` with
`fog_gate_identity` and `require_fog_gate!`). It checks the signature against
fog's keys read through `FOG_API_URL`, `iss`, `aud` among the app's own,
`exp`/`iat` within 5 seconds of skew, and `nbf`. It reads the keys at
`jwksPath`, not through OIDC discovery, whose issuer is fog's https sign-in
issuer whatever the gate's scheme. It then deletes every `Remote*` and `Fog*`
header the request brought, in any spelling, and writes back only what the
assertion says. With no valid assertion, the request has no identity and no
`Remote-*`.
The opt-out, `trust_remote_headers: true` or `FOG_GATE_TRUST_REMOTE_HEADERS=1`,
is for an app that can't reach fog's API, and logs a warning. An app can act for the person at its door: `GET /app/v1/files/search`
takes `FOG_API_TOKEN` *and* a fresh assertion for one of its gated hosts, and
answers only an app the operator granted `drive` in its `uses`, only for the
operator, and only names outside other apps' folders and peers' copies
([files-contract.md](files-contract.md)).

## Other households

A person has one home, the only fog that holds their credentials, authenticates
them and can suspend them. Data federates between apps; a person signing in to
another household's app is the exception, a **visitor**, and pairing alone
grants nothing.

**Directory.** `people.directory` ([federation](federation-contract.md)) is
granted by default at pairing, with `{"people": "members" | ["handle", …]}`. A
peer learns address, handle, display name and fog role, with identity keys and
devices only under a `chat` grant. Guests, bots and suspended people are never
advertised, and the type has no field for email.

**Both operators consent.** The home grants `people.visit` (same config,
requires `people.directory`); the person must be an active operator or member
named by it. A config that is missing or unreadable names nobody. The host grants per app and peer (`visitor_grants`), on an app
declaring `signin.visitors`, capped at `visitors.role`, never the app's first
role (`identity.DecideVisitor`).

**The flow** is OIDC between the fogs, pinned to the pairing. The login page
offers *From another household?* for households with a grant (`GET
/api/visit/peers?app=`). `/visit/start` pushes to the home's `/fed/v1/signin/par`.
At the home's `/visit/authorize` the GET only asks "Continue as …?"; a same-origin
POST issues the code. The host's `/visit/callback` redeems it at
`/fed/v1/signin/token` and verifies the `fog-visit+jwt` against
`/fed/v1/signin/keys`. Every `/fed/v1` hop is signed both ways; issuer and
audience are `fog:<instance ULID>`; `__Host-fog_visit` binds state to the browser.
The subject must sit under the peer's instance, so there is no second hop.

The home sends the browser back only to `https://<the peer's paired
address>/visit/callback`: the host of the URL it dials the peer at, set at
pairing and moved only by the peer's signed heartbeats. So a host whose
federation address isn't its `fog.<domain>` refuses to start a visit, saying so.
The code is single use; one redeemed again is refused, audited
(`visit.code.replayed`), and its session is owed a visit logout, ending at the
host whatever the first redemption opened.

**Visitors are not people.** They hold `__Host-fog_visitor` and a
`visitor_sessions` row that lasts 12 hours and never past the home session's end
(the visit token's `fog_session_exp`), resolve only at `/oauth2/*`, carry no
email, and are never provisioned. A person signed in here wins over a visitor cookie. The host's `acr`,
`prompt=login` and `max_age` become the home's; arriving back short is a refusal.

**Ending is total.** A home session ending or expiring writes `visit_logout` rows
and reaches the host over `/fed/v1/signin/logout`. Withdrawing `people.visit` or
`people.directory`, or demoting a person, recalls visits; unpairing ends both
sides'. On the host, lowering or withdrawing a grant, or a deploy that stops
hosting visitors or caps them below a grant, withdraws the grant and ends those
visits at the app. A visit running out reaches apps as a logout by `sid`, at the
latest with the hourly prune.

## One person

A person is one `people` row, and one fog session is what chat's web client runs
on and what authorize reads. Apps that declare the contract read `GET
/app/v1/people` and `/app/v1/people/{address}` with `FOG_API_TOKEN`: `{address,
handle, displayName, home: {instance, name, ours}, status, email?, chat?, pages}`.

Guests and bots are left out; suspended people stay, marked. `email` needs the
scope. A peer's people appear only for apps that also declare `fog-connect/1`,
under that peer's instance. `PUT /app/v1/person-pages {address, url, label}` says
where a person lives in the app (https on its own hosts; empty `url` removes), and
chat shows "Open in …". `chat` is `https://fog.<domain>/chat/with/<address>`, the
rooms the viewer shares with them, so the app never learns who talks to whom.

ID tokens carry no chat key material, an app-facing token is never accepted on a
chat endpoint, and signing in never registers a chat device. Chat still emits
`fog:<instance>/<handle>` addresses; fog resolves a handle or a ULID there.

## Device sign-in

RFC 8628 on clients with `signin.device`; the client is still the authenticated
app server. `POST /oauth2/device_authorization` (`scope`, optional `device_name`)
returns `device_code`, `user_code` (`XXXX-XXXX`, consonants), `verification_uri`,
`verification_uri_complete`, `expires_in` (600) and `interval` (5). At
`fog.<domain>/device` the signed-in person sees "Stream on Living Room TV wants to
sign in as you" (`POST /api/device/lookup`, `POST /api/device/decide`: from fog's
own page only, by its `Origin`, so a sibling host's page can't spend the person's
budget; 120 a minute per person, and ten wrong codes in 15 minutes per person,
after which both refuse before looking). The app polls `/oauth2/token`, getting `authorization_pending`,
`slow_down`, `access_denied`, `expired_token`, or tokens once; a device code
redeemed again revokes the access tokens it minted. The same policy, roles and passkey requirement apply: `409` `step_up` sends the
page through `/confirm`. The ID token has no `nonce`; its `sid` is the approving
session, so signing out there signs the device out.

## The app side: fog-client

Any client library works. `FogClient::Signin` is the zero-dependency Ruby relying
party; `FogClient::Signin::Rails` the shared controller (`GET /auth/oidc/start`,
`/callback`, `POST /auth/oidc/backchannel`, `/link`). Without the `OIDC_*`
variables `config` is nil and the app shows its own sign-in.

It runs PAR, PKCE and a nonce with `openid profile roles`. State is sealed in
`state` and bound to the browser by `__Host-fog_signin` for 10 minutes. The console
gets a one-time code at `/auth/complete#handoff=…`, traded for its usual token;
refusals go to `/login#signin_error=<code>`. `identity.expires_at` is
`fog_session_exp` capped at 30 days (12 hours for a visitor, and the same caps
from a provider that states none); `start_session!` gets it as `expires_at:`.
It checks `nbf` when present, refetches keys every 5 minutes, ends every session on
a *sessions-revoked* logout, keeps a return address only when it parses as a bare
path with no control character or backslash, and needs no gem beyond Ruby's own
(not even `base64`).

**Fog-only.** `GET /app/v1/signin` with `FOG_API_TOKEN` returns a
`fog-signin-proof+jwt` binding `{issuer, client_id, app, iat}`. `Proof.check`
verifies it against `OIDC_ISSUER`'s keys: the fog that knows this app's token
signs its ID tokens. Only then may an app refuse every local way in. Keeping that
state, so a failed check never reopens sign-in, is the adopter's job.

`Linker.resolve` is pure, and **never links by email**. In order: `:sign_in`
(`(iss, sub)` is linked; `:refuse` if visitor and account disagree); `:visit` (a
visitor with no account: `visit!`, or refused); `:adopt` (SCIM made an account with
this `externalId`); `:bootstrap` (no accounts, and the role is the app's admin: the
setup wizard, on fog); `:link` (accounts exist: `/auth/link#pending=…` proves one
or starts fresh, in the same browser, by `__Host-fog_link`); `:provision`.

`rails.rb`'s header lists what every adopter must do; other relying parties are
held to `clients/fog-client/test/vectors/signin.json`.

## What the operator sees

- **App › Sign-in.** *How people sign in*; *Who can sign in*, with a role per
  person and an impact sentence (handle, display name, and email when the app is
  registered for it); *Signed in so far* with *Sign everyone out*; *Visitors*, a
  role per household or *Not allowed*; *Client* with *Rotate secret* (and a note
  while a secret waits for the deploy) and *Revoke now…*.
- **Settings › People.** *Add email…* (shown unverified), *Sessions*, and
  *Signing keys* with *Rotate now…*.
- **Settings › Your sign-in.** A person's passwords and passkeys, and *Recent
  changes*: sign-ins, passwords, passkeys and sessions, newest first. Adding a
  passkey or a first password, or removing either, takes a browser's own session
  proven within 10 minutes (a sign-in, joining, or `/confirm`, which the page
  sends the person through: `409 reauth_required`). A chat device's session,
  and any session not presented as fog's cookie, can't change credentials at
  all; changing a password takes the current one. The same holds for the
  operator acting on someone else: *Set password* and *Reset identity* under
  Settings › People take the operator's own browser session, freshly proven.
  The operator's own account changes only here, never through those.
- **A paired household's People settings.** "Let … see who is in this household"
  and "Let my people sign in to …'s apps as visitors".

## Guarantees and known gaps

**This was reviewed by the people who wrote it. It has not been externally
audited, and the OpenID Foundation conformance suite has not been run.** In its
place: `oidc_interop_test.go` drives fog with coreos/go-oidc as an independent
relying party, `FuzzParseAuthorize` fuzzes requests, `TestDecideInvariants` checks
every policy combination, visitors are tested with two fogs, and
`oidc_handlers_security_test.go` replays each attack the review found. The
official plans should gate fog sign-in for any app outside the household's own
fleet.

Guarantees:

- **No app holds fog's session**, and an app learns who someone is only from
  something fog signed. Client-sent identity headers never pass Caddy.
- **Who may sign in where is decided on every use**: each gated request, each
  authorization, each code and device-code redemption, each userinfo call.
  Guests and bots are refused first.
- **Taking access away ends it.** Suspension, removal, demotion, a narrower
  audience, a lesser role, a deploy that remaps roles or stops hosting visitors,
  a withdrawn visitor grant: each owes the affected apps a logout, written in the
  same statement or request as the change.
- **No session is fixed in advance.** Signing in mints a fresh token, and fog
  reads only a cookie it set as `__Host-` wherever it can set one.
- **Bearer secrets are digests at rest**, and single use is an atomic `UPDATE`.
  Authorization, device and visit codes revoke what they minted when replayed.
- **Every credential check is limited, on the attacker's budget.** A failure
  is charged first to the real client address the attempt came from (100 in
  15 minutes), then to the account it aimed at (10 in 15 minutes). A right
  password is charged too, to its own budgets: 20 per account and 60 per
  address in 15 minutes (the stranger bucket 600), checked before any hashing,
  so a valid password looped is never free. A passkey spends nothing. A guess at a handle no active person
  holds is charged to one bucket all such guesses share (100 in 15 minutes), so
  made-up handles never make keys of their own. An attempt refused at its
  address never reaches the account limit; a refusal further on still costs
  the address. Passkeys have a bucket of their own (60 ceremonies per address),
  so password guesses never hold one up. An IPv6 address counts as its /64. A
  visitor through fog's Cloudflare Tunnel is the address Cloudflare names
  (below). Any other request from this host's own address shares one stranger
  bucket ten times an address's size, which is never unlimited; a
  `Cf-Connecting-Ip` a request carries itself is nobody's word. Also:
  confirmations and password changes per person, device codes per person,
  client secrets per client and caller, authorize per person or address
  (only a top-level page load is charged: a browser's image, fetch or frame
  pointed at it is refused uncharged, so a sibling app can't spend a
  person's budget), and the gate's and visits' callbacks per address.
- **A share link's password is guessed on the guesser's budget.** Each address
  (or /64) gets 10 wrong passwords per link per 15 minutes (then `429` until
  the window ends), and an address with 50 wrong share passwords in a day is
  locked out of share passwords (`423`) for the day. The link is never locked:
  whoever holds it and its password still gets in. A link with more than 50
  wrong passwords in a day tells the operator, once (a notice and a push). A
  right password earns a cookie for that link's path, held an hour in this
  process (at most 200 per link, a new one replacing the link's oldest), so
  the downloads and range requests that follow skip the hash; a
  check waits up to 2 seconds for its place in the hashing line before `429`.
  A share password is at least 8 characters.
- **A full limiter forgets no one.** A limiter holds at most 100,000 keys. A key
  whose window is still running is never evicted to make room — that would hand
  whoever filled the limiter a fresh budget for it — and a refused attempt
  makes no key. Keys whose window has ended leave in the order their windows
  began (a queue, so a refusal costs O(1), never a walk of the map); if none
  has ended, the new key is refused: closed to a stranger, never reopened for
  someone already being counted.
- **The host's own address is one stranger bucket, ten times an address's.**
  Every request reaching fog from one of this host's own addresses (no
  tunnel to name it) shares one bucket, in every limit that counts by
  address, at ten times one address's budget.
- **Strangers fill only strangers' room.** A caller fog knows only by its
  address — a sign-in attempt, a passkey ceremony, a made-up token at
  userinfo, a wrong client secret, a signed-out authorize, a visit start, a
  chat webhook with a wrong token, MCP, WebDAV, share links, pairing and
  restore — is limited in a limiter of addresses alone (an IPv6 address
  counts as its /64), apart from every limiter keyed by something fog checked
  (an account, a person, an app, a registered client, a live token's client, a
  hook). When strangers have filled their room, the next share one bucket ten
  times an address's size, and an address a live session was signed in from
  gets a bucket of its own beside it. A live token, a client that proves its
  secret and a signed-in person are charged only to their own buckets, after
  they are checked; a failure is charged to the address.
- **Tunnel visitors are each their own.** cloudflared runs on the host, a child
  of fogd, and reaches Caddy's tunnel listener: a unix socket,
  `<data dir>/caddy/tunnel/caddy.sock`, in a directory only fog's own user can
  open, mounted into Caddy's container alone. A tunnel's public hostnames name
  it as their service (`unix:` and the path; Settings › Tunnel shows it). No
  network reaches it, so no app container — whatever address it forges, raw
  sockets and all — can speak as cloudflared; on every TCP port it can reach,
  Caddy names it as itself. Caddy serves only while a tunnel is set up. On the
  socket Caddy names the visitor to fogd as `Cf-Connecting-Ip`, with `https`,
  under the proxy secret, so a visitor's session is pinned, and their failures
  charged, to their own address; a LAN-only route refuses them all. A tunnel
  still pointed at `:443`, where every visitor arrives from the host's bridge
  address, is refused at sign-in with the operator told where to point it: the
  bridge address is never anyone's identity. On every route Caddy deletes each
  `X-Fog-*` header a client sent before setting its own — the gate's
  sub-request included — so no client names its address or its listener.
- **A passkey ceremony can't be displaced.** Its challenge is sealed
  (AES-GCM, a key only this process holds) into the ceremony id the browser
  holds, not kept in a table a stranger's flood could evict. It is bound to its
  purpose (signing in, adding a passkey, confirming) and to its person, expires
  in 5 minutes, and is good once.
- **No open redirects.** Redirect URIs match exactly, a gate `next` stays on its
  host, a visit returns only to the peer's paired address, and the SPA's `next`
  is same-origin, or https on a sibling of fog's domain when fog is at
  `fog.<domain>`.
- **Secrets stay off logs and traces.** fogd logs a request by the route it
  matched (`/api/join/{secret}`), never its path or query, so invite, share,
  webhook and pairing secrets in a path never reach the log, the feedback
  reports drawn from it, or Lucid's traces, which record the route alone. A path
  that matches no route is cut to two segments in the log and not traced.
- **A burst can't outrun the budgets or the memory.** Each attempt is reserved
  in its buckets before the credential is checked, in one step, and refunded
  only if it passes, so parallel guesses meet the same budget as serial ones.
  At most 4 password hashes (argon2id, 19 MiB each) run at once, in a line of
  36 that every password check waits in, sign-in's and share links' alike.
  One address (or /64) holds at most 2 places in it (the stranger bucket
  20), one account — or one share link — at most 1 however many addresses
  ask, guesses at handles no one holds at most half of it, and share-link
  passwords at most a quarter, so neither a crowd of made-up handles, nor a
  looped valid password, nor a share link can keep a real account's check
  out. A check with no
  place gets `429`; the account gets its attempt back, the address doesn't, so
  holding the line full is never free. The OAuth endpoints read at most
  64 KiB of form, and a limiter's keys are built from what fog checked (a
  registered client id, never the one sent).
- **fog's deliveries go where they're declared.** A back-channel logout or a
  SCIM call follows no redirect, and a `3xx` is a failed delivery, so an app
  can't aim fogd at another address or fake a delivery.
- **A peer is trusted only for its own people**, in apps opened to it, at a capped
  role, on keys delivered over the pairing, never DNS. It cannot identify a
  browser silently: the home asks first.
- **A person hears of changes to their sign-in**: sign-ins, passwords, passkeys,
  revoked sessions and identity resets are told to them through the channels
  they set for chat (Web Push, their ntfy topic) and listed under Settings › Your
  sign-in. Any audited change of those kinds does both (`auditAs`).

Accepted:

- **The operator can act as anyone in an app**, as any identity provider's admin
  can, but not in chat. The person is told of each credential change the
  operator makes.
- **Login CSRF on fog itself**: the form relies on same-origin, the Origin and
  `Sec-Fetch-Site` checks on `/api/` and `SameSite=Lax`.
- **A browser too old to send `Origin` or `Sec-Fetch-Site`** is judged only by
  its session; the address pin still keeps an app from using a stolen one.
- **A host-network app on fog's own machine** reaches fogd from the machine's
  address, so it could use a plain-`fog_session` web sign-in made from that same
  machine (a kiosk browser on the box). Such an app was given the host.
- **`/api/visit/peers` is public**: the login page needs it before anyone signs in.
- **Password sign-in can be held up for a person.** Ten wrong passwords for one
  handle, from anywhere, refuse password sign-in to it for 15 minutes; a passkey
  still works. A hundred guesses at handles no one holds refuse
  every such guess for 15 minutes, a person's mistyped handle included.

Known gaps:

- **A person with neither Web Push nor an ntfy topic is told only in Settings.**
  fog cannot send mail.
- **An app's cache of the keys.** After an emergency rotation an app keeps
  accepting what an old key signed until it refetches the key set: within 5
  minutes on fog-client, at its own pace on other libraries.
- **Sessions-revoked is fog's own event.** An app not on fog-client ends the
  sessions of the household's people and of visitors it hosts on a rotation,
  but not one built from a forged token for a subject fog never issued.
- **A tunnel set up before the tunnel listener** points at `https://localhost:443`
  and signs no one in until its service is changed to the tunnel socket
  (`unix:<data dir>/caddy/tunnel/caddy.sock`). fog can't make that change: the tunnel is made in
  Cloudflare's dashboard, and fog holds only its connector token and a
  DNS-only API token. Instead fogd notes which listener tunnel traffic reaching
  it arrives on (Caddy says, under the proxy secret; on `:443` only a request
  from one of the host's own addresses counts, never a container's — an
  app's, or Caddy's — or the LAN's), and while a tunnel is configured and none
  has arrived through the tunnel listener since the last on `:443`, a critical Inbox issue names the exact change. A tunnel whose
  hostnames never reach fogd (open apps only) shows it nothing, and clears by
  acknowledgement. A process running as fog's own user, or root, on fog's
  machine can reach the tunnel socket and name any visitor; such a process
  was given the host.
- **A failed sign-in can say a handle exists.** Once the shared bucket for
  handles no one holds is spent, a guess at one answers `429` where a guess at
  a real handle with budget left answers `401`. Each such probe costs the
  prober's address budget. Handles are not secrets: they are chat addresses.
- **A plant that replaces fog's cookie outright.** On an http setup or a bare
  address, something sharing the host can overwrite fog's own `fog_session`
  and `fog_browser` at `/` with a session of its own signed in from the same
  address (one NAT, one IPv6 /64): that browser is then signed in as that
  person, not robbed of its own session. `__Host-` on an https setup can't be
  planted.
- **The gate on plain http.** Without https there is no `__Host-` prefix, so a
  sibling host could plant the gate's binding cookie.
- **Apps not on fog-client** must end sessions at `fog_session_exp` themselves;
  fog's logout at expiry reaches them only if they take back-channel logout.
- **Visits need the host's federation address to be its `fog.<domain>`.** A
  household that federates by another address can't host visitors.

## Not built

- Moving house: a new domain changes `iss`, and nothing re-keys identities.
- One-click Inbox approval of access requests.
- Signing with anything but ES256; guests signing in; public clients, DPoP,
  refresh tokens; pictures in claims; a people-changed event; a break-glass link
  in fog-client; chat emitting the person ULID form.
