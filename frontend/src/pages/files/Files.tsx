// Files: fog's own root, browsed like a desktop. The folder is the URL, the
// list is windowed, the keyboard reaches everything, and a selection is
// described in the pane beside it. The verbs — new folder, rename, move,
// copy, trash — act first and confirm never: the atomic ones patch the
// page before the server answers, deletion is a trash with Undo, and a
// name collision asks once for the whole batch.

import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type MouseEvent } from "react";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { ApiError, type FileNode, type FileSnapshot, type FilesConflict, type FilesConflictPolicy } from "../../api";
import { LoadError, Loading, errorText } from "../../components/Async";
import { DRIVE_GUTTER } from "./gutter";
import ContextMenu from "../../components/ContextMenu";
import { formatBytes } from "../../lib/format";
import { filesRelFromPath, paths } from "../../lib/paths";
import { APPS_DIR, CONTENT_DIR } from "../../lib/volumePlacement";
import { rawUrl, zipUrl } from "../../lib/useFiles";
import { conflictOf, useFilesActions } from "../../lib/useFilesActions";
import { useFilePins, usePinActions } from "../../lib/useFilePins";
import { enqueue, filesFromDataTransfer, filesFromInput } from "../../lib/uploads";
import ConflictDialog from "./ConflictDialog";
import DetailPane from "./DetailPane";
import EntryList from "./EntryList";
import JobsTray from "./JobsTray";
import QuickLook from "./QuickLook";
import SharedView from "./SharedView";
import Toast, { type ToastState } from "./Toast";
import UploadTray from "./UploadTray";
import PeerShareComposer from "../../components/files/PeerShareComposer";
import { useSelection } from "./useSelection";
import { parentOf } from "../../lib/drivePath";
import { DriveTabs, Empty, PathBar, ReservedEmpty, useDriveHistory } from "./DriveChrome";
import DriveToolbar from "./DriveToolbar";
import SettingsLink from "../../components/SettingsLink";
import { driveMenuItems } from "./driveMenu";
import { useDriveListing } from "./useDriveListing";
import { displayName } from "../../lib/displayName";

export interface Clipboard {
  mode: "cut" | "copy";
  paths: string[];
}

interface Pending {
  verb: "move" | "copy" | "restore";
  paths: string[];
  to: string;
  conflict: FilesConflict;
}

