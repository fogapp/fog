import { test } from "node:test";
import assert from "node:assert/strict";
import { decodeCbor, encodeCbor } from "../src/cbor.ts";
import { decodeContent, encodeContent, type Content } from "../src/content.ts";
import { attestDevice, b64u, generateKeyPair, ulid, unb64, verifyAttestation } from "../src/keys.ts";
import { Mls } from "../src/mls.ts";
import { ChatClient, CryptoUnavailableError, webCryptoAvailable, type Message, type MessageView } from "../src/client.ts";
import { reconnectDelay, Throttle } from "../src/pace.ts";
import { MemoryStorage } from "../src/storage.ts";
import { foldTimeline, inThread, isReply, sameView } from "../src/timeline.ts";

test("cbor round-trips the content shapes", () => {
  const v = { v: 1, type: "message", body: { format: "text/markdown", text: "héllo ✓" }, mentions: ["a", "b"], key: new Uint8Array([1, 2, 3]), n: 70000, neg: -5, big: 2 ** 40, flag: true, nil: null };
  const out = decodeCbor(encodeCbor(v)) as Record<string, unknown>;
  assert.equal(out.type, "message");
  assert.deepEqual(out.body, { format: "text/markdown", text: "héllo ✓" });
  assert.deepEqual(out.mentions, ["a", "b"]);
  assert.deepEqual(Array.from(out.key as Uint8Array), [1, 2, 3]);
  assert.equal(out.n, 70000);
  assert.equal(out.neg, -5);
  assert.equal(out.big, 2 ** 40);
  assert.equal(out.flag, true);
  assert.equal(out.nil, null);
});

test("cbor map keys are canonically ordered", () => {
  const a = encodeCbor({ zz: 1, a: 2, bb: 3 });
  const b = encodeCbor({ a: 2, bb: 3, zz: 1 });
  assert.deepEqual(a, b);
});

test("content encodes and decodes", () => {
  const c: Content = {
    v: 1, type: "message", id: ulid(), sentAt: "2026-09-04T00:00:00Z",
    body: { format: "text/markdown", text: "**hi**" }, topicId: "T", inReplyTo: "R", mentions: ["fog:X/dave"],
    attachments: [{ blob: "ab".repeat(32), key: new Uint8Array(32), nonce: new Uint8Array(12), name: "a.png", mime: "image/png", size: 10 }],
  };
  const d = decodeContent(encodeContent(c));
  assert.equal(d.type, "message");
  assert.equal(d.body?.text, "**hi**");
  assert.equal(d.topicId, "T");
  assert.equal(d.inReplyTo, "R");
  assert.deepEqual(d.mentions, ["fog:X/dave"]);
  assert.equal(d.attachments?.[0].name, "a.png");
  assert.equal(d.attachments?.[0].key.length, 32);
  assert.throws(() => decodeContent(encodeCbor({ v: 2, type: "message" })));
});

test("device attestation verifies and forgeries do not", () => {
  const identity = generateKeyPair();
  const device = generateKeyPair();
  const att = attestDevice(identity, device.publicKey);
  assert.ok(verifyAttestation(identity.publicKey, device.publicKey, att));
  const other = generateKeyPair();
  assert.ok(!verifyAttestation(other.publicKey, device.publicKey, att));
  assert.ok(!verifyAttestation(identity.publicKey, other.publicKey, att));
});

test("base64: url-safe round-trips, and Go's standard padded form decodes too", () => {
  const bytes = new Uint8Array([0xfb, 0xff, 0xfe, 0x00, 0x01]);
  assert.equal(b64u(bytes), "-__-AAE");
  assert.deepEqual(unb64("-__-AAE"), bytes);
  // encoding/json's []byte: "+/" alphabet, "=" padding.
  assert.deepEqual(unb64("+//+AAE="), bytes);
});

test("ulids are 26 Crockford characters and sort by time", () => {
  const a = ulid(1000), b = ulid(2000);
  assert.match(a, /^[0-9A-HJKMNP-TV-Z]{26}$/);
  assert.ok(a < b);
});

test("mls: two devices share a room, a third joins externally, keys rotate", async () => {
  const mls = await Mls.init();
  const alice = generateKeyPair(), bob = generateKeyPair(), carol = generateKeyPair();
  const kpA = await mls.keyPackage("fog:X/alice", alice);
  const kpB = await mls.keyPackage("fog:X/bob", bob);
  let a = await mls.createGroup(new Uint8Array(16), kpA);

  // Alice adds Bob; the Welcome joins him.
  const add = await mls.addMembers(a, [mls.parseKeyPackage(kpB.raw)]);
  a = add.newState;
  assert.ok(add.welcome);
  let b = await mls.joinFromWelcome(add.welcome!, kpB);
  assert.equal(mls.epoch(a), 1);
  assert.equal(mls.epoch(b), 1);
  assert.deepEqual(mls.members(a).map((m) => m.identity), ["fog:X/alice", "fog:X/bob"]);

  // Messages both ways, with state serialization in between.
  const m1 = await mls.encrypt(a, encodeContent({ v: 1, type: "message", body: { format: "text/markdown", text: "hi bob" } }));
  a = m1.newState;
  b = mls.restore(mls.serialize(b));
  const r1 = await mls.process(b, m1.wire);
  b = r1.newState;
  assert.equal(decodeContent(r1.plaintext!).body?.text, "hi bob");

  // Carol joins externally from Bob's group info; Alice applies the commit.
  const kpC = await mls.keyPackage("fog:X/carol", carol);
  const ext = await mls.externalJoin(await mls.groupInfo(b), kpC);
  const c = ext.newState;
  a = (await mls.process(a, ext.commit)).newState;
  b = (await mls.process(b, ext.commit)).newState;
  assert.equal(mls.epoch(a), 2);
  assert.equal(mls.members(c).length, 3);

  // Carol speaks; both hear.
  const m2 = await mls.encrypt(c, encodeContent({ v: 1, type: "message", body: { format: "text/markdown", text: "hello all" } }));
  assert.equal(decodeContent((await mls.process(a, m2.wire)).plaintext!).body?.text, "hello all");
  assert.equal(decodeContent((await mls.process(b, m2.wire)).plaintext!).body?.text, "hello all");

  // Alice removes Bob; Bob's copy can no longer read what follows.
  const rm = await mls.removeLeaves(a, mls.members(a).filter((m) => m.identity === "fog:X/bob").map((m) => m.index));
  a = rm.newState;
  const cAfter = (await mls.process(c, rm.commit)).newState;
  const m3 = await mls.encrypt(a, encodeContent({ v: 1, type: "message", body: { format: "text/markdown", text: "bob is gone" } }));
  assert.equal(decodeContent((await mls.process(cAfter, m3.wire)).plaintext!).body?.text, "bob is gone");
  await assert.rejects(mls.process(b, m3.wire));
});

test("mls: group info carries an RFC-encoded external_pub and still joins", async () => {
  const mls = await Mls.init();
  const alice = generateKeyPair(), bob = generateKeyPair();
  const kpA = await mls.keyPackage("fog:X/alice", alice);
  const a = await mls.createGroup(new Uint8Array(16), kpA);
  const wire = await mls.groupInfo(a);
  // MLSMessage header then GroupInfo; find the external_pub extension (type 4)
  // and check it is 33 bytes: a one-byte length prefix and a 32-byte key.
  const { decodeMlsMessage } = await import("ts-mls");
  const gi = decodeMlsMessage(wire, 0)![0];
  assert.equal(gi.wireformat, "mls_group_info");
  if (gi.wireformat !== "mls_group_info") return;
  const ext = gi.groupInfo.extensions.find((e) => e.extensionType === "external_pub")!;
  assert.equal(ext.extensionData.length, 33);
  assert.equal(ext.extensionData[0], 32);
  const kpB = await mls.keyPackage("fog:X/bob", bob);
  const joined = await mls.externalJoin(wire, kpB);
  assert.equal(mls.epoch(joined.newState), 1);
  const after = (await mls.process(a, joined.commit)).newState;
  assert.equal(mls.members(after).length, 2);
});

test("content: history bundles nest whole messages", () => {
  const inner: Content = { v: 1, type: "message", id: "A", body: { format: "text/markdown", text: "earlier" }, attachments: [{ blob: "cd".repeat(32), key: new Uint8Array(32), nonce: new Uint8Array(12) }] };
  const c: Content = { v: 1, type: "history", historyFor: ["k1", "k2"], history: [{ id: "A", seq: 3, sender: "fog:X/a", hubAt: "2026-09-04T00:00:00Z", content: inner }] };
  const d = decodeContent(encodeContent(c));
  assert.equal(d.type, "history");
  assert.deepEqual(d.historyFor, ["k1", "k2"]);
  assert.equal(d.history?.length, 1);
  assert.equal(d.history?.[0].seq, 3);
  assert.equal(d.history?.[0].content.body?.text, "earlier");
  assert.equal(d.history?.[0].content.attachments?.[0].key.length, 32);
});

test("backup: seal and open round-trip under a phrase", async () => {
  const { generatePhrase, keyFromPhrase, seal, open, validPhrase } = await import("../src/backup.ts");
  const phrase = generatePhrase();
  assert.equal(phrase.split(" ").length, 12);
  assert.ok(validPhrase(phrase));
  assert.ok(!validPhrase("not a phrase at all"));
  const key = await keyFromPhrase(phrase);
  const payload = { v: 1 as const, addr: "fog:X/a", deviceId: 3, deviceSeed: new Uint8Array(32).fill(7), identitySeed: new Uint8Array(32).fill(9), messages: [{ id: "m", roomId: "r", content: { v: 1, type: "message" } }], cursors: { r: 4 }, pins: {}, verified: {}, savedAt: "now" };
  const blob = await seal(key, payload);
  const back = await open(key, blob);
  assert.equal(back.addr, "fog:X/a");
  assert.equal(back.deviceSeed[0], 7);
  assert.equal(back.identitySeed?.[3], 9);
  assert.equal(back.cursors.r, 4);
  await assert.rejects(open(await keyFromPhrase(generatePhrase()), blob));
});

test("content: a link preview card rides inside the encrypted message", () => {
  const c: Content = {
    v: 1,
    type: "message",
    body: { format: "text/markdown", text: "look https://example.com" },
    preview: { url: "https://example.com", title: "Example", site: "Example", description: "A page." },
  };
  const back = decodeContent(encodeContent(c));
  assert.deepEqual(back.preview, { url: "https://example.com", title: "Example", description: "A page.", image: undefined, site: "Example" });
  const r: Content = { v: 1, type: "receipt", readSeq: 42 };
  assert.equal(decodeContent(encodeContent(r)).readSeq, 42);
});

test("client: without WebCrypto, init fails with a named, explained error", async () => {
  assert.equal(webCryptoAvailable(), true);
  const real = Object.getOwnPropertyDescriptor(globalThis, "crypto")!;
  Object.defineProperty(globalThis, "crypto", { value: { getRandomValues: crypto.getRandomValues.bind(crypto) }, configurable: true });
  try {
    assert.equal(webCryptoAvailable(), false);
    const c = new ChatClient({ baseUrl: "http://fog.example", storage: new MemoryStorage(), fetch: () => Promise.reject(new Error("must not be called")) });
    await assert.rejects(c.init(), (e: unknown) => e instanceof CryptoUnavailableError && /https/.test((e as Error).message) && (e as CryptoUnavailableError).code === "crypto_unavailable");
    await assert.rejects(ChatClient.restoreFromPhrase({ baseUrl: "http://fog.example", storage: new MemoryStorage() }, "x"), CryptoUnavailableError);
  } finally {
    Object.defineProperty(globalThis, "crypto", real);
  }
  assert.equal(webCryptoAvailable(), true);
});

test("pace: a throttle lets one call per key through each interval", () => {
  let now = 1000;
  const t = new Throttle(2500, () => now);
  assert.equal(t.ready("a"), true);
  assert.equal(t.ready("a"), false);
  assert.equal(t.ready("b"), true, "keys are independent");
  now += 2499;
  assert.equal(t.ready("a"), false);
  now += 1;
  assert.equal(t.ready("a"), true);
});

test("pace: reconnects back off, cap at half a minute, and spread out", () => {
  assert.equal(reconnectDelay(0, () => 0), 500);
  assert.equal(reconnectDelay(0, () => 1), 1000);
  assert.equal(reconnectDelay(3, () => 1), 8000);
  assert.equal(reconnectDelay(10, () => 1), 30_000);
  assert.equal(reconnectDelay(10, () => 0), 15_000);
  assert.equal(reconnectDelay(1000, () => 0.5), 22_500, "a long outage stays capped");
});

function msg(id: string, seq: number, content: Content, sender = "fog:h/ann"): Message {
  return { id, roomId: "r", seq, sender, senderDevice: "", hubAt: `2026-01-01T00:00:${String(seq).padStart(2, "0")}Z`, sentAt: "", content, mine: false };
}

test("timeline: edits by the author, deletes, reactions and thread counts fold in", () => {
  const views = foldTimeline("r", [
    msg("m1", 1, { v: 1, type: "message", body: { format: "text/markdown", text: "hello" } }),
    msg("m2", 2, { v: 1, type: "message", body: { format: "text/markdown", text: "reply" }, topicId: "m1" }),
    msg("e2", 3, { v: 1, type: "edit", replaces: "m1", body: { format: "text/markdown", text: "hijacked" } }, "fog:h/bob"),
    msg("e1", 4, { v: 1, type: "edit", replaces: "m1", body: { format: "text/markdown", text: "hello!" } }),
    msg("x1", 5, { v: 1, type: "reaction", reaction: { target: "m1", emoji: "👍", add: true } }, "fog:h/bob"),
    msg("m3", 6, { v: 1, type: "message", body: { format: "text/markdown", text: "gone" } }),
    msg("d1", 7, { v: 1, type: "delete", replaces: "m3" }),
  ], (a) => a);
  assert.deepEqual(views.map((v) => v.id), ["m1", "m2", "m3"]);
  const [root, reply, gone] = views;
  assert.equal(root.text, "hello!", "only the author's edit applies");
  assert.equal(root.replies, 1);
  assert.deepEqual(root.reactions, [{ emoji: "👍", by: ["fog:h/bob"] }]);
  assert.equal(isReply(reply), true);
  assert.equal(inThread(reply, "m1") && inThread(root, "m1"), true);
  assert.equal(gone.deleted, true);
  assert.equal(gone.text, "");
});

