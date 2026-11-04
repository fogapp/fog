# fog-store/1 — the incremental backup store

Fog keeps every backup — each app's, Drive's — as a **complete snapshot**
in one content-addressed store under the backups folder
(`<backups>/fog-store/`). What makes backups incremental is storage, not a
chain: a snapshot's bytes are cut into chunks by content, each chunk is
stored once, and a later snapshot that holds the same bytes refers to the
chunk already there. No snapshot depends on another snapshot. Deleting any
of them leaves the rest whole, and a restore reads exactly one.

This page is the bytes, so that anything — fogd, `fog-locker`, a script
written years from now — can read a store given the folder (and, for an
encrypted store, its key). Reference implementation: `internal/chunkstore`.

## 1. Layout

```
fog-store/
├── store.json                    §2 — format, id, chunker parameters, keys
├── packs/<aa>/<sha256>.pack      §4 — chunks, many per file
├── snapshots/<id>.json           §5 — one document per snapshot
├── gc.json                       §7 — packs found unreferenced, and since when
└── tmp/                          writers' scratch; anything here is not part
                                  of the store
```

The chunk index (§6) is a cache on fog's local disk, never in this folder.

## 2. `store.json`

```json
{
  "format": "fog-store/1",
  "id": "0123456789abcdef0123456789abcdef",
  "created_at": "2026-11-04T03:00:00Z",
  "params": { "min": 262144, "avg": 1048576, "max": 4194304 },
  "pack_size": 33554432,
  "encrypted": false,
  "secret": "<base64, 32 bytes — plain stores only>",
  "key_check": "<hex — encrypted stores only>"
}
```

Every key is derived from one 32-byte **secret** with HKDF-SHA256 (empty
salt, the `info` shown):

| Key | info | Used for |
|---|---|---|
| id key | `fog-store/1 chunk id` | chunk ids: `HMAC-SHA256(id key, plaintext)`; snapshot ids |
| gear seed | `fog-store/1 gear` | the chunker's table (§3) |
| data key | `fog-store/1 data` | XChaCha20-Poly1305 sealing (encrypted stores) |
| check key | `fog-store/1 key check` | `key_check = hex HMAC-SHA256(check key, id)` |

- **Plain store** (the default): the secret is in `store.json`. Whoever can
  read the folder can read the backups — exactly as with the files it
  replaces — and nothing more is needed to restore than the folder.
- **Encrypted store**: the secret is a key fog holds (wrapped by its vault,
  carried in the recovery kit); `store.json` holds only `key_check`, so a
  wrong key is refused before anything is read. Without the key the store
  cannot be read by anyone.

## 3. Chunking

FastCDC with normalized chunking over a 64-bit gear hash (`h = h<<1 +
gear[byte]`). The table is 256 big-endian u64s: entry `4k+j` is bytes
`8j…8j+8` of `HMAC-SHA256(gear seed, "fog-store/1 gear" ‖ u32be(4k))`. With
`bits = log2(avg)`: no cut before `min`; from `min` to `avg` a cut falls
after a byte where the hash's top `bits+2` bits are zero; from `avg` to `max`
where its top `bits−2` bits are; at `max` a cut is forced. The hash starts at
zero at `min`. A stream shorter than `min` is one chunk; an empty stream is
none.

The secret table matters: the 2025 chunking-attack papers (Alexeev, Percival
and Zhang; Truong et al.) recover public or weakly keyed parameters from
chunk sizes. Here sizes are also hidden inside packs (§4).

## 4. Packs

```
"FOGPACK1" | record … | header | u32be header length | "FOGPEND1"
```

- A **record** is one chunk as stored: a codec byte (`0` raw, `1` zstd) and
  the payload. In an encrypted store the record is `nonce(24) ‖
  XChaCha20-Poly1305(data key, codec ‖ payload, ad = chunk id)`. A chunk is
  stored raw when zstd does not make it smaller.
