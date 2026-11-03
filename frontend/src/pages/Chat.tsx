// Chat: the household's sealed rooms. The page owns the viewport the way
// Files does — rail, timeline, and side pane scroll on their own. Every
// message shown was decrypted on this device; the page never asks the
// server what a message says, because the server does not know.

import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import type { DirectoryPerson, MessageView, RoomDetail } from "@fog/chat-sdk";
import { Loading } from "../components/Async";
import { paths } from "../lib/paths";
import { useChat } from "../lib/chatClient";
import Composer from "./chat/Composer";
import DevicesPanel from "./chat/DevicesPanel";
import InfoPane from "./chat/InfoPane";
import PrefsPanel from "./chat/PrefsPanel";
import Rail from "./chat/Rail";
import RestorePanel from "./chat/RestorePanel";
import InsecurePanel from "./chat/InsecurePanel";
import SavedPane from "./chat/SavedPane";
import SearchBox from "./chat/SearchBox";
import ThreadPane from "./chat/ThreadPane";
import Timeline from "./chat/Timeline";
import Typing from "./chat/Typing";
import { nameFor, roomGlyph, roomTitle } from "./chat/names";

// What a room without readable messages shows: one empty list, so the
// memoised timeline sees the same props from render to render.
const NONE: readonly MessageView[] = [];
const NO_MARKS: ReadonlyMap<string, number> = new Map();

