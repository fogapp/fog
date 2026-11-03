// The timeline: a room's messages from the local plaintext store, day by
// day. Every fact shown is one this device decrypted itself; a room whose
// keys this device lacks says so instead of showing an empty scroll.

import { memo, useEffect, useMemo, useRef, useState } from "react";
import type { ChatClient, DirectoryPerson, MessageView, Room } from "@fog/chat-sdk";
import { systemLabel } from "@fog/chat-sdk";
import ChatMarkdown from "./ChatMarkdown";
import { useAction } from "../../lib/useAction";
import AttachmentView from "./AttachmentView";
import { dayOf, nameFor, timeOf } from "./names";
import PreviewCardView from "./PreviewCardView";
import { controlArea } from "../../components/Field";

const QUICK = ["👍", "❤️", "😂", "🎉", "👀"];

// useAction's run, which the rows share with the timeline's error line.
type Run = <R>(work: () => Promise<R>, fallback: string) => Promise<R | undefined>;

// Memoised, with the room passed as the few facts it draws on rather than
// the Room object: the page re-renders on every room's events, and this
// redraws only when its own messages, marks, or names moved.
export default memo(function Timeline({
  client, roomId, sync, canModerate, directory, messages, marks, onOpenThread, threadMode = false, emptyLabel,
}: {
  client: ChatClient;
  roomId: string;
  sync: Room["sync"];
  canModerate: boolean;
  directory: DirectoryPerson[];
  messages: readonly MessageView[];
  // marks are the room's read marks; a thread shows none.
  marks?: ReadonlyMap<string, number>;
  onOpenThread?: (id: string) => void;
  threadMode?: boolean;
  emptyLabel?: string;
}) {
  const bottom = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const { run: act, error } = useAction();
  const [saved, setSaved] = useState<Set<string>>(new Set());
  const [savedVersion, setSavedVersion] = useState(0);
  const last = messages[messages.length - 1]?.id;

  // Saved marks live on this device; the client emits "messages" for the
  // room when one changes. Keyed on that event rather than on the messages
  // array, so the store is read when a mark (or a message) moves and not
  // on every render.
  useEffect(() => {
    const off = client.addEventListener("messages", (d) => {
      if ((d as { roomId?: string } | undefined)?.roomId === roomId) setSavedVersion((v) => v + 1);
    });
    return () => {
      off();
    };
  }, [client, roomId]);
  useEffect(() => {
    let alive = true;
    client.saved().then((rows) => alive && setSaved(new Set(rows.filter((r) => r.roomId === roomId).map((r) => r.messageId)))).catch(() => {});
    return () => {
      alive = false;
    };
  }, [client, roomId, savedVersion]);

  // Stick to the bottom while the reader is near it; a reader who scrolled
  // up to read is left where they are.
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < 200;
    if (near) bottom.current?.scrollIntoView({ block: "end" });
  }, [last, roomId]);
  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [roomId]);

  // Read marks: who has read up to which seq, folded from receipts that
  // travelled sealed inside the room. Shown once, under the last message.
  const lastMsg = messages[messages.length - 1];
  const seenBy = useMemo(() => {
    if (!marks || !lastMsg) return [];
    const me = client.me().addr;
    return Array.from(marks.entries())
      .filter(([addr, seq]) => addr !== me && addr !== lastMsg.sender && seq >= lastMsg.seq)
      .map(([addr]) => nameFor(addr, directory, client));
  }, [client, marks, lastMsg, directory]);

  if (sync !== "ready") {
    return (
      <div className="flex flex-1 items-center justify-center p-8 text-center">
        <div className="max-w-sm space-y-2">
          <p className="text-sm text-ink">
            {sync === "stale" ? "This device is out of sync with the room." : "This device doesn't hold this room's keys yet."}
          </p>
          <p className="text-hint text-ink-muted">
            {sync === "stale"
              ? client.room(roomId)?.staleReason ?? "Something arrived that this device could not apply."
              : client.room(roomId)?.staleReason ?? "Messages here are sealed to the devices in the room. Once your other device or a member adds this device, they will appear."}
          </p>
          {sync === "stale" && <RemoveRefused client={client} roomId={roomId} directory={directory} />}
          {sync === "stale" && <LeaveStale client={client} roomId={roomId} />}
        </div>
      </div>
    );
  }

  let lastDay = "";
  let lastSender = "";
  let lastAt = 0;
  // Fog serving this person a different identity key than their devices
  // hold is said loudly, everywhere: their devices won't add any device it
  // vouches for.
  const ownWarning = client.identityWarnings().find((w) => w.addr === client.me().addr);
  return (
    <div ref={box} className="min-h-0 flex-1 overflow-y-auto">
      {ownWarning && (
        <div role="alert" className="m-3 rounded-md border border-warn/40 bg-warn/10 px-3 py-2 text-cell text-warn">
          ⚠ {ownWarning.text}
        </div>
      )}
      {messages.length === 0 && (
        <p className="px-6 py-10 text-center text-hint text-ink-muted">{emptyLabel ?? "Nothing here yet. Say something."}</p>
      )}
      <ol className="px-4 py-3 sm:px-6">
        {messages.map((m) => {
          const day = dayOf(m.hubAt);
          const showDay = day !== lastDay;
          const at = new Date(m.hubAt).getTime();
          // Consecutive messages from one person within five minutes share
          // a header, the way a conversation reads.
          const grouped = !showDay && m.sender === lastSender && at - lastAt < 5 * 60_000 && !m.system;
          lastDay = day;
          lastSender = m.system === "membership" ? "" : m.sender;
          lastAt = at;
          return (
            <li key={m.id}>
              {showDay && (
                <div className="my-3 flex items-center gap-3">
                  <span className="h-px flex-1 bg-hair" />
                  <span className="text-hint text-ink-muted">{day}</span>
                  <span className="h-px flex-1 bg-hair" />
                </div>
              )}
              {m.system === "identity-reset" ? (
                // The hub said this person's identity was reset: whoever
                // uses their name now holds keys nobody here has verified.
                <div role="alert" className="my-2 rounded-md border border-warn/40 bg-warn/10 px-3 py-2 text-cell text-warn">
                  ⚠ {m.text} <span className="tabular-nums text-ink-faint">{timeOf(m.hubAt)}</span>
                </div>
              ) : m.system === "undecryptable" ? (
                // One message this device couldn't read: that message only.
                <p className="my-1 px-2 text-hint italic text-ink-muted">
                  {m.text} <span className="tabular-nums text-ink-faint">{timeOf(m.hubAt)}</span>
                </p>
              ) : m.system === "membership" ? (
                // A membership line is derived from a commit this device
                // processed: quiet, centered, never a bubble.
                <div className="my-1.5 flex items-center gap-3 px-2">
                  <span className="h-px flex-1 bg-hair/60" />
                  <span className="text-hint text-ink-muted">
                    {m.text} <span className="tabular-nums text-ink-faint">{timeOf(m.hubAt)}</span>
                  </span>
                  <span className="h-px flex-1 bg-hair/60" />
                </div>
              ) : (
              <MessageRow
                m={m}
                client={client}
                roomId={roomId}
                canModerate={canModerate}
                directory={directory}
                grouped={grouped}
                editing={editing === m.id}
                onEdit={setEditing}
                onOpenThread={onOpenThread}
                threadMode={threadMode}
                act={act}
                saved={saved.has(m.id)}
              />
              )}
            </li>
          );
        })}
      </ol>
      {seenBy.length > 0 && (
        <p className="px-6 pb-1 text-right text-hint text-ink-muted" title={seenBy.join(", ")}>
          Seen by {seenBy.length <= 3 ? seenBy.join(", ") : `${seenBy.slice(0, 2).join(", ")} and ${seenBy.length - 2} others`}
        </p>
      )}
      {error && <p className="px-6 pb-2 text-hint text-warn">{error}</p>}
      <div ref={bottom} />
    </div>
  );
});

