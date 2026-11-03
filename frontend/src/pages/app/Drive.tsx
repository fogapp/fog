// The Drive tab: the storage model of the app, in two columns. On the left,
// its folders as Drive holds them, with a drop target because putting media
// into an app is just putting files in Drive. On the right, its backups:
// one chronological list of every snapshot fog knows about — taken here or
// held only by a locker — and three doors above it: take one, restore one,
// and the schedule that makes them. What used to be the Backups tab lives
// here, because a snapshot is a fact about the app's storage, not a
// separate feature; its code is in Backups.tsx.

import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import type { AppDetail as AppDetailT, FilesStatus } from "../../api";
import Button from "../../components/Button";
import FileIcon from "../../components/FileIcon";
import Modal from "../../components/Modal";
import { formatBytes } from "../../lib/format";
import { paths } from "../../lib/paths";
import type { AppVolume } from "../../lib/useAppData";
import { filesKey } from "../../lib/useFiles";
import { enqueue, filesFromDataTransfer, filesFromInput, useUploads } from "../../lib/uploads";
import { AREA_LABELS, appFolderFor, driveAreaOf, placementLabel } from "../../lib/volumePlacement";
import AddFolder from "./AddFolder";
import MoveVolume from "./MoveVolume";
import { IconButton, glyph } from "./Actions";
import { Backups, Schedule } from "./Backups";
import { Group, Pill, Veil, type Tone } from "./cockpit";

export function DriveTab({ name, detail, volumes, filesStatus, restorePending, onSaved, onShare }: {
  name: string;
  detail: AppDetailT;
  volumes: AppVolume[];
  filesStatus?: FilesStatus;
  restorePending?: boolean;
  onSaved: () => Promise<unknown>;
  onShare: (path?: string) => void;
}) {
  const [params, setParams] = useSearchParams();
  // ?tab=schedule is where an issue's fix lands; the settings dialog opens on it.
  const [settingsOpen, setSettingsOpen] = useState(params.get("tab") === "schedule");
  useEffect(() => {
    if (params.get("tab") === "schedule") setSettingsOpen(true);
  }, [params]);
  const closeSettings = () => {
    setSettingsOpen(false);
    if (params.get("tab")) {
      const next = new URLSearchParams(params);
      next.delete("tab");
      setParams(next, { replace: true });
    }
  };
  return (
    <div className="grid gap-x-[34px] gap-y-9 lg:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
      <InDrive name={name} detail={detail} volumes={volumes} filesStatus={filesStatus} onSaved={onSaved} onShare={onShare} />
      <Backups name={name} detail={detail} restorePending={restorePending} onSettings={() => setSettingsOpen(true)} />
      {settingsOpen && (
        <Modal title="Backup schedule" onClose={closeSettings}>
          <Schedule name={name} detail={detail} onSaved={onSaved} />
        </Modal>
      )}
    </div>
  );
}

// ---- this app in Drive ----

