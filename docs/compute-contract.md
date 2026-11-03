# fog-compute/1 — the app-side compute contract

*Sibling of fog-backup/1 (docs/backup-contract.md), fog-files/1
(docs/files-contract.md) and fog-connect/1 (docs/connect-contract.md).*

An app that can hand video work to another machine declares this contract,
and fog offers it every pool of paired machines (Settings › Compute)
as one **transcoder that fog provides**. The app never learns which machine runs
what: it sees "fog · VideoToolbox", with that pool's combined room, and
hands it runs exactly as it would hand them to a remote transcoder of its
own. Machines joining, leaving or being swapped change only the room.

fog is the only caller of every endpoint below, authenticated by the app's
`FOG_API_TOKEN` like every other contract. Machines never reach the app's
control surface, and hold no credential of the app's: each run carries its
own signed URLs for its input and its output.

## Declaring

In the app repo's `fog.yaml`:

```yaml
compute:
  contract: fog-compute/1
  video:
    classes: [interactive, batch]   # what the app hands over
    blurb: Live transcodes and optimized copies run on fog's transcoders.
```

`interactive` is work a person is waiting on (a live transcode); `batch` is
work that can wait (an optimized copy, a film's segments). fog serves
interactive work first, and may stop batch work to make room for it. An app
that names no classes declares both.

## The endpoints

All under the app's `scope "fog"`.

### `POST /fog/compute/transcoders`

fog introduces a pool, the first time one exists that the app can use:

```json
{
  "pool": "video/videotoolbox",
  "name": "fog · VideoToolbox",
  "hello": {"encoder": "videotoolbox", "hostname": "fog", "platform": "video/videotoolbox",
            "ffmpeg": "9.0.2", "worker": "fog-node", "filters": ["scale_vt", "…"]}
}
```

`hello.encoder` is the hardware family every machine in the pool encodes
with, so the app writes command lines for it; `hello.filters` are the
ffmpeg filters every machine in the pool has. Answer `200` with the app's
own name for the transcoder:

```json
{"id": "8f0c…"}
```

Calling it again for the same pool answers the same transcoder (the app
keys it by `pool`). fog keeps the id.

### `DELETE /fog/compute/transcoders/{id}`

The operator took the pool away from this app (Settings › Compute).
The app forgets the transcoder; its runs are lost and the app does them itself.

### `POST /fog/compute/transcoders/{id}/poll`

fog, as the transcoder, a few times a second while the pool has a machine
online:

```json
{
  "hello":    {…},                         // when it changed, and on the first poll
  "running":  ["r_81"],
  "exited":   [{"id": "r_77", "status": 0, "log": "…"}],
  "progress": {"r_80": 412.5},
  "free":     {"interactive": 3, "batch": 1},
  "capacity": {"interactive": 4, "batch": 2}
}
```

`exited[].status` is ffmpeg's exit status, `null` when the run was stopped,
displaced by interactive work, or lost with its machine; `log` is the end
of what it said, or fog's reason. A run reported exited with status `0` has
uploaded everything it wrote. `free` is the room this app has now;
`capacity` all the pool would have idle. `running` is everything fog holds
for the app, so a run that was started and is in neither list was lost with
fogd: fog keeps its runs in memory, and a machine still running one after a
restart is told to stop. A pool with no machine online is not polled: the app
sees its transcoder go quiet and treats it as offline. An app that answers
`404` no longer knows the transcoder: fog counts its runs lost and introduces
the pool again.

Answer with the runs to start and to stop:

```json
{
  "start": [{
    "id": "r_82",
    "class": "interactive",
    "argv": ["ffmpeg", "…", "-i", "{server}/fog/compute/runs/<token>/source", "…", "{out}/gop-%d.m4s"],
    "outputs": {"init": ["init.mp4"], "stream": "gop-*.m4s"},
    "upload": "{server}/fog/compute/runs/<token>/files/{name}",
    "limits": {"seconds": 3600}
  }],
  "stop": ["r_80"],
  "poll_ms": 250
}
```

- `argv` starts with `ffmpeg`. `{server}` is the app's base on fog's data
  route, `{out}` the run's scratch directory on the machine.
- `outputs` names what to upload: `init` files first (once a `stream` file
  exists, or the run ended), each `stream` file as soon as ffmpeg writes it
  under its final name, `final` files after a clean exit. Each is PUT to
  `upload` with `{name}` filled. Names are files in `{out}` (`stream` a
  pattern of them, with no `/`); `upload` must start `{server}/fog/compute/`.
- `limits` is optional and can only lower the machine's own: `seconds` from
  start to exit, `scratchBytes` held in `{out}` at once (see
  [What a machine enforces](#what-a-machine-enforces)).
- `class` defaults from `kind` for an app that speaks the remote-transcoder
  protocol it had before: `session` is interactive with fragment outputs,
  `optimize` batch with a final `output.mp4`.
- A start fog can't place (the room went while it was in flight, it is past
  the app's share of batch room, its class wasn't declared, or its `upload`,
  `outputs` or `limits` break the rules above) comes back as exited with
  `status: null` on the next poll.

### Inputs and uploads

Every URL a run uses must be under `/fog/compute/` on the app, signed by the
app for that run and valid only while it is live. fog's data route
(`/compute/v1/data/<run token>/…` on fogd) forwards a machine's `GET`,
`HEAD` and `PUT` requests there and nowhere else, on clean paths only, so
the rest of the app is unreachable from a machine. The random token in the
path only says which run: fog takes a request only from the machine the
run is on, proven by its MAC over the request (see *Trust between machine
and fog*). The route ends with the run, however the run ends: a request
still streaming then is cut off. Inputs take range requests; uploads are a
PUT of the whole file.

On the machine, ffmpeg itself reaches only a proxy fog-node keeps for the
run on loopback. The proxy forwards reads under that run's data route to
fog and nothing else, and fog-node makes the uploads. The proxy serves only
the run's ffmpeg. Confined on Linux, it is in the run's own network, where
nothing else is. Elsewhere its port is on the machine's loopback, so ffmpeg
uses it as its HTTP proxy, from `http_proxy` in its environment (which only
its own user can read), with a secret made for the run. Anyone else is
refused with a `407`.

## Inference

The second kind of pool. An app that declares it:

```yaml
compute:
  contract: fog-compute/1
  inference:
    blurb: Summaries and suggestions use the household's models.
```

gets two variables at deploy: `FOG_INFERENCE_URL`
(`http://<fogd>/app/v1/inference/v1`, an OpenAI-compatible base) and
`FOG_INFERENCE_KEY` (its own `FOG_API_TOKEN`). Any OpenAI client works
against them: `GET models` lists every model a lending machine has, and a
request is routed to a machine with the `model` it names (404 when none
has it, 503 when no machine is lending; a request that names none goes to any
machine). Of the machines with the model, the one answering the fewest
requests goes first. A machine that fails before the first byte of its answer
has sent the app nothing, so the request goes once more to the next machine
with the model; the failure stays in the activity log. Responses stream
through as they arrive. Switching "fog · Inference" off for the app in
Settings › Compute is its revocation (403). The app never learns which
machine answered, but it can limit which ones may ([Locality](#locality)).
fog records each request — not its content — in Settings › Compute ›
Activity, and in Lucid as a model call; send `traceparent` to see it in
your own trace.

A machine lends the model server it runs, found by itself: think on
`127.0.0.1:8485`, else Ollama on `127.0.0.1:11434`. `fog-node models <url>`
names another, `fog-node models off` lends none, and `fog-node models on` goes
back to finding one; each takes effect when fog-node restarts. fog-node looks
again every five seconds, so a server that starts or stops later (think
starting after Ollama, say) changes what is lent within five seconds, and the
machine tells fog at once, cutting its held poll short. The listener and
the inventory fog routes by are one record: the listener forwards to the
server whose models fog was last told of, never another.

fog-node fronts it on port `7718` of the address the machine reaches fog
from, not on every interface it has (`inferenceListen` in its config names
another address or port). It takes only requests fog MACed with a key derived
from the machine's own credential, each nonce once (see below), and forwards
only `/v1/`. fog dials it at the address the machine's polls come from, on
the port the machine names: a machine can't point fog's requests anywhere
else. The requests and answers themselves cross the LAN in the clear, like
everything on plain http; someone who can read that traffic sees the
prompts, but can't send the model server a request of their own.

### Routing by an app

An app that knows the household's machines better than fog does can order
them per request. It declares, in its `fog.yaml`:

```yaml
inferenceRouter:
  contract: fog-inference-router/1
```

For every inference request that more than one machine could answer, fogd
calls the first app declaring it, with that app's own `FOG_API_TOKEN`:

```
POST /fog/inference/route
{"app": "fit", "model": "qwen", "stream": true,
 "machines": [{"id": "…", "name": "Studio", "models": ["qwen"], "inFlight": 0}, …]}
```

and the app answers `{"machines": ["Studio", "Mini"]}`, best first, by
name. Machines it leaves out keep fogd's order after the ones it names. It
has 50 milliseconds: past that, or on an error, fogd orders them itself. The
router only orders; it can't add a machine, and locality and the app's grant
are decided before it's asked. think-fleet is one: it puts first the Mac with
the model loaded and idle, then loaded and busy, then the one that would load
and answer soonest.

### Locality

A machine's inventory says where its model server is. fog-node reports
`local: true` in `inference` only when it reaches the server on a loopback
address (`127.0.0.0/8`, `::1`, `localhost`). That covers think and Ollama where
they're found by themselves, and a URL named with `fog-node models` if it is
loopback too. A URL naming anything else reports false, this machine's own LAN
address included. A fog-node from before this field reports nothing, which reads
as false, so update the machine before relying on it.

An app that must not leave those machines says so:

```yaml
compute:
  contract: fog-compute/1
  inference:
    locality: household
```

Its requests go only to machines reporting `local: true`, and `GET models`
lists only theirs. There is no fallback, and refusals are in the OpenAI error
shape, saying why:

| Answer | When |
|---|---|
| `503` | No machine lending right now runs its model server itself. |
| `403` | The model named is only on machines that don't. |
| `404` | No machine has the model. |

`household` is the only value. Any other is held to the same limit, with a
warning in the deploy log: an app that asked for a limit gets one, even if it
misspelled it.

`local` is a statement about one hop, made by the machine. It says fog-node
dials its own loopback, and it can't say what happens next. Any model server on
`127.0.0.1` can itself be a proxy to a cloud, so **which `models` an operator
lends stays a trust decision**. A machine reporting `local: true` is one whose
owner fog already trusts with the prompts, and fog can't check further than
that.

Pinning model names is the app's job. An app that wants a particular local
model should name it, and treat a model it didn't expect as a failure: fog
routes by the name it is given, and a machine can list anything.

## Machines

**Pairing.** *Add a machine* in Settings › Compute makes a one-time
code, good for 30 minutes, and the command to run on the machine:

```sh
curl -fsSL <fog>/compute/v1/install.sh | FOG_PAIR_CODE=ABCD-EFGH sh -s -- <fog>
```

The code is never on a command line, where any user of the machine could read
it (`ps`, `/proc/<pid>/cmdline`) and pair a machine of theirs first. It goes
in the environment: the script takes it from `FOG_PAIR_CODE` (or asks on the
terminal), refuses one given as an argument, and hands it to `fog-node join`
the same way. `fog-node join` reads it from `FOG_PAIR_CODE` or stdin, and
refuses `--code`.

The script fetches fog-node from that fog (darwin or linux, amd64 or arm64)
and checks it against fog's digest, looks for ffmpeg (on a Mac it installs one
with Homebrew if it can), pairs with the code, and installs the service: a
LaunchAgent in the owner's session on a Mac, which is where VideoToolbox and
the GPU are, and a systemd unit on Linux. At pairing the machine makes
its own `fognode_…` credential, of which fog only ever learns the digest. The machine then
polls `POST /compute/v1/poll`, which fog holds open for up to eight seconds, so
the only thing listening on it is the inference listener.

A pairing code is 8 characters from a 32-letter alphabet (40 bits), good for
one machine; fog takes ten pairing attempts per address every 15 minutes.

**Trust between machine and fog.** Deployments are plain http on a LAN anyone
may be on, so neither side takes the other's word for anything, and no
secret crosses the wire in the clear:

- *Pairing.* The code is never sent. Each code has a random salt of its own,
  public. The machine asks for the live codes' salts (`POST
  /compute/v1/join` with no attempts) and, since it can't tell which is its
  code's, makes an attempt under each (at most 16). Each attempt stretches the
  code with scrypt under that salt into a key (fog keeps only that key),
  proves the code with an HMAC over a nonce of the machine's, and carries the
  digest of a credential the machine made itself, sealed with AES-GCM under a
  key derived from the code's key and the nonce. fog answers with the
  machine's id and its ed25519 instance key, with an HMAC over those, the
  nonce and the sealed credential. The machine pins the key. Someone in
  between sees no code to replay and no credential, and can't forge fog's
  answer without guessing the code, each guess slowed by the stretch and
  good for that one salt.
- *Rotation.* The machine's first poll after pairing rotates its credential:
  each side brings an X25519 key made for it (`rotate` in the poll,
  `rotated` in fog's signed answer), and the new digest comes from their
  shared secret. A recording of the pairing, even with the code cracked
  later, yields only the pairing's digest, which fog stops taking once it
  hears the machine with the new one. If the answer is lost, the machine asks
  again from the old digest, which fog still takes until then.
- *Polls.* The machine MACs each poll with the credential's digest over its
  id, a timestamp, a fresh nonce, and its body's type and digest (`Fog-Node`,
  `Fog-Node-Time`, `Fog-Node-Nonce`, `Fog-Body-SHA256`, `Fog-Node-Auth`).
  fog checks all of it from the headers before reading a byte of the body,
  then checks the body against the declared digest. There is no other way in:
  no bearer is taken. fog signs every answer with its instance key, bound to
  the machine and that nonce (`Fog-Compute-Signature`). The machine reads
  nothing that isn't signed so, whatever its status. Of what is signed it
  acts on a `200`, and on a `401` whose code is `unknown_machine` (fog has no
  such machine: never paired, or removed), which unpairs it. Any other
  signed answer is a failed poll, tried again. That includes the `403` fog
  gives a request it couldn't authenticate:
  - `unauthenticated`: a bad MAC (tampered with, or not this machine's
    credential), or a body that isn't the one declared;
  - `replayed`: a nonce fog took before within five minutes, or a request
    stamped before this fogd started, which the fogd before it may have
    taken;
  - `clock`: the request's time is more than five minutes from fog's.

  So nobody in between can get fog to unpair a machine. A machine refused
  for its clock shows in Settings › Compute as not polling, with how far off
  its clock is. A machine whose clock is behind fog's is refused for that
  long after each fogd restart.
- *A run's data.* A run's data token only names the run. Every request on the
  data route is MACed by the machine the run is on (`DataMAC`, over its id,
  time, nonce, method and target), checked like a poll's, and refused from
  anyone else. ffmpeg holds neither the token nor the machine's credential:
  its URLs name the run's data proxy and the app's path only
  (`http://127.0.0.1:<port>/fog/compute/…`), and the proxy adds the run's
  route and the MAC. The token is in no process's arguments. The only
  secret ffmpeg holds is the proxy's, in its environment (see *Inputs and
  uploads*). An upload declares its body's digest (`Fog-Body-SHA256`) under
  the MAC. fog checks the bytes as they stream to the app and holds back the
  read that would complete the upload until they match, so a swapped body
  never reaches the app whole. Inputs (the app's answers) aren't covered:
  they're ranges ffmpeg reads as they arrive, and a check at the end would
  come after it had used them. On plain http they are open to change in
  transit like any reply.
- *Inference.* fog MACs each request it relays to a machine's model server,
  under a key derived from the credential's digest, over its method, target,
  time, nonce, and body type and digest (`Fog-Inference-Time`,
  `Fog-Inference-Nonce`, `Fog-Body-SHA256`, `Fog-Inference-Auth`). The
  listener checks the headers, takes each nonce once within five minutes
  and none stamped before it started, and only then reads the body, which
  must match. No key is sent. Answers aren't MACed: they stream token by
  token, fog couldn't check what a model says anyway, and on plain http they
  are as open to change in transit as any reply. Of other headers only
  `Content-Type` changes what a request means, and it is covered.
- *Replays across restarts.* Neither side keeps the nonces it has seen. Each
  instead refuses anything stamped before it started, which bounds a replay
  to the process that saw the original.
- A machine paired before fog-node pinned fog's key lends nothing, and its
  log says to remove it in Settings › Compute and pair it again.

A machine unheard for 15 seconds is offline: its slots leave the pool and its
runs are reported lost. *Remove* ends its credential at once, with the same
result for its runs (their data routes closing with them). A poll the machine
holds open is answered `removed` there and then, and any later poll is a
`401`; either way the machine stops its runs, closes its inference listener,
forgets the pairing, and stays idle until it is paired again
(`fog-node join`). fog offers a machine on another release the matching
fog-node, with the release's `latest.json` and its signature as fog's update
source served them. The machine fetches the build from fog's own address only
(the build is public; its credential isn't sent). It installs the build only
if the manifest verifies against the release key compiled into fog-node
(`internal/selfupdate`), is the version offered, names the digest of the
bytes fetched, and is strictly newer than the fog-node running (by
self-update's version order). An older release, however validly signed, is
refused, and the machine tells fog why; the same version is skipped. fog offers nothing it has no signed manifest for: a
development build, or a host running unsigned releases, updates its machines
by hand. A machine stuck behind, or paired but never connected, is an Inbox
issue (`compute-out-of-date`, `compute-never-connected`).

**Pools.** A pool is the machines that can run the same command lines, keyed by
hardware family:

| Pool | fog-node must find |
|---|---|
| `video/videotoolbox` | `h264_videotoolbox`, `scale_vt` |
| `video/vaapi` | `h264_vaapi`, `scale_vaapi` |

fog-node probes by putting one real frame through the hardware encoder; a
machine that fails joins no video pool, and the page says why. A machine that
lends a model server is in `inference` as well, whatever its video. Every app
that declares a kind of work gets every pool of that kind, unless it's switched
off for it, so swapping a machine only moves the room. Swapping across families
is one transcoder going offline and another appearing.

**Budgets.** A machine's slots are how many runs it takes at once
(`interactive`) and how many of those may be batch: 4 and 2 on VideoToolbox, 3
and 1 on VAAPI, unless the operator sets a budget (0 to 64 of each) on the
page. A pool's room is the sum of its machines'. "Lend it" off lends nothing,
inference included. `fog-node busy on`, or memory pressure on a Mac, takes only
the batch slots away. Batch room is split evenly among the apps that declare
batch, rounded up, so one long export can't starve another app.

**Placement.** fogd is the only placer, and it holds no queue: it tells an app
only the room there is, and a start with none comes back exited. A run goes to
the pool's online machine with room in its class that is least loaded (runs
against slots). An interactive run with no free slot displaces the newest batch
run on a machine that has one, and that run is reported exited with
`status: null` for its app to make again.

**Activity.** Every video run and every inference request but the model list is
a row in Settings › Compute › Activity (`GET /api/compute/activity`),
kept 30 days: app, pool, machine, class or model, and how it ended (`ok`,
`failed`, `refused`, `lost`, `displaced`, `stopped`), with tokens and time to
first byte for inference. Tokens are the server's `usage`, or for a stream
without one a count of its chunks, marked estimated.

## What a machine enforces

The machine is someone's workstation, and its command lines come from fog
on behalf of apps, so fog-node trusts neither: it assumes fog or an app may
be hostile. A command line that breaks these rules comes back as an exit
with `status: null` and the reason.

**The command line.** A first line, not the boundary:

- `argv[0]` must be `ffmpeg`, and it is swapped for the machine's own: the one
  named when pairing (`--ffmpeg`), else the first on `PATH` or where Homebrew
  puts it.
- Every `-i`, URL and absolute path must be under the run's `{server}` or
  `{out}` (or be a GPU render node, `/dev/dri/renderD*`), and nothing may
  climb out with `..`. No protocol but the run's own `http` URLs, `file:`
  paths in `{out}` and `pipe:` (`concat:`, `subfile:`, `unix:`, `tcp:`,
  `tee:`, `data:`, `fd:`, … are refused).
- `-f lavfi`, `tee` and capture devices are refused, and `-f concat` unless
  its list is one of the run's (with `-safe` left on). So are options that
  read or write files of their own (`-filter_script`, `-attach`,
  `-dump_attachment`, `-report`, presets, stats files, any `-/option`, …,
  whatever stream specifier follows them), `-listen`, filters that open a
  file, a socket or a plugin (`movie`, `amovie`, `subtitles`, `lut3d`,
  `frei0r`, `ladspa`, `arnndn`, `textfile=`, `fontfile=`, `stats_file=`, …),
  and any `-protocol_whitelist` beyond `http`, `https`, `tcp`, `tls` and
  `file`.
- A run's id, data route, upload URL and outputs are checked too, before
  anything is made for it.

**Confinement.** The boundary. Each run's ffmpeg starts through fog-node
itself, which confines it and then becomes it:

- It writes only its run's scratch directory (`{out}`), and reads only the
  system's libraries and configuration and its own install. The owner's
  home, fog-node's own directory and its credential are out of reach, and so
  is every other program: ffmpeg is all it may run.
- Its only network is the run's data proxy (above).
- It can see no other process: not list one, nor read its command line,
  environment or status.
- It runs with an environment of its own (nothing of fog-node's), no core
  dumps, and no file larger than its scratch limit.
- What it wrote fog-node reads only inside `{out}` and only as plain files,
  so a link it made can't lead fog-node to upload anything else.

On a Mac that is `sandbox-exec`, with a profile that denies everything, then
allows Apple's own baseline (`system.sb`: system libraries and a few system
services; no Apple Events, pasteboard or launchd jobs), reading ffmpeg's own
install, the media engine (IOSurface), `{out}`, and one network address: the
data proxy's loopback port. It may not read the `kern.proc` and
`kern.procargs` sysctls, nor get information about any process but itself.
On Linux (amd64, arm64) it needs no root and no package:

- its own network namespace, holding only a loopback where the data proxy
  listens;
- its own PID, IPC, hostname and mount namespaces (inside a user namespace
  when fog-node isn't root), with a fresh `/proc` for its own PID namespace,
  so its processes are the only ones there;
- Landlock for files (Linux 5.13 or later; on 6.7 or later it also limits
  connections to the proxy's port). Of `/proc` it reads only `self`,
  `cpuinfo`, `meminfo` and `stat`; of `/sys`, only `devices`, `class/drm`,
  `dev/char` and `bus/pci`, which is CPU topology and how VA-API finds the
  GPU;
- seccomp refusing `ptrace`, mounts, namespaces, keyrings, BPF, `io_uring`,
  kernel modules, and sockets other than IP and netlink (a filesystem socket
  like Docker's is out of reach). It also refuses every change to a file
  that Landlock doesn't rule on: mode (`chmod` and kin), owner (`chown`),
  extended attributes, times (`utimensat`, `utimes`), flags (`chattr`'s
  ioctls), and `truncate` by path. Truncating a file it has open for
  writing, which can only be its own, stays allowed. Links, renames and
  device nodes are Landlock's: they need rights it grants only in `{out}`.

fog-node run as root runs ffmpeg as `nobody`, with the groups that
own the GPU's render nodes: it starts its helper as root in the run's
namespaces and drops to `nobody` itself, just before it becomes ffmpeg.
Ubuntu's restriction on unprivileged user namespaces
(`kernel.apparmor_restrict_unprivileged_userns=1`, its default) keeps
fog-node run as a user from confining; the refusal says to install it as
root, or to set that to 0. fog-node ships no AppArmor profile to grant
itself user namespaces: a user's fog-node binary is the user's to replace,
so such a profile would grant them to anything that user runs.

fog-node proves its confinement when it starts and every ten minutes: one
confined ffmpeg must run, and one must fail to read a file outside its run
that an unconfined one reads. The inventory reports the result
(`video.confinement`: `sandbox-exec` or `landlock`, or `confinementError`).
**It fails closed:** a machine that can't confine refuses every run, and fog
puts it in no video pool, saying why in Settings › Compute; a fog-node too
old to report confinement is treated the same until it updates itself. The
one way past is its owner's, on the machine: `fog-node unconfined on` warns
loudly, the machine then lends ffmpeg unconfined (limits still hold), and
Settings › Compute marks it *Unconfined*. Inference runs no command line and
is lent either way.

**Limits.** Each run has a time limit, 12 hours by default, and a scratch
limit, 32 GiB held in `{out}` at once by default; a run's `limits` can lower
either. fog-node also ends a run when the disk `{out}` is on has less than
2 GiB free. A run past any of them is ended and reported with `status: null`
and the reason. The machine's own limits are in its inventory
(`video.limits`).

**ffmpeg's version.** fog neither ships nor pins one, so `hello.ffmpeg` is
the version of one machine in the pool, and a pool takes a machine for what
its ffmpeg can do (the probe), not its number. That is deliberate: distribution
builds carry security fixes under old numbers and builds from git carry none,
so a minimum version would refuse safe builds and admit unsafe ones. What makes
a hostile input or command line harmless is the confinement above.