class CountingStorage extends MemoryStorage {
  puts = 0;
  batches: number[] = [];
  override async put(ns: string, key: string, value: unknown) {
    this.puts++;
    await super.put(ns, key, value);
  }
  override async putMany(rows: Array<{ ns: string; key: string; value: unknown }>) {
    this.batches.push(rows.length);
    await super.putMany(rows);
  }
}

test("client: a batch of messages lands in one write and one event, and reads fold once", async () => {
  const storage = new CountingStorage();
  const c = new ChatClient({ baseUrl: "http://fog.example", storage, fetch: () => Promise.reject(new Error("must not be called")) });
  // The private write path is what catch-up drives; reach it directly.
  const inner = c as unknown as {
    storeMessage(m: Message, b?: unknown): Promise<void>;
    flush(b: unknown): Promise<void>;
  };
  let events = 0;
  c.addEventListener("messages", () => events++);
  const b = { rows: new Map(), messageRooms: new Set(), refreshRooms: false };
  for (let i = 1; i <= 3; i++) {
    await inner.storeMessage(msg(`m${i}`, i, { v: 1, type: "message", body: { format: "text/markdown", text: `n${i}` } }), b);
  }
  assert.equal(events, 0, "nothing is announced before the batch lands");
  assert.equal(storage.puts, 0);
  await inner.flush(b);
  assert.deepEqual(storage.batches, [3]);
  assert.equal(events, 1);

  const first = c.messages("r");
  assert.equal(first.length, 3);
  assert.equal(c.messages("r").length, 3);
  assert.equal(c.search("n2").length, 1);
  const again = (c as unknown as { timeline(id: string): MessageView[] }).timeline("r");
  assert.equal(again, (c as unknown as { timeline(id: string): MessageView[] }).timeline("r"), "an unchanged room is not folded again");
  await inner.storeMessage(msg("m4", 4, { v: 1, type: "message", body: { format: "text/markdown", text: "n4" } }));
  assert.equal(c.messages("r").length, 4, "a new message refolds");
  assert.equal(storage.puts, 1);
  assert.equal(events, 2);
});

test("client: reads keep their identity until the room changes, and a refold reuses unchanged views", async () => {
  const c = new ChatClient({ baseUrl: "http://fog.example", storage: new MemoryStorage(), fetch: () => Promise.reject(new Error("must not be called")) });
  const inner = c as unknown as { storeMessage(m: Message): Promise<void> };
  const text = (t: string, extra: Partial<Content> = {}): Content => ({ v: 1, type: "message", body: { format: "text/markdown", text: t }, ...extra });
  await inner.storeMessage(msg("m1", 1, text("hello")));
  await inner.storeMessage(msg("m2", 2, text("in thread", { topicId: "m1" })));

  const roots = c.messages("r");
  const thread = c.messages("r", { topicId: "m1" });
  assert.equal(c.messages("r"), roots, "an unchanged room hands back the same list");
  assert.equal(c.messages("r", { topicId: "m1" }), thread);
  assert.ok(Object.isFrozen(roots), "a shared list can't be changed under its other readers");
  assert.equal(c.readMarks("r"), c.readMarks("r"), "read marks are folded once per change");

  // A receipt changes the room but no message in it: the lists survive,
  // the marks move.
  const marks = c.readMarks("r");
  await inner.storeMessage(msg("rc", 3, { v: 1, type: "receipt", readSeq: 2 }, "fog:h/bob"));
  assert.equal(c.messages("r"), roots, "nothing visible changed, so the list is the same");
  assert.notEqual(c.readMarks("r"), marks);
  assert.equal(c.readMarks("r").get("fog:h/bob"), 2);

  // A reaction on m1 changes m1's view, and so the thread, but not m2's.
  const m2 = thread[1];
  await inner.storeMessage(msg("x1", 4, { v: 1, type: "reaction", reaction: { target: "m1", emoji: "👍", add: true } }, "fog:h/bob"));
  const after = c.messages("r", { topicId: "m1" });
  assert.notEqual(after, thread, "a list with a changed member is a new list");
  assert.notEqual(after[0], thread[0]);
  assert.deepEqual(after[0].reactions, [{ emoji: "👍", by: ["fog:h/bob"] }]);
  assert.equal(after[1], m2, "an untouched message keeps its view");

  // A new root makes a new roots list and leaves the old members alone.
  await inner.storeMessage(msg("m3", 5, text("later")));
  const more = c.messages("r");
  assert.deepEqual(more.map((v) => v.id), ["m1", "m3"]);
  assert.notEqual(more, roots);
  assert.equal(more[0], after[0]);
});

test("timeline: sameView compares contents, not identity", () => {
  const fold = () => foldTimeline("r", [msg("m1", 1, { v: 1, type: "message", body: { format: "text/markdown", text: "hi" }, mentions: ["x"] })], (s) => s)[0];
  const a = fold();
  const b = fold();
  assert.notEqual(a, b);
  assert.ok(sameView(a, b));
  assert.ok(!sameView(a, { ...b, text: "ho" }));
  assert.ok(!sameView(a, { ...b, reactions: [{ emoji: "👍", by: ["y"] }] }));
  assert.ok(!sameView({ ...a, reactions: [{ emoji: "👍", by: ["y"] }] }, { ...b, reactions: [{ emoji: "👍", by: ["z"] }] }));
  assert.ok(!sameView(a, { ...b, lastReplyAt: "2026-01-01T00:00:00Z" }), "a field present on one side only differs");
});

// ---- the rules a client holds incoming commits and content to ----

async function twoInARoom() {
  const mls = await Mls.init();
  const alice = generateKeyPair(), bob = generateKeyPair();
  const kpA = await mls.keyPackage("fog:X/alice", alice);
  const kpB = await mls.keyPackage("fog:X/bob", bob);
  let a = await mls.createGroup(new Uint8Array(16), kpA);
  const add = await mls.addMembers(a, [mls.parseKeyPackage(kpB.raw)]);
  a = add.newState;
  const b = await mls.joinFromWelcome(add.welcome!, kpB);
  return { mls, a, b, alice };
}

test("mls: a commit or proposal that arrives encrypted is refused, the group untouched", async () => {
  const { mls, a, b } = await twoInARoom();
  const { createCommit, createProposal, encodeMlsMessage } = await import("ts-mls");
  const carol = await mls.keyPackage("fog:X/carol", generateKeyPair());
  // Alice adds carol in a private message the hub would never have seen.
  const hidden = await createCommit({ state: a, cipherSuite: mls.impl }, { extraProposals: [{ proposalType: "add", add: { keyPackage: carol.publicPackage } }], wireAsPublicMessage: false });
  await assert.rejects(mls.process(b, encodeMlsMessage(hidden.commit)), (e: Error & { code?: string }) => e.code === "refused" && /encrypted commit/.test(e.message));
  // And a proposal, encrypted or not: standalone ones never apply.
  for (const pub of [false, true]) {
    const p = await createProposal(a, pub, { proposalType: "add", add: { keyPackage: carol.publicPackage } }, mls.impl);
    await assert.rejects(mls.process(b, encodeMlsMessage(p.message)), (e: Error & { code?: string }) => e.code === "refused");
  }
  assert.equal(mls.members(b).length, 2);
  // Application data still decrypts.
  const msg = await mls.encrypt(a, encodeContent({ v: 1, type: "message", body: { format: "text/markdown", text: "hi" } }));
  assert.equal(decodeContent((await mls.process(b, msg.wire)).plaintext!).body?.text, "hi");
});

// rewrite decodes a public commit, lets f change it, and encodes it again —
// a member crafting MLS by hand. Its signature no longer verifies, but the
// rules refuse it before that is even asked.
async function rewrite(wire: Uint8Array, f: (content: any) => void): Promise<Uint8Array> {
  const { decodeMlsMessage, encodeMlsMessage } = await import("ts-mls");
  const m = decodeMlsMessage(wire, 0)![0] as any;
  f(m.publicMessage.content);
  return encodeMlsMessage(m);
}

test("mls: no commit replaces another member's leaf, or renames its committer", async () => {
  const { mls, a, b } = await twoInARoom();
  // An Update naming bob, with a key alice holds, slipped into her commit.
  const forged = await mls.keyPackage("fog:X/bob", generateKeyPair());
  const empty = await mls.selfUpdate(a);
  const takeover = await rewrite(empty.commit, (c) => {
    const { lifetime: _drop, ...leaf } = forged.publicPackage.leafNode as any;
    c.commit.proposals.push({ proposalOrRefType: "proposal", proposal: { proposalType: "update", update: { leafNode: { ...leaf, leafNodeSource: "update" } } } });
  });
  await assert.rejects(mls.process(b, takeover), (e: Error & { code?: string }) => e.code === "refused" && /update inside a commit/.test(e.message));
  // A path that hands alice's leaf a new name.
  const renamed = await rewrite(empty.commit, (c) => {
    c.commit.path.leafNode = { ...c.commit.path.leafNode, credential: { credentialType: "basic", identity: new TextEncoder().encode("fog:X/bob") } };
  });
  await assert.rejects(mls.process(b, renamed), (e: Error & { code?: string }) => e.code === "refused" && /another name or key/.test(e.message));
  // The honest key refresh applies.
  const ok = await mls.process(b, empty.commit);
  assert.equal(mls.epoch(ok.newState), 2);
  assert.deepEqual(mls.members(ok.newState).map((m) => m.identity), ["fog:X/alice", "fog:X/bob"]);
});

test("mls: an external commit may remove only the joiner's own leaf, and a DM's membership is fixed", async () => {
  const { mls, a, b } = await twoInARoom();
  const carol = await mls.keyPackage("fog:X/carol", generateKeyPair());
  const ext = await mls.externalJoin(await mls.groupInfo(a), carol);
  const evicting = await rewrite(ext.commit, (c) => {
    c.commit.proposals.push({ proposalOrRefType: "proposal", proposal: { proposalType: "remove", remove: { removed: 1 } } });
  });
  await assert.rejects(mls.process(b, evicting), (e: Error & { code?: string }) => e.code === "refused" && /removes someone else/.test(e.message));
  assert.equal(mls.epoch((await mls.process(b, ext.commit)).newState), 2, "the joiner's own commit applies");

  const dave = await mls.keyPackage("fog:X/dave", generateKeyPair());
  const add = await mls.addMembers(a, [mls.parseKeyPackage(dave.raw)]);
  const dm = new Set(["fog:X/alice", "fog:X/bob"]);
  await assert.rejects(mls.process(b, add.commit, { dm }), (e: Error & { code?: string }) => e.code === "refused" && /DM/.test(e.message));
});

// A client wired to a fake fog: just enough state to run its private paths.
async function clientWith(roomRoles: Record<string, number>, kind: "group" | "dm" = "group") {
  const c = new ChatClient({ baseUrl: "http://fog.example", storage: new MemoryStorage(), fetch: () => Promise.reject(new Error("offline")) });
  const inner = c as unknown as Record<string, any>;
  inner.mls = await Mls.init();
  inner.meInfo = { addr: "fog:X/me", deviceKey: "me-device", household: { instanceId: "X" } };
  inner.rooms_.set("r", { id: "r", kind, policy: defaultPolicy(), roles: roomRoles, myRole: roomRoles["fog:X/me"] ?? 2, sync: "ready", cursor: 0 });
  return { c, inner };
}

function defaultPolicy() {
  const member = ["canSendMessage", "canReact", "canEditOwn", "canDeleteOwn", "canAddParticipant", "canShareHistory"];
  return {
    roles: [
      { index: 1, name: "banned", capabilities: [] },
      { index: 2, name: "member", capabilities: member },
      { index: 3, name: "admin", capabilities: [...member, "canRemoveParticipant", "canDeleteOtherMessage", "canPin", "canChangePolicy", "canChangeRoles"] },
      { index: 4, name: "owner", capabilities: ["*"] },
    ],
    join: "invite", history: "shared", households: "hub-peers", hints: [], retention: { days: 0 },
  };
}

test("client: deletes of others' messages and pins hold only from a role that may make them", async () => {
  const { inner, c } = await clientWith({ "fog:h/ann": 2, "fog:h/bob": 2, "fog:h/mod": 3 });
  const judge = (sender: string, content: Content) => inner.judge("r", sender, content);
  const items: Message[] = [];
  const add = async (m: Message) => {
    items.push(m);
    await inner.storeMessage(m);
  };
  await add(msg("m1", 1, { v: 1, type: "message", body: { format: "text/markdown", text: "ann's" } }));
  await add(msg("m2", 2, { v: 1, type: "message", body: { format: "text/markdown", text: "ann's too" } }));
  await add(msg("m3", 3, { v: 1, type: "message", body: { format: "text/markdown", text: "ann's three" } }));
  const del = (target: string): Content => ({ v: 1, type: "delete", replaces: target });
  const pin = (target: string): Content => ({ v: 1, type: "pin", pin: { target, pinned: true } });
  // bob, a member, deletes and pins ann's message: neither holds.
  await add({ ...msg("d1", 4, del("m1"), "fog:h/bob"), ...judge("fog:h/bob", del("m1")) });
  await add({ ...msg("p1", 5, pin("m1"), "fog:h/bob"), ...judge("fog:h/bob", pin("m1")) });
  // A moderator's delete and pin hold; so does ann deleting her own.
  await add({ ...msg("d2", 6, del("m2"), "fog:h/mod"), ...judge("fog:h/mod", del("m2")) });
  await add({ ...msg("p2", 7, pin("m3"), "fog:h/mod"), ...judge("fog:h/mod", pin("m3")) });
  await add({ ...msg("d3", 8, del("m3")), ...judge("fog:h/ann", del("m3")) });
  const v = new Map(c.messages("r").map((x) => [x.id, x]));
  assert.equal(v.get("m1")!.deleted, false, "a member's delete of someone else's message");
  assert.equal(v.get("m1")!.pinned, false, "a member's pin");
  assert.equal(v.get("m2")!.deleted, true, "a moderator's delete");
  assert.equal(v.get("m3")!.deleted, true, "the author's own delete");
  // Items from before clients checked keep folding as they did.
  const legacy = foldTimeline("r", [items[0], msg("d9", 9, del("m1"), "fog:h/bob")], (s) => s);
  assert.equal(legacy[0].deleted, true);
});

