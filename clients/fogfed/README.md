# fogfed

The shared, zero-dependency implementation of Fog's federation identity and
signing wire — **fogfed1** — for Ruby apps that federate through fog (hub,
hook, feed). It is the one place the fog-to-fog wire is defined for Ruby;
fogd's Go `internal/fed` is its mirror.

## What it is

- **Identity** — one Ed25519 keypair and the instance id derived from it, the
  shape fogd mints. `Fogfed::Identity.generate` / `.from_seed`;
  `Fogfed.check_instance_id(id, public_key_b64)` refuses a derived id presented
  with any other key (a legacy ULID id can't be checked and passes).
- **Signing** — `fogfed1` per-request signatures over version, method, the
  full request target (path *and* query), timestamp, and body digest.
  Request timestamps carry nanoseconds and never repeat within a process, so
  a receiver can refuse any signature it has seen before. Answers sign with
  `sign_answer` (pseudo-method `ANSWER`) over the status and the request they
  answer — its `Fog-Instance` and `Fog-Signature` — then what they sign (in
  fogd's envelope, the answer's head). A captured answer verifies for no other
  exchange. The older `RESPONSE` pseudo-method signs the body alone: fogd
  neither sends nor accepts it, and a new wire should not use it. `Fogfed::Signature`.
  Verification is as strict as fogd's: an RFC 3339 timestamp within five
  minutes either way of now, an unpadded base64url signature, and an Ed25519
  key (`Fogfed.parse_public_key`). Any other kind of key raises
  `ArgumentError` rather than verifying under its own algorithm.
- **Sealing** — `Fogfed::Seal`: bodies encrypted as fogd seals them, HPKE
  (RFC 9180) base mode with DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and
  ChaCha20-Poly1305, framed in 64 KiB records whose order and end are
  authenticated. `request_info` / `answer_info` bind a body to its exchange;
  `derive_key_pair` makes an X25519 key pair from a 32-byte seed;
  `Identity#bind_encryption_key` and `Fogfed.verify_encryption_binding` sign and
  check the binding of an encryption key to an identity. The conformance vectors
  are opened by Go's standard-library HPKE, byte for byte.
- **Envelope** — `Fogfed::Envelope`: fogd's wire shape, every exchange a
  `POST /fed/v1/sealed` carrying the real request sealed and a `200` carrying
  the real answer sealed, status and headers inside. `encode_request` /
  `decode_request` frame the inner request; `encode_answer` / `decode_answer`
  make and check an answer's signed head and its body's digest.
- **Pairing** — the labelled transcript (`<app>fed1-pair`, `-handshake`)
  over a nonce and the sha256 of an instance document.
- **Consent grammar** — the words a share is read back in, the same in every
  app (fog:docs/consent-grammar.md). `Fogfed::Lens` holds the shared axes
  (mode, scope, provenance); `Fogfed::Impact.sentence` builds the impact
  sentence.

## Replay

A signed request verifies again for as long as its timestamp is within five
minutes of now, so anyone who captured one can send it twice. On every route
where a second copy would do something twice (a transfer, a post, a
delete), pass `seen:` to `Fogfed::Signature.verify!`:

```ruby
REPLAYS = Fogfed::Signature::ReplayCache.new          # one process
Fogfed::Signature.verify!(pub, method, target, ts, sig, body, seen: REPLAYS)
```

`seen:` is anything answering `call(key, expires_at)` with `true` when `key`
was accepted before, and recording it otherwise; a replay raises
`Fogfed::Signature::Replayed`. An app with several processes (Puma workers,
replicas) needs one shared store, for example
`->(key, exp) { !Rails.cache.write("fogfed:#{key}", 1, unless_exist: true, expires_in: exp - Time.now) }`.
It is asked only after the signature verifies, so forgeries can't fill it.
`ReplayCache` holds at most 100,000 live signatures, and when full it refuses
new requests rather than forget one that could still be replayed.

## Proving parity

`conformance/vectors.json` is a fixed set of known-answer vectors. Both this
gem (`rake test`) and fogd's Go (`go test ./internal/fed -run Conformance`)
verify against it, so a change that would drift the two wires fails a test in
whichever language drifted. Regenerate with `ruby conformance/generate.rb`.

## Rules

Zero runtime dependencies, ever — it rides vendored inside every app. Synced
into apps by `scripts/sync-fogfed` in the fog repo: with no arguments it
updates every sibling checkout that already vendors the gem, and an app dir
vendors it the first time. The vendored copy is exactly the gemspec's `files`
(the code, `conformance/vectors.json` and this README) plus the gemspec, and
anything else in it is removed. It refuses while the gem has uncommitted or untracked changes here; `--allow-dirty` overrides that. Never edit the vendored copy; edit here and
re-sync.