// One message. Memoised: every prop is a value or a stable function, and
// the client keeps an unchanged message's view object across refolds, so a
// new message, a reaction, or an edit redraws only the rows it touched.
const MessageRow = memo(function MessageRow({
  m, client, roomId, canModerate, directory, grouped, editing, onEdit, onOpenThread, threadMode, act, saved,
}: {
  m: MessageView;
  client: ChatClient;
  roomId: string;
  canModerate: boolean;
  directory: DirectoryPerson[];
  grouped: boolean;
  editing: boolean;
  // onEdit opens this message's editor with its id, and closes it with null.
  onEdit: (id: string | null) => void;
  onOpenThread?: (id: string) => void;
  threadMode: boolean;
  act: Run;
  saved: boolean;
}) {
  const [draft, setDraft] = useState(m.text);
  const [picker, setPicker] = useState(false);
  const me = client.me().addr;
  // Only fog's bot speaks as fog, and an app, webhook or MCP client posting
  // through it is named as itself: the SDK keeps a "system" stamp only from
  // the bot (of this fog or the room's hub), and never from shared history.
  const label = /^fog:[^/]+\/fog$/.test(m.sender) ? systemLabel(m.system) : undefined;
  const via = label && label.via !== "fog" ? label.via : "";
  const asFog = label?.via === "fog";
  const name = label ? label.name : nameFor(m.sender, directory, client);
  const onEditDone = () => onEdit(null);

  return (
    <div className={`group relative flex gap-3 rounded-md px-2 hover:bg-hover ${grouped ? "py-0.5" : "mt-2 py-1"}`}>
      <div className="w-8 shrink-0 pt-0.5 text-right">
        {!grouped && (
          <span className={`inline-flex h-7 w-7 items-center justify-center rounded-md text-xs font-semibold ${asFog ? "bg-accent/15 text-accent" : "bg-fog-800 text-ink"}`}>
            {name.slice(0, 1).toUpperCase()}
          </span>
        )}
      </div>
      <div className="min-w-0 flex-1">
        {!grouped && (
          <div className="flex items-baseline gap-2">
            <span className={`text-sm font-medium ${asFog ? "text-accent" : "text-ink"}`}>{name}</span>
            {via && (
              <span className="rounded border border-hair px-1 text-hint text-ink-muted" title={`Posted through fog by ${via === "app" ? "an app" : via === "webhook" ? "a webhook" : "an MCP client"}, not by a person`}>
                {via === "mcp" ? "MCP" : via}
              </span>
            )}
            <span className="text-hint tabular-nums text-ink-muted">{timeOf(m.hubAt)}</span>
            {m.pinned && <span className="text-hint text-ink-muted" title="Pinned">📌</span>}
            {saved && <span className="text-hint text-ink-muted" title="Saved on this device">🔖</span>}
            {m.sharedBy && (
              <span className="text-hint text-ink-faint" title="Re-sent into the room as history by another member; this device didn't receive it from the sender and can't check it">
                · shared by {nameFor(m.sharedBy, directory, client)}
              </span>
            )}
          </div>
        )}
        {m.deleted ? (
          <p className="text-cell italic text-ink-muted">This message was deleted.</p>
        ) : editing ? (
          <form
            className="mt-1 space-y-1"
            onSubmit={(e) => {
              e.preventDefault();
              act(() => client.edit(roomId, m.id, draft.trim()), "Couldn't save the edit").then(onEditDone);
            }}>
            <textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              autoFocus
              rows={Math.min(8, draft.split("\n").length + 1)}
              onKeyDown={(e) => {
                if (e.key === "Escape") onEditDone();
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  act(() => client.edit(roomId, m.id, draft.trim()), "Couldn't save the edit").then(onEditDone);
                }
              }}
              className={`${controlArea} w-full`}
            />
            <p className="text-hint text-ink-muted">Enter saves · Escape cancels</p>
          </form>
        ) : (
          <div className="text-cell text-ink [&_.space-y-2]:space-y-1">
            {m.text && <ChatMarkdown>{m.text}</ChatMarkdown>}
            {m.edited && <span className="ml-1 text-hint text-ink-muted">(edited)</span>}
          </div>
        )}
        {m.preview && !m.deleted && <PreviewCardView card={m.preview} />}
        {m.attachments.length > 0 && !m.deleted && (
          <div className="mt-1 flex flex-wrap gap-2">
            {m.attachments.map((a) => (
              <AttachmentView key={a.blob} client={client} att={a} />
            ))}
          </div>
        )}
        {(m.reactions.length > 0 || picker) && (
          <div className="mt-1 flex flex-wrap items-center gap-1">
            {m.reactions.map((r) => {
              const mine = r.by.includes(me);
              return (
                <button
                  key={r.emoji}
                  onClick={() => act(() => client.react(roomId, m.id, r.emoji, !mine), "Couldn't react")}
                  title={r.by.map((a) => nameFor(a, directory, client)).join(", ")}
                  className={`rounded-full border px-1.5 py-px text-xs tabular-nums ${mine ? "border-accent/40 bg-accent/10 text-ink" : "border-hair text-ink-muted hover:border-edge"}`}>
                  {r.emoji} {r.by.length}
                </button>
              );
            })}
            {picker && QUICK.map((e) => (
              <button key={e} onClick={() => { setPicker(false); act(() => client.react(roomId, m.id, e, true), "Couldn't react"); }} className="rounded-full border border-hair px-1.5 py-px text-xs hover:border-edge">
                {e}
              </button>
            ))}
          </div>
        )}
        {!threadMode && m.replies > 0 && onOpenThread && (
          <button onClick={() => onOpenThread(m.id)} className="mt-1 text-hint text-accent hover:underline">
            {m.replies} {m.replies === 1 ? "reply" : "replies"}{m.lastReplyAt ? ` · last ${timeOf(m.lastReplyAt)}` : ""}
          </button>
        )}
      </div>

      {!m.deleted && !editing && (
        <div className="absolute -top-3 right-2 hidden items-center gap-0.5 rounded-md border border-hair bg-fog-900 px-1 py-0.5 text-xs shadow group-hover:flex">
          <Act title="React" onClick={() => setPicker((p) => !p)}>☺</Act>
          {!threadMode && onOpenThread && <Act title="Reply in thread" onClick={() => onOpenThread(m.id)}>↳</Act>}
          <Act title={saved ? "Unsave" : "Save"} onClick={() => act(() => (saved ? client.unsave(roomId, m.id) : client.save(roomId, m.id)), "Couldn't save")}>{saved ? "🔖" : "☆"}</Act>
          {m.mine && <Act title="Edit" onClick={() => onEdit(m.id)}>✎</Act>}
          {/* Offered only where the room's policy lets this person do it:
              every member's client ignores a delete or pin their role
              doesn't allow. */}
          {(m.mine || (canModerate && client.can(roomId, "canDeleteOtherMessage"))) && <Act title="Delete" onClick={() => act(() => client.delete(roomId, m.id), "Couldn't delete")}>🗑</Act>}
          {canModerate && client.can(roomId, "canPin") && <Act title={m.pinned ? "Unpin" : "Pin"} onClick={() => act(() => client.pin(roomId, m.id, !m.pinned), "Couldn't pin")}>📌</Act>}
        </div>
      )}
    </div>
  );
});

