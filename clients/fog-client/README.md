# fog-client

The app-side view of Fog's Connect. Fog injects `FOG_API_URL` and
`FOG_API_TOKEN` into every app it manages; this zero-dependency client wraps
that surface.

```ruby
FogClient.available?       # running under fog with the API surface injected?
FogClient.connect.peers    # => [Peer(name:, display_name:, instance_id:, online, apps: [DirectoryApp...])]
                           # address by name; show display_name to people
FogClient.connect.request(peer: "dave", app: "feed", public_key: my_key, payload: { ... })
                           # => Grant(app:, url:, credential:, payload:, peer_instance:, peer_name:)
```

Two rules, by construction:

1. **Inert without Fog.** No env vars → `available?` is false, `peers` is
   `[]`, `request` is `nil`, nothing raises. Apps must run identically
   outside the fog stack. Key any fog-assisted UI off `available?` and
   nothing else.
2. **Never hurt the host app.** Short timeouts on every phase of the HTTP
   exchange (open, read, write); every failure becomes an empty answer.
   Fog being down costs a feature, never a request.

## Connect

`FogClient.connect.request` is the one way an app gets a credential from
its sibling in a paired household (fog:docs/connect-contract.md). Fog mints
a signed vouch, relays it to the peer's fog over the signed federation
channel, the peer's fog checks its operator's policy, and the peer's app
mints the credential its own settings page would have minted by hand. The
`Grant` that comes back carries that credential, the peer app's URL, any
handshake `payload` the peer app answered with, and which household it came
from. `app` defaults to this app's own name on the peer; `public_key` is
this app's fogfed key, bound into the vouch so the peer pins it from a
verified source; `payload` is this app's own handshake material, relayed
opaque. `nil` when refused, when the peer is unreachable, or outside fog.

## Caching and timeouts

`peers` is meant for the request path (rendering a household list), so a
successful answer is cached in-process for 30 seconds — one fog round-trip
per TTL window, not one per page view. Failures are never cached; the next
call retries. Tune or bypass per call, and tighten the HTTP bounds where a
render can't afford the defaults (open 2s / read 5s; `request` reads up to
20s because it relays across households):

```ruby
FogClient.connect.peers(fresh: true)              # skip the cache (still refills it)
FogClient.connect.peers(ttl: 5)                   # accept a 5s-old answer
FogClient.connect.peers(open_timeout: 1, read_timeout: 1)
FogClient.connect.request(peer: "dave", app: "feed", read_timeout: 10)
```

## Reachability

`FogClient.connect.reachability` asks fog whether this household is
dialable from the internet — the same verdict fog shows its operator on the
Connect page — as a `Reachability(reachable?, url, via, tunnel_configured,
tunnel_running, advice)`. `via` is `"advertised"`, `"domain"` or `"none"`;
`advice` is the operator's next step when unreachable. Use it to say so
honestly in your own UI and to pause public delivery rather than burn
attempts; an unreachable household still federates with any peer that is
reachable itself. Uncached, `nil` outside fog or when fog is unreachable.

## Connections and mounts

`FogClient.connections` lists the apps this one `uses` in its fog spec
(fog:docs/files-contract.md, "Wiring"), each a `Connection(app, url,
credential, problem, paths, unseen)`. `problem` says why one can't be used,
and `installed?` and `usable?` read it for you. `nil` when fog could not be
asked, `[]` when fog answered with nothing, so an outage never tears down
wiring.

`FogClient.files.mounts` lists the folders fog has mounted into this app's
container, each a `Mount(mount, host_path, kind, peer, share, read_only,
hydrated, status, last_synced_at)`; `peer?` picks out a peer's share. `[]`
outside fog or when fog is unreachable.

Both are uncached and take `open_timeout:` and `read_timeout:`.

## Sign in with fog

`FogClient::Signin` is the relying-party half of `fog-signin/1`
(fog:docs/signin-contract.md): authorization code with PKCE, pushed
authorization requests, ES256 ID tokens verified with OpenSSL (no JOSE
dependency), back-channel logout, and the fog-only proof. It is plain
OpenID Connect, configured by `OIDC_ISSUER`, `OIDC_CLIENT_ID` and
`OIDC_CLIENT_SECRET` — fog injects them, and any other provider works the
same way. Without them `FogClient::Signin.config` is nil. Fog also injects
`FOG_OIDC_INTERNAL_URL`, which sends back-channel calls straight to fogd.