export default function Files() {
  const rel = filesRelFromPath(useLocation().pathname) ?? "";
  const [params, setParams] = useSearchParams();
  // at: the snapshot being browsed instead of the live tree (time travel).
  const atRaw = Number(params.get("at"));
  const at = Number.isInteger(atRaw) && atRaw > 0 ? atRaw : undefined;
  const timeTravel = at !== undefined;
  // Shared is a tab, not a folder: a view of everything peers can reach,
  // gathered from wherever in the tree it lives.
  const sharedView = params.get("view") === "shared";
  const navigate = useNavigate();
  // Where we are, for the trail: the folder, the snapshot, the Shared view
  // — but not a transient ?select=, which is not a place.
  const location = useLocation();
  const trailKey = (() => {
    const qs = new URLSearchParams(location.search);
    qs.delete("select");
    const q = qs.toString();
    return `${location.pathname}${q ? `?${q}` : ""}`;
  })();
  const history = useDriveHistory(trailKey);
  const {
    status, prefs, q, setQ, listing, search, searching, snapshot,
    inApps, inContent, entries, hidden, folder, replica, dropBox, readOnly, total, trashItems,
  } = useDriveListing(rel, at, sharedView);
  const act = useFilesActions();
  const sel = useSelection(entries);
  const selection = useMemo(() => entries.filter((n) => sel.selected.has(n.path)), [entries, sel.selected]);
  // Arriving from a search result: select the named entry, then drop the
  // parameter so a refresh does not keep re-selecting it.
  const wantSelect = params.get("select");
  useEffect(() => {
    if (!wantSelect || entries.length === 0) return;
    const i = entries.findIndex((n) => n.name === wantSelect);
    if (i >= 0) sel.jump(i, false);
    const next = new URLSearchParams(params);
    next.delete("select");
    setParams(next, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wantSelect, entries]);
  const [ql, setQl] = useState<number | null>(null);
  const [renaming, setRenaming] = useState<string | null>(null);
  const [clipboard, setClipboard] = useState<Clipboard | null>(null);
  const [toast, setToast] = useState<ToastState | null>(null);
  const [menu, setMenu] = useState<{ x: number; y: number; node: FileNode | null } | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [sharing, setSharing] = useState<string | null>(null);
  useEffect(() => {
    setQl(null);
    setRenaming(null);
    setMenu(null);
  }, [rel, searching]);

  const fail = useCallback((e: unknown, what: string) => setToast({ message: errorText(e, what), tone: "error" }), []);

  // ---- pins ----
  const pins = useFilePins();
  const pinned = useMemo(() => new Set((pins.data ?? []).map((p) => p.path)), [pins.data]);
  const { pin, unpin } = usePinActions();
  const togglePin = useCallback(async (path: string, on: boolean) => {
    const name = path.slice(path.lastIndexOf("/") + 1);
    try {
      await (on ? pin : unpin).mutateAsync(path);
      setToast({ message: on ? `Pinned “${displayName(name)}” to the sidebar` : `Unpinned “${displayName(name)}”` });
    } catch (e) {
      fail(e, on ? "Could not pin" : "Could not unpin");
    }
  }, [pin, unpin, fail]);

  // ---- uploads ----
  const fileInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);
  const dropFiles = useCallback(async (dt: DataTransfer, folder: string) => {
    const list = await filesFromDataTransfer(dt);
    if (list.length > 0) enqueue(folder, list);
  }, []);
  const [pageDrop, setPageDrop] = useState(false);
  const onPageDragOver = (e: DragEvent) => {
    if (!e.dataTransfer.types.includes("Files") || searching || readOnly) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
    setPageDrop(true);
  };
  const onPageDrop = (e: DragEvent) => {
    setPageDrop(false);
    if (!e.dataTransfer.types.includes("Files") || searching || readOnly) return;
    e.preventDefault();
    dropFiles(e.dataTransfer, rel);
  };

  // By path, not identity: the detail pane hands back the stat call's
  // object, which is the same file but never the same object as the row.
  const indexOf = useCallback((n: FileNode) => entries.findIndex((e) => e.path === n.path), [entries]);
  const open = useCallback(
    (n: FileNode) => {
      if (n.kind === "dir") navigate(paths.files(n.path, at));
      else if (n.kind === "file") setQl(indexOf(n));
    },
    [navigate, indexOf, at],
  );
  const up = useCallback(() => {
    if (rel) navigate(paths.files(parentOf(rel), at));
  }, [rel, navigate, at]);
  const quickLook = useCallback(
    (i: number) => {
      const n = entries[i];
      if (n?.kind === "file") setQl((cur) => (cur === i ? null : i));
      else if (n?.kind === "dir") open(n);
    },
    [entries, open],
  );

  // ---- verbs ----

  const newFolder = useCallback(async () => {
    const taken = new Set(entries.map((e) => e.name));
    let name = "untitled folder";
    for (let i = 2; taken.has(name); i++) name = `untitled folder ${i}`;
    try {
      const n = await act.mkdir(rel ? `${rel}/${name}` : name);
      sel.set([n.path]);
      setRenaming(n.path);
    } catch (e) {
      fail(e, "Could not create the folder");
    }
  }, [entries, rel, act, sel, fail]);

  const rename = useCallback(
    async (path: string, name: string) => {
      setRenaming(null);
      try {
        const n = await act.rename(path, name);
        sel.set([n.path]);
      } catch (e) {
        fail(e, "Could not rename");
      }
    },
    [act, sel, fail],
  );

  const trash = useCallback(
    async (ps: string[]) => {
      if (ps.length === 0) return;
      try {
        const items = await act.trash(ps);
        const label = items.length === 1 ? `“${displayName(items[0].name)}”` : `${items.length} items`;
        setToast({
          message: `Moved ${label} to the Trash`,
          undo: () => act.putBack(items.map((it) => it.id)).catch((e) => fail(e, "Could not put it back")),
        });
      } catch (e) {
        fail(e, "Could not move to the Trash");
      }
    },
    [act, fail],
  );

  // move/copy try the "ask" policy first; a 409 opens the dialog, whose
  // answer re-runs the same call with a policy.
  const transfer = useCallback(
    async (verb: "move" | "copy" | "restore", ps: string[], to: string, policy: FilesConflictPolicy = "") => {
      try {
        if (verb === "move") {
          const res = await act.move(ps, to, policy);
          if (res.job) setToast({ message: `Moving ${res.job.label.replace(/^Move /, "")} across filesystems…` });
        } else if (verb === "copy") {
          await act.copy(ps, to, policy);
        } else if (at) {
          await act.restore(at, ps, undefined, policy);
          setToast({ message: `Restoring ${ps.length === 1 ? "“" + ps[0].slice(ps[0].lastIndexOf("/") + 1) + "”" : ps.length + " items"} to where ${ps.length === 1 ? "it" : "they"} lived…` });
        }
      } catch (e) {
        const c = conflictOf(e);
        if (c) setPending({ verb, paths: ps, to, conflict: c });
        else fail(e, verb === "move" ? "Could not move" : verb === "copy" ? "Could not copy" : "Could not restore");
      }
    },
    [act, fail, at],
  );

  const toggleKeep = useCallback(
    async (n: FileNode, keep: boolean) => {
      try {
        await act.setKept(n.path, keep);
        setToast({ message: keep ? `“${displayName(n.name)}” stays on this box — it leaves every share` : `“${displayName(n.name)}” may be shared again` });
      } catch (e) {
        fail(e, "Could not change that");
      }
    },
    [act, fail],
  );

  const toggleOffline = useCallback(
    async (n: FileNode, keep: boolean) => {
      try {
        setToast({ message: keep ? `Fetching “${displayName(n.name)}” from ${n.peer?.peer ?? "the peer"}…` : `Freeing “${displayName(n.name)}”…` });
        await act.setOffline(n.path, keep);
        setToast({ message: keep ? `“${displayName(n.name)}” is kept offline on this box` : `“${displayName(n.name)}” is online-only again` });
      } catch (e) {
        fail(e, keep ? "Could not fetch that" : "Could not free that");
      }
    },
    [act, fail],
  );

  // Rejecting is permanent and tells the peer, so it is the one act here
  // that asks first: the pane shows the question; the menu arms it.
  const [rejectArmed, setRejectArmed] = useState<string | null>(null);
  const reject = useCallback(
    async (n: FileNode) => {
      setRejectArmed(null);
      try {
        await act.reject(n.path);
        setToast({ message: n.peer?.stub ? `Removed “${displayName(n.name)}”` : `Rejected “${displayName(n.name)}” — ${n.peer?.peer ?? "the peer"} keeps their own` });
      } catch (e) {
        fail(e, "Could not reject that");
      }
    },
    [act, fail],
  );
  const armReject = useCallback(
    (n: FileNode) => {
      sel.set([n.path]);
      setRejectArmed(n.path);
    },
    [sel],
  );

  const toggleExclude = useCallback(
    async (n: FileNode, excluded: boolean) => {
      try {
        await act.setExcluded(n.path, excluded);
        setToast({ message: excluded ? `“${displayName(n.name)}” will be left out of backups` : `“${displayName(n.name)}” is backed up again` });
      } catch (e) {
        fail(e, "Could not change the backup setting");
      }
    },
    [act, fail],
  );

  const pickSnapshot = useCallback(
    (s: FileSnapshot | null) => navigate(paths.files(rel, s?.id)),
    [navigate, rel],
  );
  const snapshotNow = useCallback(async () => {
    try {
      await act.snapshotNow();
      setToast({ message: "Snapshot started — watch the tray." });
    } catch (e) {
      fail(e, "Could not start a snapshot");
    }
  }, [act, fail]);

  const paste = useCallback(() => {
    if (!clipboard) return;
    const ps = clipboard.paths.filter((p) => parentOf(p) !== rel || clipboard.mode === "copy");
    if (ps.length === 0) return;
    if (clipboard.mode === "cut") {
      transfer("move", ps, rel);
      setClipboard(null);
    } else {
      // Pasting a copy into its own folder is a duplicate: keep both.
      transfer("copy", ps, rel, ps.some((p) => parentOf(p) === rel) ? "rename" : "");
    }
  }, [clipboard, rel, transfer]);

  const duplicate = useCallback(() => {
    if (selection.length > 0) transfer("copy", selection.map((n) => n.path), rel, "rename");
  }, [selection, rel, transfer]);

  const selectedPaths = useMemo(() => selection.map((n) => n.path), [selection]);
  const cutPaths = useMemo(() => new Set(clipboard?.mode === "cut" ? clipboard.paths : []), [clipboard]);

  const menuItems = useMemo(
    () =>
      driveMenuItems({
        node: menu?.node ?? null, selection, selectedPaths, clipboard, pinned,
        rel, at, searching, timeTravel, replica, dropBox, peerName: folder?.peer?.peer,
        open, quickLook: (n) => setQl(entries.indexOf(n)), duplicate, paste, newFolder, trash, transfer,
        setClipboard, rename: setRenaming, share: setSharing, selectAll: sel.selectAll,
        toggleExclude, toggleKeep, toggleOffline, armReject, togglePin,
      }),
    [menu, selection, selectedPaths, entries, clipboard, pinned, rel, at, searching, timeTravel, replica, dropBox, folder, open, duplicate, paste, newFolder, trash, transfer, sel.selectAll, toggleExclude, toggleKeep, toggleOffline, armReject, togglePin],
  );

  const onContextMenu = useCallback((e: MouseEvent, node: FileNode | null) => {
    e.preventDefault();
    setMenu({ x: e.clientX, y: e.clientY, node });
  }, []);

  const selectedBytes = selection.reduce((s, n) => s + n.size, 0);
  const downloadHref =
    selection.length === 1 && selection[0].kind === "file"
      ? rawUrl(selection[0].path, true, at)
      : selection.length > 0 && !timeTravel
        ? zipUrl(selectedPaths)
        : undefined;
  // The Apps folder is made the first time an app puts its data in Drive,
  // so on a box with none the tab points at nothing yet. That is an empty
  // Apps tab, not a broken folder. Content is made when fog first prepares
  // the root, but it can be emptied or deleted, and the same holds.
  const appsRoot = inApps && rel === APPS_DIR && !searching;
  const contentRoot = inContent && rel === CONTENT_DIR && !searching;
  const reservedRoot = appsRoot || contentRoot;
  const reservedMissing = reservedRoot && listing.error instanceof ApiError && listing.error.status === 404;

  // Shared stands apart from the tree: no folder to list, no verbs that act
  // on a selection — the rows are lenses, and each one opens the real
  // folder it points at.
  if (sharedView) {
    return (
      <div className="flex h-[calc(100vh-3.5rem)] min-h-[24rem] flex-col">
        <DriveTabs active="shared" />
        <div className="flex min-w-0 min-h-0 flex-1 flex-col">
          <PathBar rel="" tail="Shared" history={history} />
          <SharedView />
        </div>
      </div>
    );
  }

  if (status.isPending) return <Loading label="Opening Drive…" />;
  if (status.data && !status.data.ok) {
    return (
      <div className="max-w-2xl space-y-3">
        <LoadError label="The Drive root is not usable right now." error={status.data.error} onRetry={() => status.refetch()} />
        <p className="text-sm text-ink-muted">
          Drive lives at <code className="font-mono text-ink">{status.data.root}</code>. Check the share it sits on, or choose another location in{" "}
          <SettingsLink to="storage/drive" className="text-accent underline" />.
        </p>
      </div>
    );
  }

  return (
    <div
      className={`flex h-[calc(100vh-3.5rem)] min-h-[24rem] flex-col ${pageDrop ? "ring-2 ring-inset ring-accent" : ""}`}
      onDragOver={onPageDragOver}
      onDragLeave={(e) => e.currentTarget === e.target && setPageDrop(false)}
      onDrop={onPageDrop}
    >
      {/* Which part of Drive: your files, an app's own data, your
          libraries, or what peers can reach. The Files tab stays lit for
          every folder under the root, so it says which tab it is rather
          than letting the URL match. */}
      <DriveTabs active={inApps ? "apps" : inContent ? "content" : "files"} at={at} />
      <input ref={fileInput} type="file" multiple className="hidden" onChange={(e) => { if (e.target.files) enqueue(rel, filesFromInput(e.target.files)); e.target.value = ""; }} />
      <input ref={folderInput} type="file" className="hidden" {...({ webkitdirectory: "" } as Record<string, string>)} onChange={(e) => { if (e.target.files) enqueue(rel, filesFromInput(e.target.files)); e.target.value = ""; }} />
      {replica && !timeTravel && folder?.peer && (
        <div className={`flex flex-wrap items-center gap-3 border-b border-accent/30 bg-accent/10 py-2 text-xs text-accent ${DRIVE_GUTTER}`}>
          <span>
            A copy of <strong>{folder.peer.peer}</strong>'s share{folder.peer.revoked ? " — no longer shared by them" : ", refreshed on every sync"}.{" "}
            {dropBox ? <>A drop box: files you add here go to {folder.peer.peer}; their entries stay theirs.</> : <>Read-only: nothing changed here reaches {folder.peer.peer}.</>}
          </span>
          <Link to={paths.federation} className="ml-auto rounded-md border border-accent/30 px-2.5 py-1 font-medium hover:bg-accent/10">Manage on Connect</Link>
        </div>
      )}
      {timeTravel && (
        <div className={`flex flex-wrap items-center gap-3 border-b border-warn/30 bg-warn/10 py-2 text-xs text-warn ${DRIVE_GUTTER}`}>
          <span>
            ⏱ Viewing Drive as of{" "}
            <strong>{snapshot ? new Date(snapshot.createdAt).toLocaleString() : `snapshot ${at}`}</strong> — read only.
            {snapshot?.mode === "mirror" && " This is the rolling mirror, not a dated snapshot."}
          </span>
          <span className="ml-auto flex gap-2">
            <button type="button" onClick={() => transfer("restore", selection.length ? selectedPaths : [rel], "")} className="rounded-md border border-warn/30 px-2.5 py-1 font-medium hover:bg-warn/10">
              Restore {selection.length > 0 ? `${selection.length} selected` : rel ? "this folder" : "everything"}
            </button>
            <button type="button" onClick={() => pickSnapshot(null)} className="rounded-md border border-warn/30 px-2.5 py-1 font-medium hover:bg-warn/10">
              Back to today
            </button>
          </span>
        </div>
      )}
      <DriveToolbar
        q={q}
        onQuery={setQ}
        rel={rel}
        at={at}
        prefs={prefs}
        onPickSnapshot={pickSnapshot}
        onSnapshotNow={snapshotNow}
        canAdd={!searching && !readOnly}
        onNewFolder={newFolder}
        onUpload={() => fileInput.current?.click()}
        onUploadFolder={() => folderInput.current?.click()}
        downloadHref={downloadHref}
        selected={selection.length}
        onTrash={readOnly ? undefined : () => trash(selectedPaths)}
        fetching={listing.isFetching || search.isFetching}
        walking={!!status.data?.walking}
        trashItems={trashItems}
        onRefresh={() => listing.refetch()}
      />

      {/* Body */}
      <div className="flex min-h-0 flex-1">
        <div className="flex min-w-0 flex-1 flex-col">
          <PathBar rel={rel} at={at} history={history} />
          {listing.isError && !listing.data && !searching && !reservedMissing ? (
            <div className="p-4">
              <LoadError label="Could not read this folder." error={listing.error} onRetry={() => listing.refetch()} />
            </div>
          ) : (listing.isPending && !searching) || (searching && search.isPending) ? (
            <div className="p-4"><Loading label={searching ? "Searching…" : "Reading folder…"} /></div>
          ) : entries.length === 0 ? (
            reservedRoot ? (
              <ReservedEmpty which={appsRoot ? "apps" : "content"} onContextMenu={onContextMenu} onNewFolder={newFolder} />
            ) : (
              <Empty rel={rel} searching={searching} q={q} root={status.data?.root ?? ""} readOnly={timeTravel} onNewFolder={newFolder} onUpload={() => fileInput.current?.click()} onContextMenu={onContextMenu} />
            )
          ) : (
            <EntryList
              entries={entries}
              view={searching ? "list" : prefs.view}
              sel={sel}
              sort={prefs.sort}
              desc={prefs.desc}
              onSort={prefs.setSort}
              onOpen={open}
              onUp={up}
              onQuickLook={quickLook}
              showPath={searching}
              autoFocus={!searching}
              hasMore={!searching && listing.hasNextPage}
              loadingMore={listing.isFetchingNextPage}
              onLoadMore={() => listing.fetchNextPage()}
              onContextMenu={onContextMenu}
              at={at}
              readOnly={readOnly}
              renaming={renaming}
              onRenameSubmit={rename}
              onRenameCancel={() => setRenaming(null)}
              cutPaths={cutPaths}
              currentDir={rel}
              onDropPaths={readOnly ? undefined : (ps, folder, copy) => transfer(copy ? "copy" : "move", ps, folder)}
              onExternalDrop={readOnly ? undefined : dropFiles}
              onDelete={readOnly ? undefined : () => trash(selectedPaths)}
              onRename={readOnly ? undefined : () => selection.length === 1 && !searching && setRenaming(selection[0].path)}
              onCopy={() => setClipboard({ mode: "copy", paths: selectedPaths })}
              onCut={readOnly ? undefined : () => setClipboard({ mode: "cut", paths: selectedPaths })}
              onPaste={readOnly ? undefined : paste}
              onNewFolder={readOnly ? undefined : newFolder}
              onDuplicate={readOnly ? undefined : duplicate}
            />
          )}
          <div className={`flex items-center gap-3 border-t border-hair py-1.5 text-[11px] text-ink-muted ${DRIVE_GUTTER}`}>
            <span>
              {searching ? `${total} result${total === 1 ? "" : "s"}` : `${total} item${total === 1 ? "" : "s"}`}
              {!searching && folder && folder.size > 0 && ` · ${formatBytes(folder.size)}${folder.walkedAt ? "" : " so far"}`}
            </span>
            {selection.length > 0 && (
              <span className="text-ink-muted">
                {selection.length} selected · {formatBytes(selectedBytes)}
              </span>
            )}
            {clipboard && (
              <span className="text-ink-muted">
                {clipboard.paths.length} {clipboard.mode === "cut" ? "cut" : "copied"} · ⌘V to paste
              </span>
            )}
            <span className="ml-auto hidden sm:inline">↑↓ move · ⇧ extend · ↵ open · space preview · ⌫ up · ⌘⌫ trash · drag to move</span>
          </div>
        </div>
        <aside className="hidden w-72 shrink-0 overflow-y-auto border-l border-hair px-4 py-3 lg:block xl:w-80">
          <DetailPane
            folder={folder}
            hidden={hidden}
            selection={selection}
            at={at}
            onOpen={open}
            onQuickLook={(n) => setQl(indexOf(n))}
            onToggleExclude={timeTravel ? undefined : toggleExclude}
            onToggleKeep={timeTravel ? undefined : toggleKeep}
            onToggleOffline={timeTravel ? undefined : toggleOffline}
            onReject={timeTravel ? undefined : reject}
            rejectArmed={rejectArmed}
            onArmReject={setRejectArmed}
            onDuplicate={timeTravel ? undefined : duplicate}
            onShare={timeTravel ? undefined : (n) => setSharing(n.path)}
          />
        </aside>
      </div>

      {ql !== null && entries[ql] && (
        <QuickLook
          entries={entries}
          index={ql}
          at={at}
          onMove={(i) => {
            setQl(i);
            sel.jump(i, false);
          }}
          onClose={() => setQl(null)}
        />
      )}
      {menu && <ContextMenu x={menu.x} y={menu.y} items={menuItems} onClose={() => setMenu(null)} />}
      {pending && (
        <ConflictDialog
          verb={pending.verb}
          count={pending.paths.length}
          conflict={pending.conflict}
          onChoose={(policy) => {
            const p = pending;
            setPending(null);
            transfer(p.verb, p.paths, p.to, policy);
          }}
          onClose={() => setPending(null)}
        />
      )}
      {sharing !== null && <PeerShareComposer path={sharing} onClose={() => setSharing(null)} />}
      <Toast toast={toast} onDone={() => setToast(null)} />
      <UploadTray />
      <JobsTray />
    </div>
  );
}
