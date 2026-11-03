// The Drive context menu: what can be done to the entry under the pointer
// (and the rest of the selection), or to the folder from its empty space.
// What is offered depends on the place: a copy of a peer's share only
// reads, a snapshot only restores, and live Drive does the rest.

import type { FileNode, FilesConflictPolicy } from "../../api";
import type { MenuItem } from "../../components/ContextMenu";
import { rawUrl, zipUrl } from "../../lib/useFiles";
import type { Clipboard } from "./Files";

export interface MenuContext {
  node: FileNode | null;
  selection: FileNode[];
  selectedPaths: string[];
  clipboard: Clipboard | null;
  pinned: Set<string>;
  rel: string;
  at?: number;
  searching: boolean;
  timeTravel: boolean;
  replica: boolean;
  dropBox: boolean;
  peerName?: string;
  open: (n: FileNode) => void;
  quickLook: (n: FileNode) => void;
  duplicate: () => void;
  paste: () => void;
  newFolder: () => void;
  trash: (paths: string[]) => void;
  transfer: (verb: "move" | "copy" | "restore", paths: string[], to: string, policy?: FilesConflictPolicy) => void;
  setClipboard: (c: Clipboard) => void;
  rename: (path: string) => void;
  share: (path: string) => void;
  selectAll: () => void;
  toggleExclude: (n: FileNode, excluded: boolean) => void;
  toggleKeep: (n: FileNode, keep: boolean) => void;
  toggleOffline: (n: FileNode, keep: boolean) => void;
  armReject: (n: FileNode) => void;
  togglePin: (path: string, on: boolean) => void;
}