- The **header** is a u32be count, then per record: chunk id (32 bytes),
  offset (u64be, from the start of the file), stored length (u32be),
  plaintext length (u32be). In an encrypted store the header is sealed the
  same way with `ad = "fog-store/1 pack header"`, so a pack reveals neither
  chunk names nor sizes.
- Records are written in a **random order**: arrival order would say which
  chunks came from the same file.
- A pack's **name** is the lowercase hex SHA-256 of the whole file; it lives
  at `packs/<first two hex>/<name>.pack`. Checking a pack is hashing it.
- Every chunk read is checked by recomputing its id from the plaintext. A
  reader refuses a header entry that points outside the pack.

## 5. Snapshot documents

`snapshots/<id>.json`, where `id` is the first 16 bytes, in hex, of
`HMAC-SHA256(id key, "snapshot\0" ‖ document)` — a reader recomputes it and
refuses a document that does not match. Sealed with `ad = "fog-store/1
snapshot"` in an encrypted store.

```json
{
  "format": "fog-store/1",
  "series": "app/hub",
  "stamp": "20261104-030000",
  "created_at": "2026-11-04T03:00:12Z",
  "meta": { "...": "what fog records about the backup" },
  "tree": ["<chunk id>", "..."],
  "tree_bytes": 1234,
  "files": 2,
  "bytes": 48211934,
  "new": 1048576
}
```

`tree` is the chunks of the **entry list**: one JSON object per line, in
the order they were added.

```json
{"path":"artifact.tar","type":"file","mode":384,"mtime":0,"size":48211934,"sha256":"…","chunks":["…","…"]}
{"path":"photos","type":"dir","mode":493,"mtime":1787836800000000000}
{"path":"photos/latest","type":"symlink","mode":511,"target":"2026"}
```

A file's content is its chunks concatenated; `size` and `sha256` are
checked when it is read back. Paths are slash-separated, relative and clean
(no `..`, no leading `/`). `series` names what the snapshot belongs to:
`app/<fog app name>`, or `drive`.

**Writing rule: what a document refers to exists before the document
does.** Packs are written and fsynced, then indexed, then the document is
written (exclusively — two identical documents are one snapshot). A crash
leaves only packs nothing refers to, which collection removes.

## 6. The index

A SQLite file on fog's own disk mapping chunk id → pack, offset and lengths.
It is a cache of the pack headers: missing, unreadable, made for another
store, or naming a pack that is not on disk, it is rebuilt by reading every
header. A pack on disk that it does not know is read in when the store
opens.

## 7. Collection

The roots are the snapshot documents on disk — never fog's database, so a
database rolled back by a recovery kit cannot make newer snapshots look like
garbage. A collection:

1. waits for every writer to finish and keeps new ones out until it is done;
2. refuses to run if any document fails to read (its chunks cannot be told
   from garbage);
3. marks every chunk a document's tree and entries name;
4. for each pack older than the grace period (24 h) with no live chunk:
   records it in `gc.json` the first time; deletes it once a later
   collection, at least the grace period after the first, still finds it
   unreferenced — index rows first, then the file;
5. rewrites packs whose live share has fallen under half into new packs, and
   points the index at the copies; the old packs then go the way of step 4.

Readers need no lock: nothing a live snapshot uses is deleted, and a pack a
reader may have looked up before a repack survives at least two grace
periods.

## 8. Checking and healing

- **Scrub**: a slice of the packs each night, least recently checked first,
  is read end to end — the file hashed against its name, every chunk decoded
  and its id recomputed — so the whole store is re-read on a rolling
  schedule.
- **Heal**: a damaged chunk is replaced from another copy (the offsite
  locker holds the same chunk ids), checked against its id before it is
  stored. The damaged pack is collected once nothing points into it.

## Versioning

`fog-store/1` is frozen by this document. Additive document fields are
allowed (readers ignore unknown keys). Anything that changes how a reader
must behave bumps the format.
