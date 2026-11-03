// Driven by internal/server/chat_sdk_e2e_test.go: two real SDK clients
// against a real fogd over HTTP. Prints one JSON object of observations the
// Go test asserts on. Argv: baseUrl adminCookie daveCookie.
//
// Node has no cookie jar, so each client is given its person cookie for the
// bootstrap calls and then switches to the bearer token registerDevice
// returns (clientKind: native) — the same path a phone app takes.

import fs from "node:fs";
import { ChatClient } from "../src/client.ts";
import { MemoryStorage } from "../src/storage.ts";

const [baseUrl, adminCookie, daveCookie] = process.argv.slice(2);
const out = { steps: [] };
// Progress goes to a file synchronously, so a hang still leaves a trail.
const dbg = (m) => { if (process.env.FOG_E2E_LOG) fs.appendFileSync(process.env.FOG_E2E_LOG, new Date().toISOString() + " " + m + "\n"); };
const step = (name, data) => { out.steps.push({ name, ...data }); dbg("step " + name); };

function withCookie(cookie) {
  return (input, init = {}) => {
    const headers = new Headers(init.headers ?? {});
    if (!headers.has("Authorization")) headers.set("Cookie", cookie);
    return fetch(input, { ...init, headers });
  };
}

function waitFor(pred, ms = 10000) {
  const where = (new Error().stack ?? "").split("\n")[2]?.trim() ?? "";
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    const tick = () => {
      if (pred()) return resolve(true);
      if (Date.now() - t0 > ms) return reject(new Error(`timeout waiting ${where}`));
      setTimeout(tick, 50);
    };
    tick();
  });
}

// A watchdog: a hang anywhere still yields the steps reached so far.
const watchdog = setTimeout(() => {
  out.error = "watchdog: script did not finish in 150s";
  console.log(JSON.stringify(out));
  process.exit(0);
}, 150_000);