function Act({ title, onClick, children }: { title: string; onClick: () => void; children: React.ReactNode }) {
  return (
    <button onClick={onClick} title={title} aria-label={title} className="rounded px-1.5 py-0.5 text-ink-muted hover:bg-hover hover:text-ink">
      {children}
    </button>
  );
}

// RemoveRefused offers a member who may remove people the way out of a room
// this device refused a device in: remove that device, which every member's
// device that refused it accepts along with the commit it refused. It names
// whose device it is and asks first; a device since vouched for isn't
// offered.
function RemoveRefused({ client, roomId, directory }: { client: ChatClient; roomId: string; directory: DirectoryPerson[] }) {
  const [refused, setRefused] = useState<{ key: string; identity: string }[]>([]);
  const { run, busy, error } = useAction();
  useEffect(() => {
    let alive = true;
    client.refusedDevices(roomId).then((r) => alive && setRefused(r)).catch(() => {});
    return () => {
      alive = false;
    };
  }, [client, roomId]);
  if (!refused.length || !client.can(roomId, "canRemoveParticipant")) return null;
  const what = refused.map((d) => `${nameFor(d.identity, directory, client)}'s device ${d.key.slice(0, 10)}…`).join(", ");
  return (
    <div className="space-y-1 pt-2">
      <p className="text-hint text-ink-muted">Refused: {what}</p>
      <button
        onClick={() => {
          if (!window.confirm(`Remove ${what} from this room? Nobody's identity key vouches for ${refused.length === 1 ? "it" : "them"}.`)) return;
          run(() => client.removeRefusedDevices(roomId), "Couldn't remove the device");
        }}
        disabled={busy}
        className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-hover disabled:opacity-50">
        Remove {refused.length === 1 ? "this device" : "these devices"}
      </button>
      {error && <p className="text-hint text-warn">{error}</p>}
    </div>
  );
}

// LeaveStale is a stale device's one way out: this device leaves the room
// and drops everything it held for it. You stay in the room, with your role
// and your other devices; this device comes back like any new device of
// yours — your other device or any member adds it — never by rejoining on
// fog's word.
function LeaveStale({ client, roomId }: { client: ChatClient; roomId: string }) {
  const { run, busy, error } = useAction();
  return (
    <div className="space-y-1 pt-2">
      <p className="text-hint text-ink-muted">
        This takes only this device out of the room and drops its keys for it. You stay in the room, with your role and your other
        devices. Messages already here stay. Your other device, or any member, can add this one back.
      </p>
      <button
        onClick={() => run(() => client.leaveDevice(roomId), "Couldn't leave on this device")}
        disabled={busy}
        className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-hover disabled:opacity-50">
        Leave this room on this device
      </button>
      {error && <p className="text-hint text-warn">{error}</p>}
    </div>
  );
}
