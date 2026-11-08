# fog-catalog/1 — publishing apps to fog's store

Fog's app store isn't built into fog. It is built from **catalogs**: git
repositories, each a list of apps and everything fog needs to install them
with one click. A fog reads the catalogs in its list (Settings › App store ›
Catalogs), first to last, and its store is everything they hold.

Out of the box the list is fog's own two:

| Catalog | What's in it |
|---|---|
| `fog-apps` | The apps built for fog that are published for anyone to run — Fin, for now |
| `fog-catalog` | Well-behaved self-hosted apps packaged for fog — the arr stack, media servers, Immich, Paperless, Gitea… |

Anyone can publish another, public or private: a household's own apps
that aren't published can live in a private catalog that only its fog
reads. Put the layout below in a repository, and anyone
who adds its URL to their list gets your apps in their store. This page is
the whole format; fog's reader is `internal/catalog` (`format.go`), and its
rules are the ones written here.

## Layout

```
catalog.yaml              the catalog itself
apps/<id>/app.yaml        one app
apps/<id>/<artwork>       the logo and screenshots app.yaml names
```

Anything else in the repository (a README, CI) is ignored.

## `catalog.yaml`

```yaml
format: fog-catalog/1
name: my-apps
title: My apps
description: Apps I've packaged for fog.
featured: [paperclip]         # shown in a fog's store until its operator chooses
order: [paperclip, notebook]  # display order within a category
```

- `format` is required and must be `fog-catalog/1`; fog refuses a catalog
  in a format it doesn't read.
- `name` is informational. A fog knows a catalog by the name it is given in
  that fog's list.
- `featured` and `order` may name only apps in this catalog. Apps that
  `order` doesn't name come after the ones it does, by title.

## `apps/<id>/app.yaml`

One app: what it is, how it runs, and what to ask at install. The folder
name is the app's `id`, which must match the `id` field. It is also the
app's default name on a fog, so it follows app-name rules: lowercase
letters, digits and dashes.

```yaml
id: paperclip
title: Paperclip
description: A one-line summary for the store's cards.
category: Utilities
icon: '📎'                 # shown wherever there's no logo
spec:                      # an AppSpec — exactly what fog's API takes
  source:
    type: image
    image: ghcr.io/someone/paperclip:1
  env:
    TZ: Etc/UTC
    BASE_URL: https://{{fqdn}}
  secrets: [SESSION_KEY]
  volumes:
    - name: data
      mount: /data
  routes:
    - port: 8080
  health:
    path: /healthz
  backup:
    enabled: true
options:                   # asked at install
  - key: SESSION_KEY
    label: Session key
    target: secret:SESSION_KEY
    required: true
    generate: hex64
  - key: tz
    label: Timezone
    target: env:TZ
    default: Etc/UTC
companions:                # "also install" checkboxes
  - id: gotenberg
    blurb: Renders documents to PDF for Paperclip's exports.
requires:
  cpu: x86-64-v2           # amd64 hosts below this are told why it won't run
footprint:                 # what the app costs the machine; bytes
  memory: 209715200
  memoryPeak: 524288000
  cpu: light               # light | moderate | heavy
  disk: 314572800
  diskGrows: the documents you add
projectUrl: https://github.com/someone/paperclip
about: |
  Two or three short paragraphs: what the app is for, and what it is
  like to live with.
highlights:
  - Three to six short feature lines
  - Each a fragment that starts with the capability
  - Shown as a list on the app's page
logo: logo.svg
screenshots:
  - src: shot-1.webp
    alt: The library, sorted by date added
developer: Someone
license: MIT
website: https://paperclip.example
docs: https://paperclip.example/docs
```

Fields are read strictly: a key fog doesn't know (a misspelling) is an
error, not something silently ignored.

### `spec`

The same `AppSpec` fog uses everywhere. Installing from the store and
creating the app by hand produce identical apps, and everything stays
editable afterwards. `{{fqdn}}` and `{{commit}}` are filled in at deploy.
The parts most catalogs use:

