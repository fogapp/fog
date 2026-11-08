# The household event stream and app connections

*An app-facing surface on `/app/v1`, alongside fog-connect/1
(docs/connect-contract.md), fog-files/1 (docs/files-contract.md) and
fog-backup/1 (docs/backup-contract.md).*

Several apps on a household server each grew their own way of saying
"something happened": Glass pushes to ntfy when a monitor trips, Hook when
a grab needs review, fogd itself when a deploy fails. Each one is set up
separately, and none of them can be told apart, filtered or routed
differently once it is on the phone.

The event stream gives them one place to say it. Apps **publish** what
happened; a consumer such as HUD **reads** the stream and decides what each
event deserves: a push, a glance at the desk display, nothing. Fog only
carries events. It never interprets them and never pushes them anywhere.

**Connections** are the other half. Some apps worth hearing from (SABnzbd,
Home Assistant) will never publish to fog, so a consumer has to watch them
itself, and for that it needs their address and an API key. Fog already
holds both, so it hands them over, but only to apps the operator has granted
access.

Fog is a convenience, not a dependency. An app that
never publishes loses nothing, and an app with nobody reading still works.

## Publishing: `POST /app/v1/events`

Any app may publish. It authenticates with the `FOG_API_TOKEN` every app is
deployed with, so there is nothing to configure. The body is one
[CloudEvent](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/formats/json-format.md)
in structured JSON, or a JSON array of up to 100 of them:

```json
{
  "id": "incident-4812-opened",
  "type": "glass.incident.opened",
  "subject": "hub: p95 latency",
  "time": "2026-10-06T14:03:11Z",
  "data": { "key": "incident-4812", "title": "hub p95 over 800ms", "body": "…", "link": "https://glass.example/monitors/17" }
}
```

| field     | rules |
|-----------|-------|
| `type`    | required, lowercase dotted: `<app>.<thing>.<happened>` |
| `id`      | optional (fog mints one); a retry with the same id is absorbed, so publish with a stable id when the event has one |
| `subject` | optional, at most 256 characters, what it is about |
| `time`    | optional RFC 3339, defaults to now |
| `data`    | optional JSON, at most 32 KiB. `title`, `body` and `link` are the conventional fields a consumer shows. `key` names what the event is *about*: a consumer may let a later event with the same key replace an earlier one, so `incident.closed` ends what `incident.opened` started |
| `source`  | ignored: fog sets it to the publishing app's name |

The answer is `202 {"accepted": n, "new": k}`, where `k` excludes ids
already in the stream. Publishing is a notice, not a transaction: an app
should treat failure as non-fatal and keep its own behaviour, such as an
ntfy push it already sends, working either way.

fogd publishes its own notifications (deploy failed, unhealthy, backups
overdue, updates) as `fog.notice` with source `fog`. The data is
`{title, body, level: "ok" | "alert"}`. These go out whether or not ntfy is
configured, so a consumer can take over routing them. Clearing fog's ntfy
topic then stops the duplicate push.

## Reading: `GET /app/v1/events`

Reading needs `events` in the app's `uses` (below).

| query   | meaning |
|---------|---------|
| `after` | the cursor from the previous read. **Omit it to start from now**: the answer carries no events, just the cursor to resume from |
| `limit` | at most 500 per page (the default) |
| `wait`  | seconds, up to 60, to long-poll when there is nothing after the cursor yet |

```json
{
  "events": [ { "specversion": "1.0", "seq": 913, "source": "glass", "type": "glass.incident.opened", "id": "…", "time": "…", "data": { … } } ],
  "cursor": 913,
  "gap": false
}
```

Store the cursor after handling a page, then read again with
`after=<cursor>&wait=50`. Delivery is at least once: a reader that crashes
before saving its cursor sees the page again, and `source` + `id`
identifies the duplicate. Fog keeps the stream for a week. `gap: true` says
events between the cursor and the first one returned were pruned before
this reader saw them.

## Connections: `GET /app/v1/connections`

Returns every app named in the caller's `uses`, with where to reach it and
the credential it offers. Naming an app in `uses` is also what wires the two
on the network: an app reaches only the apps it is wired to
([isolation](sealed-network.md#isolation-every-app-on-a-network-of-its-own)).

```json
{ "connections": [
  { "app": "sabnzbd", "url": "http://sabnzbd:8080", "credential": "…" },
  { "app": "homeassistant", "url": "http://homeassistant:8123", "problem": "set the HASS_TOKEN secret on homeassistant (Config → Secrets)" },
  { "app": "lidarr", "problem": "not installed" }
] }
```

An app that can't be used yet is listed with a `problem` the operator can
act on, so the consumer can explain the gap instead of silently missing an
app. Read connections when starting and again now and then: a credential
the operator adds later should be picked up without a redeploy.

## Declaring

Both halves live in the app spec, so the operator sees them on the app page
and every change is a revision:

```yaml
# the consumer (hud.fog.yaml)
uses: [events, sabnzbd, homeassistant]

# a provider (fog's catalog entry for SABnzbd)
connection: { port: 8080, secret: SABNZBD_API_KEY, shares: [media] }
```

Besides app names, `uses` takes three grants on fog's own surface: `events`
(above), `home` ([widgets-contract.md](widgets-contract.md)) and `chat`,
which lets the app post into the rooms fog's bot is in, each post shown as
the app's ([chat-contract.md](chat-contract.md#bots)).

`connection.secret` names one of the provider's vault secrets. It need not
be in `secrets`: Home Assistant mints its own tokens, so its `HASS_TOKEN` is
pasted in after setup, and listing it in `secrets` would fail every deploy
until then. The catalog's SABnzbd generates its key at install and holds it
in `sabnzbd.ini` at every start.

A provider that can mint a credential per consumer, and a consumer that
only reads env vars, use an integration instead
([integrations-contract.md](integrations-contract.md)): offers declared in
the provider's repo `fog.yaml`, and a click on the consumer's Integrations
tab.
