// A small CBOR codec (RFC 8949) for the message content profile: maps with
// string keys, arrays, text, bytes, integers, booleans, null. Nothing else
// is needed, and a dependency for this would be larger than the code.

export type CborValue =
  | null
  | boolean
  | number
  | bigint
  | string
  | Uint8Array
  | CborValue[]
  | { [key: string]: CborValue };

class Writer {
  private chunks: Uint8Array[] = [];
  private size = 0;
  push(b: Uint8Array) {
    this.chunks.push(b);
    this.size += b.length;
  }
  head(major: number, n: number | bigint): void {
    const m = major << 5;
    if (typeof n === "bigint") {
      if (n < 0x1_0000_0000n) return this.head(major, Number(n));
      const b = new Uint8Array(9);
      b[0] = m | 27;
      new DataView(b.buffer).setBigUint64(1, n);
      return this.push(b);
    }
    if (n < 24) return this.push(new Uint8Array([m | n]));
    if (n < 0x100) return this.push(new Uint8Array([m | 24, n]));
    if (n < 0x10000) {
      const b = new Uint8Array(3);
      b[0] = m | 25;
      new DataView(b.buffer).setUint16(1, n);
      return this.push(b);
    }
    if (n < 0x1_0000_0000) {
      const b = new Uint8Array(5);
      b[0] = m | 26;
      new DataView(b.buffer).setUint32(1, n);
      return this.push(b);
    }
    return this.head(major, BigInt(n));
  }
  bytes(): Uint8Array {
    const out = new Uint8Array(this.size);
    let o = 0;
    for (const c of this.chunks) {
      out.set(c, o);
      o += c.length;
    }
    return out;
  }
}

const te = new TextEncoder();
const td = new TextDecoder();

function encodeInto(w: Writer, v: CborValue) {
  if (v === null || v === undefined) return w.push(new Uint8Array([0xf6]));
  if (v === true) return w.push(new Uint8Array([0xf5]));
  if (v === false) return w.push(new Uint8Array([0xf4]));
  if (typeof v === "number") {
    if (!Number.isInteger(v)) {
      const b = new Uint8Array(9);
      b[0] = 0xfb;
      new DataView(b.buffer).setFloat64(1, v);
      return w.push(b);
    }
    return v >= 0 ? w.head(0, v) : w.head(1, -1 - v);
  }
  if (typeof v === "bigint") return v >= 0n ? w.head(0, v) : w.head(1, -1n - v);
  if (typeof v === "string") {
    const b = te.encode(v);
    w.head(3, b.length);
    return w.push(b);
  }
  if (v instanceof Uint8Array) {
    w.head(2, v.length);
    return w.push(v);
  }
  if (Array.isArray(v)) {
    w.head(4, v.length);
    for (const x of v) encodeInto(w, x);
    return;
  }
  // Map: canonical key order (length-first, then bytewise) so equal content
  // encodes identically everywhere.
  const keys = Object.keys(v).filter((k) => v[k] !== undefined);
  keys.sort((a, b) => {
    const ea = te.encode(a), eb = te.encode(b);
    if (ea.length !== eb.length) return ea.length - eb.length;
    for (let i = 0; i < ea.length; i++) if (ea[i] !== eb[i]) return ea[i] - eb[i];
    return 0;
  });
  w.head(5, keys.length);
  for (const k of keys) {
    encodeInto(w, k);
    encodeInto(w, v[k]);
  }
}

export function encodeCbor(v: CborValue): Uint8Array {
  const w = new Writer();
  encodeInto(w, v);
  return w.bytes();
}

class Reader {
  private b: Uint8Array;
  pos: number;
  private view: DataView;
  constructor(b: Uint8Array, pos = 0) {
    this.b = b;
    this.pos = pos;
    this.view = new DataView(b.buffer, b.byteOffset, b.byteLength);
  }
  u8() {
    if (this.pos >= this.b.length) throw new Error("cbor: truncated");
    return this.b[this.pos++];
  }
  arg(info: number): number | bigint {
    if (info < 24) return info;
    switch (info) {
      case 24:
        return this.u8();
      case 25: {
        const v = this.view.getUint16(this.pos);
        this.pos += 2;
        return v;
      }
      case 26: {
        const v = this.view.getUint32(this.pos);
        this.pos += 4;
        return v;
      }
      case 27: {
        const v = this.view.getBigUint64(this.pos);
        this.pos += 8;
        return v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : v;
      }
    }
    throw new Error("cbor: indefinite lengths not supported");
  }
  take(n: number): Uint8Array {
    if (this.pos + n > this.b.length) throw new Error("cbor: truncated");
    const out = this.b.slice(this.pos, this.pos + n);
    this.pos += n;
    return out;
  }
  value(): CborValue {
    const ib = this.u8();
    const major = ib >> 5, info = ib & 31;
    switch (major) {
      case 0:
        return this.arg(info);
      case 1: {
        const n = this.arg(info);
        return typeof n === "bigint" ? -1n - n : -1 - n;
      }
      case 2:
        return this.take(Number(this.arg(info)));
      case 3:
        return td.decode(this.take(Number(this.arg(info))));
      case 4: {
        const n = Number(this.arg(info));
        const out: CborValue[] = [];
        for (let i = 0; i < n; i++) out.push(this.value());
        return out;
      }
      case 5: {
        const n = Number(this.arg(info));
        const out: { [k: string]: CborValue } = {};
        for (let i = 0; i < n; i++) {
          const k = this.value();
          if (typeof k !== "string") throw new Error("cbor: non-string map key");
          out[k] = this.value();
        }
        return out;
      }
      case 6:
        this.arg(info); // tag: ignore
        return this.value();
      case 7:
        switch (info) {
          case 20:
            return false;
          case 21:
            return true;
          case 22:
          case 23:
            return null;
          case 25: {
            // half float
            const h = this.view.getUint16(this.pos);
            this.pos += 2;
            const s = (h & 0x8000) ? -1 : 1, e = (h >> 10) & 0x1f, f = h & 0x3ff;
            if (e === 0) return s * f * 2 ** -24;
            if (e === 31) return f ? NaN : s * Infinity;
            return s * (1 + f / 1024) * 2 ** (e - 15);
          }
          case 26: {
            const v = this.view.getFloat32(this.pos);
            this.pos += 4;
            return v;
          }
          case 27: {
            const v = this.view.getFloat64(this.pos);
            this.pos += 8;
            return v;
          }
        }
    }
    throw new Error(`cbor: unsupported item ${ib}`);
  }
}

export function decodeCbor(b: Uint8Array): CborValue {
  const r = new Reader(b);
  const v = r.value();
  if (r.pos !== b.length) throw new Error("cbor: trailing bytes");
  return v;
}