- **`volumes`** with no `files` are the app's own data. Fog places them under
  `Apps/<App>/<volume>` in Drive and backs them up. A volume that reads the
  household's media names a library: `files: Content` or
  `files: Content/<Library>` (Movies, Music…). Any other folder is refused.
- **`routes`**: at least one. The store installs apps people open.
- **`aux`** sidecars for a database or broker the app brings with it, or
  **`needs.postgres`** for a database in fog's shared Postgres.
- **`files`** with `content`: small config files or init hooks, written
  into the container.

### `options`

What the install pane asks. `target` says what an answer fills:

| target | fills |
|---|---|
| `env:<KEY>` | an environment variable |
| `secret:<KEY>` | a secret in fog's vault. `<KEY>` must be in `spec.secrets` |
| `volume:<name>` | where that volume lives. The spec's placement is the default |
| `repo` | the git clone URL, for a `git` source |

`default` prefills an answer. `generate` (`alnum32`, `hex32`, `hex64`,
`hex128`) mints a fresh random one. An option with either is tucked under
Advanced, and so is one marked `advanced: true`. Aim for an install that
asks nothing.

### `companions`

Other apps offered alongside this one. A companion may be in any catalog the
fog reads. It is offered only when every one of its options can be answered
without asking (a default, a generator, or the timezone). Otherwise fog
drops it from the offer and reports why.

### Artwork

`logo` and each screenshot's `src` are file names in the app's own folder.
They must be svg, webp, png or jpeg, at most 8 MB each. Fog serves them
from its own copy of the catalog, so a fog with no route to the internet
still shows them. A logo is drawn inside a rounded tile, unless its file
is named `tile.<ext>`, which is drawn edge to edge. Every screenshot needs
`alt` text: it is the caption, and what a screen reader says.

### Building from source

An app fog builds from git sets `spec.source.type: git` with its `repo`,
`branch` and `dockerfile`. Give a public repository's `https://` clone
URL: any fog can clone it with no key. A private repository needs its SSH
URL, plus each installing fog's deploy key added to it, so it suits a
private catalog rather than a published one.

fog's own catalogs also use `firstParty: <repo>` for apps whose repository
a fog's operator may host themselves. The clone URL then comes from that
fog's settings (Settings › App store › First-party app source), and the
entry's `publicRepo` (e.g. Fin's `https://github.com/fogapp/fin.git`) is
the fallback when there is none.

## How a fog reads a catalog

- **Fetching.** A fog clones each catalog into its data directory, then
  fetches every six hours, when its list changes, and on **Fetch now**. A
  public catalog clones over `https://`. A private one clones over SSH with
  the fog's catalog key, shown on the settings page for you to add as a
  read-only deploy key. A path on the fog's own disk works too.
- **One app at a time.** An app that doesn't validate is left out, and the
  settings page lists it with the reason. The rest of the catalog still
  loads. Only an unreadable `catalog.yaml` drops the whole catalog.
- **Ids are first come, first served.** If two catalogs in a fog's list
  have the same id, the app comes from the one higher in the list, and
  the other is reported as shadowed.
- **Trust.** Apps from fog's own catalogs install directly. Apps from any
  other catalog are created **held for review**: the operator reads what
  the app will run (its image, folders, ports, host access) and approves
  it before anything deploys. Nothing in a catalog can make its own apps
  trusted.
- **Offline.** A fog release carries a copy of fog's own catalogs, read
  until the first fetch succeeds.

## Checking a catalog

Point a fog at it (a path on its disk is fine while you work) and open
Settings › App store: every problem is listed by app. fog's own catalogs
are checked by fog's tests. With checkouts beside fog's (or under
`$FOG_CATALOGS`), `go test ./internal/catalog` loads both and fails on
any problem.

## Versioning

`fog-catalog/1` is frozen by this page. New optional fields may be added
to `app.yaml` as fog grows: a fog that doesn't know a field refuses the
entries that use it and loads the rest. Anything that changes how a
reader must behave gets a new format.
