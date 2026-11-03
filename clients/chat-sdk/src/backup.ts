// The person's backup (docs/chat-contract.md): everything this device
// knows in plaintext — identity and device seeds, decrypted messages, pins,
// verification marks — sealed under a key derived from a twelve-word
// recovery phrase and stored on the home fog as bytes it cannot open.

import { generateMnemonic, mnemonicToSeedSync, validateMnemonic } from "@scure/bip39";
import { wordlist } from "@scure/bip39/wordlists/english.js";
import { hkdf } from "@noble/hashes/hkdf.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { b64u, buf, unb64 } from "./keys.ts";

const te = new TextEncoder();
const td = new TextDecoder();

export function generatePhrase(): string {
  return generateMnemonic(wordlist, 128);
}

export function validPhrase(phrase: string): boolean {
  return validateMnemonic(normalizePhrase(phrase), wordlist);
}

export function normalizePhrase(phrase: string): string {
  return phrase.trim().toLowerCase().split(/\s+/).join(" ");
}

// keyFromPhrase derives the backup key: BIP-39 seed (PBKDF2) then HKDF with
// a fog-specific label, so the same phrase would never unlock anything else.
export async function keyFromPhrase(phrase: string): Promise<CryptoKey> {
  const seed = mnemonicToSeedSync(normalizePhrase(phrase));
  const raw = hkdf(sha256, seed, te.encode("fog-chat-backup/1"), te.encode("aes-256-gcm"), 32);
  return crypto.subtle.importKey("raw", buf(raw), "AES-GCM", false, ["encrypt", "decrypt"]);
}

// keyFromPhraseExportable is the same key, extractable, for storing on the
// device that enabled the backup so later uploads need no phrase.
export async function keyFromPhraseExportable(phrase: string): Promise<CryptoKey> {
  const seed = mnemonicToSeedSync(normalizePhrase(phrase));
  const raw = hkdf(sha256, seed, te.encode("fog-chat-backup/1"), te.encode("aes-256-gcm"), 32);
  return crypto.subtle.importKey("raw", buf(raw), "AES-GCM", true, ["encrypt", "decrypt"]);
}

export interface BackupPayload {
  v: 1;
  addr: string;
  deviceId: number;
  identitySeed?: Uint8Array;
  deviceSeed: Uint8Array;
  messages: unknown[];
  cursors: Record<string, number>;
  pins: Record<string, string>;
  verified: Record<string, string>;
  // People whose identity was reset and whose new key this device hasn't
  // accepted yet (absent in older backups).
  resets?: string[];
  savedAt: string;
}

// seal encrypts a payload: 12-byte nonce || AES-GCM ciphertext. Byte arrays
// inside are base64url'd for JSON.
export async function seal(key: CryptoKey, payload: BackupPayload): Promise<Uint8Array> {
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const json = te.encode(JSON.stringify(payload, replacer));
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: buf(nonce) }, key, buf(json)));
  const out = new Uint8Array(12 + ct.length);
  out.set(nonce);
  out.set(ct, 12);
  return out;
}

export async function open(key: CryptoKey, blob: Uint8Array): Promise<BackupPayload> {
  if (blob.length < 13) throw new Error("backup too short");
  const nonce = blob.subarray(0, 12), ct = blob.subarray(12);
  const pt = new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: buf(nonce) }, key, buf(ct)));
  const payload = JSON.parse(td.decode(pt), reviver) as BackupPayload;
  if (payload.v !== 1) throw new Error("unknown backup version");
  return payload;
}

// Byte arrays travel through JSON as tagged base64url.
function replacer(_k: string, v: unknown) {
  if (v instanceof Uint8Array) return { $bytes: b64u(v) };
  return v;
}
function reviver(_k: string, v: unknown) {
  if (v && typeof v === "object" && "$bytes" in (v as Record<string, unknown>) && typeof (v as { $bytes: unknown }).$bytes === "string") {
    return unb64((v as { $bytes: string }).$bytes);
  }
  return v;
}

