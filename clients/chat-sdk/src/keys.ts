// Identity and device keys: Ed25519, generated on the device, never sent
// anywhere but as public halves. The attestation format matches
// internal/people (fog-device-attest/1).

import { ed25519 } from "@noble/curves/ed25519.js";

export interface KeyPair {
  seed: Uint8Array; // 32-byte private seed
  publicKey: Uint8Array;
}

export function generateKeyPair(): KeyPair {
  const seed = ed25519.utils.randomSecretKey();
  return { seed, publicKey: ed25519.getPublicKey(seed) };
}

export function keyPairFromSeed(seed: Uint8Array): KeyPair {
  return { seed, publicKey: ed25519.getPublicKey(seed) };
}

const te = new TextEncoder();
const ATTEST_PREFIX = "fog-device-attest/1\n";

function attestationMessage(deviceKey: Uint8Array): Uint8Array {
  const p = te.encode(ATTEST_PREFIX);
  const out = new Uint8Array(p.length + deviceKey.length);
  out.set(p);
  out.set(deviceKey, p.length);
  return out;
}

// attestDevice signs a device key with the person's identity key.
export function attestDevice(identity: KeyPair, deviceKey: Uint8Array): Uint8Array {
  return ed25519.sign(attestationMessage(deviceKey), identity.seed);
}

export function verifyAttestation(identityKey: Uint8Array, deviceKey: Uint8Array, attestation: Uint8Array): boolean {
  try {
    return ed25519.verify(attestation, attestationMessage(deviceKey), identityKey);
  } catch {
    return false;
  }
}

// ---- encodings ----

export function b64u(b: Uint8Array): string {
  let bin = "";
  for (const x of b) bin += String.fromCharCode(x);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// unb64 also takes standard padded base64, which is what Go's JSON encoder
// emits for []byte: the alphabet swap is a no-op on it.
export function unb64(s: string): Uint8Array<ArrayBuffer> {
  const norm = s.replace(/-/g, "+").replace(/_/g, "/");
  const pad = norm.length % 4 === 0 ? "" : "=".repeat(4 - (norm.length % 4));
  const bin = atob(norm + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// buf copies a typed array into a plain ArrayBuffer, the shape WebCrypto's
// typings insist on.
export function buf(u8: Uint8Array): ArrayBuffer {
  const out = new ArrayBuffer(u8.byteLength);
  new Uint8Array(out).set(u8);
  return out;
}

export function hex(b: Uint8Array): string {
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

export function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a[i] ^ b[i];
  return d === 0;
}

// safetyWords renders a key fingerprint as five short groups people can
// read to each other.
export function fingerprint(key: Uint8Array): string {
  const h = hex(key);
  return [h.slice(0, 5), h.slice(5, 10), h.slice(10, 15), h.slice(15, 20), h.slice(20, 25)].join(" ").toUpperCase();
}

// ---- ULIDs ----

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

export function ulid(now = Date.now()): string {
  let t = now;
  const chars = new Array<string>(26);
  for (let i = 9; i >= 0; i--) {
    chars[i] = CROCKFORD[t % 32];
    t = Math.floor(t / 32);
  }
  const rnd = new Uint8Array(16);
  crypto.getRandomValues(rnd);
  for (let i = 10; i < 26; i++) chars[i] = CROCKFORD[rnd[i - 10] & 31];
  return chars.join("");
}
