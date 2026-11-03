// The MLS layer over ts-mls: key packages signed with the device key, group
// creation, membership commits, external joins, Welcomes, message
// protection, and state serialization. Everything here is RFC 9420; the
// server never sees a call into this file.

import {
  createApplicationMessage,
  createCommit,
  createGroup,
  decodeGroupState,
  decodeMlsMessage,
  defaultCapabilities,
  defaultLifetime,
  emptyPskIndex,
  encodeGroupState,
  encodeMlsMessage,
  generateKeyPackageWithKey,
  getCiphersuiteFromName,
  getCiphersuiteImpl,
  joinGroup,
  joinGroupExternal,
  processMessage,
  type CiphersuiteImpl,
  type ClientState,
  type KeyPackage,
  type PrivateKeyPackage,
  type Proposal,
} from "ts-mls";
import { decodeKeyPackage, encodeKeyPackage } from "ts-mls/keyPackage.js";
import { defaultClientConfig } from "ts-mls/clientConfig.js";

// FORWARD_RATCHET_STEPS is how far ahead in one sender's chain a message
// may be and still decrypt (ts-mls's default is 200), and
// SENDER_EPOCH_GENERATIONS how far one sender's chain may go in one epoch:
// a sender crafting far-ahead generations costs a recipient at most the
// one per message, and the other in all, before any key is derived.
const FORWARD_RATCHET_STEPS = 1_000;
const SENDER_EPOCH_GENERATIONS = 100_000;
import { createGroupInfo } from "ts-mls/createCommit.js";
import { ratchetTreeEncoder } from "ts-mls/ratchetTree.js";
import { encode } from "ts-mls/codec/tlsEncoder.js";
import { unprotectPrivateMessage } from "ts-mls/messageProtection.js";
import { decryptSenderData } from "ts-mls/privateMessage.js";
import { addToMap } from "ts-mls/util/addToMap.js";
import type { PrivateMessage } from "ts-mls/privateMessage.js";
import type { RatchetTree } from "ts-mls/ratchetTree.js";
import type { Extension, GroupInfo, IncomingMessageCallback } from "ts-mls";
import type { FramedContent } from "ts-mls/framedContent.js";
import type { Credential } from "ts-mls/credential.js";
import type { KeyPair } from "./keys.ts";

export const CIPHERSUITE = "MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519";
// CIPHERSUITE_ID is CIPHERSUITE's RFC 9420 code point, which the server
// files key packages under.
export const CIPHERSUITE_ID = 1;

// rfcExternalPubShim lets ts-mls's joinGroupExternal consume an RFC-encoded
// external_pub (length-prefixed) without breaking the GroupInfo signature
// check, which must see the original bytes. joinGroupExternal reads the
// extension data exactly once for the key derivation, before it re-encodes
// the extensions to verify the signature; the shim hands the bare key to
// that first read and the original to every later one. Pinned to ts-mls
// 1.6.x and covered by a unit test so an upgrade that changes the order
// fails loudly rather than joining wrong.
function rfcExternalPubShim(gi: GroupInfo): GroupInfo {
  const extensions = gi.extensions.map((e) => {
    const d = e.extensionData;
    if (e.extensionType !== "external_pub" || d.length !== 33 || d[0] !== 32) return e;
    let first = true;
    return {
      extensionType: e.extensionType,
      get extensionData() {
        if (first) {
          first = false;
          return d.subarray(1);
        }
        return d;
      },
    } as Extension;
  });
  return { ...gi, extensions };
}

const te = new TextEncoder();
const td = new TextDecoder();

// POLICY_EXTENSION is the GroupContext extension that carries a room's
// policy (internal/chat PolicyExtensionType), the only one a commit may set.
const POLICY_EXTENSION = 0xf0f0;
const ADDR = /^fog:[^/]+\/.+$/;

// Sender is who MLS says sent a message: the leaf that signed it.
export interface Sender {
  identity: string;
  signatureKey: Uint8Array;
}

// CommitRules are a room's rules an incoming commit is held to, beyond what
// MLS checks itself.
export interface CommitRules {
  // dm is a DM's fixed membership, every address in it; absent otherwise.
  dm?: ReadonlySet<string>;
}

// RefusedMessage is an incoming handshake this client will not apply: one
// fog's hub would never have let through, or one that would change the
// group in a way fog's rooms don't allow.
export class RefusedMessage extends Error {
  code = "refused";
  constructor(why: string) {
    super("mls: refused: " + why);
    this.name = "RefusedMessage";
  }
}

