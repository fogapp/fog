// The HTTP half: typed calls to fogd's chat and people endpoints. Every
// payload here is either public (key material, room metadata) or ciphertext.

import { CIPHERSUITE_ID } from "./mls.ts";
import { isBlobRef } from "./safe.ts";
import type { DeviceView, DirectoryPerson, EventView, KeyMaterial, ParticipantView, Policy, Prefs, PreviewCardResponse, RoomView, SessionView, Webhook } from "./wire.gen.ts";

// The responses fogd encodes from Go structs are generated from them
// (wire.gen.ts, by internal/tools/tsgen); the rest — request bodies and the
// envelopes handlers build from maps — are written out below.
export type { DeviceView, DirectoryPerson, EventView, KeyMaterial, ParticipantView, Policy, Prefs, PreviewCardResponse, RoomView, SessionView, Webhook } from "./wire.gen.ts";

// seg path-escapes one segment: an id from fog, a remote hub or a message
// can never reach another route.
function seg(v: string | number): string {
  return encodeURIComponent(String(v));
}

export class ApiError extends Error {
  status: number;
  code: string;
  body: unknown;
  constructor(status: number, message: string, code = "", body: unknown = undefined) {
    super(message);
    this.status = status;
    this.code = code;
    this.body = body;
  }
}

export interface ApiOptions {
  baseUrl?: string;
  fetch?: typeof fetch;
  // Extra headers on every request (a native client's bearer token).
  headers?: Record<string, string>;
}

export interface MeResponse {
  person: { id: number; handle: string; displayName: string; role: string; status: string };
  identityKey: string;
  devices: DeviceView[];
  deviceId: number;
  operator: boolean;
  household: { name: string; instanceId: string; domain: string };
  backup: { version: number; updatedAt: string; bytes: number } | null;
}

export interface AvailableRoom {
  id: string;
  label: string;
  topicLabel: string;
  kind: string;
  joinable: boolean;
}

interface RoomsResponse {
  rooms: RoomView[];
  available: AvailableRoom[];
  invites: { id: number; roomId: string; invitedBy: string; seq: number }[];
  me: { addr: string; handle: string; displayName: string };
}

export interface RoomDetail {
  room: RoomView;
  participants: ParticipantView[];
  leaves: { index: number; identity: string; signatureKey: string }[];
  groupInfoEpoch: number;
  impact: string;
}

export interface InviteView {
  id: number;
  roomId: string;
  invitedBy: string;
  seq: number;
  welcome: string;
  createdAt: string;
  room: { id: string; kind: string; label: string; epoch: number; seq: number };
}

export interface DirectoryHousehold {
  name: string;
  instanceId: string;
  online: boolean;
  people: DirectoryPerson[];
}

export interface DirectoryResponse {
  people: DirectoryPerson[];
  household: string;
  instanceId: string;
  households: DirectoryHousehold[];
}

export interface SubmitInput {
  id: string;
  kind: "mls_private" | "mls_commit";
  payload: string;
  sentAt?: string;
  hints?: { mentions?: string[] };
  welcome?: string;
  expiresAt?: string;
  groupInfo?: string;
}

export class Api {
  private base: string;
  private f: typeof fetch;
  private headers: Record<string, string>;

  constructor(opts: ApiOptions = {}) {
    this.base = (opts.baseUrl ?? "").replace(/\/$/, "");
    this.f = opts.fetch ?? ((input, init) => fetch(input, init));
    this.headers = opts.headers ?? {};
  }

  get baseUrl() {
    return this.base;
  }

