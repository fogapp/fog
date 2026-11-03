// The entries themselves, as a list or a grid, windowed so size never
// matters, with the keyboard model of a desktop file manager: arrows move,
// shift extends, Enter opens, space previews, Backspace goes up, typing
// jumps. The container owns focus so those keys work the moment the page
// opens, and every row is also a plain click target.

import { useEffect, useLayoutEffect, useMemo, useRef, useState, type DragEvent, type KeyboardEvent, type MouseEvent } from "react";
import type { FileNode, FilesSort } from "../../api";
import { Spinner } from "../../components/Async";
import { DRIVE_GUTTER, driveGutterPx } from "./gutter";
import FileIcon from "../../components/FileIcon";
import PlayBadge from "../../components/PlayBadge";
import { categoryOf, kindLabel, type FileCategory } from "../../lib/fileKinds";
import { formatBytes, formatWhen } from "../../lib/format";
import { rawUrl, thumbUrl } from "../../lib/useFiles";
import { useScrollBox, windowRows } from "./useVirtualRows";
import type { Selection } from "./useSelection";
import type { FilesView } from "./viewPrefs";
import { displayName, FileName } from "../../lib/displayName";

const ROW = 34;
const CELL_W = 136;
const CELL_H = 124;

export interface EntryListProps {
  entries: FileNode[];
  view: FilesView;
  sel: Selection;
  sort: FilesSort;
  desc: boolean;
  onSort: (sort: FilesSort, desc: boolean) => void;
  onOpen: (n: FileNode) => void;
  onUp: () => void;
  onQuickLook: (i: number) => void;
  // Search results show where each hit lives instead of its kind.
  showPath?: boolean;
  // Take keyboard focus on mount: the list is what the keys drive, and it
  // remounts on every folder change (so Enter, Enter, Backspace works
  // without a click). Off while a search box is being typed into.
  autoFocus?: boolean;
  hasMore?: boolean;
  loadingMore?: boolean;
  onLoadMore?: () => void;
  onContextMenu?: (e: MouseEvent, n: FileNode | null) => void;
  // Inline rename: the path being renamed, and what to do with the result.
  renaming?: string | null;
  onRenameSubmit?: (path: string, name: string) => void;
  onRenameCancel?: () => void;
  // Entries on the clipboard as a cut show dimmed until pasted.
  cutPaths?: Set<string>;
  // Rows dropped onto a folder row (alt/option held = copy).
  onDropPaths?: (paths: string[], folder: string, copy: boolean) => void;
  // Files (or folders) dragged in from the desktop, dropped on the folder
  // being viewed or on a folder row.
  onExternalDrop?: (dt: DataTransfer, folder: string) => void;
  currentDir?: string;
  // Browsing a snapshot: no drags, no renames, thumbnails from it.
  at?: number;
  readOnly?: boolean;
  onDelete?: () => void;
  onRename?: () => void;
  onCopy?: () => void;
  onCut?: () => void;
  onPaste?: () => void;
  onNewFolder?: () => void;
  onDuplicate?: () => void;
}