test("client: shared history is the sharer's word: it can't change what a device received itself", async () => {
  const { c, inner } = await clientWith({ "fog:h/ann": 2, "fog:h/eve": 2, "fog:h/nobody": 1 });
  await inner.storeMessage({ ...msg("m1", 1, { v: 1, type: "message", body: { format: "text/markdown", text: "real" } }), checked: true });
  const forged = (id: string, seq: number, content: Content, sender = "fog:h/ann") => ({ id, seq, sender, hubAt: "", content });
  const H1 = ulid();
  const bundle: Content = {
    v: 1, type: "history", historyFor: ["me-device"], history: [
      forged(H1, 0, { v: 1, type: "message", body: { format: "text/markdown", text: "old" } }),
      forged(ulid(), 2, { v: 1, type: "edit", replaces: "m1", body: { format: "text/markdown", text: "forged edit" } }),
      forged(ulid(), 3, { v: 1, type: "delete", replaces: "m1" }),
    ],
  };
  // From someone the room's rules don't let share: ignored whole.
  await inner.receiveHistory("r", "fog:h/nobody", bundle);
  assert.deepEqual(c.messages("r").map((v) => v.id), ["m1"]);
  await inner.receiveHistory("r", "fog:h/eve", bundle);
  const v = new Map(c.messages("r").map((x) => [x.id, x]));
  assert.equal(v.get("m1")!.text, "real", "a shared 'edit' rewrote a message received directly");
  assert.equal(v.get("m1")!.deleted, false, "a shared 'delete' hid a message received directly");
  assert.equal(v.get(H1)!.sharedBy, "fog:h/eve");
});

test("client: addMembers adds only devices their identity key attests, with packages that are theirs", async () => {
  const { c, inner } = await clientWith({});
  const mls: Mls = inner.mls;
  const id = generateKeyPair();
  const good = generateKeyPair(), ghost = generateKeyPair(), stolen = generateKeyPair();
  const addr = "fog:X/dave";
  const kpGood = await mls.keyPackage(addr, good);
  const kpGhost = await mls.keyPackage(addr, ghost);
  const kpStolen = await mls.keyPackage("fog:X/mallory", stolen);
  const km = {
    handle: "dave", displayName: "Dave", identityKey: b64u(id.publicKey), ciphersuite: 1,
    devices: [
      { id: 1, name: "phone", kind: "web", deviceKey: b64u(good.publicKey), attestation: b64u(attestDevice(id, good.publicKey)), verified: true, keyPackage: b64u(kpGood.raw) },
      // Registered with no attestation: what someone holding only dave's sign-in makes.
      { id: 2, name: "ghost", kind: "web", deviceKey: b64u(ghost.publicKey), verified: true, keyPackage: b64u(kpGhost.raw) },
      // Attested, but its package is someone else's leaf.
      { id: 3, name: "swapped", kind: "web", deviceKey: b64u(stolen.publicKey), attestation: b64u(attestDevice(id, stolen.publicKey)), verified: true, keyPackage: b64u(kpStolen.raw) },
    ],
  };
  inner.api = { keysByAddr: async () => km };
  let committed: Uint8Array[] = [];
  inner.commit = async (_room: string, build: (s: unknown) => Promise<unknown>) => {
    inner.mls = { ...mls, addMembers: async (_s: unknown, kps: { leafNode: { signaturePublicKey: Uint8Array } }[]) => { committed = kps.map((k) => k.leafNode.signaturePublicKey); return {}; } };
    await build(null);
    inner.mls = mls;
  };
  const res = await c.addMembers("r", [addr]);
  assert.deepEqual(res.added, [addr]);
  assert.equal(committed.length, 1);
  assert.ok(bytesEqualT(committed[0], good.publicKey), "only the attested device's own package");
  assert.equal(res.skipped.length, 2);
  assert.ok(res.skipped.some((s) => /ghost: not verified/.test(s.reason)));
  assert.ok(res.skipped.some((s) => /swapped: its key package is not that device's/.test(s.reason)));
});

function bytesEqualT(a: Uint8Array, b: Uint8Array) {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}

test("client: an identity reset drops the pin and the mark, and says so in the room", async () => {
  const { c, inner } = await clientWith({ "fog:h/ann": 2 });
  const mls: Mls = inner.mls;
  inner.rooms_.get("r").hubInstance = "h";
  let st = await mls.createGroup(new Uint8Array(16), await mls.keyPackage("fog:X/me", generateKeyPair()));
  st = (await mls.addMembers(st, [mls.parseKeyPackage((await mls.keyPackage("fog:h/ann", generateKeyPair())).raw)])).newState;
  inner.states.set("r", st);
  inner.commit = async () => {};
  await c.markVerified("fog:h/ann", "old-key");
  const heard: unknown[] = [];
  c.addEventListener("identity", (d) => heard.push(d));
  const payload = b64u(new TextEncoder().encode(JSON.stringify({ kind: "identity-reset", left: ["fog:h/ann"] })));
  await inner.applyEvent({ roomId: "r", seq: 5, id: "sys5", epoch: 0, kind: "system", hubAt: "2026-01-01T00:00:00Z", payload });
  assert.equal(await c.isVerified("fog:h/ann"), false);
  assert.equal(await inner.storage.get("identity-pins", "fog:h/ann"), undefined);
  assert.deepEqual(heard, [{ addr: "fog:h/ann", reset: true }]);
  const line = c.messages("r").find((v) => v.system === "identity-reset");
  assert.ok(line && /identity was reset/.test(line.text) && line.sender === "fog:h/ann");
});

test("mls: process names the sender MLS proves, and the client goes by it, not by the hub's label", async () => {
  const { mls, a, b } = await twoInARoom();
  const M1 = ulid();
  const wire = (await mls.encrypt(a, encodeContent({ v: 1, type: "message", id: M1, body: { format: "text/markdown", text: "from alice" } }))).wire;
  const res = await mls.process(b, wire);
  assert.equal(res.sender?.identity, "fog:X/alice");

  const { c, inner } = await clientWith({ "fog:X/alice": 2, "fog:X/bob": 3 });
  inner.states.set("r", b);
  // The hub labels alice's message as bob's (an admin's).
  await inner.applyEvent({ roomId: "r", seq: 3, id: "E3", epoch: 1, kind: "mls_private", sender: "fog:X/bob", senderDevice: "bob-device", hubAt: "2026-01-01T00:00:03Z", payload: b64u(wire) });
  const v = c.messages("r").find((x) => x.id === M1);
  assert.equal(v?.sender, "fog:X/alice");
});

test("client: an item can't take over an id another event already holds", async () => {
  const { c, inner } = await clientWith({ "fog:h/ann": 2, "fog:h/mal": 2 });
  await inner.storeMessage({ ...msg("m1", 1, { v: 1, type: "message", body: { format: "text/markdown", text: "ann's" } }), checked: true });
  // mallory sends a receipt whose id is ann's message: it would erase it.
  await inner.storeMessage({ ...msg("m1", 2, { v: 1, type: "receipt", readSeq: 1 }, "fog:h/mal"), checked: true });
  assert.equal(c.messages("r").find((x) => x.id === "m1")?.text, "ann's");
});

test("client: a moderation act is judged by the role its sender held when the hub sequenced it", async () => {
  const { inner } = await clientWith({ "fog:h/now-admin": 3 });
  const del: Content = { v: 1, type: "delete", replaces: "someone-elses" };
  // Promoted since: the delete was sent as a member, and stays a member's.
  assert.equal(inner.judge("r", "fog:h/now-admin", del, 2).moderator, false);
  assert.equal(inner.judge("r", "fog:h/now-admin", del, 3).moderator, true);
  assert.equal(inner.judge("r", "fog:h/now-admin", del).moderator, true, "no stamp: the role reported now");
});

test("client: nothing is encrypted to a departed person's leaves: their removal is committed first", async () => {
  const { mls, a } = await twoInARoom();
  const { c, inner } = await clientWith({ "fog:X/alice": 4 });
  inner.meInfo = { ...inner.meInfo, addr: "fog:X/alice", deviceKey: "alice-device" };
  inner.states.set("r", a);
  const sent: string[] = [];
  let seq = 10;
  inner.api = {
    submit: async (_room: string, input: { id: string; kind: string }) => {
      sent.push(input.kind);
      return { event: { roomId: "r", seq: seq++, id: input.id, epoch: 0, kind: input.kind, hubAt: "2026-01-01T00:00:00Z" } };
    },
    rooms: async () => ({ rooms: [], available: [] }),
  };
  inner.mls = mls;
  const bobKey = b64u(mls.members(a).find((l) => l.identity === "fog:X/bob")!.signatureKey);
  await inner.storage.put("meta", "doomed:r", [bobKey]);
  await c.send("r", "after bob was banned");
  assert.deepEqual(sent, ["mls_commit", "mls_private"], "the removal went first");
  assert.ok(!mls.members(inner.states.get("r")).some((l: { identity: string }) => l.identity === "fog:X/bob"));
});

test("client: a doomed leaf someone else removed is done with: the device back under the same key isn't removed again", async () => {
  // bob's device left by itself (device-left); another member removed its
  // leaf, then added it back — a restored device keeps its key. This
  // device's own removal, still waiting out its jitter, must not take the
  // new leaf.
  const mls = await Mls.init();
  const alice = generateKeyPair(), bob = generateKeyPair();
  const kpA = await mls.keyPackage("fog:X/alice", alice);
  let a = (await mls.addMembers(await mls.createGroup(new Uint8Array(16), kpA), [mls.parseKeyPackage((await mls.keyPackage("fog:X/bob", bob)).raw)])).newState;
  const { c, inner } = await clientWith({ "fog:X/alice": 4, "fog:X/bob": 1 });
  inner.meInfo = { ...inner.meInfo, addr: "fog:X/alice", deviceKey: "alice-device" };
  inner.mls = mls;
  inner.states.set("r", a);
  const bobKey = b64u(bob.publicKey);
  await inner.storage.put("meta", "doomed:r", [bobKey]);
  const bobLeaf = mls.members(a).find((l) => l.identity === "fog:X/bob")!.index;
  a = (await mls.removeLeaves(a, [bobLeaf])).newState;
  await inner.setState("r", a);
  a = (await mls.addMembers(a, [mls.parseKeyPackage((await mls.keyPackage("fog:X/bob", bob)).raw)])).newState;
  await inner.setState("r", a);
  let commits = 0;
  inner.commit = async () => { commits++; };
  await inner.removeDoomed("r", false);
  assert.equal(commits, 0, "the device back under its old key was removed again");
  assert.deepEqual(await inner.storage.get("meta", "doomed:r"), [], "the doom outlived the leaf it was for");
  void c;
});

test("safe: an attachment shows inline only as an inert image its bytes prove it is; links are http(s) or mailto", async () => {
  const { inlineImageType, safeHref, DOWNLOAD_TYPE } = await import("../src/safe.ts");
  const enc = new TextEncoder();
  const svg = enc.encode(`<svg xmlns="http://www.w3.org/2000/svg"><script>fetch("/api/chat/me")</script></svg>`);
  const html = enc.encode("<!doctype html><script>alert(1)</script>");
  assert.equal(inlineImageType(svg), null, "an SVG, whatever it is labelled, is never shown inline");
  assert.equal(inlineImageType(html), null);
  assert.equal(inlineImageType(new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0])), "image/png");
  assert.equal(inlineImageType(new Uint8Array([0xff, 0xd8, 0xff, 0xe0])), "image/jpeg");
  assert.equal(inlineImageType(enc.encode("GIF89a....")), "image/gif");
  assert.equal(inlineImageType(enc.encode("RIFF\0\0\0\0WEBPVP8 ")), "image/webp");
  assert.equal(DOWNLOAD_TYPE, "application/octet-stream");
  for (const bad of ["javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,<script>x</script>", "blob:https://fog/x", "file:///etc/passwd", "/api/chat/me", "vbscript:x", "https://", ""]) {
    assert.equal(safeHref(bad), undefined, bad);
  }
  assert.equal(safeHref("https://example.com/a?b"), "https://example.com/a?b");
  assert.equal(safeHref("mailto:a@b.c"), "mailto:a@b.c");
});

