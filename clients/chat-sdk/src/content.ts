// Fog's profile of the MIMI content model — the same shape as
// internal/chat/content.go, so the fog bot and every client agree. This is
// what a message *is* once decrypted; it never leaves an MLS PrivateMessage.

import { decodeCbor, encodeCbor, type CborValue } from "./cbor.ts";
import { isBlobRef } from "./safe.ts";

// What travels: internal/chat's Content.Type, checked by the shared vectors
// in test/vectors/content.json.
type WireContentType = "message" | "edit" | "delete" | "reaction" | "pin" | "receipt" | "room" | "typing" | "history";
// "membership" is a timeline line the client derives from an MLS commit; it
// is never encoded.
export type ContentType = WireContentType | "membership";

// What a member may send. Anything else — "membership" above all, which
// only this client makes, from a commit it verified — is refused at decode.
const WIRE_TYPES = new Set<string>(["message", "edit", "delete", "reaction", "pin", "receipt", "room", "typing", "history"]);

// HistoryItem is one earlier message as its sharer held it (type history).
export interface HistoryItem {
  id: string;
  seq: number;
  sender: string;
  sentAt?: string;
  hubAt?: string;
  content: Content;
}

export interface Body {
  format: "text/markdown";
  text: string;
}

export interface Reaction {
  target: string;
  emoji: string;
  add: boolean;
}

export interface Pin {
  target: string;
  pinned: boolean;
}

export interface Attachment {
  blob: string; // sha256 of the ciphertext
  key: Uint8Array;
  nonce: Uint8Array;
  name?: string;
  mime?: string;
  size?: number;
  w?: number;
  h?: number;
  thumb?: Uint8Array;
}

export interface RoomMeta {
  name?: string;
  topic?: string;
  avatar?: Uint8Array;
}

export interface Content {
  v: 1;
  type: ContentType;
  id?: string;
  sentAt?: string;
  body?: Body;
  topicId?: string;
  inReplyTo?: string;
  replaces?: string;
  reaction?: Reaction;
  pin?: Pin;
  mentions?: string[];
  attachments?: Attachment[];
  expires?: string;
  room?: RoomMeta;
  readSeq?: number;
  system?: string;
  // History re-sent for devices named in historyFor (device keys, base64url).
  history?: HistoryItem[];
  historyFor?: string[];
  // Membership is a client-side line derived from a commit: never encoded.
  membership?: { added: { identity: string; key: string }[]; removed: { identity: string; key: string }[] };
  // Preview is a link card the sender fetched (with fog's help, by choice).
  preview?: PreviewCard;
}

export interface PreviewCard {
  url: string;
  title?: string;
  description?: string;
  image?: string;
  site?: string;
}

export function encodeContent(c: Content): Uint8Array {
  const m: { [k: string]: CborValue } = { v: 1, type: c.type };
  if (c.id) m.id = c.id;
  if (c.sentAt) m.sentAt = c.sentAt;
  if (c.body) m.body = { format: c.body.format, text: c.body.text };
  if (c.topicId) m.topicId = c.topicId;
  if (c.inReplyTo) m.inReplyTo = c.inReplyTo;
  if (c.replaces) m.replaces = c.replaces;
  if (c.reaction) m.reaction = { target: c.reaction.target, emoji: c.reaction.emoji, add: c.reaction.add };
  if (c.pin) m.pin = { target: c.pin.target, pinned: c.pin.pinned };
  if (c.mentions && c.mentions.length) m.mentions = c.mentions;
  if (c.attachments && c.attachments.length) {
    m.attachments = c.attachments.map((a) => {
      const o: { [k: string]: CborValue } = { blob: a.blob, key: a.key, nonce: a.nonce };
      if (a.name) o.name = a.name;
      if (a.mime) o.mime = a.mime;
      if (a.size) o.size = a.size;
      if (a.w) o.w = a.w;
      if (a.h) o.h = a.h;
      if (a.thumb) o.thumb = a.thumb;
      return o;
    });
  }
  if (c.expires) m.expires = c.expires;
  if (c.room) {
    const r: { [k: string]: CborValue } = {};
    if (c.room.name) r.name = c.room.name;
    if (c.room.topic) r.topic = c.room.topic;
    if (c.room.avatar) r.avatar = c.room.avatar;
    m.room = r;
  }
  if (c.readSeq) m.readSeq = c.readSeq;
  if (c.system) m.system = c.system;
  if (c.history && c.history.length) {
    m.history = c.history.map((h) => {
      const o: { [k: string]: CborValue } = { id: h.id, seq: h.seq, sender: h.sender, content: decodeCbor(encodeContent(h.content)) };
      if (h.sentAt) o.sentAt = h.sentAt;
      if (h.hubAt) o.hubAt = h.hubAt;
      return o;
    });
  }
  if (c.historyFor && c.historyFor.length) m.historyFor = c.historyFor;
  if (c.preview) {
    const pv: { [k: string]: CborValue } = { url: c.preview.url };
    if (c.preview.title) pv.title = c.preview.title;
    if (c.preview.description) pv.description = c.preview.description;
    if (c.preview.image) pv.image = c.preview.image;
    if (c.preview.site) pv.site = c.preview.site;
    m.preview = pv;
  }
  return encodeCbor(m);
}

