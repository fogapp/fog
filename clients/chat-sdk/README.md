# @fog/chat-sdk

The client half of fog's chat (docs/chat-contract.md). Everything that touches
a key lives here: the person's identity key, the device key, MLS group state
(via ts-mls), message encryption and decryption, attachment encryption, and
the local plaintext store. The server only ever sees what `src/api.ts` sends.

Source-distributed: the fog frontend bundles it through a Vite alias, and a
standalone app can do the same. Node 22.18+ runs the tests with type stripping
(`make sdk-test`).

```ts
import { ChatClient, IndexedDBStorage } from "@fog/chat-sdk";

const client = new ChatClient({ storage: await IndexedDBStorage.open("fog-chat") });
await client.init();      // identity + device bootstrap against /api/chat/me
await client.connect();   // sync rooms, open the stream
client.addEventListener("messages", () => render(client.rooms()));
await client.send(roomId, "hello");
```

## Requirements

A secure context. The SDK (and ts-mls under it) uses WebCrypto's `subtle`,
which browsers expose only on `https://` origins and on `localhost`. Opened
over plain http from another host, `init()` rejects with
`CryptoUnavailableError` (`code: "crypto_unavailable"`) before touching the
network; `webCryptoAvailable()` lets an app check first. On fog, the https
address comes from Settings › Network › Domain & HTTPS (`https://fog.<domain>`).

## 1.0: what is stable

`src/index.ts` is the public surface. Everything it exports keeps its shape
across 1.x; anything reachable only through `src/*.ts` paths may change.

| Area | Exports |
| --- | --- |
| Client | `ChatClient` and its events (`rooms`, `messages`, `presence`, `typing`, `status`, `me`, `error`, `identity`), `Room`, `MessageView`, `Me` |
| Rooms and messages | `createRoom`, `addMembers`, `removeMember`, `leave`, `joinChannel`, `updateRoom`, `can`, `send`, `edit`, `delete`, `react`, `pin`, `markRead`, `readMarks`, `typing`, `messages`, `search`, `exportRoom`, `save`/`unsave`/`saved` |
| Attachments and previews | `uploadAttachment`, `openAttachment`, `preview`, `Attachment`, `PreviewCard` |
| People and devices | `directory`, `households`, `markVerified`/`unmarkVerified`/`isVerified`, `knownIdentity`, `leafAttested`, `leafTrust`, `identityWarnings`, `attestOtherDevice`, `syncSiblingDevices`, `resync`/`resyncAll`, `mlsMembers` |
| Recovery | `enableBackup`, `disableBackup`, `backupNow`, `ChatClient.restoreFromPhrase`, `generatePhrase`, `validPhrase`, `normalizePhrase` |
| Preferences | `prefs`, `setPrefs`, `cachedPrefs`, `Prefs`, and `client.api` push, webhook and session helpers |
| Storage | `IndexedDBStorage`, `MemoryStorage`, the `Storage` interface |
| Content | `Content`, `encodeContent`, `decodeContent`, `HistoryItem` |
| Rendering safely | `inlineImageType`, `safeHref`, `DOWNLOAD_TYPE` |
| Low level | `Api`, `ApiError`, `Mls`, `CIPHERSUITE`, `RefusedMessage`, `CommitRules`, `encodeCbor`/`decodeCbor`, `keys` helpers — stable, but a client rarely needs them |

The HTTP contract the SDK speaks is written down in
[`docs/chat-openapi.yaml`](../../docs/chat-openapi.yaml).

### Wire conventions the server enforces

- KeyPackages travel raw; commits, private messages, Welcome and GroupInfo
  travel MLSMessage-framed. Commits are PublicMessages; standalone proposals
  are refused (inline them in the commit).
- Ciphersuite 1, `MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519`. The leaf
  signature key is the registered device key; the BasicCredential identity is
  the person's address `fog:<instance>/<handle>`.
- Every commit is followed by `PUT /rooms/{id}/groupinfo` so others can join
  externally. ts-mls omits RFC 9420's length prefix on `external_pub`; the SDK
  assembles RFC-correct GroupInfo itself and shims the read side.
- Content is a CBOR map (`src/content.ts`; the Go mirror is
  `internal/chat/content.go`). Unknown keys are ignored, so a client may add
  fields under its own prefix.
- A device session token (`api.bearer()`) is what a native client persists to
  come back as the same device. The WebSocket accepts it as `?token=`.

### Native MLS

A native client does not need `mls-rs` behind a binding: ts-mls interoperates
with the server's `mls-go` in both directions and runs in a browser, Node,
React Native (with WebCrypto) and Tauri. A native
client that prefers `mls-rs` must keep the conventions above; the conformance
suite is how to prove it.

## Conformance

`test/conformance.mjs` runs against any fogd — a scratch one, please: it
invites two throwaway people, removes them at the end, and (optionally)
spends a person's whole request budget.

```sh
FOG_URL=http://fog.local:8080 FOG_OPERATOR_USER=admin FOG_OPERATOR_PASSWORD=… \
  node --experimental-strip-types --no-warnings test/conformance.mjs
# or: make conformance FOG_URL=… FOG_OPERATOR_USER=… FOG_OPERATOR_PASSWORD=…
# FOG_OPERATOR_COOKIE=fog_session=… replaces the user/password pair.
# FOG_CONFORMANCE_RATELIMIT=1 adds the per-person budget check.
```

It prints one JSON line and exits 1 on any failure. The checks:

| Check | Proves |
| --- | --- |
| `people-join`, `invite-single-use` | invites work once; a joined person reaches `/api/chat/me` |
| `device-bootstrap`, `device-session-token` | address format, device registration, identity seed, a bearer for the device |
| `keypackage-single-use` | one KeyPackage per device is handed out per fetch |
| `deviceless-refused`, `deviceless-cannot-post` | a session without a device gets 428 `need_device` |
| `sessions-listed` | the person sees their sessions, device named |
| `dm-decrypts` | a DM created by one device is readable by the other |
| `server-holds-ciphertext`, `blob-is-ciphertext` | no event payload, Welcome or blob on the server contains the plaintext |
| `operator-cannot-read`, `operator-does-not-see-room` | an admin who is not a participant gets nothing |
| `stale-commit-refused` | replaying an old commit is 409 `epoch_stale` |
| `cursors` | `after`/`before`/`limit` paging, clamping |
| `stream-replay` | a fresh WebSocket with cursors replays only what was missed, then `synced` |
| `rate-limit` (optional) | the per-person budget answers 429 with `Retry-After` |

The same script runs inside `go test ./internal/server` (`TestChatConformanceSuite`),
so a fogd that passes its own tests passes conformance.