// A standalone proposal never applies: fog commits carry theirs inline, and
// one held back would also stop this device sending until it was committed.
const refuseStandalone: IncomingMessageCallback = (incoming) => (incoming.kind === "proposal" ? "reject" : "accept");

function identityOf(cred: Credential): string {
  return cred.credentialType === "basic" ? td.decode(cred.identity) : "";
}

function sameBytes(a: Uint8Array, b: Uint8Array): boolean {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}

export interface OwnKeyPackage {
  publicPackage: KeyPackage;
  privatePackage: PrivateKeyPackage;
  // raw is the wire form fog publishes: a bare KeyPackage.
  raw: Uint8Array;
}

// Serialized private key package, for the store.
export interface StoredKeyPackage {
  raw: Uint8Array;
  initPrivateKey: Uint8Array;
  hpkePrivateKey: Uint8Array;
}

export class Mls {
  impl: CiphersuiteImpl;
  private constructor(impl: CiphersuiteImpl) {
    this.impl = impl;
  }

  static async init(): Promise<Mls> {
    return new Mls(await getCiphersuiteImpl(getCiphersuiteFromName(CIPHERSUITE)));
  }

  // keyPackage mints a fresh KeyPackage whose leaf is signed with the device
  // key and names the person's address as its credential.
  async keyPackage(addr: string, device: KeyPair): Promise<OwnKeyPackage> {
    const kp = await generateKeyPackageWithKey(
      { credentialType: "basic", identity: te.encode(addr) },
      defaultCapabilities(),
      defaultLifetime,
      [],
      { signKey: device.seed, publicKey: device.publicKey },
      this.impl,
    );
    return { publicPackage: kp.publicPackage, privatePackage: kp.privatePackage, raw: encodeKeyPackage(kp.publicPackage) };
  }

  storeKeyPackage(kp: OwnKeyPackage): StoredKeyPackage {
    return { raw: kp.raw, initPrivateKey: kp.privatePackage.initPrivateKey, hpkePrivateKey: kp.privatePackage.hpkePrivateKey };
  }

  restoreKeyPackage(s: StoredKeyPackage, device: KeyPair): OwnKeyPackage {
    const dec = decodeKeyPackage(s.raw, 0);
    if (!dec) throw new Error("mls: stored key package unreadable");
    return {
      publicPackage: dec[0],
      raw: s.raw,
      privatePackage: { initPrivateKey: s.initPrivateKey, hpkePrivateKey: s.hpkePrivateKey, signaturePrivateKey: device.seed },
    };
  }

  parseKeyPackage(raw: Uint8Array): KeyPackage {
    // Accept a bare KeyPackage or an MLSMessage carrying one.
    if (raw.length > 4 && raw[0] === 0 && raw[1] === 1 && raw[2] === 0 && raw[3] === 5) {
      const m = decodeMlsMessage(raw, 0);
      if (m && m[0].wireformat === "mls_key_package") return m[0].keyPackage;
    }
    const dec = decodeKeyPackage(raw, 0);
    if (!dec) throw new Error("mls: key package unreadable");
    return dec[0];
  }

  async createGroup(groupId: Uint8Array, kp: OwnKeyPackage): Promise<ClientState> {
    return createGroup(groupId, kp.publicPackage, kp.privatePackage, [], this.impl);
  }

  // addMembers commits Add proposals for the given key packages. The
  // returned state must only be adopted once the hub accepts the commit.
  async addMembers(state: ClientState, kps: KeyPackage[]): Promise<{ commit: Uint8Array; welcome: Uint8Array | null; newState: ClientState }> {
    const proposals: Proposal[] = kps.map((keyPackage) => ({ proposalType: "add", add: { keyPackage } }));
    const res = await createCommit({ state, cipherSuite: this.impl }, { extraProposals: proposals, ratchetTreeExtension: true, wireAsPublicMessage: true });
    return {
      commit: encodeMlsMessage(res.commit),
      welcome: res.welcome ? encodeMlsMessage({ welcome: res.welcome, wireformat: "mls_welcome", version: "mls10" }) : null,
      newState: res.newState,
    };
  }

