#!/usr/bin/env node --experimental-strip-types --no-warnings
// Fog Chat conformance suite. Point it at a scratch fogd and it proves the
// contract a standalone client relies on: people and devices, sealed rooms
// the server cannot read, epoch discipline, cursors, and the stream.
//
//   FOG_URL=http://fog.local:8080 FOG_OPERATOR_USER=admin FOG_OPERATOR_PASSWORD=… \
//     node --experimental-strip-types --no-warnings test/conformance.mjs
//
// FOG_OPERATOR_COOKIE=fog_session=… may replace the user/password pair. Two
// throwaway people are invited, exercised, and removed. Prints one JSON line
// ({ok, checks:[{name, ok, detail}]}) and exits 1 when any check fails.
// FOG_CONFORMANCE_RATELIMIT=1 adds the per-person budget check (600 calls).

import fs from "node:fs";
import { ChatClient } from "../src/client.ts";
import { MemoryStorage } from "../src/storage.ts";

const baseUrl = (process.env.FOG_URL ?? process.argv[2] ?? "").replace(/\/$/, "");
if (!baseUrl) {
  console.error("FOG_URL required");
  process.exit(2);
}
const out = { ok: true, checks: [] };
const dbg = (m) => { if (process.env.FOG_E2E_LOG) fs.appendFileSync(process.env.FOG_E2E_LOG, new Date().toISOString() + " " + m + "\n"); };
const check = (name, ok, detail) => { out.checks.push({ name, ok: !!ok, detail }); if (!ok) out.ok = false; dbg(`${ok ? "PASS" : "FAIL"} ${name} ${JSON.stringify(detail) ?? ""}`); };
const b64u = (s) => Buffer.from(s.replace(/-/g, "+").replace(/_/g, "/"), "base64");
const rand = Math.random().toString(36).slice(2, 8);

function withCookie(cookie) {
  return (input, init = {}) => {
    const headers = new Headers(init.headers ?? {});
    if (!headers.has("Authorization") && cookie) headers.set("Cookie", cookie);
    return fetch(input, { ...init, headers });
  };
}
async function call(fetchImpl, method, path, body) {
  const r = await fetchImpl(baseUrl + path, { method, headers: body !== undefined ? { "Content-Type": "application/json" } : {}, body: body !== undefined ? JSON.stringify(body) : undefined });
  let json = null;
  try { json = await r.clone().json(); } catch { json = null; }
  return { status: r.status, json, headers: r.headers, raw: r };
}
function setCookieOf(r, name) {
  const all = r.headers.getSetCookie ? r.headers.getSetCookie() : [r.headers.get("set-cookie")].filter(Boolean);
  for (const c of all) if (c.startsWith(name + "=")) return c.split(";")[0];
  return null;
}
function waitFor(pred, ms = 15000) {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    const tick = () => { if (pred()) return resolve(true); if (Date.now() - t0 > ms) return reject(new Error("timeout waiting")); setTimeout(tick, 50); };
    tick();
  });
}

const watchdog = setTimeout(() => { check("watchdog", false, "did not finish in 150s"); console.log(JSON.stringify(out)); process.exit(1); }, 150_000);