export function driveMenuItems(c: MenuContext): MenuItem[] {
  const {
    node: n, selection, selectedPaths, clipboard, pinned, rel, at, searching, timeTravel, replica, dropBox, peerName,
    open, quickLook, duplicate, paste, newFolder, trash, transfer, setClipboard, rename, share, selectAll,
    toggleExclude, toggleKeep, toggleOffline, armReject, togglePin,
  } = c;
  const many = selection.length > 1;
  const items: MenuItem[] = [];
  if (replica && !dropBox && !timeTravel) {
    if (n) {
      items.push(
        n.kind === "dir"
          ? { label: "Open", onSelect: () => open(n), shortcut: "↵" }
          : { label: "Quick Look", onSelect: () => quickLook(n), shortcut: "space", disabled: n.kind !== "file" },
        { label: many ? `Download ${selection.length} as zip` : n.kind === "dir" ? "Download as zip" : "Download", onSelect: () => window.location.assign(many || n.kind === "dir" ? zipUrl(selectedPaths) : rawUrl(n.path, true)) },
        { label: "Copy", onSelect: () => setClipboard({ mode: "copy", paths: selectedPaths }), shortcut: "⌘C" },
      );
      if (!many && n.peer?.detail === "previews" && !n.peer.revoked) {
        items.push({ separator: true });
        if (!(n.kind === "file" && n.peer.pinned)) items.push({ label: n.kind === "dir" ? "Keep all offline" : "Keep offline", onSelect: () => toggleOffline(n, true) });
        if (!(n.kind === "file" && n.dehydrated)) items.push({ label: "Free up space", onSelect: () => toggleOffline(n, false) });
      }
      items.push(
        { separator: true },
        { label: `Read-only: a copy of ${peerName}'s share`, disabled: true },
      );
    } else {
      items.push({ label: `Read-only: a copy of ${peerName}'s share`, disabled: true });
    }
    return items;
  }
  if (timeTravel) {
    if (n) {
      items.push(
        n.kind === "dir"
          ? { label: "Open", onSelect: () => open(n), shortcut: "↵" }
          : { label: "Quick Look", onSelect: () => quickLook(n), shortcut: "space", disabled: n.kind !== "file" },
        { label: "Download", onSelect: () => window.location.assign(rawUrl(n.path, true, at)), disabled: n.kind !== "file" },
        { separator: true },
        { label: many ? `Restore ${selection.length} items` : "Restore to where it was", onSelect: () => transfer("restore", selectedPaths, "") },
      );
    } else {
      items.push({ label: "Restore this whole folder", onSelect: () => transfer("restore", [rel], "") });
    }
    return items;
  }
  // Whose is it to change: ours, unless it is a peer's entry (in a drop
  // box) or a peer's contribution here at the origin.
  const theirs = (x: FileNode) => !!x.peer && !x.peer.mine && (x.peer.kind === "contribution" || (replica && x.kind === "file") || (replica && x.kind === "dir" && x.peer.readOnly));
  const anyTheirs = selection.some(theirs);
  if (n) {
    items.push(
      n.kind === "dir"
        ? { label: "Open", onSelect: () => open(n), shortcut: "↵" }
        : { label: "Quick Look", onSelect: () => quickLook(n), shortcut: "space", disabled: n.kind !== "file" },
      {
        label: many ? `Download ${selection.length} as zip` : n.kind === "dir" ? "Download as zip" : "Download",
        onSelect: () => window.location.assign(many || n.kind === "dir" ? zipUrl(selectedPaths) : rawUrl(n.path, true)),
        disabled: n.kind === "other",
      },
      { separator: true },
      { label: "Rename", onSelect: () => rename(n.path), shortcut: "F2", disabled: many || searching || theirs(n) },
      { label: n.peer?.kind === "contribution" ? "Make a copy" : "Duplicate", onSelect: duplicate, shortcut: "⌘D", disabled: searching },
      { label: "Cut", onSelect: () => setClipboard({ mode: "cut", paths: selectedPaths }), shortcut: "⌘X", disabled: anyTheirs },
      { label: "Copy", onSelect: () => setClipboard({ mode: "copy", paths: selectedPaths }), shortcut: "⌘C" },
    );
    if (!many && n.peer?.kind === "contribution") {
      items.push({ separator: true }, { label: n.peer.stub ? "Remove listing…" : "Reject…", onSelect: () => armReject(n), danger: true });
    }
  }
  items.push({ label: clipboard ? `Paste ${clipboard.paths.length} item${clipboard.paths.length === 1 ? "" : "s"}` : "Paste", onSelect: paste, shortcut: "⌘V", disabled: !clipboard || searching });
  items.push({ separator: true }, { label: "New folder", onSelect: newFolder, shortcut: "⇧⌘N", disabled: searching });
  if (n && !many && !searching) {
    items.push({ separator: true });
    if (n.kind !== "other") {
      items.push({ label: pinned.has(n.path) ? "Unpin from sidebar" : "Pin to sidebar", onSelect: () => togglePin(n.path, !pinned.has(n.path)) });
    }
    if (n.kind === "dir" && !n.peer) {
      items.push({ label: "Share with a peer…", onSelect: () => share(n.path), disabled: n.kept });
    }
    if (!n.peer) {
      items.push({
        label: n.kept && !n.keptBy ? "Allow sharing again" : "Keep on this box",
        onSelect: () => toggleKeep(n, !(n.kept && !n.keptBy)),
        disabled: !!n.keptBy,
      });
    }
    items.push({
      label: n.excluded && !n.excludedBy ? "Include in backups" : "Exclude from backups",
      onSelect: () => toggleExclude(n, !(n.excluded && !n.excludedBy)),
      disabled: !!n.excludedBy,
    });
  }
  if (n) items.push({ separator: true }, { label: many ? `Move ${selection.length} items to Trash` : "Move to Trash", onSelect: () => trash(selectedPaths), shortcut: "⌘⌫", danger: true, disabled: anyTheirs });
  else {
    // The folder you are standing in, from its empty space.
    if (rel && !searching) {
      items.push({ label: pinned.has(rel) ? "Unpin this folder" : "Pin this folder to sidebar", onSelect: () => togglePin(rel, !pinned.has(rel)) });
    }
    items.push({ label: "Select all", onSelect: selectAll, shortcut: "⌘A" });
  }
  return items;
}