  // removeLeaves commits Remove proposals.
  async removeLeaves(state: ClientState, leafIndexes: number[]): Promise<{ commit: Uint8Array; newState: ClientState }> {
    const proposals: Proposal[] = leafIndexes.map((removed) => ({ proposalType: "remove", remove: { removed } }));
    const res = await createCommit({ state, cipherSuite: this.impl }, { extraProposals: proposals, ratchetTreeExtension: true, wireAsPublicMessage: true });
    return { commit: encodeMlsMessage(res.commit), newState: res.newState };
  }

  // selfUpdate rotates this leaf's keys (post-compromise security).
  async selfUpdate(state: ClientState): Promise<{ commit: Uint8Array; newState: ClientState }> {
    const res = await createCommit({ state, cipherSuite: this.impl }, { ratchetTreeExtension: true, wireAsPublicMessage: true });
    return { commit: encodeMlsMessage(res.commit), newState: res.newState };
  }

  // groupInfo is what an external joiner needs, framed as an MLSMessage.
  //
  // RFC 9420 §12.4.3.1 defines the external_pub extension as
  // `opaque HPKEPublicKey<V>` — the key carries a length prefix. ts-mls 1.6
  // writes the bare key, which other implementations (mls-go, OpenMLS)
  // reject; so the GroupInfo is assembled here with the RFC encoding, and
  // externalJoin below accepts either form when reading.
  async groupInfo(state: ClientState): Promise<Uint8Array> {
    const external = await this.impl.hpke.deriveKeyPair(state.keySchedule.externalSecret);
    const pub = await this.impl.hpke.exportPublicKey(external.publicKey);
    const prefixed = new Uint8Array(pub.length + 1);
    prefixed[0] = pub.length; // a one-byte varint: lengths under 64
    prefixed.set(pub, 1);
    const extensions: Extension[] = [
      { extensionType: "external_pub", extensionData: prefixed },
      { extensionType: "ratchet_tree", extensionData: encode(ratchetTreeEncoder)(state.ratchetTree) },
    ];
    const gi = await createGroupInfo(state.groupContext, state.confirmationTag, state, extensions, this.impl);
    return encodeMlsMessage({ groupInfo: gi, wireformat: "mls_group_info", version: "mls10" });
  }

  // externalJoin enters a group from its GroupInfo (an open channel's door).
  async externalJoin(groupInfoWire: Uint8Array, kp: OwnKeyPackage): Promise<{ commit: Uint8Array; newState: ClientState }> {
    const m = decodeMlsMessage(groupInfoWire, 0);
    if (!m || m[0].wireformat !== "mls_group_info") throw new Error("mls: not a group info message");
    const res = await joinGroupExternal(rfcExternalPubShim(m[0].groupInfo), kp.publicPackage, kp.privatePackage, false, this.impl);
    return {
      commit: encodeMlsMessage({ publicMessage: res.publicMessage, wireformat: "mls_public_message", version: "mls10" }),
      newState: res.newState,
    };
  }

  // welcomedBy says a state joined from a Welcome is the epoch a commit made:
  // the commit's confirmation tag — which every member's client checks when
  // it applies the commit, from secrets only the real group holds — is the
  // joined epoch's own. A Welcome built on a joiner secret its sender chose
  // instead yields a group whose tag can't match.
  welcomedBy(state: ClientState, commitWire: Uint8Array): boolean {
    let m;
    try {
      m = decodeMlsMessage(commitWire, 0);
    } catch {
      return false;
    }
    if (!m || m[0].wireformat !== "mls_public_message") return false;
    const pm = m[0].publicMessage;
    if (pm.content.contentType !== "commit" || pm.auth.contentType !== "commit") return false;
    if (!sameBytes(pm.content.groupId, state.groupContext.groupId)) return false;
    if (pm.content.epoch + 1n !== state.groupContext.epoch) return false;
    return sameBytes(pm.auth.confirmationTag, state.confirmationTag);
  }

  // groupInfoEpoch reads a GroupInfo's epoch, or undefined if it isn't one.
  groupInfoEpoch(groupInfoWire: Uint8Array): number | undefined {
    try {
      const m = decodeMlsMessage(groupInfoWire, 0);
      if (!m || m[0].wireformat !== "mls_group_info") return undefined;
      return Number(m[0].groupInfo.groupContext.epoch);
    } catch {
      return undefined;
    }
  }

