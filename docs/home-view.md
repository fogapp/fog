# The home view

`GET /app/v1/home` is what a start page (newtab) shows of this server, in
one read: every app with its state, the Inbox summary and the host's
vitals. It is built from the same calls as the dashboard and the Inbox
badge, so the three never disagree.

## Access

Every managed app holds a `FOG_API_TOKEN`, so the view is default-deny. An
app may read it only with `home` in its `uses`, which the operator turns on
as **Read the home view** under the app's Config, App access.

| Answer | Means |
|---|---|
| 200 | The view, with an `ETag` |
| 304 | `If-None-Match` named the current view |
| 401 | No token, or one that names no app |
| 403 | The app is not granted `home` |

## The view

```json
{
  "self": "newtab",
  "server": { "name": "Amberwood", "url": "https://fog.home.example" },
  "apps": [{
    "name": "immich", "title": "Immich", "icon": "/store/immich/logo.svg",
    "url": "https://immich.home.example",
    "state": "running", "desiredState": "running", "healthy": true,
    "deploying": false, "updateAvailable": false, "openErrors": 0,
    "contracts": ["fog-files/1"]
  }],
  "inbox": {
    "errors": 0, "issues": { "critical": 0, "warning": 1, "info": 0 },
    "notices": 0,
    "fogUpdate": { "available": true, "currentVersion": "v0.13.11", "latestVersion": "v0.13.12", "notes": "…", "ignored": false }
  },
  "metrics": { "cpuPct": 12.5, "memUsed": 0, "memTotal": 0, "diskUsed": 0, "diskTotal": 0 }
}
```

- `self` is the reading app, so it can leave itself out.
- `server.name` is empty when the operator has not named the server;
  `server.url` is empty without a base domain.
- `title` and `icon` come from the app's store listing. Other apps have
  their name for a title and no icon. `icon` is a path fogd serves without
  auth.
- `url` is the app's first route. It is absent for an app with no routes.
- `state` is the main container's (`running`, `exited`, …), `new` for an
  app never deployed and `missing` when it has no container. `healthy` and
  `openErrors` are absent when unknown, never a guessed value.
- `metrics` is `null` until the host sampler has a reading.

The view changes as often as the vitals do (every ten seconds), so a page
that polls faster mostly gets 304.