Rails apps `require "fog_client/signin/rails"` and include
`FogClient::Signin::Rails` in a controller outside their own
authentication, supplying an adapter that maps a verified identity onto
the app's accounts (the module's header lists what it answers). Accounts
are never matched by email: an unlinked identity is sent to `/auth/link`
to prove an existing account is theirs, or to start fresh — in the same
browser only (`__Host-fog_link`).

A **visitor** — a paired household's person, signed in by their own fog
(fog:docs/signin-contract.md) — has `identity.visitor?`, a subject
and `home` naming their household, and never an email. The linker never
bootstraps, adopts or links one: an app that hosts visitors (its `fog.yaml`
declares `signin.visitors`) answers `visit!(identity)` with a visitor's
account (and `visitor_account?(account)`, so a visitor never signs in as
anyone else); any other app refuses them. `link!` follows `visit!`, to
remember the visitor's identity. Refusals redirect to
`/login#signin_error=<code>`; `FogClient::Signin::VISITOR_REFUSALS` holds the
codes and the sentence for each.

The kit rate-limits the link step as `fog-link`: an adopter that limited
it itself should remove its own, or the two share the budget.

**A session ends no later than fog's.** `identity.expires_at` (and
`session_exp`, in unix seconds) is the ID token's `fog_session_exp` — when
the fog session behind the sign-in ends — capped at 30 days, or 12 hours for
a visitor, even from a provider that states nothing. `start_session!`
receives it as `expires_at:` when it takes that keyword; an adopter must end
the session it starts by then. The kit refetches the provider's keys every
five minutes, so a key fog retired in an emergency stops verifying. After such a
rotation fog sends a *sessions-revoked* logout token (event
`urn:fog:event:sessions-revoked`, `sid` `urn:fog:sid:all`): the kit calls the
adapter's `end_all_sessions!(before:, issuer:)` to end every session from a
sign-in before then, and answers `501` when the adapter has none, so fog keeps
trying and its Inbox says the app hasn't heard.

What every adopter must also do is listed at the top of
`lib/fog_client/signin/rails.rb`: refuse every local way in once fog-only
(passwords, passkeys, invites, sign-up, setup and its restore), throttle
the proof (`Proof.check_throttled`) on unauthenticated pages, filter
`code`, `pending` and `handoff` from logs, prune sessions whose handoff was
never redeemed, and give fog-created accounts an email only when fog marks
it verified and nobody holds it. The kit rate-limits `link` itself.

An app that already has a constant named `FogClient` must rename it first:
the gem's module takes that name.

`rbi/fog_client_signin.rbi` is the typed surface for Sorbet. The sync vendors
it with the gem, at `vendor/gems/fog-client/rbi/`; an app's Sorbet config
ignores `vendor/`, so copy it from there to `sorbet/rbi/shims/` after
syncing.

**Behind the gate.** An app on a route with `auth: true` learns who is at the
door from `require "fog_client/signin/gate"` and
`config.middleware.insert_before 0, FogClient::Signin::Gate::Middleware`.

- **What it checks:** each request's `Fog-Assertion` against fog's keys, the
  issuer, an audience of the app's own gated host, and expiry. It reads the
  issuer and audiences from `GET /app/v1/gate`, and the keys through
  `FOG_API_URL`.
- **What the app gets:** the identity, in `env["fog.gate.identity"]`
  (`fog_gate_identity` in a controller that includes
  `FogClient::Signin::Gate::Rails`; `require_fog_gate!` answers 401 without one).
- **`Remote-*` headers:** it deletes every one the request brought, and writes
  back only what the assertion says. A bare `Remote-User` from a container on
  the network proves nothing and reaches the app as nothing.
- **Opting out:** `trust_remote_headers: true` (or
  `FOG_GATE_TRUST_REMOTE_HEADERS=1`) is only for an app that can't reach fog's
  API, and it warns.

`FogClient::Signin::SCIM` is the app's provisioning endpoint: `require
"fog_client/signin/scim"`, and without `SCIM_TOKEN` it does not exist.
`FogClient::Signin::Device` is the RFC 8628 device sign-in.

`test/vectors/signin.json` holds tokens fog's Go keyring signed, and the
verdict for each; fog's `internal/signin` tests check the same file with
go-jose, so the two implementations cannot drift apart silently.

## Tests

`cd clients/fog-client && rake test` — stdlib only, no gems to install.

Vendored into apps via `scripts/sync-fog-client` (same discipline as the
flare gem — Docker builds need zero credentials and zero network). With no
arguments it updates every sibling checkout that already vendors the gem; an
app dir vendors it the first time. The vendored copy is exactly the
gemspec's `files` plus the gemspec, and anything else in it is removed. It refuses while the gem has uncommitted or untracked changes here; `--allow-dirty` overrides that.