function InDrive({ name, detail, volumes, filesStatus, onSaved, onShare }: {
  name: string; detail: AppDetailT; volumes: AppVolume[]; filesStatus?: FilesStatus;
  onSaved: () => Promise<unknown>; onShare: (path?: string) => void;
}) {
  const [moving, setMoving] = useState<AppVolume | null>(null);
  const [adding, setAdding] = useState(false);
  const driveVols = volumes.filter((v) => v.rel !== undefined);
  const others = volumes.filter((v) => v.rel === undefined);
  const total = driveVols.reduce((n, v) => n + (v.stat?.size ?? 0), 0);
  const sized = driveVols.length > 0 && driveVols.every((v) => v.statPhase === "ready");
  // Where dropped files go: a content library the app reads if it has one —
  // dropping a film into an app means putting it in the library, not in the
  // app's own config folder — and otherwise its first writable folder.
  const writable = driveVols.filter((v) => v.placement.kind === "files" && !v.volume.readOnly);
  const dropTarget = writable.find((v) => driveAreaOf(v.rel ?? "") === "content") ?? writable[0];
  const appFolder = driveFrame(name, driveVols);

  return (
    <Group title="This app in Drive" first right={
      <span className="flex items-center gap-2">
        {driveVols.length > 0 && <Button size="sm" onClick={() => onShare(driveVols[0].rel)}>Share a folder…</Button>}
        <span className="inline-flex rounded-md border border-edge">
          <IconButton label="Add a folder" title="Add a folder…" onClick={() => setAdding(true)} align="end" edge="both">
            <svg {...glyph}><path d="M12 5v14M5 12h14" /></svg>
          </IconButton>
        </span>
      </span>
    }>
      {volumes.length === 0 ? (
        <p className="text-hint text-ink-muted">
          This app keeps nothing on disk. <button type="button" onClick={() => setAdding(true)} className="text-accent hover:text-accent-hover">Add a folder</button> if it should.
        </p>
      ) : (
        <Veil>
          <div className="flex flex-wrap items-center gap-2.5 border-b border-divider px-4 py-3">
            {appFolder !== undefined ? (
              <>
                <span className="font-mono text-cell text-ink-muted">Drive /{appFolder.parent ? ` ${appFolder.parent.split("/").join(" / ")} /` : ""}</span>
                <span className="font-mono text-cell text-ink">{appFolder.leaf}</span>
              </>
            ) : (
              <span className="font-mono text-cell text-ink-muted">Not in Drive</span>
            )}
            <span className="flex-1" />
            <span className="text-hint text-ink-muted">
              {sized ? `${formatBytes(total)} · ` : driveVols.length > 0 ? "measuring · " : ""}
              {volumes.length} mount{volumes.length === 1 ? "" : "s"}
            </span>
            {appFolder !== undefined && (
              <Link to={paths.files(appFolder.rel)} className="text-hint text-accent hover:text-accent-hover">Open in Drive ↗</Link>
            )}
          </div>
          {[...driveVols, ...others].map((v) => {
            const { tone, label } = volumeState(v);
            const size =
              v.statPhase === "ready" && v.stat ? formatBytes(v.stat.size)
              : v.statPhase === "pending" ? "measuring…"
              : v.statPhase === "missing" ? "missing"
              : v.statPhase === "error" ? "unknown"
              : "";
            const leaf = v.rel !== undefined ? (v.rel.split("/").filter(Boolean).pop() ?? "Drive") : v.volume.name;
            // The folder on one line, its mount point under it: the row has
            // half a page to itself now, and two short lines read better
            // than four squeezed columns.
            return (
              <div key={`${v.service}:${v.volume.name}`} className="grid items-center gap-3.5 border-b border-divider px-4 py-3 last:border-b-0 hover:bg-hover"
                style={{ gridTemplateColumns: "minmax(0,1fr) auto auto auto" }}>
                <span className="min-w-0">
                  <span className="flex min-w-0 items-center gap-2 text-cell text-ink">
                    {v.rel !== undefined ? (
                      <Link to={paths.files(v.rel)} className="inline-flex min-w-0 items-center gap-2 hover:text-ink" title={placementLabel(v.placement)}>
                        <FileIcon category="folder" /><span className="truncate">{leaf}</span>
                      </Link>
                    ) : (
                      <span className="truncate" title={placementLabel(v.placement)}>{leaf}</span>
                    )}
                    {v.service && <span className="shrink-0 text-hint text-ink-muted">· {v.service}</span>}
                  </span>
                  <span className="mt-0.5 block truncate font-mono text-hint text-ink-muted" title={v.volume.mount}>{v.volume.mount}{v.volume.readOnly ? " ro" : ""}{v.volume.follows ? ` · follows ${v.volume.follows}` : ""}</span>
                </span>
                <span className={`font-mono text-cell ${v.statPhase === "missing" ? "text-warn" : "text-ink-muted"}`}>{size}</span>
                <span><Pill tone={tone}>{label}</Pill></span>
                {/* A followed folder moves with the app it follows: moving it
                    here would be undone the next time that app's spec changes. */}
                <span>{v.volume.follows
                  ? <span className="text-hint text-ink-muted" title={`Moves with ${v.volume.follows.split("/")[0]}`}>Follows</span>
                  : <Button size="sm" variant="ghost" onClick={() => setMoving(v)}>Move…</Button>}</span>
              </div>
            );
          })}
        </Veil>
      )}
      {dropTarget && filesStatus?.ok && <DropTarget rel={dropTarget.rel!} mount={dropTarget.volume.mount} />}
      {moving && (
        <MoveVolume app={name} v={moving} filesRoot={filesStatus?.ok ? filesStatus.root : ""} onClose={() => setMoving(null)} />
      )}
      {adding && (
        <AddFolder app={name} detail={detail} filesRoot={filesStatus?.ok ? filesStatus.root : ""} onSaved={onSaved} onClose={() => setAdding(false)} />
      )}
    </Group>
  );
}