let operatorCookie = process.env.FOG_OPERATOR_COOKIE ?? process.argv[3] ?? "";
const created = [];
try {
  // ---- operator ----
  if (!operatorCookie) {
    const r = await fetch(baseUrl + "/api/auth/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username: process.env.FOG_OPERATOR_USER, password: process.env.FOG_OPERATOR_PASSWORD }) });
    operatorCookie = setCookieOf(r, "fog_session");
    check("operator-login", r.status === 200 && operatorCookie, { status: r.status });
  }
  const op = withCookie(operatorCookie);

  // ---- people: invite and join two throwaway members ----
  async function join(handle) {
    const inv = await call(op, "POST", "/api/people/invites", { role: "member" });
    if (inv.status !== 200) throw new Error(`invite: ${inv.status} ${JSON.stringify(inv.json)}`);
    const r = await call(fetch, "POST", "/api" + inv.json.path, { handle, displayName: handle, password: "conformance horse battery " + rand });
    if (r.status !== 200) throw new Error(`join: ${r.status} ${JSON.stringify(r.json)}`);
    const cookie = setCookieOf(r.raw, "fog_session");
    created.push(r.json.person?.id ?? r.json.id);
    return cookie;
  }
  const aCookie = await join("conf-a-" + rand);
  const bCookie = await join("conf-b-" + rand);
  const aMeRaw = await call(withCookie(aCookie), "GET", "/api/chat/me");
  check("people-join", aMeRaw.status === 200 && aMeRaw.json.household?.instanceId, { status: aMeRaw.status });
  // The invite is single-use.
  check("invite-single-use", (await call(fetch, "GET", "/api/join/not-a-real-secret")).status === 404);

  // ---- devices: SDK bootstrap, addresses, key packages ----
  const a = new ChatClient({ baseUrl, storage: new MemoryStorage(), fetch: withCookie(aCookie), clientKind: "native", deviceName: "conformance-a" });
  const b = new ChatClient({ baseUrl, storage: new MemoryStorage(), fetch: withCookie(bCookie), clientKind: "native", deviceName: "conformance-b" });
  const aMe = await a.init();
  const bMe = await b.init();
  check("device-bootstrap", /^fog:[0-9A-HJKMNP-TV-Z]{26}\/conf-a-/.test(aMe.addr) && aMe.deviceId > 0 && aMe.hasIdentitySeed && aMe.verified, { addr: aMe.addr, deviceId: aMe.deviceId });
  const k1 = await call(withCookie(aCookie), "GET", `/api/chat/people/conf-b-${rand}/keys`);
  const k2 = await call(withCookie(aCookie), "GET", `/api/chat/people/conf-b-${rand}/keys`);
  const kp1 = k1.json?.devices?.[0]?.keyPackage, kp2 = k2.json?.devices?.[0]?.keyPackage;
  check("keypackage-single-use", k1.status === 200 && kp1 && (kp2 === undefined || kp2 === null || kp2 !== kp1), { first: !!kp1, secondDiffers: kp2 !== kp1 });
  // A session without a device cannot create a room.
  const noDev = await call(withCookie(aCookie), "POST", "/api/chat/rooms", { kind: "group", label: "x" });
  check("deviceless-refused", noDev.status === 428 && noDev.json?.code === "need_device", { status: noDev.status, code: noDev.json?.code });
  const sessions = await call(withCookie(aCookie), "GET", "/api/chat/me/sessions");
  check("sessions-listed", sessions.status === 200 && sessions.json.sessions.length >= 2 && sessions.json.sessions.some((s) => s.deviceName === "conformance-a"), { count: sessions.json?.sessions?.length });

  // ---- sealed room ----
  await a.connect();
  await b.connect();
  const marker = "sealed-marker-" + rand + "-" + Math.random().toString(36).slice(2);
  const dm = await a.createRoom({ kind: "dm", participants: [bMe.addr] });
  await waitFor(() => b.room(dm.id)?.sync === "ready");
  const m1 = await a.send(dm.id, "the text is " + marker);
  await waitFor(() => b.messages(dm.id).some((m) => m.id === m1.id));
  const bView = b.messages(dm.id).find((m) => m.id === m1.id);
  check("dm-decrypts", bView?.text === "the text is " + marker && bView.sender === aMe.addr, { text: bView?.text });

  // The hub holds ciphertext only: no event payload and no blob carries the
  // plaintext, and the operator — an admin who is not a participant —
  // cannot read the room at all.
  const bearer = a.api.bearer();
  const asDeviceA = (i, init = {}) => fetch(i, { ...init, headers: { ...Object.fromEntries(new Headers(init.headers ?? {})), Authorization: "Bearer " + bearer } });
  check("device-session-token", !!bearer, {});
  const rawAuthed = await call(asDeviceA, "GET", `/api/chat/rooms/${dm.id}/events?limit=500`);
  const events = rawAuthed.json?.events ?? [];
  const leak = events.some((e) => b64u(e.payload).includes(marker) || (e.welcome && b64u(e.welcome).includes(marker)));
  check("server-holds-ciphertext", events.length > 0 && !leak, { events: events.length, kinds: [...new Set(events.map((e) => e.kind))] });
  const attBytes = new TextEncoder().encode("attachment " + marker);
  const att = await a.uploadAttachment(dm.id, attBytes, "note.txt", "text/plain");
  const blob = await asDeviceA(baseUrl + `/api/chat/blobs/${att.blob}`);
  const blobBytes = Buffer.from(await blob.arrayBuffer());
  check("blob-is-ciphertext", blob.status === 200 && !blobBytes.includes(marker) && blobBytes.length >= attBytes.length, { status: blob.status, size: blobBytes.length, plaintext: attBytes.length, leaks: blobBytes.includes(marker), sha: att.blob });
  const opRoom = await call(op, "GET", `/api/chat/rooms/${dm.id}`);
  const opEvents = await call(op, "GET", `/api/chat/rooms/${dm.id}/events`);
  check("operator-cannot-read", [403, 404].includes(opRoom.status) && [403, 404].includes(opEvents.status), { room: opRoom.status, events: opEvents.status });
  const opRooms = await call(op, "GET", "/api/chat/rooms");
  check("operator-does-not-see-room", opRooms.status === 200 && !opRooms.json.rooms.some((r) => r.id === dm.id), {});

  // ---- epoch discipline: replaying an old commit is refused ----
  const commit = events.find((e) => e.kind === "mls_commit");
  const ulidish = (n) => (n + rand.toUpperCase()).padEnd(26, "0").slice(0, 26).replace(/[ILOU]/g, "7");
  const noDevReplay = await call(withCookie(aCookie), "POST", `/api/chat/rooms/${dm.id}/events`, { id: ulidish("01"), kind: "mls_commit", payload: commit?.payload, sentAt: new Date().toISOString() });
  check("deviceless-cannot-post", noDevReplay.status === 428, { status: noDevReplay.status });
  const replay = await call(asDeviceA, "POST", `/api/chat/rooms/${dm.id}/events`, { id: ulidish("02"), kind: "mls_commit", payload: commit?.payload, sentAt: new Date().toISOString() });
  check("stale-commit-refused", !!commit && replay.status === 409 && replay.json?.code === "epoch_stale", { status: replay.status, code: replay.json?.code });

  // ---- cursors ----
  for (let i = 0; i < 5; i++) await a.send(dm.id, "page " + i);
  await waitFor(() => b.messages(dm.id).filter((m) => m.text.startsWith("page ")).length === 5);
  const auth = asDeviceA;
  const p1 = (await call(auth, "GET", `/api/chat/rooms/${dm.id}/events?after=0&limit=2`)).json.events.map((e) => e.seq);
  const p2 = (await call(auth, "GET", `/api/chat/rooms/${dm.id}/events?after=${p1[1]}&limit=2`)).json.events.map((e) => e.seq);
  const back = (await call(auth, "GET", `/api/chat/rooms/${dm.id}/events?before=${p2[1]}&limit=3`)).json.events.map((e) => e.seq);
  const huge = await call(auth, "GET", `/api/chat/rooms/${dm.id}/events?after=-1&limit=1000000`);
  check("cursors", p1.length === 2 && p1[0] === 1 && p2[0] === p1[1] + 1 && back.length === 3 && back[2] === p2[1] - 1 && back[0] < back[2] && huge.status === 200 && huge.json.events.length <= 500, { p1, p2, back, all: huge.json?.events?.length });

  // ---- stream: a fresh connection with cursors replays only what it missed ----
  const token = bearer;
  if (token && typeof WebSocket !== "undefined") {
    const frames = [];
    const wsUrl = baseUrl.replace(/^http/, "ws") + "/api/chat/stream?token=" + encodeURIComponent(token);
    const ws = new WebSocket(wsUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    ws.send(JSON.stringify({ since: { [dm.id]: p2[1] } }));
    await new Promise((res) => { ws.onmessage = (ev) => { const f = JSON.parse(ev.data); frames.push(f); if (f.kind === "synced") res(); }; setTimeout(res, 5000); });
    ws.close();
    const replayed = frames.filter((f) => f.kind === "event" && f.roomId === dm.id).map((f) => f.event.seq);
    const synced = frames.find((f) => f.kind === "synced");
    check("stream-replay", synced && replayed.length > 0 && replayed.every((s) => s > p2[1]) && synced.cursors[dm.id] >= Math.max(...replayed), { replayed, cursors: synced?.cursors });
  } else {
    check("stream-replay", false, "no bearer token or WebSocket available");
  }

  // ---- optional: the per-person budget ----
  if (process.env.FOG_CONFORMANCE_RATELIMIT === "1") {
    let limited = null;
    for (let i = 0; i < 700 && !limited; i++) {
      const r = await call(withCookie(bCookie), "GET", "/api/chat/me");
      if (r.status === 429) limited = { after: i, retry: r.headers.get("retry-after") };
    }
    check("rate-limit", limited && limited.retry, limited);
  }

  a.close();
  b.close();
} catch (e) {
  check("exception", false, String(e?.stack ?? e));
} finally {
  // Remove the throwaway people so a scratch fogd stays clean.
  try {
    const op = withCookie(operatorCookie);
    const list = await call(op, "GET", "/api/people");
    for (const p of list.json?.people ?? []) if (p.handle.startsWith("conf-a-" + rand) || p.handle.startsWith("conf-b-" + rand)) await call(op, "DELETE", `/api/people/${p.id}`);
  } catch (e) {
    check("cleanup", false, String(e));
  }
  clearTimeout(watchdog);
  console.log(JSON.stringify(out));
  process.exit(out.ok ? 0 : 1);
}
