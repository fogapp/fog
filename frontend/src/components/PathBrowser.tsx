import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, type FsEntry, type FsListing } from "../api";
import { LoadError, Loading, errorText } from "./Async";
import { formatBytes } from "../lib/format";
import Modal from "./Modal";
import { control } from "./Field";

// Directory picker over the host's real filesystem. A hand-typed host path
// fails in ways a text input can't show — it doesn't exist, it's owned by
// root while the app runs as uid 1000, or it silently lands on the system
// disk instead of the NAS. Browsing what's actually there, with ownership and
// mount facts on every row, is how a picked path ends up being a correct one.

const btn = "rounded-md px-3 py-1.5 text-sm font-medium";
const inputCls = control;

export default function PathBrowser({ title, initial, defaultOwner, onSelect, onClose }: {
  title?: string;
  // Starting directory; falls back to "/" (and to the nearest existing
  // ancestor server-side paths can't do — we walk up on 400s below).
  initial?: string;
  // Prefill for new-folder ownership (e.g. the volume's uid:gid field).
  defaultOwner?: string;
  onSelect: (path: string) => void;
  onClose: () => void;
}) {
  const [path, setPath] = useState(initial && initial.startsWith("/") ? initial : "/");
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [newOwner, setNewOwner] = useState(defaultOwner ?? "");
  const [error, setError] = useState("");
  const qc = useQueryClient();

  const listing = useQuery({
    queryKey: ["fs", path],
    queryFn: () => get<FsListing>(`/api/system/fs?path=${encodeURIComponent(path)}`),
    retry: false,
  });

  // An initial path that doesn't exist yet (common: the volume points at a
  // dir nobody created) walks up to the nearest ancestor that does.
  useEffect(() => {
    if (listing.isError && path !== "/") {
      setPath(path.slice(0, path.lastIndexOf("/")) || "/");
    }
  }, [listing.isError, path]);

  const mkdir = useMutation({
    mutationFn: () =>
      post<FsEntry>("/api/system/fs/mkdir", {
        path: `${path === "/" ? "" : path}/${newName.trim()}`,
        owner: newOwner.trim() || undefined,
      }),
    onSuccess: (ent) => {
      setCreating(false);
      setNewName("");
      setError("");
      qc.invalidateQueries({ queryKey: ["fs"] });
      setPath(ent.path);
    },
    onError: (e) => setError(errorText(e, "Couldn't create the folder")),
  });

  const d = listing.data;
  const crumbs = path === "/" ? [""] : path.split("/");
  const onSystemDisk = d && d.fsMount === "/";

  return (
    <Modal title={title ?? "Choose a folder"} onClose={onClose} width="max-w-2xl" flush>
      {/* Breadcrumbs */}
      <div className="flex flex-wrap items-center gap-0.5 px-4 py-2 font-mono text-xs">
        {crumbs.map((seg, i) => {
          const target = i === 0 ? "/" : crumbs.slice(0, i + 1).join("/");
          const label = i === 0 ? "/" : seg;
          const last = i === crumbs.length - 1;
          return (
            <span key={target} className="flex items-center gap-0.5">
              {i > 1 && <span className="text-ink-muted">/</span>}
              {last ? (
                <span className="text-ink">{label}</span>
              ) : (
                <button onClick={() => setPath(target)} className="text-ink-muted hover:text-ink">{label}</button>
              )}
            </span>
          );
        })}
      </div>

      {/* Current directory facts — the "is this path correct?" line. */}
      {d && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-y border-hair bg-fog-950/40 px-4 py-2 text-xs">
          <span className="font-mono text-ink-muted">{d.mode} · {d.owner}</span>
          {d.mountPoint && (
            <span className="rounded bg-accent/10 px-1.5 py-0.5 font-medium text-accent">mount point</span>
          )}
          <span className={onSystemDisk ? "text-warn" : "text-ink-muted"}>
            {onSystemDisk ? "system disk" : `on ${d.fsMount}`}
            {d.freeBytes != null && ` · ${formatBytes(d.freeBytes)} free`}
          </span>
          {onSystemDisk && (
            <span className="text-warn" title="This directory lives on the server's own disk, not a mounted share. Large data here fills the system drive.">
              ⚠ not on a mounted share
            </span>
          )}
        </div>
      )}

      {/* Listing */}
      <div className="min-h-40 flex-1 overflow-y-auto px-2 py-1">
        {listing.isPending && <Loading label="Reading directory…" />}
        {listing.isError && path === "/" && (
          <LoadError label="Couldn't read the filesystem." error={listing.error} onRetry={() => listing.refetch()} />
        )}
        {d && d.entries.length === 0 && (
          <p className="px-2 py-3 text-xs text-ink-muted">Empty directory.</p>
        )}
        {d?.entries.map((e) => (
          <button
            key={e.path}
            disabled={!e.dir}
            onDoubleClick={() => e.dir && onSelect(e.path)}
            onClick={() => e.dir && setPath(e.path)}
            className={`flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm ${
              e.dir ? "text-ink hover:bg-fog-800" : "cursor-default text-ink-muted"
            } ${e.name.startsWith(".") ? "opacity-60" : ""}`}>
            <span className="w-4 shrink-0 text-center">{e.dir ? "📁" : ""}</span>
            <span className="min-w-0 flex-1 truncate">{e.name}</span>
            {e.mountPoint && (
              <span className="shrink-0 rounded-full border border-hair px-2 py-px text-[11px] font-medium text-ink-muted">Mount</span>
            )}
            <span className="shrink-0 font-mono text-[11px] text-ink-muted">
              {e.dir ? e.owner : formatBytes(e.sizeBytes ?? 0)}
            </span>
          </button>
        ))}
        {d?.truncated ? (
          <p className="px-2 py-2 text-xs text-ink-muted">…and {d.truncated} more entries not shown.</p>
        ) : null}
      </div>

      {/* New folder */}
      <div className="border-t border-hair px-4 py-2">
        {creating ? (
          <div className="flex items-center gap-2">
            <input autoFocus className={`${inputCls} flex-1`} placeholder="folder name" value={newName}
              onChange={(e) => setNewName(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && newName.trim() && mkdir.mutate()} />
            <input className={`${inputCls} w-28`} placeholder="uid:gid (opt.)" value={newOwner}
              onChange={(e) => setNewOwner(e.target.value)}
              title="Ownership for the new folder. Set it to the uid the app runs as — a root-owned folder is unwritable for an app running as uid 1000." />
            <button onClick={() => mkdir.mutate()} disabled={!newName.trim() || mkdir.isPending}
              className={`${btn} bg-accent text-accent-on hover:bg-accent-hover disabled:opacity-50`}>
              {mkdir.isPending ? "Creating…" : "Create"}
            </button>
            <button onClick={() => { setCreating(false); setError(""); }}
              className={`${btn} border border-edge text-ink-muted hover:bg-fog-800`}>
              Cancel
            </button>
          </div>
        ) : (
          <button onClick={() => setCreating(true)} className="text-xs font-medium text-ink-muted hover:text-ink">
            + New folder
          </button>
        )}
        {error && <p className="mt-1 break-words text-xs text-warn">{error}</p>}
      </div>

      {/* Footer */}
      <div className="flex items-center justify-between gap-3 border-t border-hair px-4 py-3">
        <span className="min-w-0 truncate font-mono text-xs text-ink-muted">{path}</span>
        <div className="flex shrink-0 gap-2">
          <button onClick={onClose} className={`${btn} border border-edge text-ink-muted hover:bg-fog-800`}>
            Cancel
          </button>
          <button onClick={() => onSelect(path)} className={`${btn} bg-accent font-semibold text-accent-on hover:bg-accent-hover`}>
            Select this folder
          </button>
        </div>
      </div>
    </Modal>
  );
}
