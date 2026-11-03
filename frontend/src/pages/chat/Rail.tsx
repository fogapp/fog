// The left rail: this household's rooms by kind, with unread counts;
// channels open to join; the door to a new room. Rooms are the client's
// list, ordered by last activity within each group.

import { useState } from "react";
import { Link } from "react-router-dom";
import type { ChatClient, DirectoryPerson, Room } from "@fog/chat-sdk";
import { paths } from "../../lib/paths";
import { errorText } from "../../components/Async";
import NewRoomModal from "./NewRoomModal";
import { roomGlyph, roomTitle } from "./names";

export default function Rail({ client, directory, selected }: { client: ChatClient; directory: DirectoryPerson[]; selected?: string }) {
  const [creating, setCreating] = useState<null | "channel" | "group" | "dm">(null);
  const [joining, setJoining] = useState("");
  const [error, setError] = useState("");
  const rooms = client.rooms().filter((r) => !r.archivedAt);
  const groups: { title: string; kind: Room["kind"]; rooms: Room[] }[] = [
    { title: "Channels", kind: "channel", rooms: rooms.filter((r) => r.kind === "channel") },
    { title: "Groups", kind: "group", rooms: rooms.filter((r) => r.kind === "group") },
    { title: "Direct", kind: "dm", rooms: rooms.filter((r) => r.kind === "dm") },
  ];
  const available = client.available();

  async function join(id: string) {
    setJoining(id);
    setError("");
    try {
      await client.joinChannel(id);
    } catch (e) {
      setError(errorText(e, "Couldn't join"));
    } finally {
      setJoining("");
    }
  }

  return (
    <nav className="flex h-full flex-col overflow-y-auto px-2 py-3" aria-label="Rooms">
      {groups.map((g) => (
        <div key={g.kind} className="mb-4">
          <div className="flex items-center justify-between px-3 pb-1">
            <span className="group-label">{g.title}</span>
            <button
              onClick={() => setCreating(g.kind)}
              aria-label={`New ${g.kind === "dm" ? "direct message" : g.kind}`}
              title={`New ${g.kind === "dm" ? "direct message" : g.kind}`}
              className="rounded px-1 text-sm leading-none text-ink-muted hover:bg-hover hover:text-ink">
              +
            </button>
          </div>
          {g.rooms.length === 0 && <p className="px-3 py-1 text-hint text-ink-muted">None yet.</p>}
          {g.rooms.map((r) => (
            <RoomLink key={r.id} room={r} title={roomTitle(r, directory, client)} active={r.id === selected} online={client.online()} />
          ))}
        </div>
      ))}

      {available.length > 0 && (
        <div className="mb-4">
          <div className="px-3 pb-1">
            <span className="group-label">Open to join</span>
          </div>
          {available.map((a) => (
            <div key={a.id} className="flex items-center gap-2 rounded-lg px-3 py-1.5 text-cell text-ink-muted">
              <span className="text-ink-muted">#</span>
              <span className="min-w-0 flex-1 truncate">{a.label}</span>
              <button
                onClick={() => join(a.id)}
                disabled={!a.joinable || joining === a.id}
                title={a.joinable ? "Join this channel" : "Nobody has published the room's group info yet"}
                className="rounded-md border border-edge px-2 py-0.5 text-xs text-ink hover:bg-hover disabled:opacity-50">
                {joining === a.id ? "Joining…" : a.joinable ? "Join" : "Not yet"}
              </button>
            </div>
          ))}
        </div>
      )}
      {error && <p className="px-3 text-hint text-warn">{error}</p>}

      {creating && <NewRoomModal client={client} kind={creating} directory={directory} onClose={() => setCreating(null)} />}
    </nav>
  );
}

function RoomLink({ room, title, active, online }: { room: Room; title: string; active: boolean; online: Set<string> }) {
  const anyOnline = room.kind === "dm" && (room.dm ?? []).some((a) => online.has(a));
  return (
    <Link
      to={paths.chat(room.id)}
      className={`flex items-center gap-2 rounded-lg px-3 py-1.5 text-cell transition-colors ${
        active ? "bg-accent/10 text-ink" : room.unread > 0 ? "font-medium text-ink hover:bg-hover" : "text-ink-muted hover:bg-hover hover:text-ink"
      }`}>
      <span className={`w-3 shrink-0 text-center ${anyOnline ? "text-accent" : "text-ink-muted"}`}>{room.kind === "dm" && anyOnline ? "●" : roomGlyph(room.kind)}</span>
      <span className="min-w-0 flex-1 truncate">{title}</span>
      {room.sync !== "ready" && <span title="This device does not hold this room's keys" className="text-ink-muted">◌</span>}
      {room.unread > 0 && (
        <span className="rounded-full bg-accent/15 px-1.5 text-[11px] font-medium tabular-nums text-accent">{room.unread > 99 ? "99+" : room.unread}</span>
      )}
    </Link>
  );
}