function volumeState(v: AppVolume): { tone: Tone; label: string } {
  if (v.statPhase === "missing") return { tone: "warn", label: "Folder missing" };
  switch (v.placement.kind) {
    case "peer":
      return { tone: "ok", label: `Copy from ${v.placement.sub.peer ?? "a peer"}` };
    case "files": {
      const n = v.stat?.sharedWith?.length ?? 0;
      if (n > 0) return { tone: "ok", label: `Shared · ${n} peer${n === 1 ? "" : "s"}` };
      if (v.stat?.excluded) return { tone: "neutral", label: "Not in snapshots" };
      // Which half of Drive it is in — the same two words the install pane
      // uses, so a folder reads the same before and after it exists.
      return { tone: "neutral", label: AREA_LABELS[driveAreaOf(v.rel ?? "")] };
    }
    case "nas":
      return { tone: v.placement.share.mounted && !v.placement.share.stale ? "neutral" : "warn", label: `NAS · ${v.placement.share.name}` };
    case "host":
      return { tone: "off", label: "Host path" };
    case "managed":
      return { tone: "neutral", label: "Fog-managed" };
  }
}

// The Drive folder the header names. The app's own folder under Apps is the
// answer whenever anything is in it — that is the folder the app owns, even
// when it also reads a library in Content. Failing that, the folder its
// folders share. An app whose folders share nothing gets no frame rather
// than the Drive root, which would claim the whole of Drive is its.
function driveFrame(app: string, vols: AppVolume[]): { rel: string; parent: string; leaf: string } | undefined {
  const rels = vols.map((v) => v.rel!).filter((r) => r !== undefined);
  if (rels.length === 0) return undefined;
  const own = appFolderFor(app);
  if (rels.some((r) => r === own || r.startsWith(own + "/"))) return crumbsOf(own);

  const segs = rels.map((r) => r.split("/").filter(Boolean));
  let common: string[] = segs[0];
  for (const s of segs.slice(1)) {
    let i = 0;
    while (i < common.length && i < s.length && common[i] === s[i]) i++;
    common = common.slice(0, i);
  }
  // A single folder is its own answer; several share their parent.
  const rel = rels.length === 1 ? segs[0] : common;
  if (rel.length === 0) return undefined;
  return crumbsOf(rel.join("/"));
}

function crumbsOf(rel: string): { rel: string; parent: string; leaf: string } {
  const segs = rel.split("/").filter(Boolean);
  return { rel, parent: segs.slice(0, -1).join("/"), leaf: segs[segs.length - 1] ?? "" };
}

// Files dropped here land in the app's folder through the same upload
// queue the Drive page uses, so the tray, the resume memory and the
// conflict handling are all the same ones.
function DropTarget({ rel, mount }: { rel: string; mount: string }) {
  const qc = useQueryClient();
  const [over, setOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const uploads = useUploads().filter((u) => u.dir === rel && (u.status === "queued" || u.status === "uploading"));
  useEffect(() => {
    if (uploads.length === 0) qc.invalidateQueries({ queryKey: filesKey });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [uploads.length]);
  return (
    <div
      onDragOver={(e) => { e.preventDefault(); setOver(true); }}
      onDragLeave={() => setOver(false)}
      onDrop={async (e) => {
        e.preventDefault();
        setOver(false);
        const files = await filesFromDataTransfer(e.dataTransfer);
        if (files.length) enqueue(rel, files);
      }}
      onClick={() => input.current?.click()}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => e.key === "Enter" && input.current?.click()}
      className={`mt-3 cursor-pointer rounded-lg border border-dashed px-4 py-[18px] text-center transition-colors duration-150 ${
        over ? "border-accent bg-accent/5" : "border-edge hover:border-accent hover:bg-accent/5"
      }`}
    >
      <input ref={input} type="file" multiple className="hidden" onChange={(e) => {
        if (e.target.files?.length) enqueue(rel, filesFromInput(e.target.files));
        e.target.value = "";
      }} />
      <div className="text-cell text-ink-muted">
        {uploads.length > 0
          ? <>Uploading {uploads.length} file{uploads.length === 1 ? "" : "s"} to <span className="font-mono text-ink">{mount}</span>…</>
          : <>Drop files here to add them to <span className="font-mono text-ink">{mount}</span></>}
      </div>
      <div className="mt-1 text-hint text-ink-muted">They land in Drive › {rel.split("/").join(" › ")}, where the app already sees them.</div>
    </div>
  );
}
