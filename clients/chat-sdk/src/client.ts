// ChatClient: one device's view of the household's rooms. It owns the
// identity and device keys, the MLS state of every room, the plaintext it has
// decrypted, and the conversation with fogd. UIs render from it and call its
// verbs; nothing in a UI ever touches a key.

import {
  Api,
  ApiError,
  type ApiOptions,
  type AvailableRoom,
  type DirectoryHousehold,
  type DirectoryPerson,
  type EventView,
  type InviteView,
  type KeyMaterial,
  type MeResponse,
  type Prefs,
  type RoomDetail,
  type RoomView,
} from "./api.ts";
import { decodeContent, encodeContent, type Attachment, type Content } from "./content.ts";
import { attestDevice, b64u, buf, bytesEqual, fingerprint, generateKeyPair, hex, keyPairFromSeed, ulid, unb64, verifyAttestation, type KeyPair } from "./keys.ts";
import { Mls, RefusedMessage, type CommitRules, type OwnKeyPackage, type StoredKeyPackage } from "./mls.ts";
import type { Storage } from "./storage.ts";
import type { ClientState } from "ts-mls";
import { ed25519 } from "@noble/curves/ed25519.js";
import * as backup from "./backup.ts";
import { Throttle, reconnectDelay } from "./pace.ts";
import { foldTimeline, inThread, isReply, sameView } from "./timeline.ts";
import { systemLabel } from "./safe.ts";

// ---- local models ----

export interface Message {
  id: string;
  roomId: string;
  seq: number;
  sender: string;
  senderDevice: string;
  hubAt: string;
  sentAt: string;
  content: Content;
  // mine says this device's person sent it.
  mine: boolean;
  // checked says the room's rules were applied to this item when it
  // arrived; items stored before clients checked them lack it.
  checked?: boolean;
  // denied says the sender's role didn't allow this act (a reaction, an
  // edit, a delete, a pin) when it arrived: it is kept but never applies.
  denied?: boolean;
  // moderator says the sender's role let them act on others' messages (a
  // delete of someone else's, a pin) when it arrived.
  moderator?: boolean;
  // sharedBy names who re-sent this item into the room as history. It is
  // their account of the past, which this device cannot check: it may fill
  // in what the device never saw, never change what it received itself.
  sharedBy?: string;
}

// A rendered message: the base message with edits applied and reactions,
// pins, and thread stats folded in.
export interface MessageView {
  id: string;
  roomId: string;
  seq: number;
  sender: string;
  sentAt: string;
  hubAt: string;
  text: string;
  edited: boolean;
  deleted: boolean;
  topicId?: string;
  inReplyTo?: string;
  mentions: string[];
  attachments: Attachment[];
  reactions: { emoji: string; by: string[] }[];
  pinned: boolean;
  replies: number;
  lastReplyAt?: string;
  system?: string;
  mine: boolean;
  preview?: Content["preview"];
  // sharedBy: this message reached this device as history another member
  // re-sent, on their word.
  sharedBy?: string;
}

// One room's fold, and the filtered reads served from it: the roots under
// "", a thread under its topic id. stale is the previous fold's lists, so a
// list whose members all came through unchanged keeps its identity.
interface Fold {
  version: number;
  names: number;
  views: readonly MessageView[];
  lists: Map<string, readonly MessageView[]>;
  stale?: Map<string, readonly MessageView[]>;
}

// RoomMember is one person as a room's group holds them (ChatClient.members).
export interface RoomMember {
  addr: string;
  // listed: fog counts them a participant.
  listed: boolean;
  // devices: their leaves in the group; none means they can't read yet.
  devices: { key: string; attested: boolean }[];
  // unexplained: they hold a leaf fog doesn't account for.
  unexplained: boolean;
  // awaitingKey: their identity was reset and no new key is accepted yet.
  awaitingKey: boolean;
}

export interface Room extends RoomView {
  // sync says whether this device holds the room's keys and is current.
  sync: "ready" | "joining" | "no-keys" | "stale";
  // cursor is the last seq this device processed.
  cursor: number;
  lastMessageAt?: string;
  // staleReason says why a room went stale, when this device refused
  // something in it (a leaf it couldn't verify, a commit against the rules).
  staleReason?: string;
}

export interface Me {
  addr: string;
  handle: string;
  displayName: string;
  deviceId: number;
  deviceKey: string;
  identityKey: string;
  // verified says this device is attested by the person's identity key.
  verified: boolean;
  // hasIdentitySeed says this device holds the identity key and can attest
  // other devices.
  hasIdentitySeed: boolean;
  operator: boolean;
  household: { name: string; instanceId: string; domain: string };
}

export interface ClientOptions extends ApiOptions {
  storage: Storage;
  deviceName?: string;
  deviceKind?: "web" | "ios" | "android" | "desktop";
  clientKind?: "web" | "native";
  WebSocketImpl?: typeof WebSocket;
  // wsHeaders lets a native client send its bearer token on the stream.
  wsHeaders?: Record<string, string>;
  // lock makes this client the only one using this device's keys: one MLS
  // state, one sender, never a sending generation used twice. Defaults to a
  // Web Lock named by the storage (IndexedDB names one: tabs share it), or
  // none for storage only this client holds; null opts out.
  lock?: DeviceLock | null;
}

// DeviceLock is held by at most one client of a device at a time. take
// resolves true when this client now holds it — for as long as it lives —
// and false when another does.
export interface DeviceLock {
  take(): Promise<boolean>;
}

// webDeviceLock is the browser's: a Web Lock held for the page's life.
export function webDeviceLock(name: string): DeviceLock | null {
  const locks = (globalThis as { navigator?: { locks?: { request: (n: string, o: object, cb: (l: unknown) => Promise<unknown> | unknown) => Promise<unknown> } } }).navigator?.locks;
  if (!locks) return null;
  // Taken once per page: a client started again in the same tab (after a
  // restore) holds what this tab already holds.
  return {
    take: () =>
      (heldHere[name] ??= new Promise<boolean>((resolve) => {
        locks.request(name, { ifAvailable: true }, (l) => {
          if (!l) {
            resolve(false);
            return undefined;
          }
          resolve(true);
          return new Promise(() => {}); // held until the page goes
        });
      }).then((held) => {
        if (!held) delete heldHere[name];
        return held;
      })),
  };
}
const heldHere: Record<string, Promise<boolean>> = {};

// DeviceInUseError: another tab (or client) holds this device's keys.
export class DeviceInUseError extends Error {
  code = "device_in_use";
  constructor() {
    super("Chat is open in another tab of this browser. Use that tab, or close it and this one takes over.");
    this.name = "DeviceInUseError";
  }
}

const NS = {
  meta: "meta",
  groups: "groups",
  kps: "keypackages",
  messages: "messages",
  cursors: "cursors",
  pins: "identity-pins",
  verified: "verified",
  saved: "saved",
} as const;

const KEEP_KEY_PACKAGES = 10;

// LAST_RESORT prefixes the store key of the device's last-resort package,
// which joining never consumes.
const LAST_RESORT = "lr:";

// The hub's system events that take people out of a room (internal/chat
// System*): each names them in "left".
const DEPARTURES = new Set(["household-cut", "banned", "left", "identity-reset"]);

// isULID says an id is a ULID — what every message id is, and what no id
// the client mints for its own lines ("membership:…") can be.
function isULID(id: unknown): id is string {
  return typeof id === "string" && /^[0-9A-HJKMNP-TV-Z]{26}$/.test(id);
}

// leafId names a leaf by who it says it is and the key it holds, together.
function leafId(l: { identity: string; signatureKey: Uint8Array; index?: number }): string {
  return `${l.index ?? ""}\u0000${l.identity}\u0000${b64u(l.signatureKey)}`;
}

// PROVISIONAL prefixes the meta row holding, for a room joined by a Welcome
// for an earlier epoch than the hub's GroupInfo, that GroupInfo: the join
// holds only once the group matches it (settleProvisional).
const PROVISIONAL = "provisional:";

// NotYet is what keeps a Welcome from being judged now: not a verdict.
class NotYet extends Error {}

// INVITE_BUDGET is how long one invite's join may take on connect.
const INVITE_BUDGET = 60_000;

// within settles with p, or fails once ms pass.
function within<T>(ms: number, p: Promise<T>): Promise<T> {
  let t: ReturnType<typeof setTimeout>;
  return Promise.race([p, new Promise<T>((_, reject) => (t = setTimeout(() => reject(new Error("timed out")), ms)))]).finally(() => clearTimeout(t));
}

// REFUSED_INVITES is the meta row listing the invites (by id) this device
// refused: never judged again.
const REFUSED_INVITES = "refusedInvites";

// STALE prefixes the meta row holding why a room is stale: what makes a
// room stale outlives a restart, whatever made it so.
const STALE = "stale:";

// REFUSED prefixes the meta row keeping, per room, the first commit this
// device refused for a device nobody vouches for: what a member's removal
// of that device recovers from (applyRecovery, removeRefusedDevices).
const REFUSED = "refused:";
interface RefusedCommit {
  epoch: number;
  wire: string;
  bad: string[];
}

// VettedCommit is a commit applied to a copy of the group, and what it did.
interface VettedCommit {
  before: { index: number; identity: string; signatureKey: Uint8Array }[];
  newState: ClientState;
  committer?: { identity: string; signatureKey: Uint8Array };
  cleared: number[];
  fresh: { index: number; identity: string; signatureKey: Uint8Array }[];
  accepting: Map<string, string>;
  bad: { identity: string; signatureKey: Uint8Array }[];
}

// NAMES prefixes the meta row holding, per room, the name each device key
// has had in its group, across every epoch this device has seen.
const NAMES = "names:";

// RESETS is the meta row listing people awaiting a new key's acceptance.
const RESETS = "resets";

// ROLL_AFTER and ROLL_AGE: when a device moves a room to a fresh epoch —
// after this many of its own messages in one, or once one is this old.
const ROLL_AFTER = 10_000;
const ROLL_AGE = 7 * 24 * 3600_000;

// RESTORED marks a store seeded from a backup: its device's published key
// packages are replaced at the next start (init).
const RESTORED = "restored";
const RESTORED_LEAVES = "restoredLeaves";

// LAST_EPOCH prefixes the meta row holding the highest epoch this device
// has held for a room; EPOCH_AT, when it began; WELCOMES, digests of the
// Welcomes it has used there (none is used twice).
const LAST_EPOCH = "lastEpoch:";
const EPOCH_AT = "epochAt:";
const WELCOMES = "welcomes:";

// REVOKED prefixes the meta row holding which of a room's doomed keys are
// revoked devices (as opposed to departed people, who may come back).
const REVOKED = "revoked:";

// DOOMED prefixes the meta row holding a room's leaves to remove.
const DOOMED = "doomed:";

// NothingToDo ends a commit attempt that finds its work already done.
class NothingToDo extends Error {}

// A batch of local writes made while applying events: one storage
// transaction, one "messages" event per room and at most one room refresh,
// however many events a catch-up page held. A live event gets a batch of
// its own.
interface WriteBatch {
  rows: Map<string, { ns: string; key: string; value: unknown }>;
  messageRooms: Set<string>;
  refreshRooms: boolean;
}

const newBatch = (): WriteBatch => ({ rows: new Map(), messageRooms: new Set(), refreshRooms: false });

type Listener = (detail: unknown) => void;

// CryptoUnavailableError: the platform has no WebCrypto `subtle` — in a
// browser that means the page was opened over plain http from somewhere
// other than localhost, where the browser withholds it on purpose. Nothing
// in the client can run without it (MLS key generation is the first call).
export class CryptoUnavailableError extends Error {
  code = "crypto_unavailable";
  constructor() {
    super("This page has no access to the browser's cryptography (crypto.subtle), which browsers only unlock on https:// or localhost. Open fog over https to use Chat.");
    this.name = "CryptoUnavailableError";
  }
}

// webCryptoAvailable says whether the client can run here at all.
export function webCryptoAvailable(): boolean {
  return typeof globalThis.crypto !== "undefined" && !!globalThis.crypto.subtle;
}

function assertWebCrypto() {
  if (!webCryptoAvailable()) throw new CryptoUnavailableError();
}

export class ChatClient {
  api: Api;
  private storage: Storage;
  private opts: ClientOptions;
  private mls!: Mls;
  private identity: KeyPair | null = null;
  private device!: KeyPair;
  private meInfo!: Me;
  private states = new Map<string, ClientState>();
  private rooms_ = new Map<string, Room>();
  private available_: AvailableRoom[] = [];
  private messages_ = new Map<string, Map<string, Message>>();
  private cursors = new Map<string, number>();
  private processing = new Map<string, Promise<void>>();
  private ws: WebSocket | null = null;
  private wsWanted = false;
  private online_ = new Set<string>();
  private typing_ = new Map<string, Map<string, number>>();
  private listeners = new Map<string, Set<Listener>>();
  private directory_: DirectoryPerson[] = [];
  private households_: DirectoryHousehold[] = [];
  private closed = false;
  private meServer: MeResponse | null = null;
  private historySent = new Set<string>();
  // What fog's directory says, by person address: their identity key and
  // each device's attestation. A claim to check, never trust.
  private dirIdentity = new Map<string, string | undefined>();
  private dirDevices = new Map<string, Map<string, string | undefined>>();
  // Leaves this device verified: "addr\0deviceKey".
  private attested = new Set<string>();
  // When each unverified leaf last sent this device back to fog to ask.
  private refetchedFor = new Map<string, number>();
  // dirRefetch is a directory refetch under way, which a leaf check that
  // needs one waits on rather than skips; joining, each room's join.
  private dirRefetch: Promise<unknown> | null = null;
  private joining = new Map<string, Promise<void>>();
  // provisionalTries counts, per room, reads of a log that stopped short of
  // a provisional join's GroupInfo.
  private provisionalTries = new Map<string, number>();
  // lastEpochs caches LAST_EPOCH rows; pendingCommits holds, per room, a
  // commit this device submitted whose answer it hasn't had (adoptPending);
  // rolled, the epochs a roll was started for.
  private lastEpochs = new Map<string, number>();
  private pendingCommits = new Map<string, { wire: string; newState: ClientState; before: { index: number; identity: string; signatureKey: Uint8Array }[] }>();
  private rolled = new Set<string>();
  // inviteAttempts counts, per invite, tries that couldn't judge it yet.
  private inviteAttempts = new Map<number, number>();
  // People whose served identity key differs from the one held here.
  private warnings = new Map<string, string>();
  // resets holds the people whose identity a trusted hub said was reset and
  // whose new key this device hasn't accepted: no key is trusted for them
  // until a member re-adds them in a commit this device verifies, or this
  // person compares safety codes (acceptIdentity). Kept in meta "resets".
  private resets = new Set<string>();
  private backupKey: CryptoKey | null = null;
  private prefs_: Prefs | null = null;
  private backupTimer: ReturnType<typeof setTimeout> | null = null;
  private siblingTimer: ReturnType<typeof setInterval> | null = null;
  // The folded timeline per room, rebuilt only when the room's messages —
  // or the names its membership lines print — changed since the last read.
  private folded = new Map<string, Fold>();
  // Read marks per room, refolded with the room's messages.
  private marks_ = new Map<string, { version: number; marks: ReadonlyMap<string, number> }>();
  private roomVersion = new Map<string, number>();
  private namesVersion = 0;
  // At most one typing signal per room this often, whatever the UI does.
  private typingThrottle = new Throttle(2500);
  private reconnects = 0;

  constructor(opts: ClientOptions) {
    this.opts = opts;
    this.storage = opts.storage;
    this.api = new Api(opts);
  }

  // ---- events ----