function str(v: CborValue | undefined): string | undefined {
  return typeof v === "string" ? v : undefined;
}
function num(v: CborValue | undefined): number | undefined {
  return typeof v === "number" ? v : typeof v === "bigint" ? Number(v) : undefined;
}
function bytes(v: CborValue | undefined): Uint8Array | undefined {
  return v instanceof Uint8Array ? v : undefined;
}
function obj(v: CborValue | undefined): { [k: string]: CborValue } | undefined {
  return v && typeof v === "object" && !Array.isArray(v) && !(v instanceof Uint8Array) ? (v as { [k: string]: CborValue }) : undefined;
}

export function decodeContent(b: Uint8Array): Content {
  const m = obj(decodeCbor(b));
  if (!m) throw new Error("content: not a map");
  if (num(m.v) !== 1) throw new Error("content: unknown version");
  const type = str(m.type);
  if (!type) throw new Error("content: missing type");
  if (!WIRE_TYPES.has(type)) throw new Error(`content: type ${JSON.stringify(type.slice(0, 32))} is not one a member may send`);
  const c: Content = { v: 1, type: type as ContentType };
  c.id = str(m.id);
  c.sentAt = str(m.sentAt);
  const body = obj(m.body);
  if (body) c.body = { format: "text/markdown", text: str(body.text) ?? "" };
  c.topicId = str(m.topicId);
  c.inReplyTo = str(m.inReplyTo);
  c.replaces = str(m.replaces);
  const re = obj(m.reaction);
  if (re) c.reaction = { target: str(re.target) ?? "", emoji: str(re.emoji) ?? "", add: re.add !== false };
  const pin = obj(m.pin);
  if (pin) c.pin = { target: str(pin.target) ?? "", pinned: pin.pinned !== false };
  if (Array.isArray(m.mentions)) c.mentions = m.mentions.filter((x): x is string => typeof x === "string");
  if (Array.isArray(m.attachments)) {
    c.attachments = [];
    for (const raw of m.attachments) {
      const a = obj(raw);
      if (!a) continue;
      const blob = str(a.blob), key = bytes(a.key), nonce = bytes(a.nonce);
      // The blob is named by its hex sha256 and nothing else: it goes into
      // a URL path when fetched.
      if (!isBlobRef(blob) || !key || !nonce) continue;
      c.attachments.push({ blob, key, nonce, name: str(a.name), mime: str(a.mime), size: num(a.size), w: num(a.w), h: num(a.h), thumb: bytes(a.thumb) });
    }
  }
  c.expires = str(m.expires);
  const room = obj(m.room);
  if (room) c.room = { name: str(room.name), topic: str(room.topic), avatar: bytes(room.avatar) };
  c.readSeq = num(m.readSeq);
  c.system = str(m.system);
  if (Array.isArray(m.history)) {
    c.history = [];
    for (const raw of m.history) {
      const h = obj(raw);
      if (!h) continue;
      const inner = obj(h.content);
      if (!inner) continue;
      try {
        c.history.push({ id: str(h.id) ?? "", seq: num(h.seq) ?? 0, sender: str(h.sender) ?? "", sentAt: str(h.sentAt), hubAt: str(h.hubAt), content: decodeContent(encodeCbor(inner)) });
      } catch {
        /* skip a malformed item */
      }
    }
  }
  if (Array.isArray(m.historyFor)) c.historyFor = m.historyFor.filter((x): x is string => typeof x === "string");
  const pv = obj(m.preview);
  if (pv && str(pv.url)) c.preview = { url: str(pv.url)!, title: str(pv.title), description: str(pv.description), image: str(pv.image), site: str(pv.site) };
  return c;
}
