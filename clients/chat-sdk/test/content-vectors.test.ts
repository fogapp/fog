// The content vectors internal/chat's TestContentVectors checks too: the
// Go bot and this SDK must read and write content byte for byte alike.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { decodeContent, encodeContent } from "../src/content.ts";

interface Vector {
  name: string;
  content: unknown;
  cbor: string;
}

const { cases } = JSON.parse(readFileSync(new URL("./vectors/content.json", import.meta.url), "utf8")) as { cases: Vector[] };

const fromHex = (h: string) => Uint8Array.from(h.match(/../g) ?? [], (b) => parseInt(b, 16));

// {"$hex": …} becomes bytes, as on the wire.
function expected(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(expected);
  if (v && typeof v === "object") {
    const o = v as Record<string, unknown>;
    if (typeof o.$hex === "string" && Object.keys(o).length === 1) return fromHex(o.$hex);
    return Object.fromEntries(Object.entries(o).map(([k, x]) => [k, expected(x)]));
  }
  return v;
}

// decodeContent spells absent fields as undefined; the wire has no such thing.
function defined(v: unknown): unknown {
  if (v instanceof Uint8Array) return v;
  if (Array.isArray(v)) return v.map(defined);
  if (v && typeof v === "object") {
    return Object.fromEntries(Object.entries(v).filter(([, x]) => x !== undefined).map(([k, x]) => [k, defined(x)]));
  }
  return v;
}

test("there are vectors to check", () => {
  assert.ok(cases.length > 0);
});

for (const v of cases) {
  test(`content vector: ${v.name}`, () => {
    const bytes = fromHex(v.cbor);
    const decoded = decodeContent(bytes);
    assert.deepEqual(defined(decoded), expected(v.content));
    assert.deepEqual(encodeContent(decoded), bytes);
  });
}