export default function EntryList(p: EntryListProps) {
  const { entries, view, sel } = p;
  const grid = view === "grid";
  const rowHeight = grid ? CELL_H : ROW;
  const box = useScrollBox();
  // The grid's side padding is Drive's gutter, so tiles start where the
  // toolbar and the trail do; the column count has to know it in pixels.
  const gutter = driveGutterPx();
  const cols = grid ? Math.max(1, Math.floor((box.width - 2 * gutter) / CELL_W)) : 1;
  const rowCount = grid ? Math.ceil(entries.length / cols) : entries.length;
  const { start, end, topPad, bottomPad } = windowRows(box.scrollTop, box.height, rowCount, rowHeight);

  const { hasMore, loadingMore, onLoadMore } = p;
  useEffect(() => {
    if (hasMore && !loadingMore && end >= rowCount - 6) onLoadMore?.();
  }, [end, rowCount, hasMore, loadingMore, onLoadMore]);

  // Layout effect, not effect: the focus must land before the browser
  // paints, or a key pressed right after a folder opens falls on the body.
  const { autoFocus } = p;
  useLayoutEffect(() => {
    if (autoFocus) box.ref.current?.focus({ preventScroll: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const { scrollToRow } = box;
  useEffect(() => {
    if (sel.focus >= 0) scrollToRow(grid ? Math.floor(sel.focus / cols) : sel.focus, rowHeight, grid ? 0 : ROW);
  }, [sel.focus, grid, cols, rowHeight, scrollToRow]);

  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    const t = e.target as HTMLElement;
    if (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable) return;
    const meta = e.metaKey || e.ctrlKey;
    const k = e.key;
    if (k === "ArrowDown" || k === "ArrowUp") {
      e.preventDefault();
      if (meta && k === "ArrowUp") return p.onUp();
      if (meta && k === "ArrowDown" && sel.focus >= 0) return p.onOpen(entries[sel.focus]);
      sel.move((k === "ArrowDown" ? 1 : -1) * (grid ? cols : 1), e.shiftKey);
    } else if (grid && (k === "ArrowLeft" || k === "ArrowRight")) {
      e.preventDefault();
      sel.move(k === "ArrowRight" ? 1 : -1, e.shiftKey);
    } else if (k === "Home" || k === "End") {
      e.preventDefault();
      sel.jump(k === "Home" ? 0 : entries.length - 1, e.shiftKey);
    } else if (k === "Enter") {
      e.preventDefault();
      if (sel.focus >= 0 && sel.selected.size <= 1) p.onOpen(entries[sel.focus]);
      else if (sel.focus >= 0) p.onRename?.();
    } else if (k === " ") {
      e.preventDefault();
      e.stopPropagation();
      if (sel.focus >= 0) p.onQuickLook(sel.focus);
    } else if (k === "Backspace" || k === "Delete") {
      e.preventDefault();
      if (meta || k === "Delete") {
        if (sel.selected.size > 0) p.onDelete?.();
      } else if (sel.selected.size === 0) {
        p.onUp();
      } else {
        // Plain Backspace with a selection: the Finder gesture is ⌘⌫, but
        // people press Backspace too; treat it as delete when verbs exist.
        p.onDelete?.();
      }
    } else if (k === "F2") {
      e.preventDefault();
      p.onRename?.();
    } else if (meta && k.toLowerCase() === "a") {
      e.preventDefault();
      sel.selectAll();
    } else if (meta && k.toLowerCase() === "c") {
      if (sel.selected.size > 0) {
        e.preventDefault();
        p.onCopy?.();
      }
    } else if (meta && k.toLowerCase() === "x") {
      if (sel.selected.size > 0) {
        e.preventDefault();
        p.onCut?.();
      }
    } else if (meta && k.toLowerCase() === "v") {
      e.preventDefault();
      p.onPaste?.();
    } else if (meta && k.toLowerCase() === "d") {
      e.preventDefault();
      p.onDuplicate?.();
    } else if (meta && e.shiftKey && k.toLowerCase() === "n") {
      e.preventDefault();
      p.onNewFolder?.();
    } else if (k === "Escape") {
      sel.clear();
    } else if (!meta && !e.altKey && k.length === 1) {
      e.preventDefault();
      sel.typeAhead(k);
    }
  }

  const rows = useMemo(() => {
    const out: number[] = [];
    for (let i = start; i < end; i++) out.push(i);
    return out;
  }, [start, end]);

  const [dropping, setDropping] = useState(false);

  return (
    <div
      ref={box.ref}
      tabIndex={0}
      role="listbox"
      aria-multiselectable
      aria-label="Drive"
      onKeyDown={onKeyDown}
      onClick={(e) => {
        if (e.target === e.currentTarget) sel.clear();
      }}
      onContextMenu={(e) => {
        if (e.target === e.currentTarget) p.onContextMenu?.(e, null);
      }}
      onDragOver={(e) => {
        if (!hasExternalFiles(e)) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = "copy";
        setDropping(true);
      }}
      onDragLeave={(e) => {
        if (e.target === e.currentTarget) setDropping(false);
      }}
      onDrop={(e) => {
        setDropping(false);
        if (hasExternalFiles(e) && e.dataTransfer.files.length > 0) {
          e.preventDefault();
          p.onExternalDrop?.(e.dataTransfer, p.currentDir ?? "");
        }
      }}
      className={`min-h-0 flex-1 overflow-y-auto outline-none focus-visible:ring-1 focus-visible:ring-fog-500 ${
        dropping ? "ring-2 ring-inset ring-accent" : ""
      }`}
    >
      {!grid && <Header sort={p.sort} desc={p.desc} onSort={p.onSort} showPath={!!p.showPath} />}
      <div style={{ height: topPad }} />
      {grid
        ? rows.map((r) => (
            <div key={r} className="grid gap-1" style={{ height: CELL_H, paddingLeft: gutter, paddingRight: gutter, gridTemplateColumns: `repeat(${cols}, ${CELL_W}px)` }}>
              {entries.slice(r * cols, r * cols + cols).map((n, j) => (
                <GridCell key={n.path} n={n} i={r * cols + j} p={p} />
              ))}
            </div>
          ))
        : rows.map((i) => <ListRow key={entries[i].path} n={entries[i]} i={i} p={p} />)}
      <div style={{ height: bottomPad }} />
      {p.loadingMore && (
        <div className={`flex items-center gap-2 py-2 text-xs text-ink-muted ${DRIVE_GUTTER}`}>
          <Spinner /> Loading more…
        </div>
      )}
    </div>
  );
}

const DRAG_TYPE = "application/x-fog-paths";

function dragPaths(e: DragEvent): string[] | null {
  if (!e.dataTransfer.types.includes(DRAG_TYPE)) return null;
  try {
    return JSON.parse(e.dataTransfer.getData(DRAG_TYPE)) as string[];
  } catch {
    return null;
  }
}

function hasExternalFiles(e: DragEvent): boolean {
  return e.dataTransfer.types.includes("Files") && !e.dataTransfer.types.includes(DRAG_TYPE);
}

// startDrag puts the selection (or the dragged row, if it was outside the
// selection) on the data transfer.
function startDrag(e: DragEvent, n: FileNode, p: EntryListProps) {
  const paths = p.sel.isSelected(n.path) ? [...p.sel.selected] : [n.path];
  if (!p.sel.isSelected(n.path)) p.sel.set([n.path]);
  e.dataTransfer.setData(DRAG_TYPE, JSON.stringify(paths));
  e.dataTransfer.effectAllowed = "copyMove";
}

// folderDropProps makes a folder row a drop target for rows and files.
function folderDropProps(n: FileNode, p: EntryListProps, setOver: (b: boolean) => void) {
  if (n.kind !== "dir") return {};
  return {
    onDragOver: (e: DragEvent) => {
      const internal = e.dataTransfer.types.includes(DRAG_TYPE);
      if (!internal && !hasExternalFiles(e)) return;
      e.preventDefault();
      e.stopPropagation();
      e.dataTransfer.dropEffect = internal ? (e.altKey ? "copy" : "move") : "copy";
      setOver(true);
    },
    onDragLeave: () => setOver(false),
    onDrop: (e: DragEvent) => {
      e.preventDefault();
      e.stopPropagation();
      setOver(false);
      const paths = dragPaths(e);
      if (paths) {
        if (!paths.includes(n.path)) p.onDropPaths?.(paths, n.path, e.altKey);
      } else if (e.dataTransfer.files.length > 0) {
        p.onExternalDrop?.(e.dataTransfer, n.path);
      }
    },
  };
}

// RenameInput edits a name in place: the stem is selected (not the
// extension), Enter commits, Escape cancels, and clicking away commits a
// change but not a no-op.
function RenameInput({ name, onSubmit, onCancel }: { name: string; onSubmit: (v: string) => void; onCancel: () => void }) {
  const ref = useRef<HTMLInputElement>(null);
  const [v, setV] = useState(name);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.focus();
    const dot = name.lastIndexOf(".");
    el.setSelectionRange(0, dot > 0 ? dot : name.length);
  }, [name]);
  const commit = () => {
    const t = v.trim();
    if (t && t !== name) onSubmit(t);
    else onCancel();
  };
  return (
    <input
      ref={ref}
      value={v}
      onChange={(e) => setV(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        e.stopPropagation();
        if (e.key === "Enter") commit();
        else if (e.key === "Escape") onCancel();
      }}
      onClick={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      aria-label="New name"
      className="w-full rounded border border-fog-500 bg-fog-950 px-1 py-0 text-sm text-ink outline-none"
    />
  );
}

const COLS = "grid-cols-[minmax(0,1fr)_6.5rem_7.5rem_9rem]";

function Header({ sort, desc, onSort, showPath }: { sort: FilesSort; desc: boolean; onSort: (s: FilesSort, d: boolean) => void; showPath: boolean }) {
  const col = (id: FilesSort, label: string, right?: boolean) => {
    const on = sort === id;
    return (
      <button
        type="button"
        onClick={() => onSort(id, on ? !desc : false)}
        className={`flex items-center gap-1 truncate text-left caps-label hover:text-ink ${right ? "justify-end" : ""} ${on ? "text-ink" : "text-ink-muted"}`}
      >
        {label}
        {on && <span aria-hidden className="text-[9px]">{desc ? "▼" : "▲"}</span>}
      </button>
    );
  };
  return (
    <div className={`sticky top-0 z-10 grid ${COLS} gap-3 border-b border-hair bg-fog-950 py-1.5 ${DRIVE_GUTTER}`}>
      {col("name", "Name")}
      {col("size", "Size", true)}
      {col("mtime", "Modified")}
      {showPath ? <span className="caps-label">In</span> : col("kind", "Kind")}
    </div>
  );
}

function sizeText(n: FileNode): string {
  if (n.kind === "dir") return n.size === 0 && !n.walkedAt ? "—" : formatBytes(n.size);
  if (n.kind === "other") return "—";
  return formatBytes(n.size);
}

function ListRow({ n, i, p }: { n: FileNode; i: number; p: EntryListProps }) {
  const on = p.sel.isSelected(n.path);
  const focused = p.sel.focus === i;
  const renaming = p.renaming === n.path;
  const cut = p.cutPaths?.has(n.path);
  const [over, setOver] = useState(false);
  const dir = n.path.includes("/") ? n.path.slice(0, n.path.lastIndexOf("/")) : "";
  return (
    <div
      role="option"
      aria-selected={on}
      draggable={!renaming && !p.readOnly}
      onDragStart={(e) => startDrag(e, n, p)}
      {...(p.readOnly ? {} : folderDropProps(n, p, setOver))}
      onClick={(e) => p.sel.click(i, e)}
      onDoubleClick={() => p.onOpen(n)}
      onContextMenu={(e) => {
        if (!on) p.sel.click(i, { shiftKey: false, metaKey: false, ctrlKey: false });
        p.onContextMenu?.(e, n);
      }}
      style={{ height: ROW }}
      className={`grid ${COLS} cursor-default select-none items-center gap-3 text-sm ${DRIVE_GUTTER} ${
        over ? "bg-accent/10 text-ink" : on ? "bg-accent/10 text-ink edge-accent" : focused ? "bg-veil text-ink" : "text-ink hover:bg-veil"
      } ${cut ? "opacity-50" : ""}`}
    >
      <span className="flex min-w-0 items-center gap-2.5">
        <FileIcon category={categoryOf(n)} />
        {renaming ? (
          <RenameInput name={n.name} onSubmit={(v) => p.onRenameSubmit?.(n.path, v)} onCancel={() => p.onRenameCancel?.()} />
        ) : (
          <span className="truncate"><FileName name={n.name} /></span>
        )}
        {n.excluded && !p.at && (
          <span
            title={n.excludedBy ? `Not backed up: excluded with ${n.excludedBy}` : "Not backed up"}
            className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-fog-800 text-ink-muted"}`}
          >
            no backup
          </span>
        )}
        {n.sharedWith && n.sharedWith.length > 0 && !p.at && !n.peer && (
          <span title={`Shared with ${n.sharedWith.join(", ")}`} className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-accent/10 text-accent"}`}>
            → {n.sharedWith.join(", ")}
          </span>
        )}
        {n.kept && !p.at && (
          <span title={n.keptBy ? `Kept on this box with ${n.keptBy}` : "Kept on this box — never leaves through a share"} className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-warn/10 text-warn"}`}>
            kept
          </span>
        )}
        {n.peer && !p.at && (
          <span title={peerBadgeTitle(n.peer)} className={`shrink-0 rounded px-1 text-[10px] font-medium ${n.peer.rejected ? "bg-warn/10 text-warn" : n.peer.stub ? "bg-fog-800 text-ink-muted" : "bg-accent/10 text-accent"}`}>
            {peerBadge(n.peer)}
          </span>
        )}
        {n.dehydrated && !p.at && (
          <span title={`Online-only: fetched from ${n.peer?.peer ?? "its peer"} when opened`} className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-accent/10 text-accent"}`}>
            ☁ online-only
          </span>
        )}
        {n.peer?.pinned && !n.dehydrated && !p.at && (
          <span title="Kept offline on this box" className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-fog-800 text-ink-muted"}`}>
            offline
          </span>
        )}
        {n.role === "apps" && !p.at && (
          <span
            title="Fog keeps a folder here for every app that stores its own data in Drive"
            className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-accent/10 text-accent"}`}
          >
            for apps
          </span>
        )}
        {n.role === "content" && !p.at && (
          <span
            title="Your own libraries — photos, films, music, books — the folders apps read rather than own"
            className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-accent/10 text-accent"}`}
          >
            your libraries
          </span>
        )}
        {n.role !== "apps" && n.mountedBy && n.mountedBy.length > 0 && !p.at && (
          <span
            title={mountsTitle(n)}
            className={`shrink-0 rounded px-1 text-[10px] font-medium ${"bg-accent/10 text-accent"}`}
          >
            {n.mountedBy.join(", ")}
          </span>
        )}
      </span>
      <span className={`text-right tabular-nums ${"text-ink-muted"}`}>{sizeText(n)}</span>
      <span className={`truncate tabular-nums ${"text-ink-muted"}`}>{formatWhen(n.modified)}</span>
      {p.showPath ? (
        <span className={`truncate font-mono text-xs ${"text-ink-muted"}`} title={dir || "/"}>{dir || "/"}</span>
      ) : (
        <span className={`truncate ${"text-ink-muted"}`}>{kindLabel(n)}</span>
      )}
    </div>
  );
}

// The badge's tooltip: what each app sees the folder as, when it is the
// folder itself an app mounts; else who mounts something inside it.
function mountsTitle(n: FileNode): string {
  const seen = (n.mounts ?? []).map((m) => `${m.app} sees this folder as ${m.mount}${m.readOnly ? " (read-only)" : ""}`);
  const rest = (n.mountedBy ?? []).filter((a) => !(n.mounts ?? []).some((m) => m.app === a));
  if (rest.length > 0) seen.push(`${rest.join(", ")} mount${rest.length === 1 ? "s" : ""} a folder inside`);
  return `${seen.join(". ")} — it cannot be moved or trashed`;
}

// A thumbnail where the server can make one, the icon where it cannot
// (a 404 flips to the icon and stays there). Snapshots have no thumbnail
// cache, so images there come straight from the snapshot.
function Thumb({ n, cat, at }: { n: FileNode; cat: FileCategory; at?: number }) {
  const [failed, setFailed] = useState(false);
  const wants = n.kind === "file" && (cat === "image" || cat === "video");
  if (!wants || failed) return <FileIcon category={cat} className="h-12 w-12" />;
  const src = at ? (cat === "image" ? rawUrl(n.path, false, at) : "") : thumbUrl(n.path, 96);
  if (!src) return <FileIcon category={cat} className="h-12 w-12" />;
  const img = <img src={src} alt="" loading="lazy" onError={() => setFailed(true)} className="max-h-16 max-w-full rounded object-contain" />;
  if (cat !== "video") return img;
  // A frame from a film looks like a photo; the play mark says which it is.
  return (
    <span className="relative flex max-w-full items-center justify-center">
      {img}
      <span className="pointer-events-none absolute inset-0 flex items-center justify-center">
        <PlayBadge className="h-6 w-6" />
      </span>
    </span>
  );
}

function GridCell({ n, i, p }: { n: FileNode; i: number; p: EntryListProps }) {
  const on = p.sel.isSelected(n.path);
  const focused = p.sel.focus === i;
  const renaming = p.renaming === n.path;
  const cut = p.cutPaths?.has(n.path);
  const [over, setOver] = useState(false);
  const cat = categoryOf(n);
  return (
    <div
      role="option"
      aria-selected={on}
      draggable={!renaming && !p.readOnly}
      onDragStart={(e) => startDrag(e, n, p)}
      {...(p.readOnly ? {} : folderDropProps(n, p, setOver))}
      onClick={(e) => p.sel.click(i, e)}
      onDoubleClick={() => p.onOpen(n)}
      onContextMenu={(e) => {
        if (!on) p.sel.click(i, { shiftKey: false, metaKey: false, ctrlKey: false });
        p.onContextMenu?.(e, n);
      }}
      title={displayName(n.name)}
      className={`flex cursor-default select-none flex-col items-center gap-1.5 rounded-lg p-2 text-center ${
        over ? "bg-accent/10" : on ? "bg-accent/10 text-ink edge-accent" : focused ? "bg-veil" : "hover:bg-veil"
      } ${cut ? "opacity-50" : ""}`}
    >
      <div className="flex h-16 w-full items-center justify-center overflow-hidden rounded-md">
        <Thumb n={n} cat={cat} at={p.at} />
      </div>
      {renaming ? (
        <RenameInput name={n.name} onSubmit={(v) => p.onRenameSubmit?.(n.path, v)} onCancel={() => p.onRenameCancel?.()} />
      ) : (
        <span className="line-clamp-2 w-full break-words text-xs leading-tight"><FileName name={n.name} /></span>
      )}
      <span className={`text-[10px] ${"text-ink-muted"}`}>{sizeText(n)}</span>
    </div>
  );
}

// The peer pill: whose the entry is, in three words.
function peerBadge(m: NonNullable<FileNode["peer"]>): string {
  if (m.kind === "contribution") return m.stub ? `from ${m.peer} · left` : `from ${m.peer}`;
  if (m.mine) return m.rejected ? `yours · not kept by ${m.peer}` : "yours";
  if (m.author) return m.stub ? `from ${m.author} · stub` : `from ${m.author}`;
  return m.readOnly ? `${m.peer} · read-only` : `${m.peer} · drop box`;
}

function peerBadgeTitle(m: NonNullable<FileNode["peer"]>): string {
  if (m.kind === "contribution") return m.stub ? `Added by ${m.peer}, who has since unpaired — only the listing is here` : `Added by ${m.peer} — theirs to change; make a copy to edit, or reject it`;
  if (m.mine) return m.rejected ? `${m.peer} did not keep this; it is still yours here` : `Your file in ${m.peer}'s folder — edit or remove it here and ${m.peer} follows`;
  if (m.author) return m.stub ? `Added by ${m.author}; the bytes reach you only through your own peering with them` : `Added by ${m.author}, fetched from them`;
  return m.readOnly ? `A copy of ${m.peer}'s share — read-only` : `A copy of ${m.peer}'s share — you may add files of your own`;
}
