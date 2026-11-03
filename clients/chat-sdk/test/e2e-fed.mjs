// Two fogs, two SDK clients (internal/server/chat_sdk_fed_e2e_test.go): the
// operator on A adds eve, who lives on B, to a room hubbed on A. Eve's
// Welcome crosses through A's outbox, B mirrors the room, and messages flow
// both ways through B's relay and A's fan-out. Argv: aUrl adminCookie bUrl eveCookie.

import { ChatClient } from "../src/client.ts";
import { MemoryStorage } from "../src/storage.ts";

const [aUrl, adminCookie, bUrl, eveCookie] = process.argv.slice(2);
const out = { steps: [] };
const step = (name, data) => out.steps.push({ name, ...data });

function withCookie(cookie) {
  return (input, init = {}) => {
    const headers = new Headers(init.headers ?? {});
    if (!headers.has("Authorization")) headers.set("Cookie", cookie);
    return fetch(input, { ...init, headers });
  };
}

function waitFor(pred, ms = 15000) {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    const tick = () => {
      if (pred()) return resolve(true);
      if (Date.now() - t0 > ms) return reject(new Error("timeout waiting"));
      setTimeout(tick, 50);
    };
    tick();
  });
}

try {
  const op = new ChatClient({ baseUrl: aUrl, storage: new MemoryStorage(), fetch: withCookie(adminCookie), clientKind: "native", deviceName: "op" });
  const eve = new ChatClient({ baseUrl: bUrl, storage: new MemoryStorage(), fetch: withCookie(eveCookie), clientKind: "native", deviceName: "eve" });
  const opMe = await op.init();
  const eveMe = await eve.init();
  // Heartbeats are the Go test's job (it triggers them after devices exist);
  // signal readiness by writing a marker step and waiting for the directory.
  step("devices", { op: opMe.addr, eve: eveMe.addr });
  await op.connect();
  await eve.connect();
  // Poll until B's heartbeat has carried eve's device to A.
  for (let i = 0; i < 100; i++) {
    await op.directory();
    if (op.households().some((h) => h.people.some((p) => p.handle === "eve" && p.devices.length > 0))) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  step("directory", { households: op.households().map((h) => ({ name: h.name, people: h.people.map((p) => p.handle) })) });

  const room = await op.createRoom({ kind: "group", label: "Cousins" });
  const res = await op.addMembers(room.id, [eveMe.addr]);
  step("add", res);
  await waitFor(() => eve.room(room.id)?.sync === "ready");
  step("eve-joined", { hubHere: eve.room(room.id)?.hubHere, households: eve.room(room.id)?.households, members: eve.mlsMembers(room.id).map((m) => m.identity).sort() });

  const m1 = await eve.send(room.id, "hi from B");
  await waitFor(() => op.messages(room.id).some((m) => m.id === m1.id));
  const m2 = await op.send(room.id, "hi from A");
  await waitFor(() => eve.messages(room.id).some((m) => m.id === m2.id));
  step("messages", { opSees: op.messages(room.id).filter((m) => !m.system).map((m) => m.text), eveSees: eve.messages(room.id).filter((m) => !m.system).map((m) => m.text), eveNames: eve.messages(room.id).filter((m) => !m.system).map((m) => eve.nameFor(m.sender)) });

  const att = await eve.uploadAttachment(room.id, new TextEncoder().encode("cross-household bytes"), "x.txt", "text/plain");
  const m3 = await eve.send(room.id, "file", { attachments: [att] });
  await waitFor(() => op.messages(room.id).some((m) => m.id === m3.id));
  const got = await op.openAttachment(op.messages(room.id).find((m) => m.id === m3.id).attachments[0]);
  step("attachment", { roundTrip: new TextDecoder().decode(got) });
  step("done", { ok: true });
  op.close();
  eve.close();
} catch (e) {
  out.error = String(e && e.stack ? e.stack : e);
}
console.log(JSON.stringify(out));
process.exit(0);
