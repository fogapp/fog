// The clock menu: every night fog has a copy of, newest first, and the
// way to look at one. Choosing a snapshot re-opens the current folder as
// it was then; the page goes read-only and grows a Restore. Also the
// "Snapshot now" for the impatient, and a pointer at the policy when
// backups are off.

import { useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";
import { get, post, type FileSnapshot, type OffsiteFilesResponse, type OffsiteFilesSnapshot } from "../../api";
import { Spinner } from "../../components/Async";
import { formatBytes, timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { useDismissable } from "../../lib/useDismissable";
import { useSnapshots } from "../../lib/useFiles";

export default function SnapshotMenu({
  at,
  onPick,
  onSnapshotNow,
  className,
}: {
  at?: number;
  onPick: (s: FileSnapshot | null) => void;
  onSnapshotNow: () => void;
  className: string;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useDismissable(ref, open, () => setOpen(false));
  const q = useSnapshots(open || at !== undefined);
  const list = q.data?.snapshots ?? [];
  const st = q.data?.status;
  const current = list.find((s) => s.id === at);
  // Snapshots that exist only in an offsite locker: this box never made
  // them, or pruned its copy. They restore whole, into Files, as a job.
  const offsite = useQuery({
    queryKey: ["offsite-files"],
    queryFn: () => get<OffsiteFilesResponse>("/api/offsite/files"),
    enabled: open,
    staleTime: 60_000,
  });
  const cloudOnly = (offsite.data?.snapshots ?? []).filter((s) => !s.local);
  const [cloudNote, setCloudNote] = useState("");
  const restoreCloud = useMutation({
    mutationFn: (s: OffsiteFilesSnapshot) => post("/api/offsite/files/restore", { target: s.target, stamp: s.stamp }),
    onSuccess: () => { setCloudNote("Restore started — watch the jobs tray."); },
    onError: (e) => setCloudNote(e instanceof Error ? e.message : "Restore failed to start"),
  });

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="menu"
        aria-expanded={open}
        className={`${className} ${at ? "border-warn/30 text-warn" : ""}`}
        title="Browse a snapshot"
      >
        ⏱ {current ? new Date(current.createdAt).toLocaleDateString(undefined, { month: "short", day: "numeric" }) : "Today"} ▾
      </button>
      {open && (
        <div role="menu" className="absolute left-0 top-full z-40 mt-1 w-80 rounded-lg border border-veil-edge bg-fog-900 py-1 text-xs shadow-2xl shadow-black/40">
          <button
            role="menuitem"
            onClick={() => { onPick(null); setOpen(false); }}
            className={`flex w-full items-center justify-between px-3 py-1.5 text-left hover:bg-fog-800 ${at ? "text-ink" : "font-semibold text-ink"}`}
          >
            <span>Today (live)</span>
          </button>
          <div className="my-1 border-t border-hair" />
          {q.isPending ? (
            <p className="flex items-center gap-2 px-3 py-2 text-ink-muted"><Spinner /> Reading snapshots…</p>
          ) : q.isError ? (
            <p className="px-3 py-2 text-warn">Could not read the snapshots.</p>
          ) : list.length === 0 ? (
            <p className="px-3 py-2 text-ink-muted">
              {st?.enabled === false ? (
                <>Drive backups are off. <Link to={paths.settings("storage/backups", "policy")} className="underline">Policy</Link></>
              ) : st?.hourUTC === null ? (
                <>No snapshots yet — set a backup hour in <Link to={paths.settings("storage/backups", "policy")} className="underline">Policy</Link>, or take one now.</>
              ) : (
                "No snapshots yet."
              )}
            </p>
          ) : (
            <ul className="max-h-72 overflow-y-auto">
              {list.map((s) => (
                <li key={s.id}>
                  <button
                    role="menuitem"
                    onClick={() => { onPick(s); setOpen(false); }}
                    className={`flex w-full items-center justify-between gap-3 px-3 py-1.5 text-left hover:bg-fog-800 ${s.id === at ? "font-semibold text-ink" : "text-ink"}`}
                  >
                    <span>
                      {new Date(s.createdAt).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}
                      <span className="text-ink-muted"> · {timeAgo(s.createdAt)}</span>
                    </span>
                    <span className="shrink-0 text-ink-muted">
                      {s.files.toLocaleString()} files · {formatBytes(s.bytes)}
                      {s.mode === "mirror" ? " · mirror" : ""}
                      {s.warnings > 0 ? ` · ${s.warnings} torn` : ""}
                      {s.offsite === "stored" || s.offsite === "verified" ? <span title={s.offsite === "verified" ? "verified offsite" : "copied offsite"}> · ☁</span> : ""}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
          {cloudOnly.length > 0 && (
            <>
              <div className="my-1 border-t border-hair" />
              <p className="group-label px-3 pb-1 pt-1.5">In the locker only</p>
              <ul className="max-h-40 overflow-y-auto">
                {cloudOnly.map((s) => (
                  <li key={s.target + s.stamp} className="flex items-center justify-between gap-3 px-3 py-1.5 text-ink-muted">
                    <span>
                      ☁ {new Date(s.createdAt).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}
                      <span className="text-ink-muted"> · {s.files.toLocaleString()} files · {formatBytes(s.bytes)} · {s.target}</span>
                    </span>
                    <button
                      role="menuitem"
                      disabled={restoreCloud.isPending}
                      onClick={() => {
                        if (confirm(`Restore every file from the ${s.target} copy of ${s.stamp} into Drive? Files that differ are overwritten; nothing is deleted.`)) {
                          restoreCloud.mutate(s);
                        }
                      }}
                      className="shrink-0 rounded border border-edge px-2 py-0.5 font-medium text-ink hover:bg-fog-800 disabled:opacity-50"
                    >
                      Restore all
                    </button>
                  </li>
                ))}
              </ul>
              {cloudNote && <p className="px-3 py-1 text-ink-muted">{cloudNote}</p>}
            </>
          )}
          <div className="my-1 border-t border-hair" />
          <div className="flex items-center justify-between px-3 py-1.5">
            <button
              role="menuitem"
              onClick={() => { onSnapshotNow(); setOpen(false); }}
              disabled={!!st?.running || st?.enabled === false}
              className="rounded border border-edge px-2 py-0.5 font-medium text-ink hover:bg-fog-800 disabled:opacity-50"
            >
              {st?.running ? "Snapshot running…" : "Snapshot now"}
            </button>
            <Link to={paths.settings("storage/backups")} className="text-ink-muted hover:text-ink">Backups ›</Link>
          </div>
        </div>
      )}
    </div>
  );
}