  addEventListener(kind: "rooms" | "messages" | "presence" | "typing" | "status" | "me" | "error" | "identity", fn: Listener) {
    let set = this.listeners.get(kind);
    if (!set) this.listeners.set(kind, (set = new Set()));
    set.add(fn);
    return () => set!.delete(fn);
  }

  private emit(kind: string, detail: unknown = undefined) {
    this.listeners.get(kind)?.forEach((fn) => {
      try {
        fn(detail);
      } catch (e) {
        console.error("chat listener", e);
      }
    });
  }

  // ---- bootstrap ----

  me(): Me {
    return this.meInfo;
  }

  // init loads local state and makes sure this device exists on the server:
  // the person's identity key is written once (by the first device), and a
  // device key is registered with a batch of KeyPackages.
  async init(): Promise<Me> {
    assertWebCrypto();
    // One client per device's keys, before anything is read or written:
    // two would each advance the same MLS state and reuse a sending
    // generation.
    const lock = this.opts.lock !== undefined ? this.opts.lock : this.storage.lockName ? webDeviceLock(this.storage.lockName) : null;
    if (lock && !(await lock.take())) throw new DeviceInUseError();
    this.mls = await Mls.init();
    const meta = await this.storage.get<{ identitySeed?: Uint8Array; deviceSeed: Uint8Array; deviceId: number; addr: string }>(NS.meta, "self");
    let server = await this.api.me();
    const addr = `fog:${server.household.instanceId}/${server.person.handle}`;

    if (meta?.identitySeed) this.identity = keyPairFromSeed(meta.identitySeed);
    const known = meta && server.devices.find((d) => d.id === meta.deviceId && bytesEqual(unb64(d.deviceKey), keyPairFromSeed(meta.deviceSeed).publicKey));
    if (meta && known && meta.addr === addr) {
      this.device = keyPairFromSeed(meta.deviceSeed);
      if (!server.deviceId || server.deviceId !== meta.deviceId) {
        // A fresh sign-in on a browser that already holds this device's
        // key: prove possession and bind the session to it.
        const ch = await this.api.deviceChallenge(meta.deviceId);
        const sig = ed25519.sign(new TextEncoder().encode(ch.message), this.device.seed);
        const bound = await this.api.deviceSession(meta.deviceId, ch.nonce, b64u(sig), this.opts.clientKind ?? "web");
        if (bound.token) this.api = new Api({ ...this.opts, headers: { ...(this.opts.headers ?? {}), Authorization: `Bearer ${bound.token}` } });
        server = await this.api.me();
      }
    } else {
      // A new device on this browser.
      this.device = generateKeyPair();
      if (!server.identityKey) {
        this.identity = generateKeyPair();
        await this.api.setIdentity(b64u(this.identity.publicKey));
        server = await this.api.me();
      }
      const attestation = this.identity && server.identityKey && bytesEqual(unb64(server.identityKey), this.identity.publicKey)
        ? b64u(attestDevice(this.identity, this.device.publicKey))
        : undefined;
      const kps = await this.mintKeyPackages(addr, KEEP_KEY_PACKAGES);
      const lastResort = await this.mintLastResort(addr);
      const reg = await this.api.registerDevice({
        name: this.opts.deviceName ?? defaultDeviceName(),
        kind: this.opts.deviceKind ?? "web",
        deviceKey: b64u(this.device.publicKey),
        attestation,
        keyPackages: kps.map((k) => b64u(k.raw)),
        lastResort: b64u(lastResort.raw),
        clientKind: this.opts.clientKind ?? "web",
      });
      if (reg.token) this.api = new Api({ ...this.opts, headers: { ...(this.opts.headers ?? {}), Authorization: `Bearer ${reg.token}` } });
      await this.storage.put(NS.meta, "self", { identitySeed: this.identity?.seed, deviceSeed: this.device.seed, deviceId: reg.device.id, addr });
      server = await this.api.me();
    }
    const mine = server.devices.find((d) => bytesEqual(unb64(d.deviceKey), this.device.publicKey));
    this.meServer = server;
    this.meInfo = {
      addr,
      handle: server.person.handle,
      displayName: server.person.displayName,
      deviceId: mine?.id ?? server.deviceId,
      deviceKey: b64u(this.device.publicKey),
      identityKey: server.identityKey,
      verified: !!mine?.verified,
      hasIdentitySeed: !!this.identity && !!server.identityKey && bytesEqual(unb64(server.identityKey), this.identity.publicKey),
      operator: server.operator,
      household: server.household,
    };
    await this.loadLocal();
    // This person's identity key as this device holds it: its seed's, or
    // the key pinned the first time it saw one. Fog serving another is
    // said out loud (identityWarnings).
    await this.knownIdentity(addr);
    if (server.identityKey) await this.reportIdentity(addr, server.identityKey);
    // A device restored from a backup holds none of the private keys of the
    // packages it published before: they all go, and fresh ones replace
    // them, so a member's Welcome to it is one it can open.
    if (mine && (await this.storage.get(NS.meta, RESTORED))) {
      const kps = await this.mintKeyPackages(addr, KEEP_KEY_PACKAGES);
      const lr = await this.mintLastResort(addr);
      await this.api.publishKeyPackages(mine.id, kps.map((k) => b64u(k.raw)), b64u(lr.raw), true);
      await this.storage.del(NS.meta, RESTORED);
      await this.storage.put(NS.meta, RESTORED_LEAVES, true);
    } else if (mine && mine.keyPackages < KEEP_KEY_PACKAGES / 2) await this.replenishKeyPackages(mine.id);
    // A device from before last-resort packages publishes one now.
    if (mine && !(await this.storage.list(NS.kps)).some((r) => r.key.startsWith(LAST_RESORT))) {
      const lr = await this.mintLastResort(addr);
      await this.api.publishKeyPackages(mine.id, [], b64u(lr.raw)).catch(() => {});
    }
    const bk = await this.storage.get<Uint8Array>(NS.meta, "backupKey");
    if (bk) this.backupKey = await crypto.subtle.importKey("raw", buf(bk), "AES-GCM", false, ["encrypt", "decrypt"]);
    this.prefs_ = await this.api.prefs().catch(() => null);
    this.emit("me", this.meInfo);
    return this.meInfo;
  }

  private async loadLocal() {
    for (const { key, value } of await this.storage.list<Uint8Array>(NS.groups)) {
      try {
        this.states.set(key, this.mls.restore(value));
      } catch (e) {
        console.warn("chat: dropping unreadable group state", key, e);
      }
    }
    for (const { key, value } of await this.storage.list<number>(NS.cursors)) this.cursors.set(key, value);
    this.resets = new Set(await this.storage.get<string[]>(NS.meta, RESETS) ?? []);
    for (const { value } of await this.storage.list<Message>(NS.messages)) {
      let m = this.messages_.get(value.roomId);
      if (!m) this.messages_.set(value.roomId, (m = new Map()));
      m.set(value.id, value);
    }
    this.folded.clear();
    this.marks_.clear();
  }

  private async mintKeyPackages(addr: string, n: number): Promise<OwnKeyPackage[]> {
    const out: OwnKeyPackage[] = [];
    for (let i = 0; i < n; i++) {
      const kp = await this.mls.keyPackage(addr, this.device);
      await this.storage.put(NS.kps, b64u(kp.publicPackage.initKey), this.mls.storeKeyPackage(kp));
      out.push(kp);
    }
    return out;
  }

  // mintLastResort makes the device's last-resort package and keeps its
  // private keys for good: fog serves it to more than one adder, so a
  // Welcome to it may come more than once.
  private async mintLastResort(addr: string): Promise<OwnKeyPackage> {
    const kp = await this.mls.keyPackage(addr, this.device);
    for (const r of await this.storage.list(NS.kps)) if (r.key.startsWith(LAST_RESORT)) await this.storage.del(NS.kps, r.key);
    await this.storage.put(NS.kps, LAST_RESORT + b64u(kp.publicPackage.initKey), this.mls.storeKeyPackage(kp));
    return kp;
  }

  private async replenishKeyPackages(deviceId: number) {
    const kps = await this.mintKeyPackages(this.meInfo.addr, KEEP_KEY_PACKAGES);
    await this.api.publishKeyPackages(deviceId, kps.map((k) => b64u(k.raw)));
  }

  private async storedKeyPackages(): Promise<OwnKeyPackage[]> {
    const rows = await this.storage.list<StoredKeyPackage>(NS.kps);
    const out: OwnKeyPackage[] = [];
    for (const r of rows) {
      try {
        out.push(this.mls.restoreKeyPackage(r.value, this.device));
      } catch {
        await this.storage.del(NS.kps, r.key);
      }
    }
    return out;
  }

  private async consumeStoredKeyPackage(kp: OwnKeyPackage) {
    await this.storage.del(NS.kps, b64u(kp.publicPackage.initKey));
  }

  // ---- sync ----

  // connect pulls the room list, joins pending invites, catches up every
  // room, and opens the stream.
  async connect(): Promise<void> {
    await this.refreshRooms();
    // What fog serves for people — and so which keys changed — is known
    // before anything is caught up on and judged against it.
    await this.directory().catch(() => {});
    await Promise.all(Array.from(this.rooms_.keys()).map((id) => this.catchUp(id)));
    // A device restored from a backup holds no keys for its rooms, but its
    // old leaves are still in them: it leaves each (only this device), so a
    // sibling or a member can add it back.
    if (await this.storage.get(NS.meta, RESTORED_LEAVES)) {
      for (const id of this.rooms_.keys()) if (!this.states.has(id)) await this.api.updateRoom(id, { leaveDevice: true }).catch(() => {});
      await this.storage.del(NS.meta, RESTORED_LEAVES);
    }
    // Invites after the rooms already held, all at once, each within a
    // budget: a crafted Welcome can't hold up the rest, or connect.
    await this.acceptInvites();
    this.wsWanted = true;
    this.openStream();
    await this.syncSiblingDevices();
    if (!this.siblingTimer) this.siblingTimer = setInterval(() => this.syncSiblingDevices().catch(() => {}), 60_000);
  }

  close() {
    this.closed = true;
    this.wsWanted = false;
    this.ws?.close();
    if (this.siblingTimer) clearInterval(this.siblingTimer);
    if (this.backupTimer) clearTimeout(this.backupTimer);
  }

  private async refreshRooms() {
    const res = await this.api.rooms();
    const seen = new Set<string>();
    for (const rv of res.rooms) {
      seen.add(rv.id);
      const prev = this.rooms_.get(rv.id);
      const room: Room = { ...rv, sync: prev?.sync ?? "no-keys", cursor: this.cursors.get(rv.id) ?? 0, lastMessageAt: prev?.lastMessageAt, staleReason: prev?.staleReason };
      if (this.states.has(rv.id)) room.sync = room.sync === "stale" ? "stale" : "ready";
      else room.sync = room.sync === "joining" ? "joining" : "no-keys";
      // What made a room stale outlives a restart.
      const staleWhy = await this.storage.get<string>(NS.meta, STALE + rv.id);
      if (room.sync === "ready" && (staleWhy || (await this.storage.get(NS.meta, REFUSED + rv.id)))) {
        room.sync = "stale";
        room.staleReason ??= staleWhy ?? "This device refused a device in this room that nobody vouches for. A member who may remove people can remove it.";
      }
      this.rooms_.set(rv.id, room);
    }
    for (const id of Array.from(this.rooms_.keys())) if (!seen.has(id)) this.rooms_.delete(id);
    this.available_ = res.available;
    this.emit("rooms");
  }

  rooms(): Room[] {
    return Array.from(this.rooms_.values()).sort((a, b) => (b.lastMessageAt ?? b.createdAt).localeCompare(a.lastMessageAt ?? a.createdAt));
  }

  room(id: string): Room | undefined {
    return this.rooms_.get(id);
  }

  available(): AvailableRoom[] {
    return this.available_;
  }

  async roomDetail(id: string): Promise<RoomDetail> {
    return this.api.room(id);
  }

  private async acceptInvites() {
    const { invites } = await this.api.invites();
    await Promise.all(invites.map((inv) => within(INVITE_BUDGET, this.acceptInvite(inv)).catch((e) => console.warn("chat: invite", inv.roomId, e))));
  }

  // acceptInvite joins from one Welcome. Joins into one room run one at a
  // time: two at once (a stream's invite frame racing a catch-up) would
  // each judge the same Welcome while the other refetched what it needs.
  private acceptInvite(inv: InviteView): Promise<void> {
    const prev = this.joining.get(inv.roomId) ?? Promise.resolve();
    const next = prev.catch(() => {}).then(() => this.acceptInviteNow(inv));
    this.joining.set(inv.roomId, next);
    return next.finally(() => {
      if (this.joining.get(inv.roomId) === next) this.joining.delete(inv.roomId);
    });
  }

  private async acceptInviteNow(inv: InviteView) {
    // Judged and, if it holds, joined in the room's queue: nothing else
    // reads or writes the room's state meanwhile.
    const joined = await this.serial(inv.roomId, () => this.judgeWelcome(inv));
    if (!joined) return;
    await this.api.acceptInvite(inv.id).catch(() => {});
    await this.refreshRooms();
    await this.doomUnlisted(inv.roomId);
    await this.catchUp(inv.roomId);
    if (joined.behind) await this.settleProvisional(inv.roomId);
    await this.maybeReplenish();
  }