export default function Chat() {
  // Typing is left out: the Typing line listens for it on its own.
  const { client, phase, error, status, otherDevices, identityChanged, insecure } = useChat(["rooms", "messages", "presence"]);
  const { roomId } = useParams();
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const thread = params.get("thread") ?? undefined;
  const [info, setInfo] = useState(false);
  const [saved, setSaved] = useState(false);
  const [devices, setDevices] = useState(false);
  const [prefs, setPrefs] = useState(false);
  const [restore, setRestore] = useState(false);
  // The rail is a drawer below md; on wider screens it is always there.
  const [railOpen, setRailOpen] = useState(false);
  const searchRef = useRef<HTMLInputElement>(null);
  const [directory, setDirectory] = useState<DirectoryPerson[]>([]);
  const [detail, setDetail] = useState<RoomDetail | null>(null);

  // The directory names people; refresh it when the client is ready and
  // whenever a room's membership changes underneath us.
  useEffect(() => {
    if (!client || phase !== "ready") return;
    client.directory().then(setDirectory).catch(() => {});
  }, [client, phase]);

  const room = client && roomId ? client.room(roomId) : undefined;
  const openThread = useCallback((id: string) => setParams({ thread: id }), [setParams]);

  // Room detail (participants, impact) is server data: fetched on entry
  // and again when the room's epoch moves.
  useEffect(() => {
    setDetail(null);
    if (!client || !room) return;
    let alive = true;
    client.roomDetail(room.id).then((d) => alive && setDetail(d)).catch(() => {});
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, room?.id, room?.epoch, room?.participants]);

  // Reading marks the room read, once its messages are on screen.
  const lastSeq = room?.seq;
  useEffect(() => {
    if (!client || !room || room.sync !== "ready") return;
    if (room.unread > 0 && document.visibilityState === "visible") client.markRead(room.id).catch(() => {});
  }, [client, room, lastSeq]);

  // Without a room in the URL, open the first one there is.
  useEffect(() => {
    if (!client || phase !== "ready" || roomId) return;
    const first = client.rooms().find((r) => !r.archivedAt);
    if (first) navigate(paths.chat(first.id), { replace: true });
  }, [client, phase, roomId, navigate]);

  // A new room closes the drawer.
  useEffect(() => {
    setRailOpen(false);
  }, [roomId]);

  // Keys: Shift+F finds (Cmd/Ctrl+K belongs to fog's command palette),
  // Escape closes whatever pane is open, Alt+Up/Down walks the rail.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return;
      const t = e.target as HTMLElement | null;
      const typing = !!t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable);
      if (e.key === "Escape") {
        // A dialog owns its own Escape.
        if (document.querySelector('[role="dialog"]')) return;
        if (typing && t !== searchRef.current) return;
        if (thread) setParams({});
        else if (info || saved || railOpen) {
          setInfo(false);
          setSaved(false);
          setRailOpen(false);
        } else return;
        e.preventDefault();
        return;
      }
      if (e.altKey && (e.key === "ArrowDown" || e.key === "ArrowUp") && client) {
        const rooms = client.rooms().filter((r) => !r.archivedAt);
        if (!rooms.length) return;
        const i = rooms.findIndex((r) => r.id === roomId);
        const down = e.key === "ArrowDown";
        const next = i < 0 ? rooms[down ? 0 : rooms.length - 1] : rooms[(i + (down ? 1 : rooms.length - 1)) % rooms.length];
        e.preventDefault();
        navigate(paths.chat(next.id));
        return;
      }
      if (!typing && e.shiftKey && !e.metaKey && !e.ctrlKey && !e.altKey && e.key.toLowerCase() === "f") {
        e.preventDefault();
        searchRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [client, roomId, thread, info, saved, railOpen, navigate, setParams]);

  if (insecure) {
    return <InsecurePanel />;
  }
  if (!client || phase === "keys" || (phase === "sync" && !client.me())) {
    return (
      <div className="flex h-[calc(100vh-3.5rem)] flex-col items-center justify-center gap-4">
        <Loading label={phase === "error" ? error : "Preparing this device's keys…"} />
        {/* A brand-new device that should be an old one: the lost-phone case. */}
        <button onClick={() => setRestore(true)} className="text-hint text-ink-muted underline hover:text-ink">
          Restore from a recovery phrase instead
        </button>
        {restore && <RestorePanel onClose={() => setRestore(false)} />}
      </div>
    );
  }
  if (phase === "error") {
    return (
      <div className="flex h-[calc(100vh-3.5rem)] flex-col items-center justify-center gap-2 px-6 text-center">
        <p className="text-sm text-warn">Chat could not start on this device.</p>
        <p className="max-w-md text-hint text-ink-muted">{error}</p>
      </div>
    );
  }

  const me = client.me();
  const participants = detail?.participants ?? [];
  const ready = room?.sync === "ready";
  const messages = room && ready ? client.messages(room.id) : NONE;
  const marks = room && ready ? client.readMarks(room.id) : NO_MARKS;
  // Someone in this room has a new identity key since this device pinned
  // the old one: the "verify again" moment, said once, in the header.
  const changed = participants.filter((p) => !p.leftSeq && identityChanged.has(p.addr)).map((p) => nameFor(p.addr, directory, client));
  const waiting = !me.verified && otherDevices > 0;

  const paneOpen = room && (thread || info || saved);

  return (
    <div className="relative flex h-[calc(100vh-3.5rem)] min-h-[24rem]">
      <aside className={`${railOpen ? "absolute inset-y-0 left-0 z-30 block w-64 bg-fog-950 shadow-xl" : "hidden"} shrink-0 border-r border-hair md:static md:block md:w-60 md:shadow-none`}>
        <Rail client={client} directory={directory} selected={roomId} />
      </aside>
      {railOpen && <button className="absolute inset-0 z-20 bg-black/40 md:hidden" onClick={() => setRailOpen(false)} aria-label="Close the room list" />}

      <section className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-11 shrink-0 items-center gap-3 border-b border-hair px-3 sm:px-6">
          <button onClick={() => setRailOpen((v) => !v)} className="rounded px-1.5 py-0.5 text-ink-muted hover:bg-hover hover:text-ink md:hidden" title="Rooms" aria-label="Rooms">
            ☰
          </button>
          {room ? (
            <>
              <span className="text-ink-muted">{roomGlyph(room.kind)}</span>
              <h1 className="min-w-0 truncate text-sm font-medium text-ink">{roomTitle(room, directory, client)}</h1>
              {room.topicLabel && <span className="hidden min-w-0 truncate text-hint text-ink-muted lg:inline">{room.topicLabel}</span>}
              {room.households > 1 && <span className="text-hint text-ink-muted">{room.households} households</span>}
              <span className="ml-auto" />
              <SearchBox client={client} directory={directory} inputRef={searchRef} />
              <button
                onClick={() => setSaved((v) => !v)}
                className={`hidden rounded-md border px-2 py-0.5 text-xs sm:inline ${saved ? "border-accent/40 text-accent" : "border-edge text-ink-muted hover:bg-hover"}`}
                title="Saved messages">
                Saved
              </button>
              <button
                onClick={() => setInfo((v) => !v)}
                className={`rounded-md border px-2 py-0.5 text-xs ${info ? "border-accent/40 text-accent" : "border-edge text-ink-muted hover:bg-hover"}`}
                title="About this room">
                {detail ? `${detail.participants.filter((p) => !p.leftSeq).length} people` : "people"}
              </button>
            </>
          ) : (
            <>
              <h1 className="text-sm font-medium text-ink">Chat</h1>
              <span className="ml-auto" />
              <SearchBox client={client} directory={directory} inputRef={searchRef} />
            </>
          )}
          <button
            onClick={() => setDevices(true)}
            className={`rounded-md border px-2 py-0.5 text-xs ${me.verified ? "border-edge text-ink-muted hover:bg-hover" : "border-warn/40 text-warn hover:bg-warn/10"}`}
            title={me.verified ? "Your devices" : "This device is not yet vouched for by your identity key"}>
            {me.verified ? "Devices" : "Unverified device"}
          </button>
          <button onClick={() => setPrefs(true)} className="rounded-md border border-edge px-2 py-0.5 text-xs text-ink-muted hover:bg-hover" title="Chat preferences" aria-label="Chat preferences">
            ⚙
          </button>
          <span className={`h-1.5 w-1.5 rounded-full ${status === "online" ? "bg-accent" : "bg-fog-600"}`} title={status === "online" ? "Live" : "Reconnecting…"} />
        </header>

        {changed.length > 0 && (
          <p className="border-b border-hair bg-warn/5 px-6 py-1.5 text-hint text-warn">
            {changed.join(", ")} {changed.length === 1 ? "has" : "have"} a new identity — verify them again.
          </p>
        )}
        {waiting && (
          <div className="border-b border-hair px-6 py-3 text-cell text-ink-muted">
            <p className="text-warn">This device isn't verified, so it holds no room keys yet.</p>
            <ul className="mt-1.5 space-y-1 text-hint text-ink-muted">
              <li>
                <span className="text-ink">Approve from another device</span> — open chat there, choose Devices, and approve this one
                (its fingerprint is under Devices here). Rooms appear as soon as it does.
              </li>
              <li>
                <span className="text-ink">Restore from your recovery phrase</span> — if that device is gone.{" "}
                <button onClick={() => setRestore(true)} className="underline hover:text-ink">
                  Enter the twelve words
                </button>
                .
              </li>
            </ul>
          </div>
        )}
        {room ? (
          <>
            <Timeline
              client={client}
              roomId={room.id}
              sync={room.sync}
              canModerate={(room.myRole ?? 0) >= 3}
              directory={directory}
              messages={messages}
              marks={marks}
              onOpenThread={openThread}
            />
            <Typing client={client} roomId={room.id} directory={directory} />
            {room.sync === "ready" && !room.archivedAt && <Composer key={room.id} client={client} roomId={room.id} participants={participants} directory={directory} />}
            {room.archivedAt && <p className="border-t border-hair px-6 py-3 text-hint text-ink-muted">This room is archived. It can be read, not written.</p>}
          </>
        ) : (
          <div className="flex flex-1 items-center justify-center px-6 text-center">
            <div className="max-w-sm space-y-2">
              <p className="text-sm text-ink">{client.rooms().length ? "Pick a room." : "No rooms yet."}</p>
              <p className="text-hint text-ink-muted">
                {client.rooms().length
                  ? "Channels, groups, and direct messages are in the rail."
                  : "Create a channel or a group from the rail, or join one that is open. Everything you say is sealed to the devices in the room."}
              </p>
            </div>
          </div>
        )}
      </section>

      {paneOpen && (
        <aside className="absolute inset-y-0 right-0 z-10 w-full max-w-sm border-l border-hair bg-fog-950 lg:static lg:block lg:w-80 lg:max-w-none xl:w-96">
          {thread ? (
            <ThreadPane
              client={client}
              room={room}
              topicId={thread}
              participants={participants}
              directory={directory}
              onClose={() => setParams({})}
            />
          ) : saved ? (
            <SavedPane client={client} directory={directory} onClose={() => setSaved(false)} />
          ) : (
            <InfoPane
              client={client}
              room={room}
              detail={detail}
              directory={directory}
              onClose={() => setInfo(false)}
              onLeft={() => {
                setInfo(false);
                navigate(paths.chat());
              }}
            />
          )}
        </aside>
      )}

      {prefs && <PrefsPanel client={client} onClose={() => setPrefs(false)} />}
      {devices && <DevicesPanel client={client} onClose={() => setDevices(false)} onRestore={() => { setDevices(false); setRestore(true); }} />}
      {restore && <RestorePanel onClose={() => setRestore(false)} />}
    </div>
  );
}
