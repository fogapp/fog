// The frame around Drive's listing: its tabs, the path bar with its own
// back and forward, and what an empty folder says. Split from Files.tsx,
// which keeps the state and the verbs.

import { useCallback, useEffect, useReducer, type MouseEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import type { FileNode } from "../../api";
import FileIcon from "../../components/FileIcon";
import Subheader from "../../components/Subheader";
import { paths } from "../../lib/paths";
import { APPS_DIR, CONTENT_DIR } from "../../lib/volumePlacement";
import { DRIVE_GUTTER } from "./gutter";
import { buttonClass } from "../../components/Button";
import { displayName } from "../../lib/displayName";

export const tool = buttonClass();

// Drive keeps its own back and forward, the way Finder does: the pair walks
// the folders visited here, not the browser's history through the rest of
// fog. Module state, so leaving for another page and coming back keeps the
// trail; a reload starts it afresh.
const trail = { stack: [] as string[], index: -1, landing: -1 };

export function useDriveHistory(loc: string) {
  const navigate = useNavigate();
  const [, bump] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    if (trail.landing >= 0 && trail.stack[trail.landing] === loc) {
      trail.index = trail.landing;
    } else if (trail.stack[trail.index] !== loc) {
      // A fresh step drops whatever was ahead, as a browser would.
      trail.stack = [...trail.stack.slice(0, trail.index + 1), loc];
      trail.index = trail.stack.length - 1;
    }
    trail.landing = -1;
    bump();
  }, [loc]);
  const go = useCallback(
    (delta: number) => {
      const i = trail.index + delta;
      if (i < 0 || i >= trail.stack.length) return;
      trail.landing = i;
      navigate(trail.stack[i]);
    },
    [navigate],
  );
  return {
    canBack: trail.index > 0,
    canForward: trail.index < trail.stack.length - 1,
    back: () => go(-1),
    forward: () => go(1),
  };
}

export type DriveHistory = ReturnType<typeof useDriveHistory>;

// Which part of Drive: your files, the folder fog keeps for each app's own
// data, the one your libraries live in, or what peers can reach. The Files
// tab stays lit for every folder under the root, so the strip says which
// tab it is rather than letting the URL match.
export function DriveTabs({ active, at }: { active: "files" | "apps" | "content" | "shared"; at?: number }) {
  return (
    <Subheader
      label="Drive"
      gutter={DRIVE_GUTTER}
      active={active}
      items={[
        { id: "files", label: "Files", to: paths.files("", at) },
        { id: "content", label: "Content", to: paths.files(CONTENT_DIR, at) },
        { id: "apps", label: "Apps", to: paths.files(APPS_DIR, at) },
        { id: "shared", label: "Shared", to: paths.filesShared() },
      ]}
    />
  );
}

// Where you are, pinned under the listing it describes. The header names
// the page and stops there; this row spells the folder path, and every
// level above the one you are in is a link back up to it. A tail is a view
// that is not a folder — Shared — standing where the folder name would.
// The trail sits between the toolbar and the listing: back and forward on
// the left, then where you are, each ancestor a link. It sits above the
// column header so the eye reads place, then columns, then rows.
export function PathBar({ rel, at, tail, history }: {
  rel: string;
  at?: number;
  tail?: string;
  history: DriveHistory;
}) {
  const segs = rel ? rel.split("/") : [];
  const crumbs = [
    ...segs.map((seg, i) => ({ label: displayName(seg), rel: segs.slice(0, i + 1).join("/") })),
    ...(tail ? [{ label: tail, rel: undefined }] : []),
  ];
  const navBtn = "px-2 py-0.5 text-sm leading-none text-ink-muted hover:bg-fog-800 hover:text-ink disabled:cursor-default disabled:text-ink-faint disabled:hover:bg-transparent";
  return (
    <nav aria-label="Folder path" className={`flex shrink-0 items-center gap-1 overflow-x-auto border-b border-hair py-1.5 text-[11px] text-ink-muted ${DRIVE_GUTTER}`}>
      <span className="mr-1.5 flex shrink-0 overflow-hidden rounded-md border border-edge" role="group" aria-label="History">
        <button type="button" onClick={history.back} disabled={!history.canBack} className={navBtn} title="Back" aria-label="Back">‹</button>
        <button type="button" onClick={history.forward} disabled={!history.canForward} className={`${navBtn} border-l border-edge`} title="Forward" aria-label="Forward">›</button>
      </span>
      <FileIcon category="folder" className="h-3.5 w-3.5 shrink-0 opacity-70" />
      {crumbs.length === 0 ? (
        <span className="shrink-0 font-medium text-ink">Drive</span>
      ) : (
        <Link to={paths.files("", at)} className="shrink-0 hover:text-ink">Drive</Link>
      )}
      {crumbs.map((c, i) => {
        const last = i === crumbs.length - 1;
        return (
          <span key={i} className="flex shrink-0 items-center gap-1">
            <span aria-hidden className="text-ink-faint">›</span>
            {last || c.rel === undefined ? (
              <span className="font-medium text-ink">{c.label}</span>
            ) : (
              <Link to={paths.files(c.rel, at)} className="hover:text-ink">{c.label}</Link>
            )}
          </span>
        );
      })}
    </nav>
  );
}