  // sameGroupAs says a group state is the group a GroupInfo describes, at
  // that GroupInfo's epoch: same group id, epoch, tree hash and confirmed
  // transcript hash. "behind" when the GroupInfo is for a later epoch (the
  // state catches up through the commits since); false when it is for an
  // earlier one, or anything differs.
  sameGroupAs(state: ClientState, groupInfoWire: Uint8Array): boolean | "behind" {
    const m = decodeMlsMessage(groupInfoWire, 0);
    if (!m || m[0].wireformat !== "mls_group_info") return false;
    const theirs = m[0].groupInfo.groupContext;
    const mine = state.groupContext;
    if (!sameBytes(theirs.groupId, mine.groupId)) return false;
    if (theirs.epoch > mine.epoch) return "behind";
    return theirs.epoch === mine.epoch && sameBytes(theirs.treeHash, mine.treeHash) && sameBytes(theirs.confirmedTranscriptHash, mine.confirmedTranscriptHash);
  }

  async joinFromWelcome(welcomeWire: Uint8Array, kp: OwnKeyPackage): Promise<ClientState> {
    const m = decodeMlsMessage(welcomeWire, 0);
    if (!m || m[0].wireformat !== "mls_welcome") throw new Error("mls: not a welcome message");
    return joinGroup(m[0].welcome, kp.publicPackage, kp.privatePackage, emptyPskIndex, this.impl);
  }

  // process applies an incoming commit or decrypts an application message.
  // Only application data may arrive encrypted: a commit or proposal in a
  // private message is a membership change the hub never saw, and is
  // refused. A public commit must keep to fog's rules (checkCommit), and
  // after it every leaf it didn't remove must still be who it was.
  //
  // It also returns who sent the message, as MLS proves it — the leaf whose
  // key signed it — so a client need not take the hub's word for a sender.
  // kindOf says what a message's own bytes are — never what a server
  // labelled them: a public commit (with its epoch), a private application
  // message, or anything else.
  kindOf(wire: Uint8Array): { kind: "commit"; epoch: number } | { kind: "application"; epoch: number } | { kind: "other" } {
    let m;
    try {
      m = decodeMlsMessage(wire, 0);
    } catch {
      return { kind: "other" };
    }
    if (!m) return { kind: "other" };
    const msg = m[0];
    if (msg.wireformat === "mls_private_message" && msg.privateMessage.contentType === "application") return { kind: "application", epoch: Number(msg.privateMessage.epoch) };
    if (msg.wireformat === "mls_public_message" && msg.publicMessage.content.contentType === "commit") {
      return { kind: "commit", epoch: Number(msg.publicMessage.content.epoch) };
    }
    return { kind: "other" };
  }

  // process applies an incoming message. expect, when given, is what the
  // caller decided the bytes are (kindOf): anything else is refused, so a
  // commit can never be applied down the path for application data.
  async process(state: ClientState, wire: Uint8Array, rules: CommitRules = {}, expect?: "commit" | "application"): Promise<{ newState: ClientState; plaintext?: Uint8Array; sender?: Sender; removed?: number[] }> {
    if (expect && this.kindOf(wire).kind !== expect) {
      throw new RefusedMessage(`a message that isn't the ${expect === "commit" ? "public commit" : "private application message"} it was taken for`);
    }
    const m = decodeMlsMessage(wire, 0);
    if (!m) throw new Error("mls: unreadable message");
    const msg = m[0];
    let removed: Set<number> | null = null;
    let sender: Sender | undefined;
    if (msg.wireformat === "mls_private_message") {
      if (msg.privateMessage.contentType !== "application") {
        throw new RefusedMessage(`an encrypted ${msg.privateMessage.contentType}: membership changes travel as public commits the hub checks`);
      }
      return this.openApplication(state, msg.privateMessage);
    } else if (msg.wireformat === "mls_public_message") {
      const content = msg.publicMessage.content;
      removed = this.checkCommit(state, content, rules);
      if (content.sender.senderType === "member") {
        const l = this.members(state).find((x) => x.index === (content.sender as { leafIndex: number }).leafIndex);
        if (l) sender = { identity: l.identity, signatureKey: l.signatureKey };
      } else if (content.contentType === "commit" && content.commit.path) {
        const leaf = content.commit.path.leafNode;
        sender = { identity: identityOf(leaf.credential), signatureKey: leaf.signaturePublicKey };
      }
    } else {
      throw new Error("mls: unexpected wire format " + msg.wireformat);
    }
    const before = this.members(state);
    const res = await processMessage(msg, state, emptyPskIndex, refuseStandalone, this.impl);
    if (res.kind === "applicationMessage") return { newState: res.newState, plaintext: res.message, sender };
    if (res.actionTaken === "reject") throw new RefusedMessage("a standalone proposal");
    if (removed) {
      const afterLeaves = this.members(res.newState);
      const after = new Map(afterLeaves.map((l) => [l.index, l]));
      for (const b of before) {
        if (removed.has(b.index)) continue;
        const a = after.get(b.index);
        if (a && (a.identity !== b.identity || !sameBytes(a.signatureKey, b.signatureKey))) {
          throw new RefusedMessage(`a commit that changes leaf ${b.index} (${b.identity}) into someone or something else`);
        }
      }
      // A device key belongs to one name for the group's life: removed and
      // added back under another (at the same index or any other), it would
      // speak as that person with a key nobody attested for them.
      for (const a of afterLeaves) {
        const was = before.find((b) => sameBytes(b.signatureKey, a.signatureKey));
        if (was && was.identity !== a.identity) {
          throw new RefusedMessage(`a commit that hands ${was.identity}'s device key to ${a.identity}`);
        }
      }
    }
    return { newState: res.newState, sender, removed: removed ? Array.from(removed) : [] };
  }