// A room as bob's device sees it: alice and bob, both attested by identity
// keys bob's device holds, with fog's directory saying the same.
async function bobsView() {
  const mls = await Mls.init();
  const aliceId = generateKeyPair(), bobId = generateKeyPair();
  const aliceDev = generateKeyPair(), bobDev = generateKeyPair();
  const kpA = await mls.keyPackage("fog:X/alice", aliceDev);
  const kpB = await mls.keyPackage("fog:X/bob", bobDev);
  let a = await mls.createGroup(new Uint8Array(16), kpA);
  const add = await mls.addMembers(a, [mls.parseKeyPackage(kpB.raw)]);
  a = add.newState;
  const b = await mls.joinFromWelcome(add.welcome!, kpB);
  const { c, inner } = await clientWith({ "fog:X/alice": 4, "fog:X/bob": 2 });
  inner.mls = mls;
  inner.meInfo = { ...inner.meInfo, addr: "fog:X/bob", deviceKey: b64u(bobDev.publicKey), household: { instanceId: "X" } };
  inner.device = bobDev;
  inner.states.set("r", b);
  const people = new Map([["fog:X/alice", { id: aliceId, devs: [aliceDev] }], ["fog:X/bob", { id: bobId, devs: [bobDev] }]]);
  const publish = () => {
    for (const [addr, p] of people) {
      inner.dirIdentity.set(addr, b64u(p.id.publicKey));
      inner.dirDevices.set(addr, new Map(p.devs.map((d) => [b64u(d.publicKey), b64u(attestDevice(p.id, d.publicKey))])));
    }
  };
  publish();
  await inner.storage.put("identity-pins", "fog:X/alice", b64u(aliceId.publicKey));
  inner.api = { directory: async () => ({ people: [], households: [] }), rooms: async () => ({ rooms: [], available: [] }) };
  return { mls, a, b, c, inner, aliceId, people, publish };
}

test("client: a device its person's identity key doesn't attest is refused, however it got into the group", async () => {
  const { mls, a, c, inner, aliceId, people, publish } = await bobsView();
  // A hostile hub (or anyone) external-joins a ghost under alice's name.
  const ghost = generateKeyPair();
  const ghostKp = await mls.keyPackage("fog:X/alice", ghost);
  const ext = await mls.externalJoin(await mls.groupInfo(a), ghostKp);
  const before = mls.epoch(inner.states.get("r"));
  await inner.applyEvent({ roomId: "r", seq: 5, id: ulid(), epoch: before, kind: "mls_commit", sender: "fog:X/alice", senderDevice: b64u(ghost.publicKey), hubAt: "2026-01-01T00:00:05Z", payload: b64u(ext.commit) });
  assert.equal(mls.epoch(inner.states.get("r")), before, "the ghost's commit was applied");
  assert.equal(c.room("r")?.sync, "stale");
  assert.match(c.room("r")?.staleReason ?? "", /doesn't vouch/);
  // Even when fog swaps alice's identity key to one that attests the ghost.
  people.set("fog:X/alice", { id: generateKeyPair(), devs: [ghost] });
  publish();
  assert.equal(await c.leafAttested("fog:X/alice", ghost.publicKey), false, "fog's new key for alice was trusted");
  // A second device alice's own identity key attests is let in.
  const { c: c2, inner: i2, a: a2, mls: m2, aliceId: id2, people: p2, publish: pub2 } = await bobsView();
  const tablet = generateKeyPair();
  p2.get("fog:X/alice")!.devs.push(tablet);
  pub2();
  const ext2 = await m2.externalJoin(await m2.groupInfo(a2), await m2.keyPackage("fog:X/alice", tablet));
  await i2.applyEvent({ roomId: "r", seq: 5, id: ulid(), epoch: 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: b64u(tablet.publicKey), hubAt: "2026-01-01T00:00:05Z", payload: b64u(ext2.commit) });
  assert.equal(m2.epoch(i2.states.get("r")), 2, "alice's attested second device was refused");
  assert.equal(c2.room("r")?.staleReason, undefined);
  void aliceId;
  void id2;
});

test("client: a badge is drawn from the group's leaves, and an external joiner outside a DM is refused", async () => {
  const { mls, a, c, inner } = await bobsView();
  const trust = await c.leafTrust("r");
  assert.deepEqual(trust.map((t) => t.attested), [true, true]);
  // An unattested leaf sitting in the group shows unverified.
  inner.attested.clear();
  inner.dirDevices.get("fog:X/alice")!.clear();
  assert.equal((await c.leafTrust("r")).find((t) => t.identity === "fog:X/alice")?.attested, false);
  const carol = await mls.keyPackage("fog:X/carol", generateKeyPair());
  const ext = await mls.externalJoin(await mls.groupInfo(a), carol);
  await assert.rejects(mls.process(inner.states.get("r"), ext.commit, { dm: new Set(["fog:X/alice", "fog:X/bob"]) }), /DM/);
});

test("client: devices refuse siblings vouched for by any identity key but their own, and say so", async () => {
  const { c, inner } = await clientWith({});
  const own = generateKeyPair(), swapped = generateKeyPair(), ghost = generateKeyPair(), mine = generateKeyPair();
  inner.identity = own;
  inner.meInfo = { ...inner.meInfo, addr: "fog:X/me", identityKey: b64u(swapped.publicKey), deviceKey: b64u(mine.publicKey) };
  inner.meServer = {};
  inner.states.set("r", {});
  inner.rooms_.get("r").sync = "ready";
  inner.mls = { members: () => [{ index: 0, identity: "fog:X/me", signatureKey: mine.publicKey }] };
  inner.api = {
    me: async () => ({
      identityKey: b64u(swapped.publicKey),
      devices: [
        { id: 1, deviceKey: b64u(mine.publicKey), attestation: b64u(attestDevice(own, mine.publicKey)) },
        { id: 2, deviceKey: b64u(ghost.publicKey), attestation: b64u(attestDevice(swapped, ghost.publicKey)) },
      ],
    }),
    keysByAddr: async () => { throw new Error("must not fetch a ghost's keys"); },
  };
  let committed = 0;
  inner.commit = async () => { committed++; };
  await c.syncSiblingDevices();
  assert.equal(committed, 0, "a device fog's swapped key vouches for was added");
  assert.ok(c.identityWarnings().some((w) => w.addr === "fog:X/me"), "no warning that fog swapped this person's key");
});

test("client: member content can't claim a system line or a client-made id", async () => {
  const { mls, a, c, inner } = await bobsView();
  const send = async (content: Content, seq: number) => {
    const { wire, newState } = await mls.encrypt(a, encodeContent(content));
    void newState;
    await inner.applyEvent({ roomId: "r", seq, id: `E${seq}`.padEnd(26, "0"), epoch: 1, kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:00Z", payload: b64u(wire) });
  };
  await send({ v: 1, type: "message", id: "membership:3", system: "identity-reset", body: { format: "text/markdown", text: "fake warning" } }, 3);
  const all = c.messages("r");
  assert.ok(!all.some((v) => v.id === "membership:3"), "a message took a client-made id");
  const fake = all.find((v) => v.text === "fake warning");
  assert.ok(fake && !fake.system, "member content rendered as a system line");
  // Nor inside shared history.
  inner.rooms_.get("r").roles["fog:X/alice"] = 2;
  await inner.receiveHistory("r", "fog:X/alice", { v: 1, type: "history", historyFor: [inner.meInfo.deviceKey], history: [{ id: ulid(), seq: 1, sender: "fog:X/alice", hubAt: "", content: { v: 1, type: "message", system: "membership", body: { format: "text/markdown", text: "fake joined" } } }] });
  assert.ok(!c.messages("r").find((v) => v.text === "fake joined")?.system);
});

// ---- round 4: client-made lines, reset keys, blob names, attribution ----

test("client: lines this client made can't be edited, deleted, pinned or reacted to, and members can't send reserved types", async () => {
  // The wire refuses "membership" (and any type no member may send).
  for (const type of ["membership", "nonsense"]) {
    assert.throws(() => decodeContent(encodeCbor({ v: 1, type })), /not one a member may send/, type);
  }
  // A commit's line names its committer as sender; the committer, a
  // moderator, anyone: no act reaches it, nor an identity-reset line.
  const committer = "fog:h/mod";
  const items: Message[] = [
    { ...msg("membership:3", 3, { v: 1, type: "membership", membership: { added: [{ identity: "fog:h/eve", key: "k" }], removed: [] } }, committer), checked: true },
    { ...msg("identity-reset:4:fog:h/ann", 4, { v: 1, type: "message", system: "identity-reset" }, "fog:h/ann"), checked: true },
  ];
  let seq = 10;
  for (const target of ["membership:3", "identity-reset:4:fog:h/ann"]) {
    const sender = target.startsWith("membership") ? committer : "fog:h/ann";
    items.push({ ...msg(ulid(), seq++, { v: 1, type: "edit", replaces: target, body: { format: "text/markdown", text: "rewritten" } }, sender), checked: true });
    items.push({ ...msg(ulid(), seq++, { v: 1, type: "delete", replaces: target }, committer), checked: true, moderator: true });
    items.push({ ...msg(ulid(), seq++, { v: 1, type: "pin", pin: { target, pinned: true } }, committer), checked: true, moderator: true });
    items.push({ ...msg(ulid(), seq++, { v: 1, type: "reaction", reaction: { target, emoji: "x", add: true } }, committer), checked: true });
    // Unchecked (legacy) acts don't reach them either.
    items.push(msg(ulid(), seq++, { v: 1, type: "delete", replaces: target }, "fog:h/bob"));
  }
  const views = foldTimeline("r", items, (a) => a);
  for (const v of views) {
    assert.equal(v.deleted, false, v.id);
    assert.equal(v.edited, false, v.id);
    assert.equal(v.pinned, false, v.id);
    assert.deepEqual(v.reactions, [], v.id);
  }
  assert.match(views[0].text, /joined/);
  // A member who names its event (the hub's id) in the client's namespace
  // still can't take the line's id: it gets the event's seq instead.
  const { mls, a, c, inner } = await bobsView();
  const { wire } = await mls.encrypt(a, encodeContent({ v: 1, type: "message", id: "membership:9", body: { format: "text/markdown", text: "squat" } }));
  await inner.applyEvent({ roomId: "r", seq: 8, id: "membership:9", epoch: 1, kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:00Z", payload: b64u(wire) });
  assert.deepEqual(c.messages("r").map((v) => v.id), ["event:8"]);
});

test("client: who is in a room comes from the group's leaves; a leaf fog doesn't list is unexplained", async () => {
  const { c } = await bobsView();
  // Fog lists bob and carol; the group holds alice and bob.
  const members = new Map((await c.members("r", ["fog:X/bob", "fog:X/carol"])).map((m) => [m.addr, m]));
  assert.equal(members.get("fog:X/alice")?.unexplained, true, "a leaf fog doesn't list went unflagged");
  assert.equal(members.get("fog:X/alice")?.devices.length, 1);
  assert.equal(members.get("fog:X/bob")?.unexplained, false);
  assert.deepEqual(members.get("fog:X/carol")?.devices, [], "a listed person without a leaf shows none");
});

test("client: after an identity reset no new key is trusted on fog's word: only a member's re-add or a safety-code check", async () => {
  const { mls, a, c, inner, people, publish } = await bobsView();
  // Hubbed here: fog reports no hub instance.
  inner.rooms_.get("r").hubInstance = "";
  const roomView = { ...inner.rooms_.get("r") };
  inner.api.rooms = async () => ({ rooms: [roomView], available: [] });
  // A directory refetch serves what fog says now.
  inner.directory = async () => {
    publish();
    return [];
  };
  inner.commit = async () => {};
  // dave is in the room too, added by alice.
  const daveId = generateKeyPair(), daveDev = generateKeyPair();
  people.set("fog:X/dave", { id: daveId, devs: [daveDev] });
  publish();
  await inner.storage.put("identity-pins", "fog:X/dave", b64u(daveId.publicKey));
  const daveKp = await mls.keyPackage("fog:X/dave", daveDev);
  const addDave = await mls.addMembers(a, [mls.parseKeyPackage(daveKp.raw)]);
  await inner.applyEvent({ roomId: "r", seq: 2, id: ulid(), epoch: 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:02Z", payload: b64u(addDave.commit) });
  let dave = await mls.joinFromWelcome(addDave.welcome!, daveKp);
  assert.equal(mls.members(inner.states.get("r")).length, 3);

  // alice's identity is reset; fog now serves a new key and a new device.
  const reset = b64u(new TextEncoder().encode(JSON.stringify({ kind: "identity-reset", left: ["fog:X/alice"] })));
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "system", hubAt: "2026-01-01T00:00:03Z", payload: reset });
  const newId = generateKeyPair(), newDev = generateKeyPair();
  people.set("fog:X/alice", { id: newId, devs: [newDev] });
  publish();
  assert.equal(c.awaitingKey("fog:X/alice"), true);
  assert.ok(c.identityWarnings().some((w) => w.addr === "fog:X/alice"), "no visible warning while the new key waits");
  assert.equal(await c.knownIdentity("fog:X/alice"), undefined, "fog's new key was pinned at first sight");
  assert.equal(await c.leafAttested("fog:X/alice", newDev.publicKey), false);

  // The new device joins on its own: refused, whatever fog says.
  const newKp = await mls.keyPackage("fog:X/alice", newDev);
  const ext = await mls.externalJoin(await mls.groupInfo(dave), await mls.keyPackage("fog:X/alice", newDev));
  const epoch = mls.epoch(inner.states.get("r"));
  await inner.applyEvent({ roomId: "r", seq: 4, id: ulid(), epoch, kind: "mls_commit", sender: "fog:X/alice", senderDevice: b64u(newDev.publicKey), hubAt: "2026-01-01T00:00:04Z", payload: b64u(ext.commit) });
  assert.equal(mls.epoch(inner.states.get("r")), epoch, "a self-rejoin under the new key was applied");
  assert.equal(c.awaitingKey("fog:X/alice"), true);

  // dave removes alice's old leaf, then re-adds her new device: a member's
  // deliberate word, in commits bob verifies. That is what takes the key.
  const oldIdx = mls.members(dave).find((l) => l.identity === "fog:X/alice")!.index;
  const rm = await mls.removeLeaves(dave, [oldIdx]);
  dave = rm.newState;
  await inner.applyEvent({ roomId: "r", seq: 5, id: ulid(), epoch, kind: "mls_commit", sender: "fog:X/dave", senderDevice: b64u(daveDev.publicKey), hubAt: "2026-01-01T00:00:05Z", payload: b64u(rm.commit) });
  const readd = await mls.addMembers(dave, [mls.parseKeyPackage(newKp.raw)]);
  await inner.applyEvent({ roomId: "r", seq: 6, id: ulid(), epoch: epoch + 1, kind: "mls_commit", sender: "fog:X/dave", senderDevice: b64u(daveDev.publicKey), hubAt: "2026-01-01T00:00:06Z", payload: b64u(readd.commit) });
  assert.equal(mls.epoch(inner.states.get("r")), epoch + 2, "the member's re-add was refused");
  assert.equal(c.awaitingKey("fog:X/alice"), false);
  assert.equal(await c.knownIdentity("fog:X/alice"), b64u(newId.publicKey));
  assert.equal(await c.isVerified("fog:X/alice"), false, "a re-add is not a safety-code check");

  // markVerified is the other way to accept a waiting key.
  const { c: c2, inner: i2 } = await bobsView();
  i2.rooms_.get("r").hubInstance = "X";
  i2.commit = async () => {};
  await i2.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 1, kind: "system", hubAt: "2026-01-01T00:00:03Z", payload: reset });
  assert.equal(c2.awaitingKey("fog:X/alice"), true);
  await c2.markVerified("fog:X/alice", b64u(newId.publicKey));
  assert.equal(c2.awaitingKey("fog:X/alice"), false);
  assert.equal(await c2.knownIdentity("fog:X/alice"), b64u(newId.publicKey));
});

test("client: a reset line acts only from the person's home hub or this fog, about someone in the room", async () => {
  const pinned = async (inner: Record<string, any>, addr: string) => inner.storage.get("identity-pins", addr);
  const reset = (who: string) => b64u(new TextEncoder().encode(JSON.stringify({ kind: "identity-reset", left: [who] })));
  // A room hubbed on a third fog: alice (of X) is in it, but Z can't speak
  // for X's people.
  {
    const { c, inner, aliceId } = await bobsView();
    inner.rooms_.get("r").hubInstance = "Z";
    inner.commit = async () => {};
    await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 1, kind: "system", hubAt: "2026-01-01T00:00:03Z", payload: reset("fog:X/alice") });
    assert.equal(await pinned(inner, "fog:X/alice"), b64u(aliceId.publicKey), "a third hub's reset line dropped a pin");
    assert.equal(c.awaitingKey("fog:X/alice"), false);
    assert.ok(c.messages("r").some((v) => v.system === "identity-reset"), "the room still shows the line");
  }
  // The right hub, but about someone who was never in the room.
  {
    const { c, inner } = await bobsView();
    inner.rooms_.get("r").hubInstance = "X";
    inner.commit = async () => {};
    await inner.storage.put("identity-pins", "fog:X/zed", "zeds-key");
    await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 1, kind: "system", hubAt: "2026-01-01T00:00:03Z", payload: reset("fog:X/zed") });
    assert.equal(await pinned(inner, "fog:X/zed"), "zeds-key", "a reset about a non-member dropped their pin");
    assert.equal(c.awaitingKey("fog:X/zed"), false);
  }
});

