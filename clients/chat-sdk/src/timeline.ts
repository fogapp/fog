// The timeline fold: a room's raw decrypted items — messages, edits,
// deletes, reactions, pins, receipts — rendered into what a reader sees.
// Pure, so the client can fold once per change and serve every read
// (the room, each thread, search) from the result.

import type { Message, MessageView } from "./client.ts";

// foldTimeline renders every message in the room, thread replies included,
// in seq order: edits applied, deletes hidden, reactions and pins folded in,
// thread replies counted onto their roots.
//
// The room's content rules hold here. Only the author's edit applies. A
// delete applies when its sender wrote the message or held the right to
// delete others' (canDeleteOtherMessage) when it arrived; a pin when they
// held canPin; any act their role didn't allow (denied) never applies.
// History another member re-sent (sharedBy) is their word, not the sender's
// signature: it fills in messages this device never saw but cannot edit,
// delete, pin or react to anything the device received itself. Items stored
// before clients checked them (no `checked`) fold as they always did.
//
// Lines this client made itself — membership from a commit it verified, an
// identity reset the hub announced — are nobody's message: no edit, delete,
// pin or reaction applies to them, whoever sends it.
export function foldTimeline(roomId: string, items: Iterable<Message>, nameFor: (addr: string) => string): MessageView[] {
  const raw = Array.from(items).sort((a, b) => a.seq - b.seq);
  const views = new Map<string, MessageView>();
  // Every act on a message, in seq order, by target id.
  const acts = new Map<string, Message[]>();
  const act = (target: string | undefined, m: Message) => {
    if (!target || m.denied) return;
    let list = acts.get(target);
    if (!list) acts.set(target, (list = []));
    list.push(m);
  };
  const replies = new Map<string, { n: number; last: string }>();
  for (const m of raw) {
    const c = m.content;
    switch (c.type) {
      case "membership": {
        const who = (x: { identity: string }) => nameFor(x.identity);
        const parts: string[] = [];
        if (c.membership?.added.length) parts.push(`${c.membership.added.map(who).join(", ")} joined`);
        if (c.membership?.removed.length) parts.push(`${c.membership.removed.map(who).join(", ")} left`);
        views.set(m.id, {
          id: m.id, roomId, seq: m.seq, sender: m.sender, sentAt: m.sentAt, hubAt: m.hubAt,
          text: parts.join("; "), edited: false, deleted: false, mentions: [], attachments: [], reactions: [], pinned: false, replies: 0,
          system: "membership", mine: false,
        });
        break;
      }
      case "message": {
        // A line this client wrote itself for something the hub said (an
        // identity reset): rendered here so names stay current.
        const text = c.system === "identity-reset"
          ? `${nameFor(m.sender)}'s identity was reset, or they were removed, by their household's operator. Their old devices are out of this room; anyone using their name now holds new keys that nobody here has verified. Compare safety codes in person before trusting them again.`
          : c.system === "undecryptable"
            ? "A message here couldn't be decrypted on this device."
            : c.body?.text ?? "";
        views.set(m.id, {
          id: m.id, roomId, seq: m.seq, sender: m.sender, sentAt: m.sentAt, hubAt: m.hubAt,
          text, edited: false, deleted: false, topicId: c.topicId, inReplyTo: c.inReplyTo,
          mentions: c.mentions ?? [], attachments: c.attachments ?? [], reactions: [], pinned: false, replies: 0, system: c.system, mine: m.mine, preview: c.preview,
          ...(m.sharedBy ? { sharedBy: m.sharedBy } : {}),
        });
        if (c.topicId && c.topicId !== m.id) {
          const r = replies.get(c.topicId) ?? { n: 0, last: m.hubAt };
          r.n++;
          r.last = m.hubAt;
          replies.set(c.topicId, r);
        }
        break;
      }
      case "edit":
      case "delete":
        act(c.replaces, m);
        break;
      case "reaction":
        act(c.reaction?.target, m);
        break;
      case "pin":
        act(c.pin?.target, m);
        break;
      case "receipt":
      case "history":
        break;
    }
  }
  const out: MessageView[] = [];
  for (const v of views.values()) {
    // An act from shared history reaches only shared history; nothing
    // reaches a line this client made.
    const list = clientMade(v) ? [] : (acts.get(v.id) ?? []).filter((a) => !a.sharedBy || !!v.sharedBy);
    let edit: Message | undefined;
    let deleted = false;
    let pinned: boolean | undefined;
    const reactions = new Map<string, Set<string>>();
    for (const a of list) {
      const c = a.content;
      switch (c.type) {
        case "edit":
          // Only the author may edit; anyone else's "edit" is ignored.
          if (a.sender === v.sender) edit = a;
          break;
        case "delete":
          if (a.sender === v.sender || !a.checked || a.moderator) deleted = true;
          break;
        case "pin":
          if (c.pin && (!a.checked || a.moderator)) pinned = c.pin.pinned;
          break;
        case "reaction": {
          if (!c.reaction) break;
          let who = reactions.get(c.reaction.emoji);
          if (!who) reactions.set(c.reaction.emoji, (who = new Set()));
          if (c.reaction.add) who.add(a.sender);
          else who.delete(a.sender);
          break;
        }
      }
    }
    if (edit) {
      v.text = edit.content.body?.text ?? v.text;
      v.attachments = edit.content.attachments ?? v.attachments;
      v.edited = true;
    }
    if (deleted) {
      v.deleted = true;
      v.text = "";
      v.attachments = [];
    }
    if (reactions.size) v.reactions = Array.from(reactions.entries()).filter(([, who]) => who.size).map(([emoji, who]) => ({ emoji, by: Array.from(who) }));
    v.pinned = pinned ?? false;
    const r = replies.get(v.id);
    if (r) {
      v.replies = r.n;
      v.lastReplyAt = r.last;
    }
    out.push(v);
  }
  return out.sort((a, b) => a.seq - b.seq);
}

// clientMade says a line is this client's own, never a member's message.
// Member content can't take its type ("membership" is refused at decode),
// its system stamp (sanitize) or its id namespace (only ULIDs, or the
// event's "event:" fallback).
function clientMade(v: { id: string; system?: string }): boolean {
  return v.system === "membership" || v.system === "identity-reset" || v.system === "undecryptable" || v.id.startsWith("membership:") || v.id.startsWith("identity-reset:") || v.id.startsWith("undecryptable:");
}

// sameView says two folds of one message would render the same: every
// field equal, arrays and plain records compared by their contents. Bytes
// and other objects compare by identity — they come from the stored
// content, which a refold reuses rather than copies.
export function sameView(a: MessageView, b: MessageView): boolean {
  return same(a, b);
}

function same(x: unknown, y: unknown): boolean {
  if (x === y) return true;
  if (Array.isArray(x)) return Array.isArray(y) && x.length === y.length && x.every((v, i) => same(v, y[i]));
  if (!plain(x) || !plain(y)) return false;
  const kx = Object.keys(x);
  if (kx.length !== Object.keys(y).length) return false;
  return kx.every((k) => k in y && same(x[k], y[k]));
}

function plain(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && Object.getPrototypeOf(v) === Object.prototype;
}

// isReply: a message inside a thread, not the thread's root.
export function isReply(v: MessageView): boolean {
  return !!v.topicId && v.topicId !== v.id;
}

// inThread: the root itself, or one of its replies.
export function inThread(v: MessageView, topicId: string): boolean {
  return v.id === topicId || v.topicId === topicId;
}