  // judgeWelcome checks one Welcome and, if it holds, makes it the room's
  // state. It runs in the room's queue.
  private async judgeWelcome(inv: InviteView): Promise<{ behind: boolean } | null> {
    // A room this device holds keys for — in step or stale — takes no
    // Welcome: a stale one is left on this device (leave) and rejoined only
    // by a member's fresh Add, to a device that holds nothing for it.
    if (this.states.has(inv.roomId)) {
      await this.api.acceptInvite(inv.id).catch(() => {});
      return null;
    }
    // A Welcome this device refused stays refused: it isn't judged (and
    // can't cost a directory fetch) again on every connect.
    const refusedInvites = new Set(await this.storage.get<number[]>(NS.meta, REFUSED_INVITES) ?? []);
    if (refusedInvites.has(inv.id)) return null;
    const refuse = async (reason: string, quietly = false) => {
      refusedInvites.add(inv.id);
      await this.storage.put(NS.meta, REFUSED_INVITES, Array.from(refusedInvites).slice(-500));
      if (!quietly) this.markStale(inv.roomId, reason);
    };
    const welcome = unb64(inv.welcome);
    // A Welcome is used once: one this device used before (a replay, to
    // take it back to an old epoch and its spent generations) is refused.
    const digest = hex(new Uint8Array(await crypto.subtle.digest("SHA-256", buf(welcome))));
    const used = new Set(await this.storage.get<string[]>(NS.meta, WELCOMES + inv.roomId) ?? []);
    if (used.has(digest)) {
      await refuse("an invitation this device already used", true);
      return null;
    }
    let joined: ClientState | null = null;
    let usedKp: OwnKeyPackage | null = null;
    for (const kp of await this.storedKeyPackages()) {
      try {
        joined = await this.mls.joinFromWelcome(welcome, kp);
        usedKp = kp;
        break;
      } catch {
        /* not for this key package */
      }
    }
    if (!joined || !usedKp) {
      // Sealed to a package this device holds no keys for: say so where the
      // room is, rather than leave it waiting with nobody knowing why.
      console.warn("chat: welcome did not match any of our key packages", inv.roomId);
      const room = this.rooms_.get(inv.roomId);
      if (room) {
        room.staleReason = "An invitation to this room arrived that this device can't open. Ask a member to add this device again.";
        this.emit("rooms");
      }
      return null;
    }
    // Never back below what this device has held for the room; and, for a
    // room it held, only a group that continues it (the same group, and no
    // device it never saw join) — otherwise the room stays as it is, with
    // the reason, for the person to judge (recover).
    const last = this.lastEpochs.get(inv.roomId) ?? (await this.storage.get<number>(NS.meta, LAST_EPOCH + inv.roomId)) ?? -1;
    if (last >= 0 && this.mls.epoch(joined) <= last) {
      await refuse("an invitation to an earlier point in the room than this device has seen", true);
      return null;
    }
    // First, cheaply, that the Welcome is for the room's own group: the one
    // whose public state the hub verified, as its GroupInfo states it — and
    // for the very epoch the hub-ordered commit that added this device
    // made, whose confirmation tag (checked by every member's client from
    // secrets only the real group holds) the joined epoch must carry. A
    // member could otherwise welcome a newcomer into a look-alike group of
    // their own, on a joiner secret they chose, and cut them off.
    //
    // A Welcome for an earlier epoch than that GroupInfo's is provisional:
    // nothing is sent into the room until catching up has brought this
    // device to the GroupInfo's epoch and the group matches it exactly, and
    // a catch-up that ends short or differs leaves the room refused.
    //
    // Only checked data that disagrees refuses the invite for good. What
    // can't be checked yet — fog unreachable, a follower that hasn't synced
    // the adding commit, a GroupInfo not yet at this epoch — is tried again
    // later, with backoff, and never refuses.
    let same: boolean | "behind" = false;
    let gi: Uint8Array | undefined;
    let ev: EventView | undefined;
    try {
      gi = unb64((await this.api.groupInfo(inv.roomId)).groupInfo);
      const giEpoch = this.mls.groupInfoEpoch(gi);
      if (giEpoch !== undefined && giEpoch < this.mls.epoch(joined)) throw new NotYet("fog's GroupInfo is behind this invitation");
      const { events } = await this.api.events(inv.roomId, { after: inv.seq - 1, limit: 1 });
      ev = events.find((e) => e.seq === inv.seq);
      if (!ev) throw new NotYet("the commit that added this device isn't here yet");
    } catch (e) {
      // Fetching failed, or what's needed isn't there yet: not a verdict.
      this.retryInvite(inv, e);
      return null;
    }
    same = this.mls.sameGroupAs(joined, gi);
    if (same !== false && !this.mls.welcomedBy(joined, unb64(ev.payload))) same = false;
    if (same === false) {
      await refuse("This invitation's keys are not for the room's own group, as fog verified it. This device won't join with them.");
      return null;
    }
    this.inviteAttempts.delete(inv.id);
    // Every leaf of the group it would join must be a device its person's
    // identity key attests, as this device knows that key — with at most
    // one directory fetch for the whole Welcome.
    const bad = await this.unattested(this.mls.members(joined).filter((l) => !bytesEqual(l.signatureKey, this.device.publicKey)), undefined, true);
    if (bad.length) {
      await refuse(`This room holds a device of ${this.nameFor(bad[0].identity)} that their identity key doesn't vouch for. This device won't join it.`);
      return null;
    }
    // No device key under a second name, as for every join.
    try {
      await this.checkKeyNames(inv.roomId, this.mls.members(joined));
    } catch (e) {
      await refuse(e instanceof Error ? e.message : "a device key under another name");
      return null;
    }
    await this.consumeStoredKeyPackage(usedKp);
    const fresh = joined;
    // This Welcome passed every check: an earlier one's refusal, from before
    // this device held the room, no longer stands.
    if (!(await this.storage.get(NS.meta, REFUSED + inv.roomId))) this.clearStale(inv.roomId);
    if (same === "behind") await this.storage.put(NS.meta, PROVISIONAL + inv.roomId, b64u(gi!));
    used.add(digest);
    await this.storage.put(NS.meta, WELCOMES + inv.roomId, Array.from(used).slice(-200));
    await this.recordKeyNames(inv.roomId, this.mls.members(fresh));
    await this.setState(inv.roomId, fresh);
    await this.setCursor(inv.roomId, inv.seq);
    return { behind: same === "behind" };
  }

  // maybeRoll moves a room to a fresh epoch — an empty commit with an
  // update path — well before any sender's chain gets too long for members
  // to follow (100,000 in an epoch): when this device has sent ROLL_AFTER
  // in the epoch, when the epoch is a week old, or (force) when it couldn't
  // follow someone else's. Once per epoch per room; it runs behind the send
  // that prompted it.
  private async maybeRoll(roomId: string, force: boolean) {
    const state = this.states.get(roomId);
    if (!state || this.closed || !this.device) return;
    const key = `${roomId}\u0000${this.mls.epoch(state)}`;
    if (this.rolled.has(key)) return;
    let due = force;
    if (!due) {
      const me = this.mls.members(state).find((l) => bytesEqual(l.signatureKey, this.device.publicKey));
      const chain = me ? (state as unknown as { secretTree: { application?: { generation: number } }[] }).secretTree[me.index * 2]?.application : undefined;
      const since = await this.storage.get<number>(NS.meta, EPOCH_AT + roomId);
      due = (chain?.generation ?? 0) >= ROLL_AFTER || (!!since && Date.now() - since >= ROLL_AGE);
    }
    if (!due) return;
    this.rolled.add(key);
    try {
      await this.commit(roomId, (st) => this.mls.selfUpdate(st), true);
    } catch (e) {
      console.warn("chat: rolling the epoch", roomId, e);
    }
  }

  // doomUnlisted marks for removal, after any join, every leaf of someone
  // fog no longer lists in the room (they left, were cut, banned or reset
  // while this device was away): nothing is sent until they are gone.
  private async doomUnlisted(roomId: string) {
    const roles = this.rooms_.get(roomId)?.roles;
    const state = this.states.get(roomId);
    if (!roles || !state || !Object.keys(roles).length) return;
    const gone = this.mls.members(state).filter((l) => roles[l.identity] === undefined && !bytesEqual(l.signatureKey, this.device.publicKey));
    await this.doom(roomId, gone.map((l) => b64u(l.signatureKey)));
  }

  // It runs once the log has been read as far as it goes: still short of
  // the GroupInfo's epoch, or past it unmatched, the join is refused.
  //
  // Short of the GroupInfo's epoch is not a verdict: the log this device
  // reads (its own fog's copy) may lag the hub by a commit or two. It is
  // read again — five seconds doubling, a dozen times — and nothing is sent
  // meanwhile; only a group that differs at that epoch, or a log that never
  // gets there, is refused.
  private async settleProvisional(roomId: string) {
    const gi = await this.storage.get<string>(NS.meta, PROVISIONAL + roomId);
    if (!gi) return;
    const state = this.states.get(roomId);
    const same = state ? this.mls.sameGroupAs(state, unb64(gi)) : false;
    if (same === true) {
      await this.storage.del(NS.meta, PROVISIONAL + roomId);
      this.provisionalTries.delete(roomId);
      this.clearStale(roomId);
      return;
    }
    const n = (this.provisionalTries.get(roomId) ?? 0) + 1;
    if (same === "behind" && n <= 12) {
      this.provisionalTries.set(roomId, n);
      if (this.closed) return;
      const t = setTimeout(() => void this.catchUp(roomId), Math.min(5000 * 2 ** (n - 1), 600_000));
      (t as { unref?: () => void }).unref?.();
      return;
    }
    this.markStale(roomId, "The room's commits didn't bring this device to the group fog verified. It won't send here.");
  }

  // checkProvisional runs as each commit lands: the commit that brings a
  // provisional join to the GroupInfo's epoch must make it that group
  // exactly, or the join is refused.
  private async checkProvisional(roomId: string, state: ClientState) {
    const gi = await this.storage.get<string>(NS.meta, PROVISIONAL + roomId);
    if (!gi) return;
    const same = this.mls.sameGroupAs(state, unb64(gi));
    if (same === "behind") return;
    if (same === true) {
      await this.storage.del(NS.meta, PROVISIONAL + roomId);
      this.provisionalTries.delete(roomId);
      // A stale mark the wait itself left (a log that looked short) ends here.
      this.clearStale(roomId);
      return;
    }
    this.markStale(roomId, "The room's commits brought this device to a group other than the one fog verified. It won't send here.");
  }

  // retryInvite tries an invite again later — 5 seconds doubling to ten
  // minutes — after something that kept it from being judged.
  private retryInvite(inv: InviteView, why: unknown) {
    const n = (this.inviteAttempts.get(inv.id) ?? 0) + 1;
    this.inviteAttempts.set(inv.id, n);
    console.warn("chat: invite not judged yet; trying again", inv.roomId, why);
    if (this.closed) return;
    const t = setTimeout(() => void this.acceptInvite(inv).catch(() => {}), Math.min(5000 * 2 ** (n - 1), 600_000));
    (t as { unref?: () => void }).unref?.();
  }

  private async maybeReplenish() {
    const me = await this.api.me();
    const mine = me.devices.find((d) => d.id === this.meInfo.deviceId);
    if (mine && mine.keyPackages < KEEP_KEY_PACKAGES / 2) await this.replenishKeyPackages(mine.id);
  }

  private async setState(roomId: string, state: ClientState, b?: WriteBatch) {
    this.states.set(roomId, state);
    await this.write(b, NS.groups, roomId, this.mls.serialize(state));
    // The highest epoch this device has held for the room, kept past
    // leaving it: no Welcome may take it back below (acceptInviteNow); and
    // when its epoch began, for rolling it (maybeRoll).
    const epoch = this.mls.epoch(state);
    const known = this.lastEpochs.get(roomId) ?? (await this.storage.get<number>(NS.meta, LAST_EPOCH + roomId)) ?? -1;
    if (epoch > known) {
      this.lastEpochs.set(roomId, epoch);
      await this.write(b, NS.meta, LAST_EPOCH + roomId, epoch);
      await this.write(b, NS.meta, EPOCH_AT + roomId, Date.now());
      // A doomed leaf a commit has taken out is done with: a device that
      // comes back later under the same key is new, never doomed again.
      const doomed = await this.storage.get<string[]>(NS.meta, DOOMED + roomId);
      if (doomed?.length) {
        const present = new Set(this.mls.members(state).map((l) => b64u(l.signatureKey)));
        const still = doomed.filter((k) => present.has(k));
        if (still.length < doomed.length) await this.storage.put(NS.meta, DOOMED + roomId, still);
      }
    } else this.lastEpochs.set(roomId, known);
    // A stale room stays stale until what made it so is resolved
    // (clearStale): applying something else doesn't fix it.
    const room = this.rooms_.get(roomId);
    if (room && room.sync !== "stale") room.sync = "ready";
  }

  // clearStale ends a room's stale state, once what caused it is resolved.
  private clearStale(roomId: string) {
    void this.storage.del(NS.meta, STALE + roomId).catch(() => {});
    const room = this.rooms_.get(roomId);
    if (!room) return;
    room.sync = "ready";
    delete room.staleReason;
    this.emit("rooms");
  }

  private async setCursor(roomId: string, seq: number, b?: WriteBatch) {
    this.cursors.set(roomId, seq);
    await this.write(b, NS.cursors, roomId, seq);
    const room = this.rooms_.get(roomId);
    if (room) room.cursor = seq;
  }

  // write stores a row now, or holds it in the batch; a later write to the
  // same row in the batch replaces the earlier one.
  private async write(b: WriteBatch | undefined, ns: string, key: string, value: unknown) {
    if (b) b.rows.set(`${ns}\u0000${key}`, { ns, key, value });
    else await this.storage.put(ns, key, value);
  }

  // flush lands a batch: its rows in one transaction, then one "messages"
  // event per room it touched, then the room refresh a commit asked for.
  private async flush(b: WriteBatch) {
    const rows = Array.from(b.rows.values());
    b.rows.clear();
    if (rows.length) {
      if (this.storage.putMany) await this.storage.putMany(rows);
      else for (const r of rows) await this.storage.put(r.ns, r.key, r.value);
    }
    const rooms = Array.from(b.messageRooms);
    b.messageRooms.clear();
    for (const roomId of rooms) this.emit("messages", { roomId });
    if (rooms.length) this.touchBackup();
    if (b.refreshRooms) {
      b.refreshRooms = false;
      await this.refreshRooms();
    }
  }

  // serial runs fn in a room's one queue. Every read-modify-write of a
  // room's MLS state — a send, a receipt, a commit, history shared, an
  // event applied, a join — goes through it, so no two ever read the same
  // state: no generation is used twice, and no epoch is written back over a
  // newer one. fn must not wait on the queue itself (catchUp, serial).
  private serial<T>(roomId: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.processing.get(roomId) ?? Promise.resolve();
    const run = prev.then(fn, fn);
    this.processing.set(roomId, run.then(() => {}, () => {}));
    return run;
  }

  // catchUp processes every event after the cursor, one room at a time.
  private catchUp(roomId: string): Promise<void> {
    return this.serial(roomId, () => this.catchUpNow(roomId)).catch((e) => this.emit("error", e));
  }

  private async catchUpNow(roomId: string) {
    if (!this.states.has(roomId)) return;
    for (;;) {
      const after = this.cursors.get(roomId) ?? 0;
      const res = await this.api.events(roomId, { after, limit: 200 });
      if (res.events.length === 0) break;
      const b = newBatch();
      try {
        for (const ev of res.events) await this.applyEvent(ev, b);
      } finally {
        await this.flush(b);
      }
      if (res.events.length < 200) break;
    }
    // A provisional join that the log has now brought as far as it goes
    // either matches the group fog verified, or is refused.
    await this.settleProvisional(roomId);
  }

  // applyEvent is the one place an event changes state. Commits advance the
  // group; private messages decrypt into the store; our own events are
  // already reflected locally. Writes go into the caller's batch, or into
  // one of the event's own that lands before this returns.
  private async applyEvent(ev: EventView, batch?: WriteBatch) {
    const b = batch ?? newBatch();
    try {
      await this.applyEventTo(ev, b);
    } finally {
      if (!batch) await this.flush(b);
    }
  }