test("content: an attachment names its blob by hex sha256 only, and every id is path-escaped", async () => {
  const { Api } = await import("../src/api.ts");
  const key = new Uint8Array(32), nonce = new Uint8Array(12);
  const good = "ab".repeat(32);
  for (const bad of ["../../me", "../keys?addr=x", "AB".repeat(32), good + "/x", "%2e%2e", ""]) {
    const c = decodeContent(encodeCbor({ v: 1, type: "message", attachments: [{ blob: bad, key, nonce }, { blob: good, key, nonce }] }));
    assert.deepEqual(c.attachments?.map((x) => x.blob), [good], bad);
  }
  const urls: string[] = [];
  const api = new Api({ baseUrl: "http://fog.example", fetch: (async (u: string) => {
    urls.push(String(u));
    return new Response(new Uint8Array(1), { status: 200, headers: { "content-type": "application/octet-stream" } });
  }) as unknown as typeof fetch });
  await assert.rejects(api.getBlob("../me"), /not a blob reference/);
  assert.equal(urls.length, 0, "a bad blob name was fetched");
  await api.getBlob(good).catch(() => {});
  assert.ok(urls[0]?.endsWith(`/api/chat/blobs/${good}`), urls[0]);
  await api.room("a/../../me").catch(() => {});
  assert.ok(urls[1]?.endsWith("/api/chat/rooms/a%2F..%2F..%2Fme"), urls[1]);
});

test("client: an integration's stamp is kept from fog's bot only, and shared history can't speak for fog", async () => {
  const { systemLabel, linkHost } = await import("../src/safe.ts");
  const { inner, c } = await clientWith({ "fog:X/eve": 2 });
  inner.rooms_.get("r").hubInstance = "H";
  const stamp = (system: string, sender: string) => inner.sanitize({ v: 1, type: "message", system }, sender, "r").system;
  assert.equal(stamp("app:fit", "fog:X/fog"), "app:fit", "this fog's bot lost an app's attribution");
  assert.equal(stamp("webhook:ci", "fog:H/fog"), "webhook:ci", "the hub's bot lost a webhook's attribution");
  assert.equal(stamp("fog", "fog:X/fog"), "fog");
  assert.equal(stamp("app:fit", "fog:X/eve"), undefined, "a member claimed an app's voice");
  assert.equal(stamp("fog", "fog:Q/fog"), undefined, "a bot of an unrelated fog spoke as fog");
  assert.equal(stamp("identity-reset", "fog:X/fog"), undefined);
  assert.deepEqual(systemLabel("app:fit"), { name: "fit", via: "app" });
  assert.deepEqual(systemLabel("webhook:ci"), { name: "ci", via: "webhook" });
  assert.equal(systemLabel("app:"), undefined);
  // Shared history: an item claiming the bot, or carrying any stamp, is refused.
  const H1 = ulid(), H2 = ulid(), H3 = ulid();
  await inner.receiveHistory("r", "fog:X/eve", {
    v: 1, type: "history", historyFor: ["me-device"], history: [
      { id: H1, seq: 1, sender: "fog:X/fog", hubAt: "", content: { v: 1, type: "message", body: { format: "text/markdown", text: "from fog?" } } },
      { id: H2, seq: 2, sender: "fog:X/eve", hubAt: "", content: { v: 1, type: "message", system: "app:bank", body: { format: "text/markdown", text: "from the bank app?" } } },
      { id: H3, seq: 3, sender: "fog:X/eve", hubAt: "", content: { v: 1, type: "message", body: { format: "text/markdown", text: "eve's" } } },
    ],
  });
  assert.deepEqual(c.messages("r").map((v) => v.id), [H3]);
  // A link card names the link's own host, whatever "site" it claims.
  assert.equal(linkHost("https://evil.example/login"), "evil.example");
  assert.equal(linkHost("javascript:alert(1)"), "");
});

// ---- round 5: relabelled leaves, look-alike Welcomes, re-shared history ----

test("client: a device key removed and added back under another name is refused, and every add and remove is a line", async () => {
  const { mls, a, c, inner, people, publish } = await bobsView();
  inner.commit = async () => {};
  const roomView = { ...inner.rooms_.get("r") };
  inner.api.rooms = async () => ({ rooms: [roomView], available: [] });
  const malId = generateKeyPair(), m1 = generateKeyPair(), m2 = generateKeyPair();
  people.set("fog:X/mallory", { id: malId, devs: [m1, m2] });
  publish();
  await inner.storage.put("identity-pins", "fog:X/mallory", b64u(malId.publicKey));
  const kp1 = await mls.keyPackage("fog:X/mallory", m1), kp2 = await mls.keyPackage("fog:X/mallory", m2);
  const add = await mls.addMembers(a, [mls.parseKeyPackage(kp1.raw), mls.parseKeyPackage(kp2.raw)]);
  await inner.applyEvent({ roomId: "r", seq: 2, id: ulid(), epoch: 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:02Z", payload: b64u(add.commit) });
  let mal = await mls.joinFromWelcome(add.welcome!, kp1);
  const epoch = mls.epoch(inner.states.get("r"));
  assert.equal(epoch, 2);
  const { createCommit, encodeMlsMessage } = await import("ts-mls");
  const m2Index = mls.members(mal).find((l) => bytesEqualT(l.signatureKey, m2.publicKey))!.index;

  // Remove m2 and add it back, same key, as alice: m2 would speak as her.
  const forged = await mls.keyPackage("fog:X/alice", m2);
  const relabel = await createCommit({ state: mal, cipherSuite: mls.impl }, {
    extraProposals: [{ proposalType: "remove", remove: { removed: m2Index } }, { proposalType: "add", add: { keyPackage: forged.publicPackage } }],
    ratchetTreeExtension: true, wireAsPublicMessage: true,
  });
  const relabelWire = encodeMlsMessage(relabel.commit);
  await assert.rejects(mls.process(inner.states.get("r"), relabelWire), (e: Error & { code?: string }) => e.code === "refused" && /device key/.test(e.message));
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch, kind: "mls_commit", sender: "fog:X/mallory", senderDevice: b64u(m1.publicKey), hubAt: "2026-01-01T00:00:03Z", payload: b64u(relabelWire) });
  assert.equal(mls.epoch(inner.states.get("r")), epoch, "the relabelled leaf was applied");
  assert.equal(c.room("r")?.sync, "stale");
  assert.ok(!mls.members(inner.states.get("r")).some((l: { identity: string; signatureKey: Uint8Array }) => l.identity === "fog:X/alice" && bytesEqualT(l.signatureKey, m2.publicKey)));

  // The same device removed and added back as itself, at the same index:
  // both are lines.
  const { c: c2, inner: i2, a: a2, mls: mls2, people: p2, publish: pub2 } = await bobsView();
  i2.commit = async () => {};
  p2.set("fog:X/mallory", { id: malId, devs: [m1, m2] });
  pub2();
  await i2.storage.put("identity-pins", "fog:X/mallory", b64u(malId.publicKey));
  const k1 = await mls2.keyPackage("fog:X/mallory", m1), k2 = await mls2.keyPackage("fog:X/mallory", m2);
  const add2 = await mls2.addMembers(a2, [mls2.parseKeyPackage(k1.raw), mls2.parseKeyPackage(k2.raw)]);
  await i2.applyEvent({ roomId: "r", seq: 2, id: ulid(), epoch: 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:02Z", payload: b64u(add2.commit) });
  mal = await mls2.joinFromWelcome(add2.welcome!, k1);
  const idx = mls2.members(mal).find((l) => bytesEqualT(l.signatureKey, m2.publicKey))!.index;
  const again = await mls2.keyPackage("fog:X/mallory", m2);
  const swap = await createCommit({ state: mal, cipherSuite: mls2.impl }, {
    extraProposals: [{ proposalType: "remove", remove: { removed: idx } }, { proposalType: "add", add: { keyPackage: again.publicPackage } }],
    ratchetTreeExtension: true, wireAsPublicMessage: true,
  });
  await i2.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "mls_commit", sender: "fog:X/mallory", senderDevice: b64u(m1.publicKey), hubAt: "2026-01-01T00:00:03Z", payload: b64u(encodeMlsMessage(swap.commit)) });
  assert.equal(mls2.epoch(i2.states.get("r")), 3, "an honest re-add was refused");
  const line = c2.messages("r").find((v) => v.id === "membership:3");
  assert.ok(line && /joined/.test(line.text) && /left/.test(line.text), `the remove and the add aren't both said: ${line?.text}`);
});

