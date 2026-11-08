# fog-integration/1 — connecting one app to another

An app that offers something to the other apps on the box (Glass takes
their telemetry) declares the offer. An app that can use another declares
that it wants it. Its **Integrations** tab lists one entry per app it
wants, and connecting one is a single click: fog asks the provider to mint
a credential for that app alone, writes it into the app's environment,
grants it any access to the app it asked for, and redeploys it. Nobody
copies a key, and the tab never shows which variables are set.

This sits beside [connections](events-contract.md#connections-get-appv1connections),
which hand every consumer the provider's one shared secret at runtime.
An integration is for a provider that can mint per-app credentials and a
consumer that reads plain environment variables: each consumer's
credential can be revoked on its own, and the consumer needs no fog
client.

## Declaring

A git app declares its offers and wants in its repo's `fog.yaml`, beside
`signin` and `gettingStarted`. Fog reads that file at every deploy and
records the `integrations` block, so a push reaches the installed app.
The catalog and an imported manifest only seed an app's spec at install,
and are never re-read.

```yaml
# glass's fog.yaml
integrations:
  offers:
    - id: monitoring
      title: Monitoring
      blurb: Requests, errors, logs and profiles from this app show up in Glass, …
      env:
        GLASS_URL: "{{url}}"
      secrets: [GLASS_API_KEY, GLASS_READ_TOKEN]
      capabilities:
        - Requests, errors, logs and profiles show up in Glass
        - Read Glass's charts, monitors, incidents and deploys

# newtab's fog.yaml
integrations:
  wants:
    - offer: glass/monitoring
      as: { GLASS_READ_TOKEN: NEWTAB_GLASS_TOKEN }

# hook's fog.yaml
integrations:
  wants:
    - app: sabnzbd
      capabilities: [Sends downloads to SABnzbd]
    - app: stream
    - offer: stream/library
```

Make one offer per thing an operator would connect, not one per
credential. Connecting an app to Glass is one choice, so Glass's single
offer mints both the ingest key the app reports with and the read-only
token its widgets read with; an app that only reports leaves the token
unread.

A block fog can't read, such as a misspelt field or an offer with no
title, is left out of that deploy, with a warning in its log.

An image app has no repo, so the same blocks go in its spec:
`connection.offers` and `wants`. A spec can also override a git app's
declaration, because the spec is the operator's. An offer or want in the
spec wins over the repo's under the same name, and so does
`connection.port`. Without a connection in the spec, `integrations.port`
or else the app's health port is where `{{url}}` points.

### Offers

- `id` names the offer in calls to the provider; lowercase, like an app
  name.
- `env` is set in the consumer's spec as written. `{{url}}` is the
  provider's address: `http://<app>[:<port>]<path>`, with the port left
  out when it is 80. An env value naming the provider so is what wires the
  consumer to it: the consumer reaches it, one way, through fog-wire
  ([isolation](sealed-network.md#wires)).
- `secrets` names what the provider's mint answers. Fog stores the values
  in the consumer's vault and adds the names to its `secrets`.

- `capabilities` are short lines saying what a connected app gets; the tab
  lists them under the provider's icon. Without any, it shows the blurb.

The tab warns, without naming them, when connecting would overwrite a
value the consumer sets by hand.

### Wants

A consumer lists every app it can connect to, and the tab lists exactly
those: no other app's offers, and never the app's own. Each want is one
of:

- `offer: <provider>/<offer id>` — the offer is minted for this app.
  `as` renames a variable the offer sets to the name the consumer reads.
- `app: <provider>` — this app is handed the provider's
  [connection](events-contract.md#connections-get-appv1connections)
  (its address, shared credential and path translations), and the
  folders it shares are mounted. Connecting adds the provider to the
  spec's `uses`, which is how an image app such as SABnzbd, which can't
  mint, is connected.

Wants of the same provider are one entry, and one Connect sets up all of
them in one revision. `capabilities` on a want say what this app does
with the provider, and replace the offers' lines on the tab. An app
that is wanted but not installed is still listed, saying to install it.
When a consumer comes to want more of a provider it is already connected
to, the entry offers to update the connection, which sets up only what
is missing. A want is only a suggestion: the operator still connects
each one.

## The provider's surface

Fog calls the provider at its container's address with the provider's own
`FOG_API_TOKEN` as a bearer token, as for backups. Errors are
`{"errors": ["…"]}`, and fog shows the first one to the operator as
written, so make it a sentence they can act on.

### `POST /fog/integrations`

```json
{ "consumer": "newtab", "offer": "monitoring" }
```

Mint a credential for the consumer, which is a fog app name. Answer `201`:

```json
{ "id": "8c1e…", "secrets": { "GLASS_API_KEY": "glass_ing_…", "GLASS_READ_TOKEN": "glass_…" } }
```

`id` is opaque to fog and is how fog names the credential from then on.
`secrets` must hold every name the offer declares. If one is missing, fog
revokes the id and applies nothing. Refuse with `422` when the provider
can't mint yet, for example because Glass has no household until its
setup is finished.

### `GET /fog/integrations/{id}`

```json
{ "id": "8c1e…", "active": true,
  "created_at": "2026-10-13T09:00:00Z", "last_activity_at": "2026-10-13T09:41:00Z" }
```

The tab shows the status. `last_activity_at` is when the consumer last
used the credential, or null. `active: false` (revoked in the provider's
own UI) and `404` both show as revoked, with a prompt to reconnect.

### `DELETE /fog/integrations/{id}`

Revoke it and answer `204`. `404` means it is already gone, which fog
treats as done.

### Consent: `"consent": true`

Some credentials belong to a person, not to the operator's choice: a
provider whose users are not fog's operators may want one of its own
people to approve each one. Such an offer adds a step at the provider,
and needs fog to have a domain, so use it only when the operator's click
is not enough:

1. Fog's mint call carries `return_to`, an address on fog with a one-time
   `state` in its query. The provider answers `202 {"id": "…", "consent":
   "/connect/<ticket>"}`, where the path is a page on its own console.
2. Fog records the connection as pending and sends the operator's browser
   to that page under the provider's address on the domain. If fog has no
   domain, it refuses and revokes the id.
3. The person approves or declines. The provider sends them back to
   `return_to` with `result=approved` (or anything else for no).
4. Approved, fog claims the credential once with `POST
   /fog/integrations/{id}/claim`, which answers `{"secrets": {…}}` as a
   mint would. A provider answers `409` while nobody has approved, and
   refuses a second claim. Fog then applies it like any other connection
   and lands the person back on the Integrations tab. Declined, or on any
   failure, fog revokes the id and forgets it.

A pending connection shows as waiting, and can be cancelled. After 30
minutes it has expired, and connecting again replaces it.

## What fog does

**Connect** (`POST /api/apps/{app}/integrations {provider, deploy}`):

1. Refuse unless the app wants the provider, the provider is installed,
   running, and makes every offer wanted, and something wanted is not yet
   connected.
2. Ask a consent offer's provider first, if one is wanted (one at a time).
3. Mint every other wanted offer through the provider.
4. Write one revision of the app's spec, `integration: connect glass`,
   with the offers' env and secret names, and the provider in `uses` for
   an `app` want. The secret values go to the vault, so the Config tab
   and History show the change like any other.
5. Record each credential (`app_integrations`), and redeploy the app
   when asked and running. Env is read at deploy.

If any step after a mint fails, fog revokes every credential it minted.

**Disconnect** (`DELETE /api/apps/{app}/integrations/{provider}?deploy=1`)
does the reverse for the whole provider: it revokes each credential at
the provider, removes exactly the env and secret names they set and the
provider from `uses`, and redeploys. A request still waiting for
approval is cancelled. If the provider is
stopped or refuses, fog stops and says so. `?force=1` goes ahead anyway,
for a provider that is gone for good. Deleting the app revokes its
credentials at every provider, best-effort.

A provider that is uninstalled leaves its integrations in place, shown
with that problem, so the operator can disconnect them and clean up the
env.