  private async applyEventTo(ev: EventView, b: WriteBatch) {
    const roomId = ev.roomId;
    const cursor = this.cursors.get(roomId) ?? 0;
    if (ev.seq <= cursor) return;
    const state = this.states.get(roomId);
    if (!state) return;
    // What an MLS event is comes from its own bytes, never from the kind or
    // sender the hub labelled it with: a commit relabelled as a private
    // message still goes through every check a commit does, and anything
    // that is neither a public commit nor a private application message is
    // refused.
    const mlsEvent = ev.kind === "mls_commit" || ev.kind === "mls_private";
    let wire: Uint8Array | undefined;
    let what: ReturnType<Mls["kindOf"]> = { kind: "other" };
    if (mlsEvent) {
      try {
        wire = unb64(ev.payload);
        what = this.mls.kindOf(wire);
      } catch {
        what = { kind: "other" };
      }
      if (what.kind === "other") {
        // Fog labelled as MLS something that isn't a commit or a message:
        // fog's fault, and one bad event — never the room's state, and
        // never a reason to rejoin on fog's word.
        await this.storeMessage({
          id: `undecryptable:${ev.seq}`, roomId, seq: ev.seq, sender: ev.sender ?? "", senderDevice: "", hubAt: ev.hubAt, sentAt: ev.hubAt,
          content: { v: 1, type: "message", system: "undecryptable" }, mine: false, checked: true,
        }, b);
        await this.setCursor(roomId, ev.seq, b);
        return;
      }
    }
    // A commit for an epoch this device is already past is its own, applied
    // when it was made; one for this epoch is applied whoever fog says sent
    // it. A private message fog says this device sent is skipped (fog could
    // only hide a message by that, which it can do anyway).
    const isCommit = what.kind === "commit" && what.epoch >= this.mls.epoch(state);
    const ownCommit = what.kind === "commit" && what.epoch < this.mls.epoch(state);
    const own = ev.senderDevice === this.meInfo.deviceKey;
    if (ownCommit) {
      // nothing to apply
    } else if (isCommit && wire && (await this.adoptPending(roomId, wire, ev, b))) {
      b.refreshRooms = true;
    } else if (isCommit && wire) {
      try {
        const refused = await this.storage.get<RefusedCommit>(NS.meta, REFUSED + roomId);
        if (refused && what.kind === "commit" && this.mls.epoch(state) === refused.epoch && what.epoch === refused.epoch + 1) {
          // The commit after one this device refused for a device nobody
          // vouches for: it counts only as the removal of that device.
          await this.applyRecovery(roomId, state, refused, wire, ev, b);
        } else {
          const v = await this.vetCommit(roomId, state, wire);
          if (v.bad.length) {
            // Kept, so a member with the right can remove what was refused.
            if (!refused) await this.storage.put(NS.meta, REFUSED + roomId, { epoch: what.kind === "commit" ? what.epoch : -1, wire: b64u(wire), bad: v.bad.map((l) => b64u(l.signatureKey)) } satisfies RefusedCommit);
            throw new RefusedMessage(`This room was given a device of ${this.nameFor(v.bad[0].identity)} that their identity key doesn't vouch for. This device refused it and won't send here until a member removes it.`);
          }
          await this.landCommit(roomId, v, ev, b);
        }
      } catch (e) {
        console.warn("chat: cannot apply commit", roomId, ev.seq, e);
        this.markStale(roomId, e instanceof RefusedMessage ? e.message.replace(/^mls: refused: /, "") : undefined);
        await this.setCursor(roomId, ev.seq, b);
        return;
      }
      b.refreshRooms = true;
    } else if (what.kind === "application" && wire && !own) {
      try {
        const { newState, plaintext, sender: proven } = await this.mls.process(state, wire, {}, "application");
        await this.setState(roomId, newState, b);
        // Who sent it is what MLS proves — the leaf whose key signed it —
        // not what the hub says: a hub could otherwise relabel one member's
        // words, or a delete or pin, as another's.
        if (plaintext && proven && (proven.identity !== ev.sender || b64u(proven.signatureKey) !== ev.senderDevice)) {
          console.warn("chat: the hub named a different sender than signed the message; going by the signature", roomId, ev.seq);
        }
        if (plaintext && proven) {
          const sender = proven.identity;
          const content = this.sanitize(decodeContent(plaintext), sender, roomId);
          if (content.type === "history") {
            await this.flush(b);
            await this.receiveHistory(roomId, sender, content);
          } else {
            await this.storeMessage({
              // A member names its message; failing a ULID, the event's
              // seq does — never an id in the client's own namespaces.
              id: isULID(content.id) ? content.id! : isULID(ev.id) ? ev.id : `event:${ev.seq}`, roomId, seq: ev.seq, sender, senderDevice: b64u(proven.signatureKey),
              hubAt: ev.hubAt, sentAt: content.sentAt ?? ev.sentAt ?? ev.hubAt, content, mine: sender === this.meInfo.addr,
              ...this.judge(roomId, sender, content, sender === ev.sender ? ev.senderRole : undefined),
            }, b);
          }
        }
      } catch (e) {
        console.warn("chat: cannot decrypt", roomId, ev.seq, e);
        // A sender too far along their chain for this device to follow: a
        // fresh epoch resets every chain, so this device starts one.
        if (e instanceof RefusedMessage && /too far/.test(e.message)) void this.maybeRoll(roomId, true).catch(() => {});
        // One message this device can't read — garbage from a member, a gap
        // past the ratchet's reach, a generation another tab already used —
        // costs that message, shown as unreadable, never the room. (A room
        // this device joined into a look-alike group is refused at the join,
        // by the commit's confirmation tag.)
        await this.storeMessage({
          id: `undecryptable:${ev.seq}`, roomId, seq: ev.seq, sender: ev.sender ?? "", senderDevice: "", hubAt: ev.hubAt, sentAt: ev.hubAt,
          content: { v: 1, type: "message", system: "undecryptable" }, mine: false, checked: true,
        }, b);
      }
    } else if (ev.kind === "system") {
      // A departure (a household cut, a ban, a leave, an identity reset)
      // names people the hub no longer counts as members. The hub holds no
      // key, so a member device commits the MLS removal of their leaves —
      // any member may, for people already out — and until one does they
      // can still decrypt. An identity reset also drops what this device
      // trusted about them and says so in the room.
      let sys: { kind?: string; left?: string[]; devices?: string[] } = {};
      try {
        sys = JSON.parse(new TextDecoder().decode(unb64(ev.payload)));
      } catch {
        /* not JSON: nothing to act on */
      }
      if (sys.kind && DEPARTURES.has(sys.kind) && Array.isArray(sys.left) && sys.left.length) {
        if (sys.left.includes(this.meInfo.addr)) {
          // This person is out: the hub sends nothing more, and the keys
          // this device holds for the room are of no further use.
          await this.setCursor(roomId, ev.seq, b);
          await this.flush(b);
          await this.forgetRoom(roomId);
          b.refreshRooms = true;
          return;
        }
        const left = sys.left.filter((a) => typeof a === "string");
        if (sys.kind === "identity-reset") {
          for (const addr of left) {
            // What this device trusts about the person changes only on the
            // word of a hub that would know: their own fog, or this one,
            // about someone who was in the room. Any other hub's line is
            // that room's to show, and touches nothing else.
            if (this.resetTrusted(roomId, addr, state)) await this.forgetIdentity(addr);
            await this.storeMessage({
              id: `identity-reset:${ev.seq}:${addr}`, roomId, seq: ev.seq, sender: addr, senderDevice: "", hubAt: ev.hubAt, sentAt: ev.hubAt,
              content: { v: 1, type: "message", system: "identity-reset" }, mine: false, checked: true,
            }, b);
          }
        }
        await this.setCursor(roomId, ev.seq, b);
        // The leaves they hold now — by key, so a later re-add's new leaves
        // are never taken for them.
        const gone = new Set(left);
        await this.doom(roomId, this.mls.members(state).filter((l) => gone.has(l.identity)).map((l) => b64u(l.signatureKey)));
        return;
      }
      // A device its person revoked: its leaf goes, whoever removes it.
      if (sys.kind === "device-revoked" && Array.isArray(sys.devices) && sys.devices.length) {
        // What this device checked about it no longer holds.
        for (const k of Array.from(this.attested)) if (sys.devices.some((d) => typeof d === "string" && k.endsWith("\u0000" + d))) this.attested.delete(k);
        await this.setCursor(roomId, ev.seq, b);
        if (!sys.devices.includes(this.meInfo.deviceKey)) await this.doom(roomId, sys.devices, true);
        return;
      }
      // A device that left by itself (its person stays): its leaf goes,
      // whoever removes it, and it comes back as any new device of theirs.
      if (sys.kind === "device-left" && Array.isArray(sys.devices) && sys.devices.length) {
        await this.setCursor(roomId, ev.seq, b);
        if (!sys.devices.includes(this.meInfo.deviceKey)) await this.doom(roomId, sys.devices.filter((d) => typeof d === "string"));
        return;
      }
    }
    await this.setCursor(roomId, ev.seq, b);
  }

  // doom notes leaves (by signature key) the hub has said must go — of
  // people it counted out, or of a revoked device — and starts their
  // removal. Until it lands, this device sends nothing into the room: a
  // message encrypted now would still reach those leaves (sendContent).
  //
  // revoked marks keys of devices their person revoked: those never come
  // back, whoever can or can't remove them.
  private async doom(roomId: string, keys: string[], revoked = false) {
    if (!keys.length) return;
    const held = new Set(await this.storage.get<string[]>(NS.meta, DOOMED + roomId) ?? []);
    for (const k of keys) held.add(k);
    await this.storage.put(NS.meta, DOOMED + roomId, Array.from(held));
    if (revoked) {
      const r = new Set(await this.storage.get<string[]>(NS.meta, REVOKED + roomId) ?? []);
      for (const k of keys) r.add(k);
      await this.storage.put(NS.meta, REVOKED + roomId, Array.from(r));
    }
    void this.removeDoomed(roomId, true).catch(() => {});
  }

  // removeDoomed commits the removal of a room's doomed leaves. Every member
  // device that sees a departure tries, after a moment's jitter (none when
  // it is about to send); the first commit lands, and the rest find nothing
  // left to remove. Leaves are found by key on each attempt, never by an
  // index from an older epoch, which may since have gone to someone else.
  private async removeDoomed(roomId: string, jitter: boolean) {
    if (!(await this.storage.get<string[]>(NS.meta, DOOMED + roomId))?.length) return;
    if (jitter) {
      await new Promise((r) => setTimeout(r, 200 + Math.random() * 1500));
      await (this.processing.get(roomId) ?? Promise.resolve());
    }
    // Read after the wait: a commit applied meanwhile may have taken the
    // leaves out, and a device back under the same key is not doomed.
    const keys = new Set(await this.storage.get<string[]>(NS.meta, DOOMED + roomId) ?? []);
    if (!keys.size) return;
    const doomed = (l: { signatureKey: Uint8Array }) => keys.has(b64u(l.signatureKey));
    const state = this.states.get(roomId);
    if (state && this.mls.members(state).some(doomed)) {
      try {
        await this.commit(roomId, async (st) => {
          // Again inside the room's queue, for the same reason.
          const live = new Set(await this.storage.get<string[]>(NS.meta, DOOMED + roomId) ?? []);
          const idx = this.mls.members(st).filter((l) => live.has(b64u(l.signatureKey))).map((l) => l.index);
          if (!idx.length) throw new NothingToDo();
          return this.mls.removeLeaves(st, idx);
        }, false);
      } catch (e) {
        if (e instanceof ApiError && e.code === "forbidden") {
          // Refused. A departed person fog now lists in the room again is
          // back, and their leaves theirs to read with. But a revoked
          // device never comes back, nor does anyone fog doesn't list: the
          // removal waits for someone the hub lets make it, and until then
          // nothing is sent here — never to a device that should be gone.
          const revoked = new Set(await this.storage.get<string[]>(NS.meta, REVOKED + roomId) ?? []);
          const roles = this.rooms_.get(roomId)?.roles ?? {};
          const still = this.mls.members(this.states.get(roomId) ?? state).filter(doomed).filter((l) => revoked.has(b64u(l.signatureKey)) || roles[l.identity] === undefined);
          if (!still.length) {
            await this.storage.del(NS.meta, DOOMED + roomId);
            return;
          }
          await this.storage.put(NS.meta, DOOMED + roomId, still.map((l) => b64u(l.signatureKey)));
          throw new RefusedMessage(`This room is waiting for someone who may remove ${this.nameFor(still[0].identity)}'s device that should be gone. Nothing is sent here until then.`);
        }
        if (!(e instanceof NothingToDo)) throw e;
      }
    }
    const now = this.states.get(roomId);
    if (!now || !this.mls.members(now).some(doomed)) {
      await this.storage.del(NS.meta, DOOMED + roomId);
      await this.storage.del(NS.meta, REVOKED + roomId);
    }
  }

  // rulesFor is what an incoming commit in a room is held to.
  private rulesFor(roomId: string): CommitRules {
    const room = this.rooms_.get(roomId);
    // A DM's set is known where its hub is: a replica doesn't carry it.
    if (room?.kind !== "dm" || !room.dm?.length) return {};
    return { dm: new Set([this.meInfo.addr, ...room.dm]) };
  }

  // may says whether a person's role in a room holds a capability, by the
  // roles the hub reports. Someone it no longer lists — they left before
  // this device caught up — gets ifUnknown.
  private may(roomId: string, addr: string, cap: string, ifUnknown: boolean, roleAtSend?: number): boolean {
    const room = this.rooms_.get(roomId);
    const idx = roleAtSend || room?.roles?.[addr];
    if (!room || idx === undefined) return ifUnknown;
    const caps = room.policy.roles.find((r) => r.index === idx)?.capabilities ?? [];
    return caps.includes("*") || caps.includes(cap);
  }

  // can says whether this device's person may do something in a room.
  can(roomId: string, cap: string): boolean {
    return this.may(roomId, this.meInfo.addr, cap, false);
  }

  // judge holds an arriving item to the room's content rules (policy
  // capabilities the hub can't see, since it can't read content), as the
  // sender's role stands when it arrives. Acts on one's own messages are
  // allowed for someone the hub no longer lists; acts on others' are not.
  //
  // roleAtSend is the sender's role the hub recorded when it sequenced the
  // event; given, it decides, so a device catching up judges each act by
  // the role its sender held then, not now.
  private judge(roomId: string, sender: string, c: Content, roleAtSend?: number): Pick<Message, "checked" | "denied" | "moderator"> {
    const may = (cap: string, ifUnknown: boolean) => this.may(roomId, sender, cap, ifUnknown, roleAtSend);
    switch (c.type) {
      case "reaction":
        return { checked: true, denied: !may("canReact", true) };
      case "edit":
        return { checked: true, denied: !may("canEditOwn", true) };
      case "delete": {
        const target = c.replaces ? this.messages_.get(roomId)?.get(c.replaces) : undefined;
        if (target?.sender === sender) return { checked: true, denied: !may("canDeleteOwn", true) };
        return { checked: true, moderator: may("canDeleteOtherMessage", false) };
      }
      case "pin": {
        const moderator = may("canPin", false);
        return { checked: true, moderator, denied: !moderator };
      }
      default:
        return { checked: true };
    }
  }

  // storeMessage files an item. An id already held by another event stays
  // as it is: an item's id is the sender's choice, made inside the
  // ciphertext, and a later item claiming it would otherwise replace (and
  // so erase) someone else's message, a moderator's act or a membership line.
  private async storeMessage(m: Message, b?: WriteBatch) {
    let map = this.messages_.get(m.roomId);
    if (!map) this.messages_.set(m.roomId, (map = new Map()));
    const held = map.get(m.id);
    if (held && held.seq !== m.seq) return;
    map.set(m.id, m);
    this.roomVersion.set(m.roomId, (this.roomVersion.get(m.roomId) ?? 0) + 1);
    const room = this.rooms_.get(m.roomId);
    if (room && m.content.type === "message") room.lastMessageAt = m.hubAt;
    if (b) {
      await this.write(b, NS.messages, `${m.roomId}/${m.id}`, m);
      b.messageRooms.add(m.roomId);
      return;
    }
    await this.storage.put(NS.messages, `${m.roomId}/${m.id}`, m);
    this.emit("messages", { roomId: m.roomId });
    this.touchBackup();
  }

  // ---- the stream ----

  private openStream() {
    if (!this.wsWanted || this.closed) return;
    const WS = this.opts.WebSocketImpl ?? WebSocket;
    let ws: WebSocket;
    try {
      ws = new WS(this.api.streamUrl());
    } catch (e) {
      this.emit("error", e);
      setTimeout(() => this.openStream(), reconnectDelay(this.reconnects++));
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      this.reconnects = 0;
      const since: Record<string, number> = {};
      for (const [id, seq] of this.cursors) since[id] = seq;
      ws.send(JSON.stringify({ since }));
      this.emit("status", "online");
    };
    ws.onmessage = (e) => {
      let frame: { kind: string; roomId?: string; event?: EventView; person?: string; signal?: string; online?: string[] };
      try {
        frame = JSON.parse(String(e.data));
      } catch {
        return;
      }
      this.onFrame(frame);
    };
    ws.onclose = () => {
      this.ws = null;
      this.emit("status", "offline");
      if (this.wsWanted && !this.closed) setTimeout(() => this.openStream(), reconnectDelay(this.reconnects++));
    };
    ws.onerror = () => {
      /* onclose follows */
    };
  }