test("client: a Welcome into anything but the room's own group, as the hub verified it, is refused", async () => {
  const { mls, c, inner, people, publish } = await bobsView();
  const bobDev = inner.device as ReturnType<typeof generateKeyPair>;
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  inner.states.delete("r");
  inner.commit = async () => {};
  inner.catchUp = async () => {};
  inner.maybeReplenish = async () => {};
  inner.consumeStoredKeyPackage = async () => {};
  publish();
  const gid = crypto.getRandomValues(new Uint8Array(16));
  // The real group, as the hub verified it…
  let real = await mls.createGroup(gid, await mls.keyPackage("fog:X/alice", aliceDev));
  const kpReal = await mls.keyPackage("fog:X/bob", bobDev);
  const addReal = await mls.addMembers(real, [mls.parseKeyPackage(kpReal.raw)]);
  real = addReal.newState;
  // …and a look-alike under the same group id, made by a member to cut the
  // newcomer off.
  let fake = await mls.createGroup(gid, await mls.keyPackage("fog:X/alice", aliceDev));
  const kpFake = await mls.keyPackage("fog:X/bob", bobDev);
  const addFake = await mls.addMembers(fake, [mls.parseKeyPackage(kpFake.raw)]);
  fake = addFake.newState;
  const hubInfo = b64u(await mls.groupInfo(real));
  const hubEvent = { roomId: "r", seq: 2, id: ulid(), epoch: 0, kind: "mls_commit", hubAt: "2026-01-01T00:00:02Z", payload: b64u(addReal.commit) };
  inner.api = { ...inner.api, groupInfo: async () => ({ groupInfo: hubInfo, epoch: 1 }), events: async () => ({ events: [hubEvent], seq: 2, epoch: 1 }), acceptInvite: async () => ({ ok: true }) };
  inner.storedKeyPackages = async () => [kpFake];
  await inner.acceptInvite({ id: 1, roomId: "r", seq: 2, welcome: b64u(addFake.welcome!) });
  assert.equal(inner.states.has("r"), false, "joined a look-alike group");
  assert.match(c.room("r")?.staleReason ?? "", /not for the room's own group/);
  inner.storedKeyPackages = async () => [kpReal];
  await inner.acceptInvite({ id: 2, roomId: "r", seq: 2, welcome: b64u(addReal.welcome!) });
  assert.equal(inner.states.has("r"), true, "the room's own group was refused");
});

test("client: history another member shared is never re-shared as this device's own", async () => {
  const { c, inner } = await clientWith({ "fog:X/me": 4, "fog:X/eve": 2 });
  inner.rooms_.get("r").policy.history = "shared";
  inner.rooms_.get("r").myRole = 4;
  const mine = ulid(), theirs = ulid();
  await inner.storeMessage({ ...msg(mine, 1, { v: 1, type: "message", body: { format: "text/markdown", text: "received here" } }, "fog:X/eve"), checked: true });
  await inner.storeMessage({ ...msg(theirs, 2, { v: 1, type: "message", body: { format: "text/markdown", text: "eve says ann said" } }, "fog:X/ann"), checked: true, sharedBy: "fog:X/eve" });
  const sent: Content[] = [];
  inner.sendContent = async (_r: string, content: Content) => { sent.push(content); };
  await inner.maybeShareHistory("r", ["new-device"]);
  const shared = sent.flatMap((x) => x.history ?? []).map((h) => h.id);
  assert.deepEqual(shared, [mine], "shared history went out as this device's own word");
  void c;
});

// ---- round 6: what bytes are, recovery, safety codes, provisional joins ----

// bobsRoom is bobsView with a room list that survives refreshes, and dave
// (an admin, attested) in the group too.
async function bobsRoom(bobRole = 2) {
  const v = await bobsView();
  const { mls, a, inner, people, publish } = v;
  inner.rooms_.get("r").roles = { "fog:X/alice": 4, "fog:X/bob": bobRole, "fog:X/dave": 3 };
  inner.rooms_.get("r").myRole = bobRole;
  inner.rooms_.get("r").hubInstance = "";
  const roomView = { ...inner.rooms_.get("r") };
  inner.api.rooms = async () => ({ rooms: [roomView], available: [] });
  inner.commit = async () => {};
  inner.directory = async () => {
    publish();
    return [];
  };
  const daveId = generateKeyPair(), daveDev = generateKeyPair();
  people.set("fog:X/dave", { id: daveId, devs: [daveDev] });
  publish();
  await inner.storage.put("identity-pins", "fog:X/dave", b64u(daveId.publicKey));
  const daveKp = await mls.keyPackage("fog:X/dave", daveDev);
  const add = await mls.addMembers(a, [mls.parseKeyPackage(daveKp.raw)]);
  await inner.applyEvent({ roomId: "r", seq: 2, id: ulid(), epoch: 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:02Z", payload: b64u(add.commit) });
  const dave = await mls.joinFromWelcome(add.welcome!, daveKp);
  return { ...v, a: add.newState, dave, daveDev };
}

test("client: a commit is a commit whatever fog labels it: relabelled as a message, or as this device's own, it gets every check", async () => {
  for (const label of [{ kind: "mls_private" }, { kind: "mls_commit", own: true }]) {
    const { mls, a, c, inner } = await bobsRoom();
    const ghost = generateKeyPair();
    const add = await mls.addMembers(a, [mls.parseKeyPackage((await mls.keyPackage("fog:X/alice", ghost)).raw)]);
    const epoch = mls.epoch(inner.states.get("r"));
    await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch, kind: label.kind, sender: "fog:X/alice", senderDevice: label.own ? inner.meInfo.deviceKey : "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(add.commit) });
    assert.equal(mls.epoch(inner.states.get("r")), epoch, `${JSON.stringify(label)}: an unattested device got in`);
    assert.equal(c.room("r")?.sync, "stale", `${JSON.stringify(label)}: no stale marker`);
  }
  // Bytes that are neither a commit nor a message are fog's fault and one
  // bad event: shown as such, never the room's state.
  const { c, inner } = await bobsRoom();
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(new Uint8Array([1, 2, 3])) });
  assert.equal(c.room("r")?.sync, "ready", "a hub-labelled non-commit made the room stale (and open to a rejoin on fog's word)");
  assert.ok(c.messages("r").some((m) => m.system === "undecryptable"));
});

test("client: a room stale over a device nobody vouches for recovers when a member removes it, and only then", async () => {
  const { mls, a, c, inner } = await bobsRoom(3);
  const ghost = generateKeyPair();
  const ext = await mls.externalJoin(await mls.groupInfo(a), await mls.keyPackage("fog:X/alice", ghost));
  const epoch = mls.epoch(inner.states.get("r"));
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch, kind: "mls_commit", sender: "fog:X/alice", senderDevice: b64u(ghost.publicKey), hubAt: "2026-01-01T00:00:03Z", payload: b64u(ext.commit) });
  assert.equal(c.room("r")?.sync, "stale");
  assert.equal((await c.refusedDevices("r")).length, 1);
  // A message decrypting meanwhile doesn't make the room ready again.
  // A commit after it that leaves the ghost in: still refused.
  const withGhost = (await mls.process(a, ext.commit)).newState;
  const keep = await mls.selfUpdate(withGhost);
  const { c: c2, inner: i2, a: a2, mls: m2 } = await bobsRoom(3);
  const ext2 = await m2.externalJoin(await m2.groupInfo(a2), await m2.keyPackage("fog:X/alice", ghost));
  await i2.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(ext2.commit) });
  const a2g = (await m2.process(a2, ext2.commit)).newState;
  const keep2 = await m2.selfUpdate(a2g);
  await i2.applyEvent({ roomId: "r", seq: 4, id: ulid(), epoch: epoch + 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:04Z", payload: b64u(keep2.commit) });
  assert.equal(c2.room("r")?.sync, "stale", "a commit that kept the ghost ended the refusal");
  void keep;
  // alice removes the ghost: bob takes the pair, and the room is whole.
  const ghostIdx = mls.members(withGhost).find((l) => bytesEqualT(l.signatureKey, ghost.publicKey))!.index;
  const rm = await mls.removeLeaves(withGhost, [ghostIdx]);
  await inner.applyEvent({ roomId: "r", seq: 4, id: ulid(), epoch: epoch + 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:04Z", payload: b64u(rm.commit) });
  assert.equal(mls.epoch(inner.states.get("r")), epoch + 2);
  assert.equal(c.room("r")?.sync, "ready");
  assert.ok(!mls.members(inner.states.get("r")).some((l: { signatureKey: Uint8Array }) => bytesEqualT(l.signatureKey, ghost.publicKey)));
  // Or bob, an admin, removes it himself.
  const { c: c3, inner: i3, a: a3, mls: m3 } = await bobsRoom(3);
  const ext3 = await m3.externalJoin(await m3.groupInfo(a3), await m3.keyPackage("fog:X/alice", ghost));
  await i3.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(ext3.commit) });
  let submitted = 0;
  i3.api.submit = async () => ({ event: { roomId: "r", seq: 4 + submitted++, id: ulid(), epoch: epoch + 1, kind: "mls_commit", hubAt: "2026-01-01T00:00:04Z" } });
  i3.catchUp = async () => {};
  await c3.removeRefusedDevices("r");
  assert.equal(submitted, 1);
  assert.equal(m3.epoch(i3.states.get("r")), epoch + 2);
  assert.equal(c3.room("r")?.sync, "ready");
  assert.ok(!m3.members(i3.states.get("r")).some((l: { signatureKey: Uint8Array }) => bytesEqualT(l.signatureKey, ghost.publicKey)));
});

test("client: a stale room stays stale when a message decrypts", async () => {
  const { mls, a, c, inner } = await bobsRoom();
  inner.markStale("r", "refused something");
  const { wire } = await mls.encrypt(a, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: "hi" } }));
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(wire) });
  assert.equal(c.room("r")?.sync, "stale");
  await assert.rejects(c.send("r", "into a refused room"), /refused/);
});

test("client: safety codes come from this device's seed and pins, never from the keys fog serves", async () => {
  const { c, inner } = await clientWith({});
  const own = generateKeyPair(), fogsForMe = generateKeyPair(), alice = generateKeyPair(), fogsForAlice = generateKeyPair();
  inner.identity = own;
  inner.meInfo = { ...inner.meInfo, identityKey: b64u(fogsForMe.publicKey) };
  inner.dirIdentity.set("fog:X/me", b64u(fogsForMe.publicKey));
  inner.dirIdentity.set("fog:X/alice", b64u(fogsForAlice.publicKey));
  await inner.storage.put("identity-pins", "fog:X/alice", b64u(alice.publicKey));
  const codes = await c.safetyCodes("fog:X/alice");
  const { fingerprint } = await import("../src/keys.ts");
  assert.equal(codes.mine, fingerprint(own.publicKey), "my code is fog's key for me");
  assert.notEqual(codes.mine, fingerprint(fogsForMe.publicKey));
  assert.equal(codes.theirs, fingerprint(alice.publicKey), "their code is fog's key for them");
  assert.equal(codes.theirKey, b64u(alice.publicKey));
  assert.ok(codes.ownSwapped && codes.theirsSwapped, "no warning that fog swapped keys");
});

test("client: a Welcome behind the hub's GroupInfo holds only once the commits since match it", async () => {
  for (const catchesUp of [false, true]) {
    const { mls, c, inner, people, publish } = await bobsView();
    const bobDev = inner.device as ReturnType<typeof generateKeyPair>;
    const aliceDev = people.get("fog:X/alice")!.devs[0];
    inner.states.delete("r");
    inner.commit = async () => {};
    inner.maybeReplenish = async () => {};
    inner.consumeStoredKeyPackage = async () => {};
    inner.closed = true; // the test drives the re-reads
    publish();
    let g = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", aliceDev));
    const kp = await mls.keyPackage("fog:X/bob", bobDev);
    const add = await mls.addMembers(g, [mls.parseKeyPackage(kp.raw)]);
    g = add.newState;
    const later = await mls.selfUpdate(g); // the hub's GroupInfo is one epoch on
    const hubInfo = b64u(await mls.groupInfo(later.newState));
    const roomView = { ...inner.rooms_.get("r") };
    const hubEvent = { roomId: "r", seq: 2, id: ulid(), epoch: 0, kind: "mls_commit", hubAt: "2026-01-01T00:00:02Z", payload: b64u(add.commit) };
    inner.api = { ...inner.api, rooms: async () => ({ rooms: [roomView], available: [] }), groupInfo: async () => ({ groupInfo: hubInfo, epoch: 2 }), events: async () => ({ events: [hubEvent], seq: 2, epoch: 2 }), acceptInvite: async () => ({ ok: true }) };
    inner.storedKeyPackages = async () => [kp];
    inner.catchUp = async () => {
      if (catchesUp) await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(later.commit) });
    };
    await inner.acceptInvite({ id: 1, roomId: "r", seq: 2, welcome: b64u(add.welcome!) });
    if (catchesUp) {
      assert.equal(c.room("r")?.sync, "ready", "a join that caught up was refused");
      assert.equal(await inner.storage.get("meta", "provisional:r"), undefined);
    } else {
      // A log one commit short (a follower's copy lagging the hub) is a
      // wait, not a verdict: nothing is sent meanwhile…
      assert.notEqual(c.room("r")?.sync, "stale", "a lagging copy of the log locked the joiner out");
      await assert.rejects(c.send("r", "too soon"), /hasn't yet matched/);
      // …and once the commit arrives, the join holds.
      inner.catchUp = async () => {
        await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 1, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(later.commit) });
      };
      await inner.catchUp("r");
      assert.equal(c.room("r")?.sync, "ready", "the join never held once the log caught up");
      assert.equal(await inner.storage.get("meta", "provisional:r"), undefined);
    }
  }
});

test("client: a re-add's key is taken only with the commit; fog's bot vouches for no one", async () => {
  const reset = b64u(new TextEncoder().encode(JSON.stringify({ kind: "identity-reset", left: ["fog:X/alice"] })));
  // dave re-adds alice's new device alongside one nobody vouches for: the
  // commit is refused, and so is the key.
  {
    const { mls, c, inner, people, publish, dave } = await bobsRoom();
    await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "system", hubAt: "2026-01-01T00:00:03Z", payload: reset });
    const newId = generateKeyPair(), newDev = generateKeyPair(), ghost = generateKeyPair();
    people.set("fog:X/alice", { id: newId, devs: [newDev] });
    publish();
    const both = await mls.addMembers(dave, [mls.parseKeyPackage((await mls.keyPackage("fog:X/alice", newDev)).raw), mls.parseKeyPackage((await mls.keyPackage("fog:X/erin", ghost)).raw)]);
    await inner.applyEvent({ roomId: "r", seq: 4, id: ulid(), epoch: 2, kind: "mls_commit", sender: "fog:X/dave", senderDevice: "", hubAt: "2026-01-01T00:00:04Z", payload: b64u(both.commit) });
    assert.equal(c.awaitingKey("fog:X/alice"), true, "a refused commit's re-add pinned the key");
    assert.equal(await inner.storage.get("identity-pins", "fog:X/alice"), undefined);
  }
  // fog's bot re-adds her: no vouching.
  {
    const { mls, a, c, inner, people, publish } = await bobsRoom();
    const botId = generateKeyPair(), botDev = generateKeyPair();
    people.set("fog:X/fog", { id: botId, devs: [botDev] });
    publish();
    await inner.storage.put("identity-pins", "fog:X/fog", b64u(botId.publicKey));
    const botKp = await mls.keyPackage("fog:X/fog", botDev);
    const addBot = await mls.addMembers(a, [mls.parseKeyPackage(botKp.raw)]);
    await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(addBot.commit) });
    const bot = await mls.joinFromWelcome(addBot.welcome!, botKp);
    await inner.applyEvent({ roomId: "r", seq: 4, id: ulid(), epoch: 3, kind: "system", hubAt: "2026-01-01T00:00:04Z", payload: reset });
    const newId = generateKeyPair(), newDev = generateKeyPair();
    people.set("fog:X/alice", { id: newId, devs: [newDev] });
    publish();
    const readd = await mls.addMembers(bot, [mls.parseKeyPackage((await mls.keyPackage("fog:X/alice", newDev)).raw)]);
    await inner.applyEvent({ roomId: "r", seq: 5, id: ulid(), epoch: 3, kind: "mls_commit", sender: "fog:X/fog", senderDevice: "", hubAt: "2026-01-01T00:00:05Z", payload: b64u(readd.commit) });
    assert.equal(c.awaitingKey("fog:X/alice"), true, "fog's bot vouched for a new key");
    assert.equal(c.room("r")?.sync, "stale");
  }
});