  // openApplication decrypts application data as ts-mls's processMessage
  // does, keeping what it drops: the sender leaf, whose key verified the
  // signature, read from the tree of the message's epoch.
  private async openApplication(state: ClientState, pm: PrivateMessage): Promise<{ newState: ClientState; plaintext: Uint8Array; sender?: Sender }> {
    // A sender's chain is followed far ahead: messages pruned by retention
    // (or never delivered) leave gaps, and a gap shorter than this is
    // stepped over. A longer one costs only the messages past it from that
    // sender in that epoch, never the room (the client marks each one).
    const cfg = { ...state.clientConfig.keyRetentionConfig, maximumForwardRatchetSteps: FORWARD_RATCHET_STEPS };
    // The sender and generation, read cheaply from the sender data first:
    // past either bound, the message is refused before a single step.
    const old = pm.epoch < state.groupContext.epoch ? state.historicalReceiverData.get(pm.epoch) : undefined;
    const sd = await decryptSenderData(pm, old ? old.senderDataSecret : state.keySchedule.senderDataSecret, this.impl);
    if (sd) {
      const chain = (old ? old.secretTree : state.secretTree)[sd.leafIndex * 2]?.application;
      if (sd.generation > SENDER_EPOCH_GENERATIONS) throw new RefusedMessage("a message too far along its sender's chain for one epoch");
      if (chain && sd.generation - chain.generation > FORWARD_RATCHET_STEPS) throw new RefusedMessage("a message too far ahead of what its sender sent before");
    }
    let tree: RatchetTree;
    let newState: ClientState;
    let res;
    if (pm.epoch < state.groupContext.epoch) {
      const rd = state.historicalReceiverData.get(pm.epoch);
      if (!rd) throw new Error("mls: cannot process message, epoch too old");
      res = await unprotectPrivateMessage(rd.senderDataSecret, pm, rd.secretTree, rd.ratchetTree, rd.groupContext, cfg, this.impl);
      newState = { ...state, historicalReceiverData: addToMap(state.historicalReceiverData, pm.epoch, { ...rd, secretTree: res.tree }) };
      tree = rd.ratchetTree;
    } else {
      res = await unprotectPrivateMessage(state.keySchedule.senderDataSecret, pm, state.secretTree, state.ratchetTree, state.groupContext, cfg, this.impl);
      newState = { ...state, secretTree: res.tree };
      tree = state.ratchetTree;
    }
    const c = res.content.content;
    if (c.contentType !== "application") throw new RefusedMessage(`an encrypted ${c.contentType}`);
    let sender: Sender | undefined;
    if (c.sender.senderType === "member") {
      const node = tree[c.sender.leafIndex * 2];
      if (node?.nodeType === "leaf") sender = { identity: identityOf(node.leaf.credential), signatureKey: node.leaf.signaturePublicKey };
    }
    return { newState, plaintext: c.applicationData, sender };
  }