  private onFrame(f: { kind: string; roomId?: string; event?: EventView; person?: string; signal?: string; online?: string[] }) {
    switch (f.kind) {
      case "event":
        if (f.event) {
          const ev = f.event;
          void this.serial(ev.roomId, async () => {
            const cursor = this.cursors.get(ev.roomId) ?? 0;
            if (ev.seq > cursor + 1) await this.catchUpNow(ev.roomId);
            else await this.applyEvent(ev);
          }).catch((e) => this.emit("error", e));
        }
        break;
      case "synced":
        if (f.online) {
          this.online_ = new Set(f.online);
          this.emit("presence");
        }
        break;
      case "rooms":
      case "invite":
        this.refreshRooms().then(() => this.acceptInvites()).then(() => {
          if (f.roomId) return this.catchUp(f.roomId);
        }).catch((e) => this.emit("error", e));
        break;
      case "ephemeral":
        if (!f.person) break;
        if (f.signal === "typing" && f.roomId) {
          let m = this.typing_.get(f.roomId);
          if (!m) this.typing_.set(f.roomId, (m = new Map()));
          m.set(f.person, Date.now());
          this.emit("typing", { roomId: f.roomId });
          setTimeout(() => this.emit("typing", { roomId: f.roomId }), 5500);
        } else if (f.signal === "presence:online") {
          this.online_.add(f.person);
          this.emit("presence");
        } else if (f.signal === "presence:offline") {
          this.online_.delete(f.person);
          this.emit("presence");
        }
        break;
    }
  }

  online(): Set<string> {
    return this.online_;
  }

  typingIn(roomId: string): string[] {
    const m = this.typing_.get(roomId);
    if (!m) return [];
    const now = Date.now();
    return Array.from(m.entries()).filter(([p, at]) => now - at < 5000 && p !== this.meInfo.addr).map(([p]) => p);
  }

  // ---- reading ----

  // messages renders a room's timeline: edits applied, deletes hidden,
  // reactions and pins folded in, thread replies counted onto their roots.
  // Without a topic it lists the roots; with one, that thread. The list is
  // cached and frozen, so a UI can compare it by identity: that changes
  // only when something in the list did.
  messages(roomId: string, opts: { topicId?: string } = {}): readonly MessageView[] {
    const fold = this.fold(roomId);
    const topic = opts.topicId;
    const key = topic ?? "";
    const hit = fold.lists.get(key);
    if (hit) return hit;
    let list: readonly MessageView[] = topic ? fold.views.filter((v) => inThread(v, topic)) : fold.views.filter((v) => !isReply(v));
    const prev = fold.stale?.get(key);
    if (prev && prev.length === list.length && prev.every((v, i) => v === list[i])) list = prev;
    else Object.freeze(list);
    fold.lists.set(key, list);
    return list;
  }

  // timeline is the room's fold, recomputed only when it changed.
  private timeline(roomId: string): readonly MessageView[] {
    return this.fold(roomId).views;
  }

  // fold refolds a room that changed. A message whose view came out the
  // same as last time keeps the old object, so a renderer memoised on it
  // redraws only what moved — one new message doesn't repaint the room.
  private fold(roomId: string): Fold {
    const version = this.roomVersion.get(roomId) ?? 0;
    const hit = this.folded.get(roomId);
    if (hit && hit.version === version && hit.names === this.namesVersion) return hit;
    let views = foldTimeline(roomId, this.messages_.get(roomId)?.values() ?? [], (addr) => this.nameFor(addr));
    if (hit) {
      const prev = new Map(hit.views.map((v) => [v.id, v]));
      views = views.map((v) => {
        const p = prev.get(v.id);
        return p && sameView(p, v) ? p : v;
      });
    }
    const fold: Fold = { version, names: this.namesVersion, views: Object.freeze(views), lists: new Map(), stale: hit?.lists };
    this.folded.set(roomId, fold);
    return fold;
  }

  // readMarks folds the room's read receipts: who has read up to which seq.
  // It is cached with the room's messages, so a render that asks again
  // costs nothing until a receipt (or anything else) lands.
  readMarks(roomId: string): ReadonlyMap<string, number> {
    const version = this.roomVersion.get(roomId) ?? 0;
    const hit = this.marks_.get(roomId);
    if (hit && hit.version === version) return hit.marks;
    const out = new Map<string, number>();
    for (const m of this.messages_.get(roomId)?.values() ?? []) {
      if (m.content.type !== "receipt" || !m.content.readSeq) continue;
      if ((out.get(m.sender) ?? 0) < m.content.readSeq) out.set(m.sender, m.content.readSeq);
    }
    this.marks_.set(roomId, { version, marks: out });
    return out;
  }

  // preview asks fog for a link card (only when the person opted in) and
  // returns it for the sender to encrypt into the message.
  async preview(url: string) {
    return this.api.preview(url);
  }

  // ---- preferences and push ----

  async prefs(): Promise<Prefs> {
    this.prefs_ = await this.api.prefs();
    return this.prefs_;
  }

  async setPrefs(p: Prefs): Promise<Prefs> {
    this.prefs_ = await this.api.setPrefs(p);
    return this.prefs_;
  }

  cachedPrefs(): Prefs | null {
    return this.prefs_;
  }

  // ---- saved messages: a local bookmark, never sent anywhere ----

  async save(roomId: string, messageId: string) {
    await this.storage.put(NS.saved, `${roomId}/${messageId}`, { roomId, messageId, at: new Date().toISOString() });
    this.emit("messages", { roomId });
  }

  async unsave(roomId: string, messageId: string) {
    await this.storage.del(NS.saved, `${roomId}/${messageId}`);
    this.emit("messages", { roomId });
  }

  async saved(): Promise<{ roomId: string; messageId: string; at: string }[]> {
    const rows = await this.storage.list<{ roomId: string; messageId: string; at: string }>(NS.saved);
    return rows.map((r) => r.value).sort((a, b) => b.at.localeCompare(a.at));
  }

  // exportRoom renders a room's decrypted timeline as JSON lines, threads
  // included — for the person to keep or hand to Files.
  exportRoom(roomId: string): string {
    const raw = Array.from(this.messages_.get(roomId)?.values() ?? [])
      .filter((m) => m.content.type !== "history" && m.content.type !== "receipt")
      .sort((a, b) => a.seq - b.seq);
    return raw.map((m) => JSON.stringify({ id: m.id, seq: m.seq, sender: m.sender, sentAt: m.sentAt, hubAt: m.hubAt, content: m.content }, (_k, v) => (v instanceof Uint8Array ? b64u(v) : v))).join("\n") + "\n";
  }

  // search scans decrypted text across every room this device holds,
  // thread replies included.
  search(query: string, limit = 50): MessageView[] {
    const q = query.trim().toLowerCase();
    if (!q) return [];
    const hits: MessageView[] = [];
    for (const roomId of this.messages_.keys()) {
      for (const v of this.timeline(roomId)) if (!v.deleted && v.text.toLowerCase().includes(q)) hits.push(v);
    }
    return hits.sort((a, b) => b.hubAt.localeCompare(a.hubAt)).slice(0, limit);
  }

  // ---- writing ----

  private requireState(roomId: string): ClientState {
    const s = this.states.get(roomId);
    if (!s) throw new Error("this device does not hold the keys for that room");
    return s;
  }

  // sendContent encrypts and submits one content item, retrying once if the
  // room moved to a new epoch underneath us.
  private async sendContent(roomId: string, c: Content, hints?: { mentions?: string[] }): Promise<Message> {
    // Nothing goes into a room this device refused something in, or joined
    // provisionally and hasn't matched to the group fog verified.
    const staleWhy = await this.storage.get<string>(NS.meta, STALE + roomId);
    if (this.rooms_.get(roomId)?.sync === "stale" || staleWhy) throw new RefusedMessage(this.rooms_.get(roomId)?.staleReason ?? staleWhy ?? "this device refused something in this room and won't send here");
    if (await this.storage.get(NS.meta, PROVISIONAL + roomId)) throw new RefusedMessage("this device hasn't yet matched this room's group to the one fog verified");
    const id = c.id ?? ulid();
    c.id = id;
    const sentAt = (c.sentAt = c.sentAt ?? new Date().toISOString());
    const plaintext = encodeContent(c);
    for (let attempt = 0; attempt < 5; attempt++) {
      // Leaves the hub said must go are removed before anything more is
      // encrypted to the group: a departed person's or a revoked device's
      // keys get nothing sent after their departure. Another member's
      // removal racing ours is caught up on, and checked again.
      try {
        await this.removeDoomed(roomId, false);
      } catch (e) {
        if (e instanceof ApiError && e.code === "epoch_stale" && attempt < 4) {
          await this.catchUp(roomId);
          continue;
        }
        throw e;
      }
      // Encrypting and keeping the advanced state happen in the room's
      // queue, before the network: the generation is spent whether or not
      // the hub takes the message — a message lost is better than a
      // generation used twice, or a state rewound.
      const sent = await this.serial(roomId, async (): Promise<Message | "stale"> => {
        const state = this.requireState(roomId);
        const { wire, newState } = await this.mls.encrypt(state, plaintext);
        await this.setState(roomId, newState);
        try {
          const res = await this.api.submit(roomId, { id, kind: "mls_private", payload: b64u(wire), sentAt, hints, expiresAt: c.expires });
          const m: Message = {
            id, roomId, seq: res.event.seq, sender: this.meInfo.addr, senderDevice: this.meInfo.deviceKey,
            hubAt: res.event.hubAt, sentAt, content: c, mine: true, ...this.judge(roomId, this.meInfo.addr, c, res.event.senderRole),
          };
          await this.storeMessage(m);
          if (res.event.seq === (this.cursors.get(roomId) ?? 0) + 1) await this.setCursor(roomId, res.event.seq);
          return m;
        } catch (e) {
          if (e instanceof ApiError && e.code === "epoch_stale" && attempt < 4) return "stale";
          throw e;
        }
      });
      if (sent !== "stale") {
        void this.maybeRoll(roomId, false).catch(() => {});
        return sent;
      }
      await this.catchUp(roomId);
    }
    throw new Error("could not send after the room changed repeatedly");
  }

  send(roomId: string, text: string, opts: { topicId?: string; inReplyTo?: string; mentions?: string[]; attachments?: Attachment[]; expires?: string; preview?: Content["preview"] } = {}) {
    const c: Content = { v: 1, type: "message", body: { format: "text/markdown", text } };
    if (opts.topicId) c.topicId = opts.topicId;
    if (opts.inReplyTo) c.inReplyTo = opts.inReplyTo;
    if (opts.mentions?.length) c.mentions = opts.mentions;
    if (opts.attachments?.length) c.attachments = opts.attachments;
    if (opts.expires) c.expires = opts.expires;
    if (opts.preview) c.preview = opts.preview;
    return this.sendContent(roomId, c, opts.mentions?.length ? { mentions: opts.mentions } : undefined);
  }

  edit(roomId: string, messageId: string, text: string) {
    return this.sendContent(roomId, { v: 1, type: "edit", replaces: messageId, body: { format: "text/markdown", text } });
  }

  delete(roomId: string, messageId: string) {
    return this.sendContent(roomId, { v: 1, type: "delete", replaces: messageId });
  }

  react(roomId: string, messageId: string, emoji: string, add = true) {
    return this.sendContent(roomId, { v: 1, type: "reaction", reaction: { target: messageId, emoji, add } });
  }

  pin(roomId: string, messageId: string, pinned = true) {
    return this.sendContent(roomId, { v: 1, type: "pin", pin: { target: messageId, pinned } });
  }

  async markRead(roomId: string, seq?: number) {
    const room = this.rooms_.get(roomId);
    const to = seq ?? room?.seq ?? 0;
    const before = room?.readSeq ?? 0;
    await this.api.markRead(roomId, to);
    if (room) {
      room.readSeq = Math.max(room.readSeq, to);
      room.unread = Math.max(0, room.seq - room.readSeq);
      this.emit("rooms");
    }
    // An encrypted receipt into the room, when the person chose to share
    // them and the mark actually moved past a message.
    if (this.prefs_?.receipts && room && to > before && this.states.has(roomId)) {
      const lastMsg = Array.from(this.messages_.get(roomId)?.values() ?? []).filter((m) => m.content.type === "message" && !m.mine).some((m) => m.seq > before && m.seq <= to);
      if (lastMsg) this.sendContent(roomId, { v: 1, type: "receipt", readSeq: to }).catch(() => {});
    }
  }

  typing(roomId: string) {
    if (!this.typingThrottle.ready(roomId)) return Promise.resolve();
    return this.api.typing(roomId).catch(() => {});
  }

  // ---- attachments ----