test("client: a device key never takes another name, whatever epochs pass between", async () => {
  const { mls, a, c, inner, people, publish } = await bobsRoom();
  // alice's device key is recorded under her name; in a later epoch the
  // same key comes in as erin, whose identity key (colluding) attests it.
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  const erinId = generateKeyPair();
  people.set("fog:X/erin", { id: erinId, devs: [aliceDev] });
  publish();
  await inner.storage.put("identity-pins", "fog:X/erin", b64u(erinId.publicKey));
  const step = await mls.selfUpdate(a);
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "mls_commit", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(step.commit) });
  await assert.rejects(inner.checkKeyNames("r", [{ identity: "fog:X/erin", signatureKey: aliceDev.publicKey }]), /hands fog:X\/alice's device key to fog:X\/erin/);
  void c;
});

test("client: a revoked device's cached attestation goes with it", async () => {
  const { inner, people, publish } = await bobsRoom();
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  assert.equal(await inner.leafAttested("fog:X/alice", aliceDev.publicKey), true);
  const payload = b64u(new TextEncoder().encode(JSON.stringify({ kind: "device-revoked", person: "fog:X/alice", devices: [b64u(aliceDev.publicKey)] })));
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "system", hubAt: "2026-01-01T00:00:03Z", payload });
  people.get("fog:X/alice")!.devs = [];
  inner.dirDevices.clear();
  publish();
  assert.equal(await inner.leafAttested("fog:X/alice", aliceDev.publicKey), false, "a revoked device stayed attested");
});

test("client: two joins from one Welcome at once, before the directory has the inviter, end ready, not refused", async () => {
  const { mls, c, inner, people, publish } = await bobsView();
  const bobDev = inner.device as ReturnType<typeof generateKeyPair>;
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  inner.states.delete("r");
  inner.commit = async () => {};
  inner.catchUp = async () => {};
  inner.maybeReplenish = async () => {};
  inner.consumeStoredKeyPackage = async () => {};
  let g = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", aliceDev));
  const kp = await mls.keyPackage("fog:X/bob", bobDev);
  const add = await mls.addMembers(g, [mls.parseKeyPackage(kp.raw)]);
  g = add.newState;
  const hubInfo = b64u(await mls.groupInfo(g));
  const roomView = { ...inner.rooms_.get("r") };
  const hubEvent = { roomId: "r", seq: 2, id: ulid(), epoch: 0, kind: "mls_commit", hubAt: "2026-01-01T00:00:02Z", payload: b64u(add.commit) };
  inner.api = { ...inner.api, rooms: async () => ({ rooms: [roomView], available: [] }), groupInfo: async () => ({ groupInfo: hubInfo, epoch: 1 }), events: async () => ({ events: [hubEvent], seq: 2, epoch: 1 }), acceptInvite: async () => ({ ok: true }) };
  inner.storedKeyPackages = async () => [kp];
  // The directory doesn't know alice yet; fetching it takes a moment.
  inner.dirIdentity.clear();
  inner.dirDevices.clear();
  await inner.storage.del("identity-pins", "fog:X/alice");
  inner.directory = async () => {
    await new Promise((r) => setTimeout(r, 50));
    publish();
    return [];
  };
  const inv = { id: 1, roomId: "r", seq: 2, welcome: b64u(add.welcome!) };
  await Promise.all([inner.acceptInvite(inv), inner.acceptInvite(inv)]);
  assert.equal(inner.states.has("r"), true, "the join was refused");
  assert.equal(c.room("r")?.sync, "ready", `the room was left ${c.room("r")?.sync}: ${c.room("r")?.staleReason}`);
});

// ---- round 7 ----

test("client: a Welcome whose secrets aren't the ones the hub-ordered commit made is refused, though everything public matches", async () => {
  const { mls, c, inner, people, publish } = await bobsView();
  const bobDev = inner.device as ReturnType<typeof generateKeyPair>;
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  inner.states.delete("r");
  inner.commit = async () => {};
  inner.catchUp = async () => {};
  inner.maybeReplenish = async () => {};
  inner.consumeStoredKeyPackage = async () => {};
  publish();
  let g = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", aliceDev));
  const kp = await mls.keyPackage("fog:X/bob", bobDev);
  const add = await mls.addMembers(g, [mls.parseKeyPackage(kp.raw)]);
  g = add.newState;
  const hubInfo = b64u(await mls.groupInfo(g));
  const hubEvent = { roomId: "r", seq: 2, id: ulid(), epoch: 0, kind: "mls_commit", hubAt: "2026-01-01T00:00:02Z", payload: b64u(add.commit) };
  const roomView = { ...inner.rooms_.get("r") };
  inner.api = { ...inner.api, rooms: async () => ({ rooms: [roomView], available: [] }), groupInfo: async () => ({ groupInfo: hubInfo, epoch: 1 }), events: async () => ({ events: [hubEvent], seq: 2, epoch: 1 }), acceptInvite: async () => ({ ok: true }) };
  inner.storedKeyPackages = async () => [kp];
  // The adder's own Welcome: the same group id, epoch, tree and transcript,
  // on a joiner secret she chose — so the epoch's confirmation tag differs.
  const real = mls;
  inner.mls = Object.assign(Object.create(Object.getPrototypeOf(real)), real, {
    joinFromWelcome: async (w: Uint8Array, k: unknown) => {
      const st = await real.joinFromWelcome(w, k as never);
      return { ...st, confirmationTag: crypto.getRandomValues(new Uint8Array(st.confirmationTag.length)) };
    },
  });
  await inner.acceptInvite({ id: 7, roomId: "r", seq: 2, welcome: b64u(add.welcome!) });
  assert.equal(inner.states.has("r"), false, "joined a look-alike group");
  assert.equal(c.room("r")?.sync, "stale");
  // Refused for good: the same invite isn't judged again.
  let judged = 0;
  inner.mls = Object.assign(Object.create(Object.getPrototypeOf(real)), real, { joinFromWelcome: async () => { judged++; throw new Error("x"); } });
  await inner.acceptInvite({ id: 7, roomId: "r", seq: 2, welcome: b64u(add.welcome!) });
  assert.equal(judged, 0, "a refused invite was judged again");
  // The real one joins.
  inner.mls = real;
  await inner.acceptInvite({ id: 8, roomId: "r", seq: 2, welcome: b64u(add.welcome!) });
  assert.equal(inner.states.has("r"), true);
});

test("client: a message that can't be read costs that message, never the room; a stale room stays stale across a restart", async () => {
  const { mls, a, c, inner } = await bobsRoom();
  // Garbage for the current epoch, from another group: what a member could
  // send everyone.
  const other = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", generateKeyPair()));
  let o = other;
  const epoch = mls.epoch(inner.states.get("r"));
  while (mls.epoch(o) < epoch) o = (await mls.selfUpdate(o)).newState;
  const { wire } = await mls.encrypt(o, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: "?" } }));
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch, kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(wire) });
  assert.equal(c.room("r")?.sync, "ready", "one unreadable message froze the room");
  assert.ok(c.messages("r").some((m) => m.system === "undecryptable"), "the unreadable message isn't marked");
  // The room goes on: alice's next real message reads.
  const ok = await mls.encrypt(a, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: "still here" } }));
  await inner.applyEvent({ roomId: "r", seq: 4, id: ulid(), epoch, kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:04Z", payload: b64u(ok.wire) });
  assert.ok(c.messages("r").some((m) => m.text === "still here"));
  // A refusal does make it stale, and that outlives a restart.
  inner.markStale("r", "refused a commit");
  await new Promise((r) => setTimeout(r, 10));
  inner.rooms_.get("r").sync = "ready";
  delete inner.rooms_.get("r").staleReason;
  await inner.refreshRooms();
  assert.equal(c.room("r")?.sync, "stale", "a restart made a stale room ready");
  await assert.rejects(c.send("r", "after a restart"));
});

test("client: a gap in one sender's messages — pruned, never delivered — is stepped over, and one past reach is that message alone", async () => {
  const { mls, a, c, inner } = await bobsRoom();
  // alice sends 600 messages bob never gets (retention pruned them), then one.
  let st = a;
  for (let i = 0; i < 600; i++) st = (await mls.encrypt(st, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: "pruned" } }))).newState;
  const after = await mls.encrypt(st, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: "after the gap" } }));
  const epoch = mls.epoch(inner.states.get("r"));
  await inner.applyEvent({ roomId: "r", seq: 700, id: ulid(), epoch, kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:10:00Z", payload: b64u(after.wire) });
  assert.ok(c.messages("r").some((m) => m.text === "after the gap"), "a gap past ts-mls's default window lost the message");
  assert.equal(c.room("r")?.sync, "ready");
});

test("client: one tab per device holds its keys: a second client of the same device can't start, so no generation is sent twice", async () => {
  const { ChatClient, DeviceInUseError } = await import("../src/client.ts");
  let held = false;
  const lock = { take: async () => (held ? false : (held = true)) };
  const storage = new MemoryStorage();
  const calls: string[] = [];
  const fetch = (async (u: string) => {
    calls.push(String(u));
    throw new Error("offline");
  }) as unknown as typeof globalThis.fetch;
  const tab1 = new ChatClient({ baseUrl: "http://fog.example", storage, fetch, lock });
  const tab2 = new ChatClient({ baseUrl: "http://fog.example", storage, fetch, lock });
  await assert.rejects(tab1.init(), /offline/); // tab 1 holds the lock, and goes on
  const before = calls.length;
  await assert.rejects(tab2.init(), (e: Error) => e instanceof DeviceInUseError);
  assert.equal(calls.length, before, "the second tab touched the server before finding the device in use");
});

test("client: two tabs over one browser storage, with the real Web Locks: the second can't take the device's keys", async () => {
  const { ChatClient, webDeviceLock } = await import("../src/client.ts");
  const { Worker } = await import("node:worker_threads");
  const name = "fog-chat-device:two-tab-test-" + Math.random();
  class SharedStorage extends MemoryStorage {
    readonly lockName = name;
  }
  const fetch = (async () => {
    throw new Error("offline");
  }) as unknown as typeof globalThis.fetch;
  // This tab starts its client: it takes the lock, and keeps it.
  const tab1 = new ChatClient({ baseUrl: "http://fog.example", storage: new SharedStorage(), fetch });
  await assert.rejects(tab1.init(), /offline/);
  // Another tab — another realm — asks for the same device's lock.
  const other = await new Promise<boolean>((resolve, reject) => {
    const w = new Worker(`
      const { parentPort, workerData } = require("node:worker_threads");
      navigator.locks.request(workerData, { ifAvailable: true }, (l) => { parentPort.postMessage(!!l); });
    `, { eval: true, workerData: name });
    w.once("message", (held) => {
      resolve(held);
      w.terminate();
    });
    w.once("error", reject);
  });
  assert.equal(other, false, "a second tab got the device's lock while the first held it");
  // This tab starting a client again (a restore) still holds it.
  assert.equal(await webDeviceLock(name)!.take(), true);
});

test("client: a Welcome that can't be checked yet — fog away, the adding commit not synced — is tried again, never refused", async () => {
  const { mls, c, inner, people, publish } = await bobsView();
  const bobDev = inner.device as ReturnType<typeof generateKeyPair>;
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  inner.states.delete("r");
  inner.commit = async () => {};
  inner.catchUp = async () => {};
  inner.maybeReplenish = async () => {};
  inner.consumeStoredKeyPackage = async () => {};
  inner.closed = true; // no timers: the test drives the retries
  publish();
  let g = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", aliceDev));
  const kp = await mls.keyPackage("fog:X/bob", bobDev);
  const add = await mls.addMembers(g, [mls.parseKeyPackage(kp.raw)]);
  g = add.newState;
  const hubInfo = b64u(await mls.groupInfo(g));
  const hubEvent = { roomId: "r", seq: 2, id: ulid(), epoch: 0, kind: "mls_commit", hubAt: "2026-01-01T00:00:02Z", payload: b64u(add.commit) };
  const roomView = { ...inner.rooms_.get("r") };
  let away = true, synced = false;
  inner.api = {
    ...inner.api, rooms: async () => ({ rooms: [roomView], available: [] }), acceptInvite: async () => ({ ok: true }),
    groupInfo: async () => {
      if (away) throw new Error("fog is away");
      return { groupInfo: hubInfo, epoch: 1 };
    },
    events: async () => ({ events: synced ? [hubEvent] : [], seq: 2, epoch: 1 }),
  };
  inner.storedKeyPackages = async () => [kp];
  const inv = { id: 9, roomId: "r", seq: 2, welcome: b64u(add.welcome!) };
  await inner.acceptInvite(inv);
  away = false;
  await inner.acceptInvite(inv); // reachable, but the commit isn't synced here yet
  assert.equal(inner.states.has("r"), false);
  assert.notEqual(c.room("r")?.sync, "stale", "a transient failure was taken as a forgery");
  assert.deepEqual(await inner.storage.get("meta", "refusedInvites") ?? [], [], "a transient failure refused the invite for good");
  synced = true;
  await inner.acceptInvite(inv);
  assert.equal(inner.states.has("r"), true, "the invite never joined once it could be checked");
});

