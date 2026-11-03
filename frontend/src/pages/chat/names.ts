// Naming people and rooms for the eye. Addresses are fog:<instance>/<handle>;
// the directory maps local handles to display names, and anything we
// cannot resolve falls back to the handle, never to the raw address.

import type { ChatClient, DirectoryPerson, Room } from "@fog/chat-sdk";

export function handleOf(addr: string): string {
  const m = /^fog:[^/]+\/(.+)$/.exec(addr);
  return m ? m[1] : addr;
}

function instanceOf(addr: string): string {
  const m = /^fog:([^/]+)\//.exec(addr);
  return m ? m[1] : "";
}

export function nameFor(addr: string, directory: DirectoryPerson[], client?: ChatClient | null): string {
  const handle = handleOf(addr);
  if (client && instanceOf(addr) && instanceOf(addr) !== client.me().household.instanceId) return handle;
  const p = directory.find((d) => d.handle === handle);
  return p?.displayName || handle;
}

export function roomTitle(room: Room, directory: DirectoryPerson[], client?: ChatClient | null): string {
  if (room.kind === "dm") {
    const names = (room.dm ?? []).map((a) => nameFor(a, directory, client));
    return names.length ? names.join(", ") : "Just you";
  }
  return room.label || (room.kind === "group" ? `Group · ${room.participants}` : "Untitled");
}

export function roomGlyph(kind: Room["kind"]): string {
  return kind === "channel" ? "#" : kind === "dm" ? "@" : "◦";
}

const timeFmt = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });
const dayFmt = new Intl.DateTimeFormat(undefined, { weekday: "long", month: "long", day: "numeric" });

export function timeOf(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : timeFmt.format(d);
}

export function dayOf(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const today = new Date();
  const same = (a: Date, b: Date) => a.toDateString() === b.toDateString();
  if (same(d, today)) return "Today";
  const y = new Date(today);
  y.setDate(today.getDate() - 1);
  if (same(d, y)) return "Yesterday";
  return dayFmt.format(d);
}

export const ROLE_NAMES: Record<number, string> = { 1: "banned", 2: "member", 3: "admin", 4: "owner" };

// identityKeyFor finds a person's identity key: in this household's
// directory, or in a paired household's advertised one.
export function identityKeyFor(addr: string, directory: DirectoryPerson[], client?: ChatClient | null): string | undefined {
  const handle = handleOf(addr);
  const inst = instanceOf(addr);
  if (client && inst && inst !== client.me().household.instanceId) {
    return client.households().find((h) => h.instanceId === inst)?.people.find((p) => p.handle === handle)?.identityKey;
  }
  return directory.find((d) => d.handle === handle)?.identityKey;
}