  async uploadAttachment(roomId: string, bytes: Uint8Array, name: string, mime: string): Promise<Attachment> {
    const key = crypto.getRandomValues(new Uint8Array(32));
    const nonce = crypto.getRandomValues(new Uint8Array(12));
    const k = await crypto.subtle.importKey("raw", buf(key), "AES-GCM", false, ["encrypt"]);
    const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: buf(nonce) }, k, buf(bytes)));
    const res = await this.api.putBlob(roomId, ct);
    return { blob: res.sha256, key, nonce, name, mime, size: bytes.length };
  }

  async openAttachment(att: Attachment): Promise<Uint8Array> {
    const ct = await this.api.getBlob(att.blob);
    const k = await crypto.subtle.importKey("raw", buf(att.key), "AES-GCM", false, ["decrypt"]);
    return new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: buf(att.nonce) }, k, buf(ct)));
  }

  // ---- rooms and membership ----

  async createRoom(input: { kind: "channel" | "group" | "dm"; label?: string; topic?: string; participants?: string[]; workspaceVisible?: boolean }): Promise<Room> {
    const groupId = crypto.getRandomValues(new Uint8Array(16));
    const kp = await this.mls.keyPackage(this.meInfo.addr, this.device);
    const state = await this.mls.createGroup(groupId, kp);
    // The hub verifies every later commit against the group's public state,
    // which starts from this GroupInfo.
    const groupInfo = b64u(await this.mls.groupInfo(state));
    const res = await this.api.createRoom({ ...input, groupId: b64u(groupId), groupInfo });
    const room = res.room;
    if (room.epoch !== 0 || room.seq !== 1) {
      // An existing DM came back: we hold no state for it; join through
      // a Welcome from someone in it, or an external join if allowed.
      await this.refreshRooms();
      return this.rooms_.get(room.id)!;
    }
    await this.setState(room.id, state);
    await this.setCursor(room.id, room.seq);

    await this.refreshRooms();
    const others = (input.participants ?? []).filter((a) => a !== this.meInfo.addr);
    if (others.length) await this.addMembers(room.id, others);
    return this.rooms_.get(room.id)!;
  }

  // addMembers fetches key material for each person and commits Adds for
  // the devices their identity key attests: each device's attestation is
  // checked here against that identity key, and its key package must be that
  // device's, under that person's address. A device without a valid
  // attestation is what anyone holding only the person's sign-in (their
  // household's operator, say) could register, and is left out. A changed
  // identity key drops this device's verification mark for them.
  async addMembers(roomId: string, addrs: string[]): Promise<{ added: string[]; skipped: { addr: string; reason: string }[] }> {
    const kps: import("ts-mls").KeyPackage[] = [];
    const added: string[] = [];
    const skipped: { addr: string; reason: string }[] = [];
    for (const addr of addrs) {
      const m = /^fog:([^/]+)\/(.+)$/.exec(addr);
      if (!m) {
        skipped.push({ addr, reason: "bad address" });
        continue;
      }
      let km: KeyMaterial;
      try {
        km = await this.api.keysByAddr(addr);
      } catch (e) {
        skipped.push({ addr, reason: e instanceof Error ? e.message : "no key material" });
        continue;
      }
      if (!km.identityKey) {
        skipped.push({ addr, reason: "they have no identity key yet" });
        continue;
      }
      // Adding someone is deliberate: the key fog serves is the one taken,
      // which also ends a reset's wait for it.
      if (this.resets.has(addr)) await this.acceptIdentity(addr, km.identityKey, false);
      else await this.pinIdentity(addr, km.identityKey);
      let got = 0;
      // A device already in the group is skipped: adding a person adds the
      // devices of theirs it doesn't hold — a device that left comes back.
      const held = this.states.get(roomId);
      const inGroup = new Set(held ? this.mls.members(held).map((l) => b64u(l.signatureKey)) : []);
      for (const d of km.devices) {
        if (inGroup.has(d.deviceKey)) continue;
        const kp = this.attestedPackage(addr, km.identityKey, d);
        if (typeof kp === "string") {
          if (kp) skipped.push({ addr, reason: `${d.name || "a device"}: ${kp}` });
          continue;
        }
        kps.push(kp);
        got++;
      }
      if (got) added.push(addr);
      else skipped.push({ addr, reason: "none of their verified devices has published keys" });
    }
    if (kps.length === 0) return { added, skipped };
    await this.commit(roomId, (state) => this.mls.addMembers(state, kps), true);
    return { added, skipped };
  }

  // attestedPackage returns a device's key package when its person's
  // identity key attests the device and the package is that device's, under
  // that person's address; otherwise why not ("" for a device that simply
  // has no package left).
  private attestedPackage(addr: string, identityKey: string, d: { deviceKey: string; attestation?: string; keyPackage?: string | null }): import("ts-mls").KeyPackage | string {
    if (!d.keyPackage) return "";
    if (!this.deviceVerified(identityKey, d.deviceKey, d.attestation)) return "not verified by their identity key";
    let kp: import("ts-mls").KeyPackage;
    try {
      kp = this.mls.parseKeyPackage(unb64(d.keyPackage));
    } catch {
      return "its key package is unreadable";
    }
    const cred = kp.leafNode.credential;
    const who = cred.credentialType === "basic" ? new TextDecoder().decode(cred.identity) : "";
    if (who !== addr || !bytesEqual(kp.leafNode.signaturePublicKey, unb64(d.deviceKey))) return "its key package is not that device's";
    return kp;
  }

  // removeMember removes every leaf a person holds.
  async removeMember(roomId: string, addr: string) {
    await this.commit(roomId, async (state) => {
      const idx = this.mls.members(state).filter((l) => l.identity === addr).map((l) => l.index);
      if (!idx.length) throw new Error("that person is not in the room");
      return this.mls.removeLeaves(state, idx);
    }, false);
  }

  // leave takes this person out of a room, every device of theirs. MLS
  // forbids a commit removing its own committer, so the hub counts them out
  // (they see nothing sequenced after) and the members' devices commit the
  // removal of their leaves.
  async leave(roomId: string) {
    await this.api.updateRoom(roomId, { leave: true });
    await this.forgetRoom(roomId);
    await this.refreshRooms();
  }

  // leaveDevice takes only this device out of a room — the way out of one it
  // went stale in. The hub marks its leaf as its own departure, which any
  // member's device then removes; the person stays, with their role and
  // their other devices. This device forgets everything it held there, and
  // comes back like any new device of theirs: a sibling or any member adds
  // it — a fresh join, with every check.
  async leaveDevice(roomId: string) {
    await this.api.updateRoom(roomId, { leaveDevice: true });
    await this.forgetRoom(roomId);
    await this.refreshRooms();
  }

  // forgetRoom drops what this device held for a room it is out of: its
  // keys, and what a refusal or a provisional join kept, so a later return
  // starts clean.
  private async forgetRoom(roomId: string) {
    this.states.delete(roomId);
    const room = this.rooms_.get(roomId);
    if (room) delete room.staleReason;
    this.pendingCommits.delete(roomId);
    this.provisionalTries.delete(roomId);
    await this.storage.del(NS.groups, roomId);
    for (const k of [REFUSED, PROVISIONAL, STALE, DOOMED, REVOKED]) await this.storage.del(NS.meta, k + roomId);
  }

  // commit builds a commit from the current state and submits it; the new
  // state is adopted only when the hub accepts. On epoch_stale we catch up
  // and rebuild.
  private async commit(roomId: string, build: (state: ClientState) => Promise<{ commit: Uint8Array; welcome?: Uint8Array | null; newState: ClientState }>, withGroupInfo: boolean) {
    for (let attempt = 0; attempt < 3; attempt++) {
      // Built, submitted and kept in the room's queue: nothing else reads or
      // writes the state between, and the new state is kept only once the
      // hub has taken the commit.
      const outcome = await this.serial(roomId, async (): Promise<"done" | "stale" | "again"> => {
        const state = this.requireState(roomId);
        const built = await build(state);
        // Group info rides every commit: the hub requires it.
        void withGroupInfo;
        const groupInfo = b64u(await this.mls.groupInfo(built.newState));
        // Kept until the hub answers: if the answer is lost but the commit
        // lands, it comes back in the log, and is this device's own state.
        const pending = { wire: b64u(built.commit), newState: built.newState, before: this.mls.members(state) };
        this.pendingCommits.set(roomId, pending);
        try {
          const res = await this.api.submit(roomId, {
            id: ulid(), kind: "mls_commit", payload: b64u(built.commit),
            welcome: built.welcome ? b64u(built.welcome) : undefined, groupInfo,
          });
          if (this.pendingCommits.get(roomId) === pending) this.pendingCommits.delete(roomId);
          const before = this.mls.members(state);
          await this.recordKeyNames(roomId, this.mls.members(built.newState));
          await this.setState(roomId, built.newState);
          if (res.event.seq === (this.cursors.get(roomId) ?? 0) + 1) await this.setCursor(roomId, res.event.seq);
          await this.refreshRooms();
          const fake: EventView = { roomId, seq: res.event.seq, id: res.event.id, epoch: res.event.epoch, kind: "mls_commit", sender: this.meInfo.addr, senderDevice: this.meInfo.deviceKey, hubAt: res.event.hubAt, payload: "" };
          await this.noteMembership(roomId, fake, before, this.mls.members(built.newState));
          return "done";
        } catch (e) {
          // Refused by the hub: it didn't land. Anything else (no answer)
          // may have, and stays pending.
          if (e instanceof ApiError && this.pendingCommits.get(roomId) === pending) this.pendingCommits.delete(roomId);
          if (e instanceof ApiError && e.code === "epoch_stale" && attempt < 2) return "stale";
          // A room from before the hub verified commits seeds its state from
          // this epoch's GroupInfo: publish ours, then try again.
          if (e instanceof ApiError && e.code === "need_group_info" && attempt < 2) {
            await this.api.putGroupInfo(roomId, this.mls.epoch(state), b64u(await this.mls.groupInfo(state)));
            return "again";
          }
          throw e;
        }
      });
      if (outcome === "done") return;
      if (outcome === "stale") await this.catchUp(roomId);
    }
    throw new Error("could not commit after the room changed repeatedly");
  }

  // joinChannel enters an open household channel by external commit.
  async joinChannel(roomId: string) {
    await this.serial(roomId, async () => {
      const gi = await this.api.groupInfo(roomId);
      const kp = await this.mls.keyPackage(this.meInfo.addr, this.device);
      const { commit, newState } = await this.mls.externalJoin(unb64(gi.groupInfo), kp);
      await this.refuseUnattested(newState);
      await this.checkKeyNames(roomId, this.mls.members(newState));
      const groupInfo = b64u(await this.mls.groupInfo(newState));
      const res = await this.api.submit(roomId, { id: ulid(), kind: "mls_commit", payload: b64u(commit), groupInfo });
      await this.recordKeyNames(roomId, this.mls.members(newState));
      await this.setState(roomId, newState);
      await this.setCursor(roomId, res.event.seq);
    });
    await this.refreshRooms();
    await this.doomUnlisted(roomId);
    await this.catchUp(roomId);
  }

  // acceptHub: this person (holding canChangePolicy) agrees to their own fog
  // taking the room over from its hub; for an hour, the hub may hand it over.
  async updateRoom(roomId: string, patch: { label?: string; topic?: string; workspaceVisible?: boolean; notify?: string; archive?: boolean; role?: { addr: string; role: number }; leave?: boolean; leaveDevice?: boolean; acceptHub?: boolean }) {
    await this.api.updateRoom(roomId, patch);
    await this.refreshRooms();
  }

  // ---- people ----

  async directory(): Promise<DirectoryPerson[]> {
    const res = await this.api.directory();
    this.directory_ = res.people;
    this.households_ = res.households ?? [];
    this.namesVersion++;
    this.dirIdentity.clear();
    this.dirDevices.clear();
    const inst = this.meInfo.household.instanceId;
    const index = (addr: string, p: DirectoryPerson) => {
      this.dirIdentity.set(addr, p.identityKey || undefined);
      this.dirDevices.set(addr, new Map(p.devices.map((d) => [d.deviceKey, d.attestation || undefined])));
    };
    for (const p of res.people) index(`fog:${inst}/${p.handle}`, p);
    for (const h of this.households_) for (const p of h.people) index(`fog:${h.instanceId}/${p.handle}`, p);
    // A device fog no longer lists — revoked — is no longer one this device
    // checked: its cached attestation goes.
    for (const k of Array.from(this.attested)) {
      const [addr, key] = k.split("\u0000");
      const listed = this.dirDevices.get(addr);
      if (listed && !listed.get(key)) this.attested.delete(k);
    }
    // A key that differs from the one this device holds for someone is a
    // replaced identity, whichever room it shows up in: say so now. The
    // pinned key stays — fog's word doesn't change who this device trusts.
    for (const [addr, key] of this.dirIdentity) if (key) await this.reportIdentity(addr, key);
    return res.people;
  }

  cachedDirectory(): DirectoryPerson[] {
    return this.directory_;
  }

  // households lists the paired households whose people may join rooms
  // with ours, as their heartbeats last advertised them.
  households(): DirectoryHousehold[] {
    return this.households_;
  }

  // nameFor renders a person address for display: a local handle, or a
  // peer's handle with their household.
  nameFor(addr: string): string {
    const m = /^fog:([^/]+)\/(.+)$/.exec(addr);
    if (!m) return addr;
    if (m[1] === this.meInfo.household.instanceId) {
      const p = this.directory_.find((x) => x.handle === m[2]);
      return p?.displayName || m[2];
    }
    const h = this.households_.find((x) => x.instanceId === m[1]);
    const p = h?.people.find((x) => x.handle === m[2]);
    return `${p?.displayName || m[2]} · ${h?.name ?? "another household"}`;
  }

  // pinIdentity remembers the first identity key seen for a person, and,
  // when someone here deliberately adds a person whose key changed, takes
  // the new one — dropping the verification mark and saying so.
  private async pinIdentity(addr: string, key?: string): Promise<"new" | "same" | "changed" | "none"> {
    if (!key) return "none";
    const prev = await this.storage.get<string>(NS.pins, addr);
    if (!prev) {
      await this.storage.put(NS.pins, addr, key);
      return "new";
    }
    if (prev === key) return "same";
    await this.storage.put(NS.pins, addr, key);
    await this.storage.del(NS.verified, addr);
    this.warnings.delete(addr);
    for (const k of Array.from(this.attested)) if (k.startsWith(addr + "\u0000")) this.attested.delete(k);
    this.emit("identity", { addr, changed: true });
    return "changed";
  }

  // reportIdentity compares the key fog serves for someone with the one
  // this device holds, and warns — without changing what it trusts — when
  // they differ.
  private async reportIdentity(addr: string, served: string) {
    const held = await this.knownIdentity(addr, false);
    if (!held || held === served) {
      if (this.warnings.delete(addr)) this.emit("identity", { addr });
      return;
    }
    if (this.warnings.has(addr)) return;
    const own = addr === this.meInfo.addr;
    this.warnings.set(addr, own
      ? "Fog now serves a different identity key for you than the one your devices hold. Your devices won't add any device it vouches for. Tell your household's operator."
      : `Fog now serves a different identity key for ${this.nameFor(addr)} than the one this device knows. Devices it vouches for won't be trusted here until someone deliberately adds them again or you compare safety codes.`);
    await this.storage.del(NS.verified, addr);
    this.emit("identity", { addr, changed: true, warning: this.warnings.get(addr) });
  }

  // identityWarnings lists the people whose identity key fog now serves
  // differently from the one this device holds.
  // It also lists the people whose identity was reset and whose new key
  // this device hasn't accepted.
  identityWarnings(): { addr: string; text: string }[] {
    const out = Array.from(this.warnings, ([addr, text]) => ({ addr, text }));
    for (const addr of this.resets) {
      if (this.warnings.has(addr)) continue;
      out.push({ addr, text: `${this.nameFor(addr)}'s identity was reset. This device trusts none of their new devices until someone in a room adds them again or you compare safety codes.` });
    }
    return out;
  }

  // knownIdentity is the identity key this device holds for a person: for
  // itself, the key of the identity seed it holds, or else the one pinned
  // at first sight; for anyone else the key pinned. A person never seen is
  // pinned now, from what fog serves, when pin says so: first contact
  // trusts fog until safety codes are compared.
  async knownIdentity(addr: string, pin = true): Promise<string | undefined> {
    if (addr === this.meInfo.addr && this.identity) return b64u(this.identity.publicKey);
    const pinned = await this.storage.get<string>(NS.pins, addr);
    if (pinned || !pin || this.resets.has(addr)) return pinned ?? undefined;
    const served = this.dirIdentity.get(addr) ?? (addr === this.meInfo.addr ? this.meInfo.identityKey : undefined);
    if (served) await this.storage.put(NS.pins, addr, served);
    return served;
  }

  // leafAttested says this device has checked, itself, that the identity
  // key it holds for addr attests a leaf's signature key.
  async leafAttested(addr: string, key: Uint8Array, refetch = true): Promise<boolean> {
    const k = b64u(key);
    const id = `${addr}\u0000${k}`;
    if (this.attested.has(id)) return true;
    const check = async () => {
      const att = this.dirDevices.get(addr)?.get(k);
      const identity = await this.knownIdentity(addr);
      return !!identity && !!att && !this.warnings.has(addr) && verifyAttestation(unb64(identity), key, unb64(att));
    };
    // What fog said a moment ago may predate the attestation: ask again
    // once before refusing.
    let ok = await check();
    if (!refetch) {
      if (ok) this.attested.add(id);
      return ok;
    }
    if (!ok && this.dirRefetch) {
      // Another check is already asking: its answer is this one's too.
      await this.dirRefetch.catch(() => {});
      ok = await check();
    }
    if (!ok && Date.now() - (this.refetchedFor.get(id) ?? 0) > 5000) {
      this.refetchedFor.set(id, Date.now());
      const p = (this.dirRefetch ??= this.directory().finally(() => {
        this.dirRefetch = null;
      }));
      await p.catch(() => {});
      ok = await check();
    }
    if (ok) this.attested.add(id);
    return ok;
  }

  // unattested lists the leaves this device can't verify.
  // accepting holds keys a member's re-add vouches for, not yet pinned: a
  // leaf of such a person is checked against that key.
  //
  // once: a whole set judged with at most one directory fetch between them
  // (a crafted Welcome can't make it one per leaf).
  private async unattested(leaves: { identity: string; signatureKey: Uint8Array }[], accepting?: Map<string, string>, once = false) {
    if (once) {
      const quick = [];
      for (const l of leaves) if (!(await this.leafAttested(l.identity, l.signatureKey, false))) quick.push(l);
      if (!quick.length) return [];
      await this.directory().catch(() => {});
      const out = [];
      for (const l of quick) if (!(await this.leafAttested(l.identity, l.signatureKey, false))) out.push(l);
      return out;
    }
    const out: { identity: string; signatureKey: Uint8Array }[] = [];
    for (const l of leaves) {
      const key = accepting?.get(l.identity);
      if (key) {
        const att = this.dirDevices.get(l.identity)?.get(b64u(l.signatureKey));
        if (!att || !verifyAttestation(unb64(key), l.signatureKey, unb64(att))) out.push(l);
        continue;
      }
      if (!(await this.leafAttested(l.identity, l.signatureKey))) out.push(l);
    }
    return out;
  }

  // refuseUnattested stops a join into a group that holds a leaf this
  // device can't verify.
  private async refuseUnattested(state: ClientState) {
    const bad = await this.unattested(this.mls.members(state).filter((l) => !bytesEqual(l.signatureKey, this.device.publicKey)));
    if (bad.length) throw new RefusedMessage(`this room holds a device of ${this.nameFor(bad[0].identity)} that their identity key doesn't vouch for`);
  }

  // members says who is in a room as its group's leaves have it — the
  // truth of who can read what is sent — set against the participants fog
  // lists (listed). Someone holding a leaf whom fog doesn't list is
  // unexplained: fog can't account for why they read the room, and the
  // room should not be trusted until a member removes them. Someone fog
  // lists who holds no leaf can't read it yet.
  async members(roomId: string, listed: string[]): Promise<RoomMember[]> {
    const state = this.states.get(roomId);
    const out = new Map<string, RoomMember>();
    const listedSet = new Set(listed);
    for (const l of state ? this.mls.members(state) : []) {
      let m = out.get(l.identity);
      if (!m) out.set(l.identity, (m = { addr: l.identity, listed: listedSet.has(l.identity), devices: [], unexplained: false, awaitingKey: this.resets.has(l.identity) }));
      m.devices.push({ key: b64u(l.signatureKey), attested: await this.leafAttested(l.identity, l.signatureKey) });
    }
    for (const m of out.values()) m.unexplained = !m.listed;
    for (const addr of listed) if (!out.has(addr)) out.set(addr, { addr, listed: true, devices: [], unexplained: false, awaitingKey: this.resets.has(addr) });
    return Array.from(out.values());
  }

  // leafTrust says, for each leaf of a room's group, whether this device has
  // verified it: what a "verified" badge is drawn from.
  async leafTrust(roomId: string): Promise<{ index: number; identity: string; key: string; attested: boolean }[]> {
    const state = this.states.get(roomId);
    if (!state) return [];
    const out = [];
    for (const l of this.mls.members(state)) out.push({ index: l.index, identity: l.identity, key: b64u(l.signatureKey), attested: await this.leafAttested(l.identity, l.signatureKey) });
    return out;
  }

  // markStale marks a room this device refused something in, with why.
  private markStale(roomId: string, reason?: string) {
    const room = this.rooms_.get(roomId);
    if (room) {
      room.sync = "stale";
      if (reason) room.staleReason = reason;
    }
    // Kept, so a restart never makes the room ready again.
    void this.storage.put(NS.meta, STALE + roomId, reason ?? room?.staleReason ?? "This device refused something in this room.").catch(() => {});
    this.emit("rooms");
    if (reason) this.emit("error", new Error(reason));
  }

  // sanitize drops what member content may not claim: a "system" line is
  // the client's own (from a verified commit or a hub event), or fog's bot.
  //
  // Fog's bot (of this fog, or of the room's hub) may stamp "fog" or name
  // the integration that posted through it (app:, webhook:, mcp:), which is
  // kept so the line says who spoke; anyone else's stamp is dropped.
  private sanitize(c: Content, sender: string, roomId: string): Content {
    if (c.system && !(systemLabel(c.system) && this.isBot(sender, roomId))) delete c.system;
    return c;
  }

  // isBot says an address is fog's bot: this fog's, or the room's hub's.
  private isBot(addr: string, roomId: string): boolean {
    const hub = this.rooms_.get(roomId)?.hubInstance;
    return addr === `fog:${this.meInfo.household.instanceId}/fog` || (!!hub && addr === `fog:${hub}/fog`);
  }

  // resetTrusted says an identity-reset line in a room may change what this
  // device trusts about addr: they held a leaf in the room's group, and the
  // room's hub is their home fog or this one.
  private resetTrusted(roomId: string, addr: string, state: ClientState): boolean {
    const room = this.rooms_.get(roomId);
    // A room hubbed here reports no hub instance.
    const hub = room ? room.hubInstance || this.meInfo.household.instanceId : undefined;
    const home = /^fog:([^/]+)\//.exec(addr)?.[1];
    if (!hub || !home) return false;
    if (hub !== home && hub !== this.meInfo.household.instanceId) return false;
    return this.mls.members(state).some((l) => l.identity === addr);
  }

  // adoptPending takes a commit of this device's own that lands without its
  // answer having come back: the state it built is the room's now. Any
  // other commit at that epoch means it never will.
  private async adoptPending(roomId: string, wire: Uint8Array, ev: EventView, b: WriteBatch): Promise<boolean> {
    const p = this.pendingCommits.get(roomId);
    if (!p) return false;
    if (b64u(wire) !== p.wire) {
      this.pendingCommits.delete(roomId);
      return false;
    }
    this.pendingCommits.delete(roomId);
    await this.recordKeyNames(roomId, this.mls.members(p.newState));
    await this.setState(roomId, p.newState, b);
    await this.flush(b);
    await this.noteMembership(roomId, ev, p.before, this.mls.members(p.newState));
    return true;
  }

  // vetCommit applies a public commit to a copy of the group and holds it to
  // every check a commit gets, however it arrived: fog's structural and DM
  // rules (Mls.process), no device key under a second name, and every leaf
  // it brings in attested — bad lists the ones that aren't.
  private async vetCommit(roomId: string, state: ClientState, wire: Uint8Array, skipAttestation = false): Promise<VettedCommit> {
    const before = this.mls.members(state);
    const { newState, sender: committer, removed: cleared = [] } = await this.mls.process(state, wire, this.rulesFor(roomId), "commit");
    // Every leaf the commit brings in — an Add, an external joiner — must
    // be a device its person's identity key attests, as this device knows
    // that key; fog's word for it is not enough. A leaf is who it names and
    // the key it holds, together: one whose name, key or place changed is
    // new, and is checked like any other. A leaf at an index the commit
    // removed is new whatever it holds.
    const was = new Set(before.filter((l) => !cleared.includes(l.index)).map(leafId));
    const fresh = this.mls.members(newState).filter((l) => !was.has(leafId(l)));
    await this.checkKeyNames(roomId, [...before, ...fresh]);
    const accepting = await this.reAddsToAccept(before, committer, fresh);
    const bad = skipAttestation ? [] : await this.unattested(fresh, accepting);
    return { before, newState, committer, cleared, fresh, accepting, bad };
  }

  // landCommit makes a vetted commit this device's state.
  private async landCommit(roomId: string, v: VettedCommit, ev: EventView, b: WriteBatch) {
    // Only now, with the commit accepted, does a re-add's key count.
    for (const [addr, key] of v.accepting) await this.acceptIdentity(addr, key, false);
    await this.recordKeyNames(roomId, this.mls.members(v.newState));
    await this.setState(roomId, v.newState, b);
    await this.checkProvisional(roomId, v.newState);
    // Membership lines and history sharing write and send on their own, so
    // everything held so far lands first.
    await this.flush(b);
    await this.noteMembership(roomId, ev, v.before, this.mls.members(v.newState), v.cleared);
  }

  // applyRecovery takes a refused commit together with the one after it,
  // when that one — from an attested member who isn't one of the refused
  // devices — removes every device this device refused, and brings in
  // nothing unattested: the pair is the room without them. Anything else
  // stays refused.
  private async applyRecovery(roomId: string, state: ClientState, refused: RefusedCommit, wire: Uint8Array, ev: EventView, b: WriteBatch) {
    const first = await this.vetCommit(roomId, state, unb64(refused.wire), true);
    const second = await this.vetCommit(roomId, first.newState, wire);
    const badKeys = new Set(refused.bad);
    const by = second.committer;
    if (!by || badKeys.has(b64u(by.signatureKey)) || !first.before.some((l) => bytesEqual(l.signatureKey, by.signatureKey)) || !(await this.leafAttested(by.identity, by.signatureKey))) {
      throw new RefusedMessage("A commit followed one this device refused, from someone it can't take a removal from. This room stays refused.");
    }
    if (this.mls.members(second.newState).some((l) => badKeys.has(b64u(l.signatureKey))) || second.bad.length) {
      throw new RefusedMessage("A commit followed one this device refused without removing the device it refused. This room stays refused.");
    }
    const net = { ...second, before: first.before, cleared: [...first.cleared, ...second.cleared] };
    await this.storage.del(NS.meta, REFUSED + roomId);
    this.clearStale(roomId);
    await this.landCommit(roomId, net, ev, b);
  }

  // refusedDevices lists the devices this device refused in a room (the
  // commit that brought them in left the room stale): what a member with
  // canRemoveParticipant may remove with removeRefusedDevices.
  //
  // Each comes with whose device it says it is. A device that has since been
  // vouched for (its person's identity key, as this device holds it, now
  // attests it) is no longer listed: what is left is for a member to judge.
  async refusedDevices(roomId: string): Promise<{ key: string; identity: string }[]> {
    const refused = await this.storage.get<RefusedCommit>(NS.meta, REFUSED + roomId);
    const state = this.states.get(roomId);
    if (!refused || !state) return [];
    let leaves: { identity: string; signatureKey: Uint8Array }[] = [];
    try {
      leaves = this.mls.members((await this.vetCommit(roomId, state, unb64(refused.wire), true)).newState);
    } catch {
      return [];
    }
    const out = [];
    for (const key of refused.bad) {
      const l = leaves.find((x) => b64u(x.signatureKey) === key);
      if (l && !(await this.leafAttested(l.identity, l.signatureKey, false))) out.push({ key, identity: l.identity });
    }
    return out;
  }

  // removeRefusedDevices recovers a room this device refused a device in:
  // it takes the refused commit and at once commits the removal of every
  // device it refused, so nothing is ever encrypted to them. Members'
  // devices that refused the same accept the pair (applyRecovery).
  async removeRefusedDevices(roomId: string) {
    const refused = await this.storage.get<RefusedCommit>(NS.meta, REFUSED + roomId);
    if (!refused) return;
    if (!this.can(roomId, "canRemoveParticipant")) throw new Error("removing a device from this room takes a role that may remove people");
    await this.serial(roomId, async () => {
      const state = this.requireState(roomId);
      const first = await this.vetCommit(roomId, state, unb64(refused.wire), true);
      const bad = new Set(refused.bad);
      const idx = this.mls.members(first.newState).filter((l) => bad.has(b64u(l.signatureKey))).map((l) => l.index);
      const { commit, newState } = await this.mls.removeLeaves(first.newState, idx);
      const groupInfo = b64u(await this.mls.groupInfo(newState));
      const res = await this.api.submit(roomId, { id: ulid(), kind: "mls_commit", payload: b64u(commit), groupInfo });
      await this.storage.del(NS.meta, REFUSED + roomId);
      await this.recordKeyNames(roomId, this.mls.members(newState));
      this.clearStale(roomId);
      await this.setState(roomId, newState);
      await this.setCursor(roomId, res.event.seq);
    });
    await this.refreshRooms();
    await this.catchUp(roomId);
  }

  // checkKeyNames refuses a leaf whose device key has had another name in
  // this room's group, in any epoch this device saw: a key is one device's,
  // for the group's life.
  private async checkKeyNames(roomId: string, leaves: { identity: string; signatureKey: Uint8Array }[]) {
    const names = await this.storage.get<Record<string, string>>(NS.meta, NAMES + roomId) ?? {};
    for (const l of leaves) {
      const was = names[b64u(l.signatureKey)];
      if (was && was !== l.identity) throw new RefusedMessage(`a commit that hands ${was}'s device key to ${l.identity}`);
    }
  }

  private async recordKeyNames(roomId: string, leaves: { identity: string; signatureKey: Uint8Array }[]) {
    const names = await this.storage.get<Record<string, string>>(NS.meta, NAMES + roomId) ?? {};
    let changed = false;
    for (const l of leaves) {
      const k = b64u(l.signatureKey);
      if (!names[k]) {
        names[k] = l.identity;
        changed = true;
      }
    }
    if (changed) await this.storage.put(NS.meta, NAMES + roomId, names);
  }

  // reAddsToAccept takes a new key — a reset person's, or one fog serves in
  // place of the pinned one — on a member's deliberate
  // word: a commit this device verified, made by a current member's
  // attested leaf (not an external join, and not the person themselves),
  // that adds devices of theirs which the key fog now serves attests.
  //
  // It says which keys to take; they are pinned only once the commit is
  // accepted. Fog's bot never vouches: whoever holds its key is fog's
  // operator, the party a reset is a warning about.
  private async reAddsToAccept(before: { identity: string; signatureKey: Uint8Array }[], committer: { identity: string; signatureKey: Uint8Array } | undefined, fresh: { identity: string; signatureKey: Uint8Array }[]): Promise<Map<string, string>> {
    const out = new Map<string, string>();
    if (!committer || (!this.resets.size && !this.warnings.size)) return out;
    // Waiting on a new key: after a reset line, or because fog serves a key
    // other than the one this device pinned.
    const pending = Array.from(new Set(fresh.map((l) => l.identity))).filter((a) => (this.resets.has(a) || this.warnings.has(a)) && a !== committer.identity);
    if (!pending.length) return out;
    if (!before.some((l) => bytesEqual(l.signatureKey, committer.signatureKey))) return out;
    if (/^fog:[^/]+\/fog$/.test(committer.identity)) return out;
    if (this.resets.has(committer.identity) || !(await this.leafAttested(committer.identity, committer.signatureKey))) return out;
    // The key fog serves now, if it attests every device of theirs the
    // commit adds; what fog said a moment ago may predate them, so ask once.
    const vouching = (addr: string) => {
      const served = this.dirIdentity.get(addr);
      const devices = this.dirDevices.get(addr);
      if (!served || !devices) return undefined;
      const ok = fresh.filter((l) => l.identity === addr).every((l) => {
        const att = devices.get(b64u(l.signatureKey));
        return !!att && verifyAttestation(unb64(served), l.signatureKey, unb64(att));
      });
      return ok ? served : undefined;
    };
    if (pending.some((a) => !vouching(a))) await this.directory().catch(() => {});
    for (const addr of pending) {
      const key = vouching(addr);
      if (key) out.set(addr, key);
    }
    return out;
  }

  // acceptIdentity pins a key for someone deliberately — a safety-code
  // comparison (verified), a member's re-add, this person adding them —
  // which ends a reset's wait.
  private async acceptIdentity(addr: string, key: string, verified: boolean) {
    await this.storage.put(NS.pins, addr, key);
    if (verified) await this.storage.put(NS.verified, addr, key);
    if (this.resets.delete(addr)) await this.storage.put(NS.meta, RESETS, Array.from(this.resets));
    this.warnings.delete(addr);
    this.touchBackup();
    this.emit("identity", { addr, accepted: true, verified });
  }

  // forgetIdentity drops what this device trusted about a person whose
  // identity the hub says was reset: the pinned key and any verification
  // mark. Their next key is new to this device, and unverified.
  //
  // Nothing takes the old key's place on fog's word: until a member re-adds
  // them in a commit this device verifies (reAddsToAccept), this person adds
  // them, or compares safety codes (markVerified), no key is trusted for
  // them, so none of their devices is, and identityWarnings says so.
  private async forgetIdentity(addr: string) {
    if (addr === this.meInfo.addr) return;
    await this.storage.del(NS.pins, addr);
    await this.storage.del(NS.verified, addr);
    for (const k of Array.from(this.attested)) if (k.startsWith(addr + "\u0000")) this.attested.delete(k);
    this.resets.add(addr);
    await this.storage.put(NS.meta, RESETS, Array.from(this.resets));
    this.touchBackup();
    this.emit("identity", { addr, reset: true });
  }

  // ---- verification marks: a person is verified on this device
  // once their identity key's safety code was compared in person ----

  async markVerified(addr: string, identityKey: string) {
    await this.acceptIdentity(addr, identityKey, true);
  }

  // safetyCodes are what two people compare in person, computed from what
  // this device holds — never from what fog serves now, or comparing them
  // would compare fog's key with itself. mine is this device's identity
  // seed's (or, without the seed, the key it pinned for its own person);
  // theirs the key this device pinned for addr, and only when it pinned
  // none (first contact, or a reset awaiting a new key) the key fog serves,
  // with theirsPinned false: comparing that one in person is how it is
  // taken (markVerified with theirKey). ownSwapped and theirsSwapped say fog
  // serves another key than the one held — a loud warning.
  async safetyCodes(addr: string): Promise<{ mine?: string; theirs?: string; theirKey?: string; theirsPinned: boolean; ownSwapped: boolean; theirsSwapped: boolean }> {
    const myKey = this.identity ? b64u(this.identity.publicKey) : await this.storage.get<string>(NS.pins, this.meInfo.addr) ?? undefined;
    const served = (a: string) => this.dirIdentity.get(a) ?? (a === this.meInfo.addr ? this.meInfo.identityKey || undefined : undefined);
    const pinned = addr === this.meInfo.addr ? myKey : await this.storage.get<string>(NS.pins, addr) ?? undefined;
    const theirKey = pinned ?? served(addr);
    const myServed = served(this.meInfo.addr);
    const theirServed = served(addr);
    return {
      mine: myKey ? fingerprint(unb64(myKey)) : undefined,
      theirs: theirKey ? fingerprint(unb64(theirKey)) : undefined,
      theirKey,
      theirsPinned: !!pinned,
      ownSwapped: !!myKey && !!myServed && myServed !== myKey,
      theirsSwapped: !!pinned && !!theirServed && theirServed !== pinned,
    };
  }

  // awaitingKey says a person's identity was reset and this device has not
  // yet accepted their new key.
  awaitingKey(addr: string): boolean {
    return this.resets.has(addr);
  }

  async unmarkVerified(addr: string) {
    await this.storage.del(NS.verified, addr);
    this.emit("identity", { addr, verified: false });
  }

  // isVerified says whether the person's current identity key is the one
  // this device marked verified.
  async isVerified(addr: string, identityKey?: string): Promise<boolean> {
    const v = await this.storage.get<string>(NS.verified, addr);
    if (!v) return false;
    return identityKey ? v === identityKey : true;
  }

  // ---- devices of the same person ----

  // syncSiblingDevices keeps every room's leaves in step with this person's
  // device list: verified devices of mine that hold no leaf get added (and
  // then history, if the room shares it); leaves whose key belongs to no
  // current device of mine — revoked ones — get removed.
  async syncSiblingDevices() {
    if (!this.meServer) return;
    let me: MeResponse;
    try {
      me = await this.api.me();
    } catch {
      return;
    }
    this.meServer = me;
    // Siblings are vouched for by this person's identity key as this device
    // holds it — never by whatever key fog serves now, which a fog could
    // swap to vouch for a device of its own making.
    const ownKey = await this.knownIdentity(this.meInfo.addr);
    if (me.identityKey) await this.reportIdentity(this.meInfo.addr, me.identityKey);
    const trusted = ownKey && ownKey === me.identityKey ? ownKey : undefined;
    const current = new Map(me.devices.map((d) => [d.deviceKey, d]));
    for (const [roomId, room] of this.rooms_) {
      const state = this.states.get(roomId);
      if (!state || room.sync !== "ready" || room.archivedAt) continue;
      const mine = this.mls.members(state).filter((l) => l.identity === this.meInfo.addr);
      const held = new Set(mine.map((l) => b64u(l.signatureKey)));
      const stale = mine.filter((l) => !current.has(b64u(l.signatureKey))).map((l) => l.index);
      // A sibling is added only when this device can check, itself, that
      // the person's identity key attests it: fog's "verified" is a claim.
      const missing = trusted ? me.devices.filter((d) => !held.has(d.deviceKey) && d.deviceKey !== this.meInfo.deviceKey && this.deviceVerified(trusted, d.deviceKey, d.attestation)) : [];
      if (!stale.length && !missing.length) continue;
      try {
        const kps: import("ts-mls").KeyPackage[] = [];
        for (const d of missing) {
          const km = await this.api.keysByAddr(this.meInfo.addr, d.id).catch(() => null);
          const dv = km?.devices.find((x) => x.deviceKey === d.deviceKey);
          const kp = dv && trusted && this.attestedPackage(this.meInfo.addr, trusted, dv);
          if (kp && typeof kp !== "string") kps.push(kp);
        }
        // Found by key in the state the commit is built on: another removal
        // (a revoked device's line, another sibling) may land first.
        if (stale.length) {
          const gone = new Set(mine.filter((l) => stale.includes(l.index)).map((l) => b64u(l.signatureKey)));
          await this.commit(roomId, async (st) => {
            const idx = this.mls.members(st).filter((l) => gone.has(b64u(l.signatureKey))).map((l) => l.index);
            if (!idx.length) throw new NothingToDo();
            return this.mls.removeLeaves(st, idx);
          }, true).catch((e) => {
            if (!(e instanceof NothingToDo)) throw e;
          });
        }
        if (kps.length) await this.commit(roomId, (st) => this.mls.addMembers(st, kps), true);
      } catch (e) {
        console.warn("chat: sibling sync", roomId, e);
      }
    }
  }

  // ---- membership lines and history sharing ----

  private async noteMembership(roomId: string, ev: EventView, before: { index: number; identity: string; signatureKey: Uint8Array }[], after: { index: number; identity: string; signatureKey: Uint8Array }[], cleared: number[] = []) {
    // By place, name and key together, and every leaf the commit removed
    // counts as gone: a removal and an add at one index are both said,
    // whatever the new leaf holds.
    const key = (l: { signatureKey: Uint8Array }) => b64u(l.signatureKey);
    const kept = new Set(before.filter((l) => !cleared.includes(l.index)).map(leafId));
    const now = new Set(after.map(leafId));
    const added = after.filter((l) => !kept.has(leafId(l))).map((l) => ({ identity: l.identity, key: key(l) }));
    const removed = before.filter((l) => cleared.includes(l.index) || !now.has(leafId(l))).map((l) => ({ identity: l.identity, key: key(l) }));
    if (!added.length && !removed.length) return;
    // Its id is the client's own namespace: no message id (always a ULID)
    // can take it, so a commit's line always renders.
    await this.storeMessage({
      id: `membership:${ev.seq}`, roomId, seq: ev.seq, sender: ev.sender ?? "", senderDevice: ev.senderDevice ?? "", hubAt: ev.hubAt, sentAt: ev.hubAt,
      content: { v: 1, type: "membership", membership: { added, removed } }, mine: false, checked: true,
    });
    // History goes only to devices this device has verified itself.
    const verified = [];
    for (const a of added) if (await this.leafAttested(a.identity, unb64(a.key))) verified.push(a.key);
    if (verified.length) void this.maybeShareHistory(roomId, verified);
  }

  // maybeShareHistory re-sends the room's past to devices that just joined,
  // when the policy shares history and this device may share it. Every
  // eligible online device races with a little jitter; duplicates are
  // harmless, since receivers skip what they hold.
  private async maybeShareHistory(roomId: string, deviceKeys: string[]) {
    const room = this.rooms_.get(roomId);
    if (!room || room.policy.history !== "shared") return;
    const role = room.policy.roles.find((r) => r.index === room.myRole);
    if (!role?.capabilities.some((c) => c === "*" || c === "canShareHistory")) return;
    const targets = deviceKeys.filter((k) => k !== this.meInfo.deviceKey && !this.historySent.has(`${roomId}/${k}`));
    if (!targets.length) return;
    await new Promise((r) => setTimeout(r, 300 + Math.random() * 1500));
    const items = Array.from(this.messages_.get(roomId)?.values() ?? [])
      // Only what this device received itself: an item another member
      // shared is their word, and re-sent it would lose who said so.
      .filter((m) => !m.sharedBy && m.content.type !== "history" && m.content.type !== "membership" && m.content.system !== "identity-reset" && m.content.system !== "undecryptable")
      .sort((a, b) => a.seq - b.seq)
      .slice(-400)
      .map((m) => ({ id: m.id, seq: m.seq, sender: m.sender, sentAt: m.sentAt, hubAt: m.hubAt, content: m.content }));
    if (!items.length) return;
    for (const k of targets) this.historySent.add(`${roomId}/${k}`);
    // Chunk so no single message outgrows the hub's payload cap.
    for (let i = 0; i < items.length; i += 100) {
      const chunk = items.slice(i, i + 100);
      try {
        await this.sendContent(roomId, { v: 1, type: "history", history: chunk, historyFor: targets });
      } catch (e) {
        console.warn("chat: history share", roomId, e);
        return;
      }
    }
  }

  // receiveHistory files the past another member re-sent for this device,
  // if their role may share history. Each item is their account of who said
  // what, which this device cannot check: it is kept as shared by them, and
  // never changes a message this device received itself (see foldTimeline).
  private async receiveHistory(roomId: string, sharer: string, c: Content) {
    if (!c.historyFor?.includes(this.meInfo.deviceKey) || !c.history) return;
    if (!this.may(roomId, sharer, "canShareHistory", false)) return;
    const map = this.messages_.get(roomId);
    let n = 0;
    for (const h of c.history) {
      if (!isULID(h.id) || map?.has(h.id) || !h.content || h.content.type === "history") continue;
      // A sharer's account can't speak for fog or an integration: an item
      // claiming the bot as sender, or carrying a system stamp, is refused.
      if (h.content.system || this.isBot(h.sender, roomId) || /^fog:[^/]+\/fog$/.test(h.sender)) continue;
      await this.storeMessage({
        id: h.id, roomId, seq: h.seq, sender: h.sender, senderDevice: "", hubAt: h.hubAt ?? "", sentAt: h.sentAt ?? h.hubAt ?? "", content: h.content,
        mine: h.sender === this.meInfo.addr, sharedBy: sharer, ...this.judge(roomId, h.sender, h.content),
      });
      n++;
    }
    if (n) this.emit("messages", { roomId });
  }

  // ---- recovery ----

  // enableBackup mints a recovery phrase, remembers its key on this device,
  // and uploads the first backup. The phrase is returned once; it is never
  // stored anywhere in plaintext.
  async enableBackup(): Promise<string> {
    const phrase = backup.generatePhrase();
    const key = await backup.keyFromPhrase(phrase);
    const raw = new Uint8Array(await crypto.subtle.exportKey("raw", await backup.keyFromPhraseExportable(phrase)));
    await this.storage.put(NS.meta, "backupKey", raw);
    this.backupKey = key;
    await this.backupNow();
    return phrase;
  }

  backupEnabled(): boolean {
    return !!this.backupKey;
  }

  async disableBackup() {
    this.backupKey = null;
    await this.storage.del(NS.meta, "backupKey");
    await this.api.del("/api/chat/me/backup").catch(() => {});
  }

  private touchBackup() {
    if (!this.backupKey) return;
    if (this.backupTimer) return;
    this.backupTimer = setTimeout(() => {
      this.backupTimer = null;
      this.backupNow().catch((e) => console.warn("chat: backup", e));
    }, 15_000);
  }

  async backupNow() {
    if (!this.backupKey) return;
    const messages = [];
    for (const m of this.messages_.values()) for (const v of m.values()) messages.push(v);
    const pins: Record<string, string> = {}, verified: Record<string, string> = {}, cursors: Record<string, number> = {};
    for (const { key, value } of await this.storage.list<string>(NS.pins)) pins[key] = value;
    for (const { key, value } of await this.storage.list<string>(NS.verified)) verified[key] = value;
    for (const [k, v] of this.cursors) cursors[k] = v;
    const payload: backup.BackupPayload = {
      v: 1, addr: this.meInfo.addr, deviceId: this.meInfo.deviceId, identitySeed: this.identity?.seed, deviceSeed: this.device.seed,
      messages, cursors, pins, verified, resets: Array.from(this.resets), savedAt: new Date().toISOString(),
    };
    const sealed = await backup.seal(this.backupKey, payload);
    await this.api.putBackup(sealed, Date.now());
  }

  // restoreFromPhrase pulls the backup and seeds this storage with it, so a
  // following init() finds the person's identity and device and binds to
  // them. Its rooms come back when a member adds this device again (it has no
  // keys for them, and joins nothing by itself).
  static async restoreFromPhrase(opts: ClientOptions, phrase: string): Promise<{ addr: string; messages: number }> {
    assertWebCrypto();
    if (!backup.validPhrase(phrase)) throw new Error("that is not a valid recovery phrase");
    const api = new Api(opts);
    const got = await api.getBackup();
    if (!got) throw new Error("this person has no backup on file");
    const key = await backup.keyFromPhrase(phrase);
    const payload = await backup.open(key, got.bytes);
    const st = opts.storage;
    await st.put(NS.meta, "self", { identitySeed: payload.identitySeed, deviceSeed: payload.deviceSeed, deviceId: payload.deviceId, addr: payload.addr });
    const raw = new Uint8Array(await crypto.subtle.exportKey("raw", await backup.keyFromPhraseExportable(phrase)));
    await st.put(NS.meta, "backupKey", raw);
    await st.put(NS.meta, RESTORED, true);
    for (const m of payload.messages as Message[]) await st.put(NS.messages, `${m.roomId}/${m.id}`, m);
    for (const [k, v] of Object.entries(payload.pins)) await st.put(NS.pins, k, v);
    for (const [k, v] of Object.entries(payload.verified)) await st.put(NS.verified, k, v);
    if (payload.resets?.length) await st.put(NS.meta, RESETS, payload.resets);
    return { addr: payload.addr, messages: payload.messages.length };
  }

  // deviceVerified checks a device's attestation against its person's
  // identity key.
  deviceVerified(identityKey: string | undefined, deviceKey: string, attestation: string | undefined): boolean {
    if (!identityKey || !attestation) return false;
    return verifyAttestation(unb64(identityKey), unb64(deviceKey), unb64(attestation));
  }

  // attestDevice signs another of this person's devices with the identity
  // key this device holds.
  async attestOtherDevice(deviceId: number, deviceKey: string) {
    if (!this.identity) throw new Error("this device does not hold the identity key");
    await this.api.attestDevice(deviceId, b64u(attestDevice(this.identity, unb64(deviceKey))));
  }

  // identitySeed exposes the identity key for the recovery/backup flows.
  identitySeed(): Uint8Array | null {
    return this.identity?.seed ?? null;
  }

  // mlsMembers lists the leaves this device sees in a room.
  mlsMembers(roomId: string) {
    const s = this.states.get(roomId);
    return s ? this.mls.members(s) : [];
  }
}

function defaultDeviceName(): string {
  if (typeof navigator === "undefined") return "Device";
  const ua = navigator.userAgent.toLowerCase();
  if (ua.includes("iphone")) return "iPhone";
  if (ua.includes("ipad")) return "iPad";
  if (ua.includes("android")) return "Android";
  if (ua.includes("macintosh")) return "Mac";
  if (ua.includes("windows")) return "Windows PC";
  if (ua.includes("linux")) return "Linux";
  return "Browser";
}