  // checkCommit holds a public commit to what fog's clients send and its hub
  // accepts, and returns the leaves it removes. A member's commit carries
  // inline Adds, Removes and at most a policy change, and its path may
  // refresh the committer's keys but not its name or signature key. An
  // Update inside a commit would be the committer's own, which MLS forbids;
  // no commit ever replaces another member's leaf. An external commit
  // carries one external init and may remove only the joiner's own old leaf.
  checkCommit(state: ClientState, content: FramedContent, rules: CommitRules): Set<number> {
    if (content.contentType !== "commit") throw new RefusedMessage("a standalone proposal: fog commits carry their proposals inline");
    const sender = content.sender;
    const external = sender.senderType === "new_member_commit";
    if (sender.senderType !== "member" && !external) throw new RefusedMessage("a commit from outside the group");
    const leaves = new Map(this.members(state).map((l) => [l.index, l]));
    const path = content.commit.path?.leafNode;
    const removed = new Set<number>();
    let inits = 0;
    for (const por of content.commit.proposals) {
      if (por.proposalOrRefType !== "proposal") throw new RefusedMessage("a commit of a standalone proposal");
      const p = por.proposal;
      switch (p.proposalType) {
        case "add": {
          if (external) throw new RefusedMessage("an external commit that adds others");
          const who = identityOf(p.add.keyPackage.leafNode.credential);
          if (!ADDR.test(who)) throw new RefusedMessage(`an add of ${JSON.stringify(who)}, which is no one's address`);
          if (rules.dm && !rules.dm.has(who)) throw new RefusedMessage(`an add of ${who} to a DM, whose membership is fixed`);
          break;
        }
        case "remove": {
          const leaf = leaves.get(p.remove.removed);
          if (!leaf) throw new RefusedMessage("a remove of an empty leaf");
          if (external && (!path || leaf.identity !== identityOf(path.credential))) throw new RefusedMessage("an external commit that removes someone else");
          removed.add(p.remove.removed);
          break;
        }
        case "external_init":
          if (!external) throw new RefusedMessage("an external init in a member's commit");
          inits++;
          break;
        case "group_context_extensions":
          if (external) throw new RefusedMessage("an external commit that changes the group");
          if (!p.groupContextExtensions.extensions.every((e) => e.extensionType === POLICY_EXTENSION)) {
            throw new RefusedMessage("a group context change other than the room's policy");
          }
          break;
        case "update":
          throw new RefusedMessage("an update inside a commit, which can only be the committer's own and which MLS forbids");
        default:
          throw new RefusedMessage(`a ${String(p.proposalType)} proposal, which fog does not use`);
      }
    }
    if (external) {
      if (inits !== 1 || removed.size > 1 || !path) throw new RefusedMessage("a malformed external commit");
      const who = identityOf(path.credential);
      if (rules.dm && !rules.dm.has(who)) throw new RefusedMessage(`${who} joining a DM, whose membership is fixed`);
    } else {
      const me = leaves.get(sender.leafIndex);
      if (!me) throw new RefusedMessage("a commit from an empty leaf");
      if (path && (identityOf(path.credential) !== me.identity || !sameBytes(path.signaturePublicKey, me.signatureKey))) {
        throw new RefusedMessage(`a commit in which ${me.identity}'s leaf takes another name or key`);
      }
    }
    return removed;
  }

  async encrypt(state: ClientState, plaintext: Uint8Array): Promise<{ wire: Uint8Array; newState: ClientState }> {
    const res = await createApplicationMessage(state, plaintext, this.impl);
    return { wire: encodeMlsMessage({ privateMessage: res.privateMessage, wireformat: "mls_private_message", version: "mls10" }), newState: res.newState };
  }

  epoch(state: ClientState): number {
    return Number(state.groupContext.epoch);
  }

  // members lists the leaves: identity (person address) and signature key.
  members(state: ClientState): { index: number; identity: string; signatureKey: Uint8Array }[] {
    const out: { index: number; identity: string; signatureKey: Uint8Array }[] = [];
    const td = new TextDecoder();
    state.ratchetTree.forEach((node, i) => {
      if (!node || node.nodeType !== "leaf") return;
      if (i % 2 !== 0) return; // leaves sit at even node indexes in the array representation
      const leaf = node.leaf;
      const cred = leaf.credential;
      out.push({
        index: i / 2,
        identity: cred.credentialType === "basic" ? td.decode(cred.identity) : "",
        signatureKey: leaf.signaturePublicKey,
      });
    });
    return out;
  }

  serialize(state: ClientState): Uint8Array {
    return encodeGroupState(state);
  }

  restore(bytes: Uint8Array): ClientState {
    const dec = decodeGroupState(bytes, 0);
    if (!dec) throw new Error("mls: stored group state unreadable");
    return { ...dec[0], clientConfig: defaultClientConfig };
  }
}
