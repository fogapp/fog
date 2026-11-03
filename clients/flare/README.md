# flare

Error reporting to Fog's built-in tracker. Zero runtime dependencies; the
whole client is a `Rails.error` subscriber, a Sentry-schema event builder,
and one background thread with a bounded queue.

## How apps get it

Vendored, not fetched — Docker builds need zero credentials and zero network:

```ruby
# Gemfile
gem "flare", path: "vendor/gems/flare"
```

List it **after** `gem "rails"`: the railtie only loads if Rails is already
defined when flare is required. With the order reversed, flare repairs
itself on the first `Flare.configure` or `Flare.notify` call after Rails
has loaded — but errors raised before that (boot, initializers) go
unreported, so get the order right.

Copies are synced from this canonical source by `scripts/sync-flare` in the
fog repo. Edit here, commit, sync, commit in each app. With no arguments it updates
every sibling checkout that already vendors the gem; an app dir vendors it
the first time. The vendored copy is exactly the gemspec's `files` plus the
gemspec, and anything else in it is removed. It refuses while the gem has uncommitted or untracked changes here; `--allow-dirty` overrides that.

## How it works

- Fog injects `FLARE_DSN` into every managed app's container. No DSN
  (development, test, running outside Fog) → the gem is completely inert.
- On Rails 7.1+ the railtie's `Rails.error.subscribe` covers unhandled
  errors in HTTP requests, ActiveJob jobs, and runners — no middleware.
- Rescued errors report via `Rails.error.report(e, handled: true)` (the
  Activity bridge in each app) or `Flare.notify(e, context: {...})`.
- Delivery: bounded queue (100), drop on overflow, drop on connection
  failure, pause on 429 Retry-After, 2s flush at exit. The reporter can
  never hurt the app it reports for.
- Params ride through `request.filtered_parameters` (the app's own
  filter_parameters) plus a built-in key scrub (password/token/secret/…).
  Keys are matched by Rails' own rules (`ActiveSupport::ParameterFilter`): a
  string or symbol anywhere in the key, case-insensitively; a regexp against
  the key; and either, when it holds a dot (`"credit_card.number"`), against
  the dotted path to the key.
- The request URL is Rails' `filtered_path` where the request has one, and its
  query then gets the same scrub, with keys read as Rack nests them
  (`credit_card[number]`). A filtered key keeps its name and its value becomes
  `[FILTERED]`; a query that won't decode is replaced whole.
