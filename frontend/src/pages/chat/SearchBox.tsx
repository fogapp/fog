// Search over what this device has decrypted — there is nothing else to
// search. Results jump to the room (and thread) they live in.

import { useState, type RefObject } from "react";
import { useNavigate } from "react-router-dom";
import type { ChatClient, DirectoryPerson } from "@fog/chat-sdk";
import { paths } from "../../lib/paths";
import { nameFor, roomTitle, timeOf } from "./names";
import { control } from "../../components/Field";

export default function SearchBox({ client, directory, inputRef }: { client: ChatClient; directory: DirectoryPerson[]; inputRef?: RefObject<HTMLInputElement | null> }) {
  const [q, setQ] = useState("");
  const navigate = useNavigate();
  const hits = q.trim().length >= 2 ? client.search(q, 30) : [];
  return (
    <div className="relative">
      <input
        ref={inputRef}
        value={q}
        onChange={(e) => setQ(e.target.value)}
        placeholder="Search messages · Shift+F"
        aria-label="Search messages"
        className={`${control} w-44 sm:w-56`}
        onKeyDown={(e) => {
          if (e.key === "Escape") {
            if (q) {
              e.preventDefault();
              setQ("");
            } else (e.target as HTMLInputElement).blur();
          }
        }}
      />
      {q.trim().length >= 2 && (
        <div className="absolute right-0 top-full z-20 mt-1 max-h-96 w-80 overflow-y-auto rounded-md border border-veil-edge bg-fog-900 shadow-xl">
          {hits.length === 0 && <p className="px-3 py-2 text-hint text-ink-muted">Nothing on this device matches.</p>}
          {hits.map((h) => {
            const room = client.room(h.roomId);
            return (
              <button
                key={`${h.roomId}/${h.id}`}
                onClick={() => {
                  setQ("");
                  navigate(paths.chat(h.roomId, { thread: h.topicId && h.topicId !== h.id ? h.topicId : undefined }));
                }}
                className="block w-full border-b border-divider px-3 py-2 text-left hover:bg-hover">
                <div className="flex items-baseline gap-2 text-xs text-ink-muted">
                  <span className="text-ink-muted">{room ? roomTitle(room, directory, client) : "room"}</span>
                  <span>{nameFor(h.sender, directory, client)}</span>
                  <span className="ml-auto tabular-nums">{timeOf(h.hubAt)}</span>
                </div>
                <div className="truncate text-cell text-ink">{h.text}</div>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