try {
  const op = new ChatClient({ baseUrl, storage: new MemoryStorage(), fetch: withCookie(adminCookie), clientKind: "native", deviceName: "op-node" });
  const dave = new ChatClient({ baseUrl, storage: new MemoryStorage(), fetch: withCookie(daveCookie), clientKind: "native", deviceName: "dave-node" });
  const opMe = await op.init();
  const daveMe = await dave.init();
  step("init", { opVerified: opMe.verified, daveVerified: daveMe.verified, opHasIdentity: opMe.hasIdentitySeed, opAddr: opMe.addr, daveAddr: daveMe.addr });
  await op.connect();
  await dave.connect();

  // The operator creates a group and adds dave; dave's Welcome arrives over
  // the stream and his client joins.
  const room = await op.createRoom({ kind: "group", label: "Family" });
  await op.addMembers(room.id, [daveMe.addr]);
  await waitFor(() => dave.room(room.id)?.sync === "ready");
  step("dave-joined", { epoch: dave.room(room.id)?.epoch, members: dave.mlsMembers(room.id).map((m) => m.identity).sort() });

  // Messages both ways, with a thread and a reaction.
  const m1 = await op.send(room.id, "hello dave");
  await waitFor(() => dave.messages(room.id).some((m) => m.id === m1.id));
  const m2 = await dave.send(room.id, "hello back", { topicId: m1.id });
  await dave.react(room.id, m1.id, "👍");
  await waitFor(() => op.messages(room.id).find((m) => m.id === m1.id)?.reactions.length === 1);
  await waitFor(() => op.messages(room.id, { topicId: m1.id }).some((m) => m.id === m2.id));
  const opView = op.messages(room.id);
  step("messages", {
    opSees: opView.filter((m) => !m.system).map((m) => m.text),
    daveSees: dave.messages(room.id).filter((m) => !m.system).map((m) => m.text),
    thread: op.messages(room.id, { topicId: m1.id }).filter((m) => !m.system).map((m) => m.text),
    reactions: opView.find((m) => m.id === m1.id)?.reactions,
    replies: opView.find((m) => m.id === m1.id)?.replies,
  });

  // Edit and delete propagate.
  await op.edit(room.id, m1.id, "hello dave (edited)");
  await waitFor(() => dave.messages(room.id).find((m) => m.id === m1.id)?.edited === true);
  await dave.delete(room.id, m2.id);
  await waitFor(() => op.messages(room.id, { topicId: m1.id }).find((m) => m.id === m2.id)?.deleted === true);
  step("edit-delete", { edited: dave.messages(room.id).find((m) => m.id === m1.id)?.text, deleted: op.messages(room.id, { topicId: m1.id }).find((m) => m.id === m2.id)?.deleted });

  // An encrypted attachment round-trips.
  const bytes = new TextEncoder().encode("attachment bytes");
  const att = await op.uploadAttachment(room.id, bytes, "note.txt", "text/plain");
  const m3 = await op.send(room.id, "see attached", { attachments: [att] });
  await waitFor(() => dave.messages(room.id).some((m) => m.id === m3.id));
  const got = await dave.openAttachment(dave.messages(room.id).find((m) => m.id === m3.id).attachments[0]);
  step("attachment", { roundTrip: new TextDecoder().decode(got) });

  // Dave joins #fog (hubbed by the bot) externally and reads the bot's notice.
  const fog = dave.available().find((a) => a.label === "fog");
  step("fog-available", { present: !!fog, joinable: fog?.joinable });
  if (fog) {
    await dave.joinChannel(fog.id);
    // The notice the bot posted before dave joined stays unreadable to him
    // (no history bundle yet); a fresh exchange proves the bot is live.
    await dave.send(fog.id, "ping from dave");
    await waitFor(() => dave.messages(fog.id).some((m) => m.system === "fog" && m.text.startsWith("pong")), 15000).catch(() => {});
    step("fog-joined", { epoch: dave.room(fog.id)?.epoch, botMessages: dave.messages(fog.id).filter((m) => m.system === "fog").map((m) => m.text) });
  }

  // Search finds decrypted text.
  step("search", { hits: op.search("edited").filter((m) => !m.system).map((m) => m.text) });

  // ---- Phase 3: a second device for dave ----
  // It arrives unverified (this browser holds no identity seed); dave's
  // first device attests it, then adds it to every room, and shares
  // history where the policy allows.
  dbg("phase3: second device");
  const dave2 = new ChatClient({ baseUrl, storage: new MemoryStorage(), fetch: withCookie(daveCookie), clientKind: "native", deviceName: "dave-tablet" });
  const d2me = await dave2.init();
  await dave2.connect();
  step("second-device", { verified: d2me.verified, hasIdentity: d2me.hasIdentitySeed, sameAddr: d2me.addr === daveMe.addr });
  dbg("attest tablet");
  await dave.attestOtherDevice(d2me.deviceId, d2me.deviceKey);
  dbg("sibling sync");
  await dave.syncSiblingDevices();
  dbg("wait tablet ready");
  await waitFor(() => dave2.room(room.id)?.sync === "ready", 20000);
  // The group's history is since-join, so the tablet sees only what follows.
  const m4 = await op.send(room.id, "for both of dave's devices");
  await waitFor(() => dave2.messages(room.id).some((m) => m.id === m4.id));
  step("sibling", { tabletSees: dave2.messages(room.id).filter((m) => !m.system).map((m) => m.text), tabletSyncedRoom: dave2.room(room.id)?.sync });

  // A channel shares history: create one, add dave, and his tablet (already
  // a sibling) gets the past through a history bundle from the operator.
  dbg("channel");
  const chan = await op.createRoom({ kind: "channel", label: "kitchen" });
  const c1 = await op.send(chan.id, "before dave");
  await op.addMembers(chan.id, [daveMe.addr]);
  await waitFor(() => dave.messages(chan.id).some((m) => m.id === c1.id), 20000);
  await dave.syncSiblingDevices();
  await waitFor(() => dave2.messages(chan.id).some((m) => m.id === c1.id), 20000);
  step("history", { daveSees: dave.messages(chan.id).filter((m) => !m.system).map((m) => m.text), tabletSees: dave2.messages(chan.id).filter((m) => !m.system).map((m) => m.text) });

  // Membership lines appear in the timeline.
  step("membership-lines", { lines: op.messages(chan.id).filter((m) => m.system === "membership").map((m) => m.text) });

  // Recovery: dave enables a backup, then "loses" the phone: a fresh
  // storage is restored from the phrase and binds to the same device. It
  // brings back history and verification marks, replaces the device's key
  // packages, leaves its old places in its rooms (only this device), and
  // joins nothing by itself: the operator adds it back, and it reads on.
  dbg("backup");
  const phrase = await dave.enableBackup();
  await dave.markVerified(opMe.addr, opMe.identityKey);
  await dave.backupNow();
  dave.close(); // the lost phone
  const restoredStore = new MemoryStorage();
  dbg("restore");
  const restored = await ChatClient.restoreFromPhrase({ baseUrl, storage: restoredStore, fetch: withCookie(daveCookie), clientKind: "native" }, phrase);
  const dave3 = new ChatClient({ baseUrl, storage: restoredStore, fetch: withCookie(daveCookie), clientKind: "native", deviceName: "restored" });
  dbg("dave3 init");
  const d3me = await dave3.init();
  dbg("dave3 connect");
  await dave3.connect();
  dbg("dave3 connected");
  const joinedNothing = dave3.room(room.id)?.sync === "no-keys";
  // The operator's device removes the old leaf the restored device left,
  // then adds it back: a Welcome to its fresh key packages.
  await waitFor(() => !op.mlsMembers(room.id).some((l) => Buffer.from(l.signatureKey).toString("base64url") === daveMe.deviceKey), 20000);
  await op.addMembers(room.id, [daveMe.addr]);
  await waitFor(() => dave3.room(room.id)?.sync === "ready", 20000);
  const m5 = await op.send(room.id, "after restore");
  await waitFor(() => dave3.messages(room.id).some((m) => m.id === m5.id), 20000);
  step("restore", {
    phraseWords: phrase.split(" ").length,
    restoredMessages: restored.messages,
    sameDevice: d3me.deviceId === daveMe.deviceId && d3me.deviceKey === daveMe.deviceKey,
    verifiedOp: await dave3.isVerified(opMe.addr, opMe.identityKey),
    hasHistory: dave3.messages(room.id).some((m) => m.text === "hello dave (edited)"),
    joinedNothing,
    readsNew: dave3.messages(room.id).some((m) => m.id === m5.id),
  });

  // Revoking the tablet: its leaf leaves every room on the next sibling sync
  // of a device that holds the rooms' keys.
  dbg("revoke");
  await dave3.api.revokeDevice(d2me.deviceId);
  await dave3.syncSiblingDevices();
  await waitFor(() => dave3.mlsMembers(room.id).filter((l) => l.identity === daveMe.addr).length === 1, 20000).catch(() => {});
  step("revoke", { tabletLeaves: dave3.mlsMembers(room.id).filter((l) => l.identity === daveMe.addr).length });
  dave2.close();
  dave3.close();
  clearTimeout(watchdog);

  // Race: a room change and a send at once both land.
  await Promise.all([op.updateRoom(room.id, { topic: "racing" }), op.send(room.id, "race message")]);
  step("done", { ok: true });
  op.close();
} catch (e) {
  out.error = String(e && e.stack ? e.stack : e);
}
console.log(JSON.stringify(out));
process.exit(0);
