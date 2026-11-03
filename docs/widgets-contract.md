# fog-widgets/1

An app says what it has to show on a household's start page, and what it can
find for the search box. The start page draws it. Today that is
newtab, and this document describes what its
client does: `internal/sources/contract` and `internal/search` in that repo are
the other half of this contract.

The app never talks to the start page. fogd relays, so the start page holds no
credential of the app's and needs no way to reach it, and fogd keeps no cache
and no schedule of its own.

```
newtab ── GET /app/v1/widgets/hook ──▶ fogd ── GET /fog/widgets ──▶ hook
           bearer: newtab's token              bearer: hook's token
```

## Declaring it

In the repo's `fog.yaml`:

```yaml
widgets:
  contract: fog-widgets/1
```

fog records it at each deploy, and it appears in the app's `contracts` in the
home view (docs/home-view.md), which is how the start page finds the apps to
ask. Only a git app has a `fog.yaml`, so only a git app can declare it.

## What the app serves

Both endpoints authenticate fog's call with the app's own `FOG_API_TOKEN` as a
bearer, as every `/fog/*` contract does. An app should refuse anything else.

### `GET /fog/widgets`

Required. It answers `200` with JSON, and should send an `ETag`. fog passes
`If-None-Match` on, so an unchanged poll can answer `304`.

```json
{ "widgets": [
  { "id": "now-playing", "title": "Now playing", "size": "2x1", "ttl": 15,
    "href": "/watch/812", "sensitive": false, "audience": "household",
    "blocks": [
      { "type": "media", "title": "Severance", "subtitle": "S2 · E4 · Living room",
        "image": "/artwork/812/3/320.webp", "fraction": 0.42, "href": "/watch/812" }
    ] }
] }
```

| Field | Meaning |
|---|---|
| `id` | Required. `^[a-z0-9-]{1,40}$`, unique in the answer. A widget without a valid id is skipped. |
| `title` | Required, not blank. A widget without one is skipped. |
| `href` | Where the widget links. See [Links and pictures](#links-and-pictures). |
| `size` | `1x1`, `2x1`, `1x2`, `2x2` or `4x1`. Anything else, the bar sizes included, is `1x1`. |
| `ttl` | Seconds until it reads as stale. Missing means 60; held to 5 through 86400. |
| `sensitive` | `true` keeps it out of the browser and off disk. |
| `audience` | `household`, or `operator`, the default. |
| `blocks` | What it says, in order. |

At most 8 widgets, 8 blocks in each and 12 items in a list are taken, and text
is clipped to 200 characters. Past that a source is trimmed, not rejected, and a
block of an unknown type is dropped.

### Blocks

An app says what is true; the start page decides how it looks. `tone`, where a
block has it, is `ok` or `warn`. Every type but `list` may carry an `href`,
which links the whole block.

| `type` | Fields |
|---|---|
| `stat` | `value`, `label`, `unit`, `tone`. Stats in a row share one. |
| `list` | `items`: each `title`, `subtitle`, `href`, `image`, `tone`, and `at`, an RFC 3339 time shown as an age. |
| `media` | `title`, `subtitle`, `image`, `fraction` |
| `progress` | `label`, `detail`, `fraction` |
| `spark` | `label`, `points` (numbers, plotted in order, at most 240), `max` (scaled to their own range if unset) |
| `status` | `label`, `detail`, `tone` |
| `callout` | `text`, `tone` |

`fraction` is 0 to 1.

### `GET /fog/search?q=…`

Optional. An app that doesn't search answers `404`, and the start page leaves it
alone for ten minutes.

```json
{ "results": [
  { "title": "Severance", "subtitle": "Series · 2022", "href": "/shows/812", "image": "/artwork/812/1/320.webp" }
] }
```

The first 8 with a `title` and a usable `href` are taken. It has 1.2 seconds to
answer. newtab only asks on the operator's behalf, because apps search as their
household's first admin; nothing in the request says who is asking.

### Links and pictures

An `href` is a path on the app, which the start page makes absolute against the
app's address, or an absolute `https` URL. Anything else is no link.

An `image` is a path on the app; anything else is no picture. newtab fetches it
itself from `http://<app>`, with no credentials, a `Host` of the app's public
name and `X-Forwarded-Proto: https`, then resizes it and serves it from its own
origin. So pictures must be public on that path. An app holding the `home`
grant is wired to every app that declares `fog-widgets/1` for that
([isolation](sealed-network.md#wires)): it reaches them through fog-wire, on
port 80, and they can't reach it.

## The relay

Called with the reader's own `FOG_API_TOKEN`, like the rest of `/app/v1`, by an
app whose `uses` includes `home`: the same grant as the home view, since these
are the rest of that page.

```
GET /app/v1/widgets/{app}          relays to GET /fog/widgets
GET /app/v1/search/{app}?q=…       relays to GET /fog/search?q=…
```

| fogd answers | When |
|---|---|
| `200`, with the app's body and `ETag` | The app did. |
| `304`, with the `ETag` | The app did, to the `If-None-Match` fog passed on. |
| `401` | No token, or not an app's. |
| `403` | The reader doesn't have `home` in its `uses`. |
| `404` | No such app, it doesn't declare the contract, or it answered `404`. |
| `429` | The reader is over `/app/v1`'s limit, 120 requests a minute. |
| `502` | The app isn't running, didn't answer in 4 seconds, answered anything else, or sent more than 256 KiB. |

An app's `403` is a `502` here on purpose: a reader takes `403` to mean its own
grant is gone. Redirects aren't followed. Only `q` reaches the app, and no
response header but `ETag` comes back.

The limit counts every `/app/v1` call by the reader. Polling is about every 15
seconds per app while someone is looking and every 5 minutes otherwise, and a
keystroke in search is a call per app, so a large fleet can meet it. A search
that does just misses that app's results.

## Sealed apps

The relay reaches an app wherever fog does, a sealed app's network included, so
a sealed app can serve widgets (docs/sealed-network.md). Two things follow from that:

- What it answers leaves the boundary, to a reader that may have the internet.
  An app that holds private things should answer with nothing they wouldn't put on a screen in the
  hall, and mark the rest `sensitive`.
- newtab fetches pictures from the app itself, not through fogd. Unless newtab
  declares `sealedPeer: true`, fog doesn't wire it to a sealed app, and that
  app's pictures don't appear.

The relay itself hasn't run against a real sealed app: see the
verification steps in docs/sealed-network.md.