test("client: a revoked device someone can't remove stays excluded: nothing is sent to it, the room says why, and it waits", async () => {
  const { ApiError } = await import("../src/api.ts");
  const { mls, c, inner, people } = await bobsRoom();
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  // alice (the owner) revoked a device of hers; bob, a member, may not
  // remove an owner's leaf.
  inner.commit = async () => {
    throw new ApiError(403, "only their own household, or someone who outranks them, removes their revoked device", "forbidden");
  };
  let sent = 0;
  inner.api.submit = async () => {
    sent++;
    return { event: { roomId: "r", seq: 99, id: ulid(), epoch: 2, kind: "mls_private", hubAt: "" } };
  };
  const payload = b64u(new TextEncoder().encode(JSON.stringify({ kind: "device-revoked", person: "fog:X/alice", devices: [b64u(aliceDev.publicKey)] })));
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: 2, kind: "system", hubAt: "2026-01-01T00:00:03Z", payload });
  await assert.rejects(c.send("r", "to a revoked device?"), /waiting for someone who may remove/);
  assert.equal(sent, 0, "a message went to a group still holding a revoked device");
  assert.deepEqual(await inner.storage.get("meta", "doomed:r"), [b64u(aliceDev.publicKey)], "the revoked leaf was taken as back");
  void mls;
});

test("client: commits, sends, receipts and incoming events at once never rewind the epoch or reuse a generation", async () => {
  const { ApiError } = await import("../src/api.ts");
  const { mls, a, c, inner } = await bobsRoom();
  inner.commit = Object.getPrototypeOf(inner).commit; // the real one
  const bobKey = inner.meInfo.deviceKey;
  // A hub: one order, commits only for the current epoch.
  const hub = { epoch: mls.epoch(inner.states.get("r")), seq: inner.cursors.get("r") ?? 2, log: [] as any[] };
  const pause = () => new Promise((r) => setTimeout(r, Math.random() * 4));
  inner.api.submit = async (_room: string, input: { id: string; kind: string; payload: string }) => {
    await pause();
    const k = mls.kindOf(unb64(input.payload));
    if (k.kind === "commit" && k.epoch !== hub.epoch) throw new ApiError(409, "the room moved on", "epoch_stale");
    const ev = { roomId: "r", seq: ++hub.seq, id: input.id, epoch: k.kind === "other" ? 0 : k.epoch, kind: input.kind, sender: "fog:X/bob", senderDevice: bobKey, hubAt: "2026-01-01T00:00:00Z", payload: input.payload };
    if (k.kind === "commit") hub.epoch++;
    hub.log.push(ev);
    await pause();
    return { event: ev };
  };
  inner.api.events = async (_room: string, q: { after: number }) => ({ events: hub.log.filter((e) => e.seq > q.after), seq: hub.seq, epoch: hub.epoch });
  // alice follows the hub's log, and speaks too.
  let alice = a;
  let read = 0;
  const followed: string[] = [];
  const aliceFollow = async () => {
    for (; read < hub.log.length; read++) {
      const ev = hub.log[read];
      if (ev.sender === "fog:X/alice") continue;
      const wire = unb64(ev.payload);
      const k = mls.kindOf(wire);
      const res = await mls.process(alice, wire, {}, k.kind === "commit" ? "commit" : "application");
      alice = res.newState;
      if (res.plaintext) followed.push(decodeContent(res.plaintext).id!);
    }
  };
  const sentIds: string[] = [];
  for (let round = 0; round < 15; round++) {
    await aliceFollow();
    const fromAlice = await mls.encrypt(alice, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: `alice ${round}` } }));
    alice = fromAlice.newState;
    const aliceEv = { roomId: "r", seq: ++hub.seq, id: ulid(), epoch: mls.epoch(alice), kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:00Z", payload: b64u(fromAlice.wire) };
    hub.log.push(aliceEv);
    await Promise.all([
      c.send("r", `bob ${round}`).then((m: { id: string }) => sentIds.push(m.id)),
      inner.sendContent("r", { v: 1, type: "receipt", readSeq: hub.seq }).then((m: { id: string }) => sentIds.push(m.id)),
      inner.commit("r", (st: unknown) => mls.selfUpdate(st as never), true),
      Promise.resolve(inner.onFrame({ kind: "event", event: aliceEv })),
    ]);
    await inner.catchUp("r");
  }
  await aliceFollow();
  assert.equal(c.room("r")?.sync, "ready", `the room went stale: ${c.room("r")?.staleReason}`);
  assert.equal(mls.epoch(inner.states.get("r")), hub.epoch, "bob's epoch isn't the hub's");
  for (const id of sentIds) assert.ok(followed.includes(id), "a message bob sent couldn't be read by alice");
  for (let r = 0; r < 15; r++) assert.ok(c.messages("r").some((m) => m.text === `alice ${r}`), `bob lost alice's message ${r}`);
});

test("client: a commit whose answer was lost, that landed anyway, is this device's own state — not someone else's commit", async () => {
  const { mls, c, inner } = await bobsRoom();
  inner.commit = Object.getPrototypeOf(inner).commit;
  const before = mls.epoch(inner.states.get("r"));
  let landed: { payload: string } | undefined;
  inner.api.submit = async (_r: string, input: { payload: string }) => {
    landed = { payload: input.payload };
    throw new TypeError("fetch failed: the answer never came");
  };
  await assert.rejects(inner.commit("r", (st: unknown) => mls.selfUpdate(st as never), true));
  assert.ok(landed);
  // The commit comes back in the log.
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch: before, kind: "mls_commit", sender: "fog:X/bob", senderDevice: inner.meInfo.deviceKey, hubAt: "", payload: landed!.payload });
  assert.equal(mls.epoch(inner.states.get("r")), before + 1, "the committer didn't take its own commit");
  assert.equal(c.room("r")?.sync, "ready", "the committer went stale on its own commit");
});

test("client: a device moves a room to a fresh epoch before its chain gets too long", async () => {
  const { mls, inner } = await bobsRoom();
  let rolls = 0;
  inner.commit = async () => {
    rolls++;
  };
  // A week-old epoch: the next send rolls it.
  await inner.storage.put("meta", "epochAt:r", Date.now() - 8 * 24 * 3600_000);
  await inner.maybeRoll("r", false);
  assert.equal(rolls, 1);
  await inner.maybeRoll("r", false);
  assert.equal(rolls, 1, "rolled twice in one epoch");
  void mls;
});

test("client: a crafted far-ahead generation is that one message, refused before any ratcheting", async () => {
  const { mls, a, c, inner } = await bobsRoom();
  let st = a;
  for (let i = 0; i < 1100; i++) st = (await mls.encrypt(st, encodeContent({ v: 1, type: "message", id: ulid() }))).newState;
  const far = await mls.encrypt(st, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: "far" } }));
  const epoch = mls.epoch(inner.states.get("r"));
  const t0 = Date.now();
  await inner.applyEvent({ roomId: "r", seq: 3, id: ulid(), epoch, kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:03Z", payload: b64u(far.wire) });
  assert.ok(!c.messages("r").some((m) => m.text === "far"), "a message past the per-message bound decrypted");
  assert.ok(c.messages("r").some((m) => m.system === "undecryptable"));
  assert.equal(c.room("r")?.sync, "ready");
  assert.ok(Date.now() - t0 < 2000);
});


test("client: a stale device leaves on this device only, is re-added like any new device, and works", async () => {
  const { mls, a, c, inner, people } = await bobsRoom();
  const bobDev = inner.device as ReturnType<typeof generateKeyPair>;
  const aliceDev = people.get("fog:X/alice")!.devs[0];
  void aliceDev;
  inner.markStale("r", "refused something");
  let patched: { leave?: boolean; leaveDevice?: boolean } = {};
  inner.api.updateRoom = async (_r: string, patch: { leave?: boolean; leaveDevice?: boolean }) => {
    patched = patch;
    return { room: {} };
  };
  // The person stays in the room, with their role: only this device left.
  const roomView = { ...inner.rooms_.get("r"), roles: { "fog:X/alice": 4, "fog:X/bob": 2, "fog:X/dave": 3 } };
  inner.api.rooms = async () => ({ rooms: [roomView], available: [] });
  await c.leaveDevice("r");
  assert.ok(patched.leaveDevice && !patched.leave, "the whole person left, not this device");
  assert.equal(inner.states.has("r"), false, "the stale keys stayed");
  assert.equal(c.room("r")?.sync, "no-keys", "the room isn't waiting for this device to be added back");
  assert.equal(await inner.storage.get("meta", "stale:r"), undefined);
  // A member removes bob's old leaf and adds him back: a fresh Welcome.
  const bobIdx = mls.members(a).find((l: { identity: string }) => l.identity === "fog:X/bob")!.index;
  const rm = await mls.removeLeaves(a, [bobIdx]);
  const kp = await mls.keyPackage("fog:X/bob", bobDev);
  const add = await mls.addMembers(rm.newState, [mls.parseKeyPackage(kp.raw)]);
  const alice = add.newState;
  const back = { ...roomView, roles: { "fog:X/alice": 4, "fog:X/bob": 2, "fog:X/dave": 3 } };
  inner.api.rooms = async () => ({ rooms: [back], available: [] });
  const addEvent = { roomId: "r", seq: 9, id: ulid(), epoch: mls.epoch(rm.newState), kind: "mls_commit", hubAt: "2026-01-01T00:00:09Z", payload: b64u(add.commit) };
  inner.api.groupInfo = async () => ({ groupInfo: b64u(await mls.groupInfo(alice)), epoch: mls.epoch(alice) });
  inner.api.events = async () => ({ events: [addEvent], seq: 9, epoch: mls.epoch(alice) });
  inner.api.acceptInvite = async () => ({ ok: true });
  inner.catchUp = async () => {};
  inner.maybeReplenish = async () => {};
  inner.consumeStoredKeyPackage = async () => {};
  inner.storedKeyPackages = async () => [kp];
  await inner.acceptInvite({ id: 31, roomId: "r", seq: 9, welcome: b64u(add.welcome!) });
  assert.equal(c.room("r")?.sync, "ready", `the re-added device isn't ready: ${c.room("r")?.staleReason}`);
  // It works: alice's next message reads.
  const msg = await mls.encrypt(alice, encodeContent({ v: 1, type: "message", id: ulid(), body: { format: "text/markdown", text: "welcome back" } }));
  await inner.applyEvent({ roomId: "r", seq: 10, id: ulid(), epoch: mls.epoch(alice), kind: "mls_private", sender: "fog:X/alice", senderDevice: "", hubAt: "2026-01-01T00:00:10Z", payload: b64u(msg.wire) });
  assert.ok(c.messages("r").some((m) => m.text === "welcome back"));
});

test("client: fog can't make a device join a group of its choosing: no rejoin on connect, no Welcome over held keys", async () => {
  const { mls, c, inner, people } = await bobsRoom();
  // A stale room this device holds keys for, and a room it has none for.
  inner.markStale("r", "refused something");
  const keyless = { ...inner.rooms_.get("r"), id: "k", sync: "no-keys" };
  const stale = { ...inner.rooms_.get("r") };
  let groupInfoAsked = 0, submitted = 0;
  inner.api.rooms = async () => ({ rooms: [stale, keyless], available: [] });
  inner.api.groupInfo = async () => {
    groupInfoAsked++;
    const forged = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", people.get("fog:X/alice")!.devs[0]));
    return { groupInfo: b64u(await mls.groupInfo(forged)), epoch: 0 };
  };
  inner.api.submit = async () => {
    submitted++;
    throw new Error("must not submit");
  };
  inner.api.events = async () => ({ events: [], seq: 0, epoch: 0 });
  inner.api.invites = async () => ({ invites: [] });
  inner.openStream = () => {};
  inner.syncSiblingDevices = async () => {};
  await c.connect();
  assert.equal(submitted, 0, "a device joined something by itself");
  assert.equal(groupInfoAsked, 0, "a device went looking for a group to join by itself");
  assert.equal(inner.states.has("k"), false);
  assert.equal("resync" in inner || "recover" in inner, false, "the automatic rejoin is still there");
  // A Welcome for the room it holds stale keys for doesn't replace them.
  const before = inner.states.get("r");
  const g = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", people.get("fog:X/alice")!.devs[0]));
  const kp = await mls.keyPackage("fog:X/bob", inner.device);
  const add = await mls.addMembers(g, [mls.parseKeyPackage(kp.raw)]);
  inner.api.acceptInvite = async () => ({ ok: true });
  inner.storedKeyPackages = async () => [kp];
  await inner.acceptInvite({ id: 41, roomId: "r", seq: 2, welcome: b64u(add.welcome!) });
  assert.equal(inner.states.get("r"), before, "a Welcome replaced a stale room's keys");
  c.close();
});

test("client: an invitation this device can't open is said where the room is, not only logged", async () => {
  const { mls, c, inner, people } = await bobsView();
  inner.states.delete("r");
  const g = await mls.createGroup(crypto.getRandomValues(new Uint8Array(16)), await mls.keyPackage("fog:X/alice", people.get("fog:X/alice")!.devs[0]));
  const someoneElses = await mls.keyPackage("fog:X/bob", generateKeyPair());
  const add = await mls.addMembers(g, [mls.parseKeyPackage(someoneElses.raw)]);
  inner.storedKeyPackages = async () => [await mls.keyPackage("fog:X/bob", inner.device)];
  await inner.acceptInvite({ id: 51, roomId: "r", seq: 2, welcome: b64u(add.welcome!) });
  assert.match(c.room("r")?.staleReason ?? "", /can't open/);
});