- Context passed to `notify` (or Rails' reporter) rides under `extra`,
  scrubbed the same way. Only a hash, `ActionController::Parameters` (through
  `to_unsafe_h`), a `Struct`, a `Data` or an `OpenStruct` is expanded, and
  arrays are walked. Every other value that isn't a string, symbol, number,
  boolean or nil is sent as its class name alone (`#<Money>`), never its `to_s`, `inspect` or `to_h`. `ENV` reads
  `#<Object>`.
- Credentials are redacted from every exception's message (causes too),
  from the source lines sent with in-app frames, and from every string value
  in params and context, whatever key it sits under. Each becomes
  `[FILTERED]`:
  - a URL's userinfo, everything from `://` to the last `@` before the next
    space, quote or scheme, so `redis://:p@ss@cache` and
    `postgres://u:p/ss@db/x` go whole, and a DSN's key with them. An `@` in a
    URL's path reads the same way: `https://site/@ann` becomes
    `https://[FILTERED]@ann`.
  - an HTTP credential: `Bearer …`, `Basic …`, `Token …`, `Digest …`.
  - the value given to a sensitive key in text: `password=…` (a query, a
    keyword DSN), `"password":"…"` (JSON, its quotes kept),
    `X-Api-Key: …` (a header), and Ruby's own inspect output
    (`{"password" => "…"}`, `{password: "…"}`, `{:password=>"…"}`,
    `@password=#<Secret …>`). The value goes whole: a quoted one keeps its
    quotes, and a balanced `{…}`, `[…]` or `#<…>` goes with everything inside
    it (`auth: {user: …, pass: …}`). One left unclosed goes to the end of its
    line. A key counts when it contains `password`, `passwd`,
    `pwd`, `secret`, `token`, `api_key`, `access_key`, `private_key`, `auth`,
    `credential`, `cookie` or `session`, with `-` and `_` read alike or left
    out (`api-key`, `X-Api-Key`, `apikey`). So `author=` is redacted too.
    The built-in key scrub for params and context uses the same list.
  - a `Cookie:` or `Set-Cookie:` header's whole value, every cookie in it, to
    the end of its line.

  Everything is redacted whole before it is cut: a message before its
  2,000-character cut, flare's own failure log line before its 500. So a
  credential running past a cut doesn't leave its first half. A string the
  patterns can't finish within their time limit is dropped whole.

## Changing what's sent

`before_send` gets every event once it is built and scrubbed, and what it
returns is what goes out. Return nil to drop the event.

```ruby
Flare.configure do |c|
  c.before_send = lambda do |event|
    next nil if event[:exception][:values].last[:type] == "ActiveRecord::RecordNotFound"
    event[:extra]&.delete(:meal)
    event
  end
end
```

It runs on the thread that hit the error, so keep it quick. It can't raise
into the app: a hook that raises, or returns anything but a hash or nil, loses that
event and logs the failure's class. The event is dropped rather than sent
unhooked, because the hook may be what takes the private part out.

## Reports with no content

```ruby
Flare.configure { |c| c.message_policy = :class_only }
```

Every exception's `value` is sent empty, causes included, for an app whose
errors could quote what someone wrote. An exception whose message is known to
be safe says so:

```ruby
class Fit::RateLimited < StandardError
  def flare_safe_message? = true
end
```

Only `true` counts, and a `flare_safe_message?` that raises counts as no.

Blanking the message isn't enough, because code copies it: into `context:`,
into a parameter, into a breadcrumb. So this mode decides what leaves, not what
doesn't. A report holds only the event's id, time, level, platform, host,
environment and release, the action or job name, the HTTP method, a job's
class, queue and id, and each exception's class, mechanism and stack frames.
Frames keep their file, line and function, and lose their source lines, where
the literal in `raise "..."` would sit. So context, request parameters, the URL
(path and query both), fingerprints, breadcrumbs, tags and users don't leave.
Each of these is checked for shape, since code can put words in any of
them. A frame's function is kept only when it reads as a Ruby method label
(`Foo::Bar#baz`, `block in Foo.qux`, `<main>`). Its file is kept only when it
is Ruby's own (`<internal:kernel>`) or an existing file under the app root, a
gem directory or Ruby's library, with a plain path below that root (no
spaces, no `..`). Anything else, such as the name
`eval(code, binding, "Dear Bob…")` gives its frames, reads `(eval)`. The
action or job name must be a constant or `Controller#action`, a job's class a
constant, its queue and id one plain identifier, and the HTTP method
upper-case letters. Each one that fails is dropped.

`before_send` runs after this, and what it returns is held to what the
library built: every value that leaves is one flare itself derived, never one
the hook wrote. A field on the list survives only if it still equals the
value flare put there, and flare sends its own copy. So a hook can drop the
event, drop fields, drop or reorder exceptions and frames, and set `level` to
one of `fatal`, `error`, `warning`, `info` or `debug`. Free text written into
any field (the host, a frame's function, an exception's class, a job's
queue) drops that field instead of leaving; a mechanism falls back to
`generic`. An exception's message is empty or one of the safe messages flare
built for this event, and nothing off the list comes back.

Errors group by class and stack, so losing the message costs little in fog's
tracker. A mistyped policy fails shut: anything unrecognized is `:class_only`.

## The wire format

The wire format is Sentry's envelope + event schema, so pointing the
official `sentry-ruby` SDK at the same DSN is always a drop-in alternative.

## The DSN and the ingest route

Fog mints each app its own DSN at deploy:

```
FLARE_DSN=http://<errors_key>@host.docker.internal:<fogd-port>/ingest/<app_id>
```

The app's compose file points `host.docker.internal` at the docker gateway, so
the hop goes straight to fogd. Sentry's DSN rules turn this into
`POST /ingest/api/<app_id>/envelope/`, outside `/api`, so no session applies.

The key authorizes one thing, reporting errors for that app. It is separate
from `FOG_API_TOKEN`. A client may send it in `X-Sentry-Auth`
(`sentry_key=…`), as `?sentry_key=`, or in the envelope header's `dsn`, and
fog looks in that order.

A body may be gzipped (`Content-Encoding: gzip`), and is capped at 1 MiB on
the wire and 8 MiB unpacked, with up to 100 items of up to 1 MiB each. Only
`event` items are stored; sessions, transactions, client reports and the like
are dropped silently.

| Answer | Means |
| --- | --- |
| `200 {"id": "<event_id>"}` | Taken. The id is the envelope's last event, and empty if it held none. An event that won't parse is logged and skipped, not refused |
| `400` | Bad gzip or a bad envelope |
| `403` | Wrong key. Official SDKs stop sending; flare loses that event and carries on |
| `404` | No such app |
| `429` | Over the rate limit, with `Retry-After` |

## Grouping

The first of these that applies decides an event's group:

1. **Explicit.** The event's `fingerprint` array, from
   `Flare.notify(e, fingerprint: ["billing", "{{ default }}"])`.
   `"{{ default }}"` stands for the computed grouping, so a fingerprint can
   split a group without losing the stack half. Reports through `Rails.error`
   don't carry one.
2. **Stack.** The raised exception's class, plus the file and function of each
   in-app frame. Never line numbers, so a deploy doesn't split a group. With no
   in-app frames, the innermost 8 frames stand in. For flare, in-app means
   under the app root and outside `vendor/` and `.bundle/`.
3. **Message.** With no stack, the class plus the message with quoted strings,
   long hex ids and numbers templated out, so `timeout after 30s` and
   `timeout after 45s` are one group.

A group's list line is `Class: message`, cut at 300 characters, and its
culprit: the transaction (`PostsController#show`, a job's class), or else the
innermost in-app frame. Both follow the latest occurrence.

## Back-pressure and retention

- **Rate limit:** 120 envelopes a minute per app, in a fixed window, and flare
  sends one event per envelope. Over that, fog answers `429` with
  `Retry-After: 60` and the envelope is lost, uncounted. Flare pauses for what
  the header says, at least 1s and at most 300s, while its queue keeps
  filling.
- **Retries:** an `event_id` its group already holds changes nothing, so a
  retry neither counts nor reopens.
- **Counts:** every other accepted occurrence counts, whatever the group's
  status and whatever the storage caps below drop.
- **Payloads:** the newest 50 per group, at most 256 KiB each. A bigger event
  loses its `request` and whatever fog doesn't parse (`extra`, breadcrumbs),
  and one still too big is stored as a stub.
- **Retention:** once a day, payloads older than 90 days go, along with
  resolved and ignored groups not seen for a year. Open and regressed groups
  stay.

## Notifications

Flare files no notices: a group is already a row on the Inbox's Errors tab,
and the tab's badge counts them. The interruption is an ntfy push, when the
operator has set ntfy up, on three moments only: a new group, a regression, and
an ignore running out. It is never per occurrence, and it is held to one push
per group per hour and 10 per app per hour. Ignored groups stay quiet.

A resolved group regresses on its next event. Resolved with "Resolve in next
deploy", it regresses only on a release other than the commit deployed then,
which is why events need a real `release`. Flare sends `GIT_SHA`, else
`GIT_REV`, else the `REVISION` file in the app root; an event with no release
always regresses.

## Forwarding

Fog-to-fog forwarding is covered by
[`fog:docs/federation-contract.md`](../../docs/federation-contract.md).

## Tests

```
cd clients/flare && rake
```
