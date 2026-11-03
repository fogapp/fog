// Where a volume lives, as one value the pickers agree on. A spec says it
// three ways — nothing (Fog-managed), a folder in Drive, a host path — and
// the picker offers five: managed, a Files folder, a peer's copy inside
// Files, a NAS share, an arbitrary host path. This module is the
// translation, so the spec editor and the install pane can never disagree
// about what a given spec means or how a given choice is written back.

import type { FileFedSubscription, NasShare } from "../api";

// The two reserved folders in Drive, the same names as files.AppsDir and
// files.ContentDir on the server. Apps holds one folder per app — the data
// that app owns; Content holds the household's own libraries, which more
// than one app reads and no single app owns.
export const APPS_DIR = "Apps";
export const CONTENT_DIR = "Content";

export type Placement =
  | { kind: "managed" }
  | { kind: "files"; rel: string }
  | { kind: "peer"; rel: string; sub: FileFedSubscription }
  | { kind: "nas"; share: NasShare; sub: string }
  | { kind: "host"; path: string };

export interface PlacementContext {
  // "" while Files is unusable or its status hasn't landed.
  filesRoot: string;
  shares: NasShare[];
  peerSubs: FileFedSubscription[];
}

// The placement-bearing part of a volume, as the spec editor drafts it.
export interface Located {
  host: string;
  files: string;
  readOnly: boolean;
}

const under = (p: string, dir: string) => p === dir || p.startsWith(dir + "/");

function peerFor(rel: string, subs: FileFedSubscription[]): FileFedSubscription | undefined {
  return subs.find((s) => under(rel, s.folder));
}

// placementOf reads a volume. A host path that happens to sit inside the
// Files root is read as a Files folder too — specs written before volumes
// could name one directly — and is rewritten as one on the next edit.
export function placementOf(v: { host?: string; files?: string }, ctx: PlacementContext): Placement {
  const files = v.files ?? "";
  const host = v.host ?? "";
  if (files) {
    const sub = peerFor(files, ctx.peerSubs);
    return sub ? { kind: "peer", rel: files, sub } : { kind: "files", rel: files };
  }
  if (!host) return { kind: "managed" };
  const share = ctx.shares.find((s) => under(host, s.mountPoint));
  if (share) return { kind: "nas", share, sub: host.slice(share.mountPoint.length).replace(/^\//, "") };
  if (ctx.filesRoot && under(host, ctx.filesRoot)) {
    const rel = host.slice(ctx.filesRoot.length).replace(/^\//, "");
    const sub = peerFor(rel, ctx.peerSubs);
    return sub ? { kind: "peer", rel, sub } : { kind: "files", rel };
  }
  return { kind: "host", path: host };
}

// applyPlacement writes a choice back. Only the fields a placement owns
// change; a peer's copy is always read-only, whatever the box said.
export function applyPlacement<T extends Located>(v: T, p: Placement): T {
  switch (p.kind) {
    case "managed":
      return { ...v, host: "", files: "" };
    case "files":
      return { ...v, host: "", files: p.rel.replace(/^\/+/, "") };
    case "peer":
      return { ...v, host: "", files: p.rel, readOnly: true };
    case "nas":
      return { ...v, files: "", host: p.sub ? `${p.share.mountPoint}/${p.sub}` : p.share.mountPoint };
    case "host":
      return { ...v, files: "", host: p.path };
  }
}

// The breadcrumb a Drive folder is shown as: Drive › Content › Books.
export function filesCrumbs(rel: string): string[] {
  return ["Drive", ...rel.split("/").filter(Boolean)];
}

export function placementLabel(p: Placement): string {
  switch (p.kind) {
    case "managed":
      return "Fog-managed";
    case "files":
      return filesCrumbs(p.rel).join(" › ");
    case "peer":
      return `${p.sub.peer ?? "peer"}'s ${p.sub.title} (read-only copy)`;
    case "nas":
      return `${p.share.name}${p.sub ? " › " + p.sub.split("/").join(" › ") : ""} (NAS)`;
    case "host":
      return p.path;
  }
}

// A name as Drive spells a folder: first letter up, the rest as typed.
function folderName(s: string, fallback: string): string {
  const n = s.trim();
  return n ? n[0].toUpperCase() + n.slice(1) : fallback;
}

// The folder in Apps that belongs to one app: Apps/<App>, the app's name
// with its first letter up ("paperless" → Apps/Paperless). It is what fog
// would call the folder; a folder already there under another spelling wins
// over this, which is why the picker resolves it against the real listing.
// The same rule as apps.AppDataFolder on the server.
export function appFolderFor(app: string): string {
  return `${APPS_DIR}/${folderName(app, "App")}`;
}

// Where one of an app's own volumes goes when nothing says otherwise:
// Apps/<App>/<volume>. The server fills the same path in at create time
// (apps.PlaceAppData); this is the install pane saying so beforehand.
export function appDataFolder(app: string, volumeName: string): string {
  return `${appFolderFor(app)}/${volumeName.trim() || "data"}`;
}

// The folder fog would give a content library that names none:
// Content/<Name>, the volume's name with its first letter up ("music" →
// Content/Music).
export function defaultContentFolder(volumeName: string): string {
  return `${CONTENT_DIR}/${folderName(volumeName, "Library")}`;
}

// Which half of Drive a folder is in — what the install pane and the Drive
// tab badge each mount with. "app" is the app's own data, "content" a
// shared library, "drive" anywhere else the operator has pointed it.
export type DriveArea = "app" | "content" | "drive";

export function driveAreaOf(rel: string): DriveArea {
  const segs = rel.split("/").filter(Boolean);
  if (segs[0] === APPS_DIR) return "app";
  if (segs[0] === CONTENT_DIR) return "content";
  return "drive";
}

export const AREA_LABELS: Record<DriveArea, string> = {
  app: "App data",
  content: "Content",
  drive: "In Drive",
};

export const AREA_WHY: Record<DriveArea, string> = {
  app: "This app's own data — kept in its folder under Apps, and in its backups.",
  content: "A shared library under Content — other apps can read the same folder, and it is covered by Drive's nightly snapshot rather than this app's.",
  drive: "A folder elsewhere in Drive — covered by Drive's nightly snapshot rather than this app's backups.",
};

// The folder a Drive path sits directly under, when that folder is one of
// the two reserved ones' children: Content/Media for Content/Media/Films,
// Apps/Paperless for Apps/Paperless/config. "" for anything else. One
// Content/Media across the arr stack is the point, so the folder a volume
// already names is worth offering subfolders of.
export function driveLibraryOf(rel: string): string {
  const segs = rel.split("/").filter(Boolean);
  if (segs.length < 2) return "";
  return segs[0] === APPS_DIR || segs[0] === CONTENT_DIR ? `${segs[0]}/${segs[1]}` : "";
}

// The install pane keeps option values as strings; a placement travels as
// "files:<rel>" or "host:<abs>". "" means "as the template has it".
export function encodePlacement(p: Placement): string {
  switch (p.kind) {
    case "managed":
      return "";
    case "files":
    case "peer":
      return `files:${p.rel}`;
    case "nas":
    case "host": {
      const host = p.kind === "nas" ? (p.sub ? `${p.share.mountPoint}/${p.sub}` : p.share.mountPoint) : p.path;
      return `host:${host}`;
    }
  }
}

export function decodePlacement(value: string, ctx: PlacementContext): Placement {
  if (value.startsWith("files:")) return placementOf({ files: value.slice(6) }, ctx);
  if (value.startsWith("host:")) return placementOf({ host: value.slice(5) }, ctx);
  return { kind: "managed" };
}