// The two reserved folders when there is nothing in them: each says what it
// is for and offers the one thing that fills it — installing an app, or
// making a library.
export function ReservedEmpty({ which, onContextMenu, onNewFolder }: {
  which: "apps" | "content";
  onContextMenu: (e: MouseEvent, n: FileNode | null) => void;
  onNewFolder: () => void;
}) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center" onContextMenu={(e) => onContextMenu(e, null)}>
      <FileIcon category={which} className="h-16 w-16 opacity-60" />
      {which === "apps" ? (
        <>
          <p className="text-sm text-ink-muted">No app folders yet.</p>
          <p className="max-w-md text-xs text-ink-muted">
            Every app that keeps its own data in Drive gets a folder here, named after it, the first time it
            is installed — what is inside belongs to that app and travels in its backups. Your own libraries
            live under Content instead, where any app can be pointed at them.
          </p>
          <Link to={paths.newApp} className={tool}>Add an app</Link>
        </>
      ) : (
        <>
          <p className="text-sm text-ink-muted">No libraries here yet.</p>
          <p className="max-w-md text-xs text-ink-muted">
            Your own libraries live here — photos, films, music, books, documents. Drop files into one and
            every app pointed at it sees them, so several apps can read the same folder. These are covered
            by Drive's nightly snapshot rather than by any one app's backups.
          </p>
          <button type="button" onClick={onNewFolder} className={tool}>New library</button>
        </>
      )}
    </div>
  );
}

export function Empty({
  rel, searching, q, root, readOnly, onNewFolder, onUpload, onContextMenu,
}: {
  rel: string; searching: boolean; q: string; root: string; readOnly?: boolean; onNewFolder: () => void; onUpload: () => void; onContextMenu: (e: MouseEvent, n: FileNode | null) => void;
}) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center" onContextMenu={(e) => onContextMenu(e, null)}>
      <FileIcon category="folder" className="h-16 w-16 opacity-60" />
      {readOnly ? (
        <p className="text-sm text-ink-muted">This folder was empty in that snapshot.</p>
      ) : searching ? (
        <p className="text-sm text-ink-muted">Nothing named like “{q.trim()}”{rel ? ` in ${rel}` : ""}.</p>
      ) : (
        <>
          <p className="text-sm text-ink-muted">{rel ? "This folder is empty." : "Nothing here yet."}</p>
          <p className="max-w-md text-xs text-ink-muted">
            Drop files or folders here to upload them.
            {!rel && (
              <>
                {" "}They will live at <code className="font-mono text-ink-muted">{root}</code> as ordinary files — any tool that can reach the
                server can put things there too, and they will show up here.
              </>
            )}
          </p>
          <div className="flex gap-2">
            <button type="button" onClick={onUpload} className={tool}>Upload</button>
            <button type="button" onClick={onNewFolder} className={tool}>New folder</button>
          </div>
        </>
      )}
    </div>
  );
}