  async request<T>(method: string, path: string, body?: unknown, raw?: { body: BodyInit; headers?: Record<string, string> }): Promise<T> {
    const headers: Record<string, string> = { ...this.headers };
    let payload: BodyInit | undefined;
    if (raw) {
      payload = raw.body;
      Object.assign(headers, raw.headers ?? {});
    } else if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }
    // Every call has a deadline: a server that never answers can't hang a
    // client. Attachments, which move megabytes, get longer.
    const ms = raw || path.startsWith("/api/chat/blobs") ? 10 * 60_000 : 30_000;
    const signal = typeof AbortSignal !== "undefined" && "timeout" in AbortSignal ? AbortSignal.timeout(ms) : undefined;
    const res = await this.f(this.base + path, { method, headers, body: payload, credentials: "same-origin", signal });
    if (!res.ok) {
      let message = res.statusText, code = "", parsed: unknown;
      try {
        parsed = await res.json();
        const e = parsed as { error?: string; code?: string };
        if (e.error) message = e.error;
        if (e.code) code = e.code;
      } catch {
        /* non-JSON */
      }
      throw new ApiError(res.status, message, code, parsed);
    }
    if (res.status === 204) return undefined as T;
    const ct = res.headers.get("Content-Type") ?? "";
    if (ct.includes("application/json")) return (await res.json()) as T;
    return (await res.arrayBuffer()) as unknown as T;
  }

  get<T>(path: string) {
    return this.request<T>("GET", path);
  }
  post<T>(path: string, body?: unknown) {
    return this.request<T>("POST", path, body);
  }
  put<T>(path: string, body?: unknown) {
    return this.request<T>("PUT", path, body);
  }
  patch<T>(path: string, body?: unknown) {
    return this.request<T>("PATCH", path, body);
  }
  del<T>(path: string) {
    return this.request<T>("DELETE", path);
  }

  // ---- people / self ----
  me() {
    return this.get<MeResponse>("/api/chat/me");
  }
  setIdentity(identityKey: string) {
    return this.put<{ ok: boolean }>("/api/chat/me/identity", { identityKey });
  }
  registerDevice(input: { name: string; kind: string; deviceKey: string; attestation?: string; keyPackages: string[]; lastResort?: string; clientKind?: "web" | "native" }) {
    return this.post<{ device: DeviceView; token?: string }>("/api/chat/me/devices", input);
  }
  attestDevice(id: number, attestation: string) {
    return this.post<{ device: DeviceView }>(`/api/chat/me/devices/${seg(id)}/attest`, { attestation });
  }
  revokeDevice(id: number) {
    return this.del<{ ok: boolean }>(`/api/chat/me/devices/${seg(id)}`);
  }
  sessions() {
    return this.get<{ sessions: SessionView[]; current: number }>("/api/chat/me/sessions");
  }
  revokeSession(id: number) {
    return this.del<{ ok: boolean }>(`/api/chat/me/sessions/${seg(id)}`);
  }
  // lastResort, when given, replaces the device's last-resort package: one
  // fog serves again whenever the one-use ones run out or an asker is past
  // its budget, so draining them can't keep the device out of rooms.
  // replace drops every package the device published before (a device
  // restored from a backup holds none of their private keys).
  publishKeyPackages(deviceId: number, keyPackages: string[], lastResort?: string, replace = false) {
    return this.post<{ keyPackages: number }>(`/api/chat/me/devices/${seg(deviceId)}/keypackages`, { ciphersuite: CIPHERSUITE_ID, keyPackages, lastResort, replace });
  }
  directory() {
    return this.get<DirectoryResponse>("/api/chat/people");
  }
  keysFor(handle: string) {
    return this.get<KeyMaterial>(`/api/chat/people/${encodeURIComponent(handle)}/keys`);
  }
  // keysByAddr resolves key material for anyone: our own people directly,
  // a peer's through their home fog.
  keysByAddr(addr: string, deviceId?: number) {
    return this.get<KeyMaterial>(`/api/chat/keys?addr=${encodeURIComponent(addr)}${deviceId ? `&device=${deviceId}` : ""}`);
  }
  deviceChallenge(id: number) {
    return this.get<{ nonce: string; message: string }>(`/api/chat/me/devices/${seg(id)}/challenge`);
  }
  deviceSession(id: number, nonce: string, signature: string, clientKind: "web" | "native") {
    return this.post<{ device: DeviceView; token?: string }>(`/api/chat/me/devices/${seg(id)}/session`, { nonce, signature, clientKind });
  }
  async putBackup(bytes: Uint8Array, version: number) {
    return this.request<{ ok: boolean; version: number }>("PUT", "/api/chat/me/backup", undefined, {
      body: bytes as unknown as BodyInit,
      headers: { "Content-Type": "application/octet-stream", "X-Fog-Backup-Version": String(version) },
    });
  }
  async getBackup(): Promise<{ bytes: Uint8Array; version: number } | null> {
    const res = await this.f(this.base + "/api/chat/me/backup", { method: "GET", headers: this.headers, credentials: "same-origin" });
    if (res.status === 404) return null;
    if (!res.ok) throw new ApiError(res.status, res.statusText);
    return { bytes: new Uint8Array(await res.arrayBuffer()), version: Number(res.headers.get("X-Fog-Backup-Version") ?? 0) };
  }

  // ---- rooms ----
  rooms() {
    return this.get<RoomsResponse>("/api/chat/rooms");
  }
  room(id: string) {
    return this.get<RoomDetail>(`/api/chat/rooms/${seg(id)}`);
  }
  createRoom(input: { kind: string; label?: string; topic?: string; groupId: string; groupInfo: string; participants?: string[]; workspaceVisible?: boolean; policy?: Partial<Policy> }) {
    return this.post<{ room: RoomView }>("/api/chat/rooms", input);
  }
  updateRoom(id: string, patch: Record<string, unknown>) {
    return this.patch<{ room: RoomView }>(`/api/chat/rooms/${seg(id)}`, patch);
  }
  events(id: string, q: { after?: number; before?: number; limit?: number }) {
    const sp = new URLSearchParams();
    if (q.after !== undefined) sp.set("after", String(q.after));
    if (q.before !== undefined) sp.set("before", String(q.before));
    if (q.limit !== undefined) sp.set("limit", String(q.limit));
    return this.get<{ events: EventView[]; seq: number; epoch: number }>(`/api/chat/rooms/${seg(id)}/events?${sp}`);
  }
  submit(id: string, input: SubmitInput) {
    return this.post<{ event: EventView; replayed: boolean }>(`/api/chat/rooms/${seg(id)}/events`, input);
  }
  groupInfo(id: string) {
    return this.get<{ groupInfo: string; epoch: number }>(`/api/chat/rooms/${seg(id)}/groupinfo`);
  }
  putGroupInfo(id: string, epoch: number, groupInfo: string) {
    return this.put<{ ok: boolean }>(`/api/chat/rooms/${seg(id)}/groupinfo`, { epoch, groupInfo });
  }
  markRead(id: string, seq: number) {
    return this.post<{ ok: boolean }>(`/api/chat/rooms/${seg(id)}/read`, { seq });
  }
  typing(id: string) {
    return this.post<void>(`/api/chat/rooms/${seg(id)}/typing`);
  }
  invites() {
    return this.get<{ invites: InviteView[] }>("/api/chat/invites");
  }
  acceptInvite(id: number) {
    return this.post<{ ok: boolean }>(`/api/chat/invites/${seg(id)}/accept`);
  }
  putBlob(roomId: string, bytes: Uint8Array) {
    return this.request<{ sha256: string; size: number }>("POST", `/api/chat/blobs?room=${encodeURIComponent(roomId)}`, undefined, {
      body: bytes as unknown as BodyInit,
      headers: { "Content-Type": "application/octet-stream" },
    });
  }
  async getBlob(sha: string): Promise<Uint8Array> {
    if (!isBlobRef(sha)) throw new Error("chat: not a blob reference");
    const buf = await this.request<ArrayBuffer>("GET", `/api/chat/blobs/${seg(sha)}`);
    return new Uint8Array(buf);
  }

  // ---- preferences, push, previews, webhooks ----
  prefs() {
    return this.get<Prefs>("/api/chat/me/prefs");
  }
  setPrefs(p: Prefs) {
    return this.put<Prefs>("/api/chat/me/prefs", p);
  }
  vapidKey() {
    return this.get<{ publicKey: string }>("/api/chat/push/vapid");
  }
  subscribePush(deviceId: number, subscription: unknown) {
    return this.post<{ ok: boolean }>(`/api/chat/me/devices/${seg(deviceId)}/push`, subscription);
  }
  unsubscribePush(deviceId: number) {
    return this.del<{ ok: boolean }>(`/api/chat/me/devices/${seg(deviceId)}/push`);
  }
  preview(url: string) {
    return this.get<PreviewCardResponse>(`/api/chat/preview?url=${encodeURIComponent(url)}`);
  }
  webhooks(roomId: string) {
    return this.get<{ webhooks: Webhook[]; botInRoom: boolean; botAddr: string }>(`/api/chat/rooms/${seg(roomId)}/webhooks`);
  }
  createWebhook(roomId: string, name: string) {
    return this.post<{ id: number; name: string; url: string }>(`/api/chat/rooms/${seg(roomId)}/webhooks`, { name });
  }
  deleteWebhook(roomId: string, id: number) {
    return this.del<{ ok: boolean }>(`/api/chat/rooms/${seg(roomId)}/webhooks/${seg(id)}`);
  }

  // migrateRoom moves a room's hub to another household (owners/admins, on
  // the hub). The MLS group is untouched; only who orders the log changes.
  migrateRoom(roomId: string, toInstance: string) {
    return this.post<{ ok: boolean; hubInstance: string; seq: number }>(`/api/chat/rooms/${seg(roomId)}/migrate`, { to: toInstance });
  }

  // bearer is the device session token this Api sends, if any — what a
  // native client persists to come back as the same device.
  bearer(): string | null {
    const auth = this.headers.Authorization;
    return auth?.startsWith("Bearer ") ? auth.slice(7) : null;
  }

  // streamUrl is the WebSocket address for /api/chat/stream.
  streamUrl(): string {
    const base = this.base || (typeof location !== "undefined" ? location.origin : "");
    let url = base.replace(/^http/, "ws") + "/api/chat/stream";
    const auth = this.headers.Authorization;
    if (auth?.startsWith("Bearer ")) url += "?token=" + encodeURIComponent(auth.slice(7));
    return url;
  }
}
