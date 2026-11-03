// Saved messages: the ones this person kept, on this device. A mark is a
// local fact — it is never sent anywhere — so the list is exactly what this
// device has decrypted and starred.

import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import type { ChatClient, DirectoryPerson, MessageView } from "@fog/chat-sdk";
import { paths } from "../../lib/paths";
import { nameFor, roomTitle, timeOf } from "./names";

export default function SavedPane({ client, directory, onClose }: { client: ChatClient; directory: DirectoryPerson[]; onClose: () => void }) {
  const [rows, setRows] = useState<{ roomId: string; messageId: string; at: string }[] | null>(null);
  const navigate = useNavigate();
  const [version, setVersion] = useState(0);

  useEffect(() => {
    const off = client.addEventListener("messages", () => setVersion((v) => v + 1));
    return () => {
      off();
    };
  }, [client]);

  useEffect(() => {
    let alive = true;
    client.saved().then((r) => alive && setRows(r)).catch(() => alive && setRows([]));
    return () => {
      alive = false;
    };
  }, [client, version]);

  return (
    <div className="flex h-full flex-col overflow-y-auto">
      <div className="flex h-11 shrink-0 items-center justify-between border-b border-hair px-4">
        <span className="text-sm font-medium text-ink">Saved</span>
        <button onClick={onClose} className="rounded px-1 text-ink-muted hover:text-ink" title="Close" aria-label="Close">
          ✕
        </button>
      </div>
      <div className="px-2 py-2">
        {rows === null && <p className="px-2 text-hint text-ink-muted">Loading…</p>}
        {rows && rows.length === 0 && <p className="px-2 py-6 text-center text-hint text-ink-muted">Nothing saved yet. Hover a message and choose Save.</p>}
        {rows?.map((r) => {
          const room = client.room(r.roomId);
          const m: MessageView | undefined = room && room.sync === "ready" ? client.messages(r.roomId).find((x) => x.id === r.messageId) ?? threadMessage(client, r.roomId, r.messageId) : undefined;
          return (
            <div key={`${r.roomId}/${r.messageId}`} className="group rounded-md px-2 py-2 hover:bg-hover">
              <button
                onClick={() => navigate(paths.chat(r.roomId, { thread: m?.topicId && m.topicId !== m.id ? m.topicId : undefined }))}
                className="block w-full text-left">
                <div className="flex items-baseline gap-2 text-xs text-ink-muted">
                  <span className="text-ink-muted">{room ? roomTitle(room, directory, client) : "a room this device no longer holds"}</span>
                  {m && <span>{nameFor(m.sender, directory, client)}</span>}
                  {m && <span className="ml-auto tabular-nums">{timeOf(m.hubAt)}</span>}
                </div>
                <div className="line-clamp-3 text-cell text-ink">{m ? m.text || (m.attachments.length ? `📎 ${m.attachments.map((a) => a.name).join(", ")}` : "(no text)") : "Message not on this device."}</div>
              </button>
              <button
                onClick={() => client.unsave(r.roomId, r.messageId)}
                className="mt-1 hidden text-hint text-ink-muted hover:text-ink group-hover:inline">
                Unsave
              </button>
            </div>
          );
        })}
      </div>
    </div>
  );
}

// threadMessage finds a saved reply, which lives under its thread rather
// than in the room's top-level list.
function threadMessage(client: ChatClient, roomId: string, id: string): MessageView | undefined {
  for (const top of client.messages(roomId)) {
    if (top.replies > 0) {
      const hit = client.messages(roomId, { topicId: top.id }).find((x) => x.id === id);
      if (hit) return hit;
    }
  }
  return undefined;
}
